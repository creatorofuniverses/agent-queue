package notify

import (
	"context"
	"fmt"
	"github.com/creatorofuniverses/agent-queue/internal/config"
	"github.com/creatorofuniverses/agent-queue/internal/model"
	"github.com/creatorofuniverses/agent-queue/internal/store"
	"log/slog"
	"math/rand/v2"
	"path/filepath"
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
		if it, err = o.st.Get(ctx, e.ItemID); err == store.ErrNotFound {
			err = nil
		} else if err != nil {
			err = fmt.Errorf("cannot read item: %w", err) // an ordinary failed attempt: backoff, then give up
		}
	}
	msg := Message{Event: *e, Item: it}
	if it != nil && !it.IsReservation() {
		msg.StdoutPath = filepath.Join(o.paths.JobDir(it.ID), "stdout.log")
	}
	d := o.deliverers[ch]
	switch {
	case err != nil:
	case d == nil:
		err = fmt.Errorf("channel %s is not configured", ch)
	default:
		attempt, cancel := context.WithTimeout(ctx, o.cfg.Notify.DeliveryTimeout)
		err = d.Deliver(attempt, msg)
		cancel()
	}
	now := o.now()
	if err == nil {
		// A finished delivery is recorded even during shutdown, so restart does not repeat it.
		ok := o.record(ctx, "mark sent", e.ID, func(c context.Context) error { return o.st.MarkSent(c, e.ID, now) })
		return ok && ctx.Err() == nil
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
		if !o.record(ctx, "mark failed", e.ID, func(c context.Context) error { return o.st.MarkFailed(c, e.ID, attempts, lastErr, evs) }) {
			return false
		}
		o.Kick()
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
		if !o.record(ctx, "mark retry", e.ID, func(c context.Context) error { return o.st.MarkRetry(c, e.ID, attempts, now.Add(delay), lastErr) }) {
			return false
		}
	}
	return true
}

// record writes a delivery outcome, retrying until it lands: ClaimDue only sees pending
// rows, so a lost outcome would strand the event in delivering until a restart. The
// writes are no-ops once the row left delivering, so repeating one is safe. During
// shutdown it tries once; ResetDelivering picks up whatever is left.
func (o *Outbox) record(ctx context.Context, what, id string, write func(context.Context) error) bool {
	for delay := o.cfg.Notify.RetryMin; ; delay = min(2*delay, o.cfg.Notify.RetryMax) {
		err := write(context.WithoutCancel(ctx))
		if err == nil {
			return true
		}
		slog.Error("outbox: "+what, "event", id, "err", err)
		select {
		case <-ctx.Done():
			return false
		case <-time.After(delay):
		}
	}
}
