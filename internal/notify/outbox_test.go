package notify

import (
	"context"
	"database/sql"
	"errors"
	"github.com/creatorofuniverses/agent-queue/internal/config"
	"github.com/creatorofuniverses/agent-queue/internal/model"
	"github.com/creatorofuniverses/agent-queue/internal/store"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type deliverFunc func(context.Context, Message) error

func (f deliverFunc) Deliver(ctx context.Context, m Message) error { return f(ctx, m) }
func outboxStore(t *testing.T) *store.Store {
	t.Helper()
	s, e := store.Open(filepath.Join(t.TempDir(), "aq.db"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
func put(t *testing.T, s *store.Store, id string, ch model.Channel, at time.Time, aud model.Audience) {
	t.Helper()
	e := s.AddEvents(context.Background(), []model.Event{{ID: id, ItemID: "i", Channel: ch, Status: model.EventPending, Audience: aud, CreatedAt: at, Kind: model.EventTaskFinished}})
	if e != nil {
		t.Fatal(e)
	}
}
func eventStatus(s *store.Store, id string) model.Event {
	evs, _ := s.Events(context.Background(), "i")
	for _, e := range evs {
		if e.ID == id {
			return e
		}
	}
	return model.Event{}
}
func await(t *testing.T, f func() bool) {
	t.Helper()
	end := time.Now().Add(3 * time.Second)
	for !f() {
		if time.Now().After(end) {
			t.Fatal("condition timed out")
		}
		time.Sleep(time.Millisecond)
	}
}
func runBox(t *testing.T, o *Outbox) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); o.Run(ctx) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Error("outbox did not stop")
			}
		})
	}
	t.Cleanup(stop)
	return stop
}
func TestOutboxDeliversInOrder(t *testing.T) {
	s := outboxStore(t)
	ch := model.ExecChannel("codex")
	at := time.Now().Add(-time.Second)
	for _, id := range []string{"a", "b", "c"} {
		put(t, s, id, ch, at, model.AudienceSession)
	}
	var mu sync.Mutex
	var ids []string
	d := deliverFunc(func(_ context.Context, m Message) error {
		mu.Lock()
		defer mu.Unlock()
		ids = append(ids, m.Event.ID)
		return nil
	})
	o := NewOutbox(s, config.Default(), config.Paths{}, NewRouter(config.Default()), map[model.Channel]Deliverer{ch: d}, time.Now)
	runBox(t, o)
	await(t, func() bool { return eventStatus(s, "c").Status == model.EventSent })
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(ids, []string{"a", "b", "c"}) {
		t.Fatal(ids)
	}
	if eventStatus(s, "a").SentAt.IsZero() {
		t.Fatal("missing sent_at")
	}
}
func TestOutboxRetryBackoff(t *testing.T) {
	s := outboxStore(t)
	at := time.Now().UTC()
	ch := model.ExecChannel("codex")
	put(t, s, "e", ch, at, model.AudienceSession)
	n := 0
	var ids []string
	d := deliverFunc(func(_ context.Context, m Message) error {
		n++
		ids = append(ids, m.Event.ID)
		if n < 3 {
			return errors.New("boom")
		}
		return nil
	})
	o := NewOutbox(s, config.Default(), config.Paths{}, NewRouter(config.Default()), map[model.Channel]Deliverer{ch: d}, func() time.Time { return at })
	o.jitter = func() float64 { return 1 }
	for i := 1; i <= 3; i++ {
		if !o.deliverOne(context.Background(), ch) {
			t.Fatal("not due")
		}
		e := eventStatus(s, "e")
		if i < 3 {
			delay := time.Duration(1<<(i-1)) * time.Second
			if e.Attempts != i || e.NextAttemptAt.Sub(at) != delay {
				t.Fatal(e)
			}
			at = e.NextAttemptAt
		} else if e.Status != model.EventSent {
			t.Fatal(e)
		}
	}
	if !reflect.DeepEqual(ids, []string{"e", "e", "e"}) {
		t.Fatal(ids)
	}
}
func TestOutboxGivesUp(t *testing.T) {
	for _, aud := range []model.Audience{model.AudienceSession, model.AudienceOperator} {
		s := outboxStore(t)
		cfg := config.Default()
		cfg.Webhook.URL = "http://x"
		at := time.Now()
		put(t, s, "e", model.ChannelWebhook, at.Add(-25*time.Hour), aud)
		o := NewOutbox(s, cfg, config.Paths{}, NewRouter(cfg), map[model.Channel]Deliverer{model.ChannelWebhook: deliverFunc(func(context.Context, Message) error { return errors.New("boom") })}, func() time.Time { return at })
		o.deliverOne(context.Background(), model.ChannelWebhook)
		evs, _ := s.Events(context.Background(), "i")
		want := 1
		if aud == model.AudienceSession {
			want = 2
		}
		if len(evs) != want || evs[0].Status != model.EventFailed {
			t.Fatal(evs)
		}
		if want == 2 && (evs[1].Kind != model.EventDeliveryFailed || evs[1].Channel != model.ChannelWebhook) {
			t.Fatal(evs)
		}
	}
}
func TestOutboxLanesIndependent(t *testing.T) {
	s := outboxStore(t)
	at := time.Now()
	put(t, s, "w", model.ChannelWebhook, at, model.AudienceOperator)
	put(t, s, "e", model.ExecChannel("codex"), at, model.AudienceSession)
	entered := make(chan struct{})
	done := make(chan struct{})
	o := NewOutbox(s, config.Default(), config.Paths{}, NewRouter(config.Default()), map[model.Channel]Deliverer{model.ChannelWebhook: deliverFunc(func(ctx context.Context, _ Message) error { close(entered); <-ctx.Done(); return ctx.Err() }), model.ExecChannel("codex"): deliverFunc(func(context.Context, Message) error { close(done); return nil })}, time.Now)
	runBox(t, o)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("webhook not started")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("blocked lane")
	}
}
func TestAckedPendingNotDelivered(t *testing.T) {
	s := outboxStore(t)
	ch := model.ExecChannel("codex")
	put(t, s, "e", ch, time.Now(), model.AudienceSession)
	s.Ack(context.Background(), "e", time.Now())
	o := NewOutbox(s, config.Default(), config.Paths{}, NewRouter(config.Default()), map[model.Channel]Deliverer{ch: deliverFunc(func(context.Context, Message) error { t.Error("delivered acked"); return nil })}, time.Now)
	if o.deliverOne(context.Background(), ch) {
		t.Fatal("claimed acked")
	}
}
func TestAckWhileEarlierEventBlocks(t *testing.T) { testAckWhileBlocked(t, false) }
func TestAckAfterClaimStaysAcked(t *testing.T)    { testAckWhileBlocked(t, true) }
func testAckWhileBlocked(t *testing.T, ackFirst bool) {
	s := outboxStore(t)
	ch := model.ExecChannel("codex")
	at := time.Now()
	put(t, s, "a", ch, at, model.AudienceSession)
	put(t, s, "b", ch, at, model.AudienceSession)
	entered := make(chan struct{})
	release := make(chan struct{})
	var mu sync.Mutex
	var ids []string
	d := deliverFunc(func(ctx context.Context, m Message) error {
		mu.Lock()
		ids = append(ids, m.Event.ID)
		mu.Unlock()
		if m.Event.ID == "a" {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	})
	o := NewOutbox(s, config.Default(), config.Paths{}, NewRouter(config.Default()), map[model.Channel]Deliverer{ch: d}, time.Now)
	stop := runBox(t, o)
	<-entered
	id := "b"
	if ackFirst {
		id = "a"
	}
	s.Ack(context.Background(), id, time.Now())
	close(release)
	other := "a"
	if ackFirst {
		other = "b"
	}
	await(t, func() bool { return eventStatus(s, other).Status == model.EventSent })
	stop()
	if eventStatus(s, id).Status != model.EventAcked {
		t.Fatal(eventStatus(s, id))
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{"a"}
	if ackFirst {
		want = append(want, "b")
	}
	if !reflect.DeepEqual(ids, want) {
		t.Fatal(ids)
	}
}
func TestOrphanChannelFails(t *testing.T) {
	s := outboxStore(t)
	put(t, s, "e", model.ExecChannel("gone"), time.Now(), model.AudienceSession)
	o := NewOutbox(s, config.Default(), config.Paths{}, NewRouter(config.Default()), nil, time.Now)
	runBox(t, o)
	await(t, func() bool { return eventStatus(s, "e").Status == model.EventFailed })
	if e := eventStatus(s, "e"); e.LastError != "channel exec:gone is not configured" {
		t.Fatal(e)
	}
	evs, _ := s.Events(context.Background(), "i")
	if len(evs) != 2 || evs[1].Kind != model.EventDeliveryFailed {
		t.Fatal(evs)
	}
}
func TestOutboxMarksSentDuringShutdown(t *testing.T) {
	s := outboxStore(t)
	ch := model.ExecChannel("codex")
	put(t, s, "e", ch, time.Now(), model.AudienceSession)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	o := NewOutbox(s, config.Default(), config.Paths{}, NewRouter(config.Default()), map[model.Channel]Deliverer{ch: deliverFunc(func(context.Context, Message) error { cancel(); return nil })}, time.Now)
	o.deliverOne(ctx, ch)
	if e := eventStatus(s, "e"); e.Status != model.EventSent {
		t.Fatal(e)
	}
}
func TestOutboxRetriesFailedStart(t *testing.T) {
	s := outboxStore(t)
	s.Close() // every store call fails
	o := NewOutbox(s, config.Default(), config.Paths{}, NewRouter(config.Default()), nil, time.Now)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); o.Run(ctx) }()
	select {
	case <-done:
		t.Fatal("outbox gave up after a failed start")
	case <-time.After(100 * time.Millisecond):
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("outbox did not stop")
	}
}
func TestOutboxRecordRetriesOutcomeWrite(t *testing.T) {
	cfg := config.Default()
	cfg.Notify.RetryMin = time.Millisecond
	o := NewOutbox(outboxStore(t), cfg, config.Paths{}, NewRouter(cfg), nil, time.Now)
	calls := 0
	ok := o.record(context.Background(), "mark retry", "e", func(context.Context) error {
		if calls++; calls < 3 {
			return errors.New("database is locked")
		}
		return nil
	})
	if !ok || calls != 3 {
		t.Fatal(ok, calls)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls = 0
	if o.record(ctx, "mark sent", "e", func(context.Context) error { calls++; return errors.New("x") }) || calls != 1 {
		t.Fatal("shutdown: want one attempt and false", calls)
	}
}
func TestOutboxItemReadFailureIsAnAttempt(t *testing.T) {
	p := filepath.Join(t.TempDir(), "aq.db")
	s, e := store.Open(p)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	ctx := context.Background()
	if _, _, e = s.InsertItem(ctx, &model.Item{ID: "i", Kind: model.KindTask, State: model.StateSucceeded, Argv: []string{"true"}, CreatedAt: time.Now()}); e != nil {
		t.Fatal(e)
	}
	db, e := sql.Open("sqlite", p)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	if _, e = db.Exec(`UPDATE items SET argv='{' WHERE id='i'`); e != nil { // Get now fails to decode
		t.Fatal(e)
	}
	ch := model.ExecChannel("codex")
	put(t, s, "e", ch, time.Now(), model.AudienceSession)
	delivered := false
	o := NewOutbox(s, config.Default(), config.Paths{}, NewRouter(config.Default()), map[model.Channel]Deliverer{ch: deliverFunc(func(context.Context, Message) error { delivered = true; return nil })}, time.Now)
	o.jitter = func() float64 { return 1 }
	o.deliverOne(ctx, ch)
	ev := eventStatus(s, "e")
	if delivered || ev.Status != model.EventPending || ev.Attempts != 1 || !strings.HasPrefix(ev.LastError, "cannot read item") {
		t.Fatal(delivered, ev)
	}
}
func TestOutboxRedeliversAfterRestart(t *testing.T) {
	s := outboxStore(t)
	ch := model.ExecChannel("codex")
	put(t, s, "e", ch, time.Now(), model.AudienceSession)
	entered := make(chan struct{})
	o := NewOutbox(s, config.Default(), config.Paths{}, NewRouter(config.Default()), map[model.Channel]Deliverer{ch: deliverFunc(func(ctx context.Context, _ Message) error { close(entered); <-ctx.Done(); return ctx.Err() })}, time.Now)
	stop := runBox(t, o)
	<-entered
	stop()
	if eventStatus(s, "e").Status != model.EventDelivering {
		t.Fatal(eventStatus(s, "e"))
	}
	o = NewOutbox(s, config.Default(), config.Paths{}, NewRouter(config.Default()), map[model.Channel]Deliverer{ch: deliverFunc(func(_ context.Context, m Message) error {
		if m.Event.ID != "e" {
			t.Error(m.Event)
		}
		return nil
	})}, time.Now)
	runBox(t, o)
	await(t, func() bool { return eventStatus(s, "e").Status == model.EventSent })
}
