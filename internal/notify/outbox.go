package notify

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"path/filepath"
	"resource-dispatcher/internal/config"
	"resource-dispatcher/internal/model"
	"resource-dispatcher/internal/store"
	"sync"
	"time"
)

type Outbox struct {
	st         *store.Store
	cfg        config.Config
	paths      config.Paths
	router     *Router
	deliverers map[model.Channel]Deliverer
	now        func() time.Time
	jitter     func() float64 // multiplicative factor, normally uniform [0.8,1.2)
	mu         sync.Mutex
	wakes      map[model.Channel]chan struct{}
}

func NewOutbox(st *store.Store, cfg config.Config, paths config.Paths, router *Router, deliverers map[model.Channel]Deliverer, now func() time.Time) *Outbox {
	if now == nil {
		now = time.Now
	}
	o := &Outbox{st: st, cfg: cfg, paths: paths, router: router, deliverers: deliverers, now: now, jitter: func() float64 { return .8 + .4*rand.Float64() }, wakes: map[model.Channel]chan struct{}{}}
	for ch := range deliverers {
		if ch.Retried() {
			o.wakes[ch] = make(chan struct{}, 1)
		}
	}
	return o
}
func (o *Outbox) Kick() {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, wake := range o.wakes {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
}
func (o *Outbox) Run(ctx context.Context) {
	chs, err := o.start(ctx)
	for delay := time.Second; err != nil; delay = min(2*delay, o.cfg.Notify.RetryMax) {
		slog.Error("outbox: start", "err", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		chs, err = o.start(ctx)
	}
	o.mu.Lock()
	for _, ch := range chs {
		if _, ok := o.wakes[ch]; !ok {
			o.wakes[ch] = make(chan struct{}, 1)
		}
	}
	var wg sync.WaitGroup
	for ch, wake := range o.wakes {
		wg.Add(1)
		go func() { defer wg.Done(); o.lane(ctx, ch, wake) }()
	}
	o.mu.Unlock()
	wg.Wait()
}

// start releases claims left by a previous run and lists channels with persisted work.
func (o *Outbox) start(ctx context.Context) ([]model.Channel, error) {
	if _, err := o.st.ResetDelivering(ctx); err != nil {
		return nil, fmt.Errorf("reset claims: %w", err)
	}
	return o.st.PendingChannels(ctx)
}
func (o *Outbox) lane(ctx context.Context, ch model.Channel, wake <-chan struct{}) {
	for ctx.Err() == nil {
		if o.deliverOne(ctx, ch) {
			continue
		}
		delay := 30 * time.Second
		if next, ok, err := o.st.NextDue(ctx, ch); err != nil {
			slog.Error("outbox: next due", "channel", ch, "err", err)
		} else if ok {
			delay = min(delay, max(time.Second, next.Sub(o.now())))
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-wake:
			timer.Stop()
		case <-timer.C:
		}
	}
}
func (o *Outbox) deliverOne(ctx context.Context, ch model.Channel) bool {
	claimAt := o.now()
	if o.deliverers[ch] == nil {
		claimAt = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	} // orphaned retries fail immediately
	e, err := o.st.ClaimDue(ctx, ch, claimAt)
	if err != nil {
		slog.Error("outbox: claim", "channel", ch, "err", err)
		return false
	}
	if e == nil {
		return false
	}
	var it *model.Item
	if e.ItemID != "" {
		it, err = o.st.Get(ctx, e.ItemID)
		if err != nil && err != store.ErrNotFound {
			if ctx.Err() == nil {
				_ = o.st.MarkRetry(ctx, e.ID, e.Attempts, o.now().Add(time.Second), "cannot read item")
			}
			return false
		}
	}
	msg := Message{Event: *e, Item: it}
	if it != nil && !it.IsReservation() {
		msg.StdoutPath = filepath.Join(o.paths.JobDir(it.ID), "stdout.log")
	}
	d := o.deliverers[ch]
	if d == nil {
		err = fmt.Errorf("channel %s is not configured", ch)
	} else {
		attempt, cancel := context.WithTimeout(ctx, o.cfg.Notify.DeliveryTimeout)
		err = d.Deliver(attempt, msg)
		cancel()
	}
	now := o.now()
	if err == nil {
		// A finished delivery is recorded even during shutdown, so restart does not repeat it.
		if err = o.st.MarkSent(context.WithoutCancel(ctx), e.ID, now); err != nil {
			slog.Error("outbox: mark sent", "event", e.ID, "err", err)
		}
		return err == nil && ctx.Err() == nil
	}
	// Shutdown leaves a failed claim durable; restart retries the same event ID.
	if ctx.Err() != nil {
		return false
	}
	attempts := e.Attempts + 1
	lastErr := err.Error()
	if len(lastErr) > 4096 {
		lastErr = lastErr[:4096]
	}
	if d == nil || now.Sub(e.CreatedAt) >= o.cfg.Notify.GiveUpAfter {
		var evs []model.Event
		if e.Audience != model.AudienceOperator {
			ch := o.router.Route("", model.AudienceOperator)
			status := model.EventPending
			if ch == model.ChannelNone {
				status = model.EventSkipped
			}
			evs = []model.Event{{ID: string(model.NewID()), ItemID: e.ItemID, Kind: model.EventDeliveryFailed, Audience: model.AudienceOperator, Channel: ch, Status: status, Detail: fmt.Sprintf("event %s (%s): %s", e.ID, e.Channel, lastErr), CreatedAt: now}}
		} else {
			slog.Error("outbox: operator delivery failed", "event", e.ID, "err", lastErr)
		}
		if err = o.st.MarkFailed(ctx, e.ID, attempts, lastErr, evs); err != nil {
			slog.Error("outbox: mark failed", "event", e.ID, "err", err)
		} else {
			o.Kick()
		}
	} else {
		delay := o.cfg.Notify.RetryMin
		for n := 1; n < attempts && delay < o.cfg.Notify.RetryMax; n++ {
			if delay > o.cfg.Notify.RetryMax/2 {
				delay = o.cfg.Notify.RetryMax
			} else {
				delay *= 2
			}
		}
		delay = time.Duration(float64(min(delay, o.cfg.Notify.RetryMax)) * o.jitter())
		if err = o.st.MarkRetry(ctx, e.ID, attempts, now.Add(delay), lastErr); err != nil {
			slog.Error("outbox: mark retry", "event", e.ID, "err", err)
		}
	}
	return err == nil
}
