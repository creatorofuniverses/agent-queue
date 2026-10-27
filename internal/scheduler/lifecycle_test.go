package scheduler

import (
	"errors"
	"strings"
	"testing"
	"time"

	"resource-dispatcher/internal/model"
)

// grantTTL reserves stand + budget for ttl and grants it with one Tick; t0 is the grant time.
func (e *env) grantTTL(t *testing.T, ttl time.Duration, session string) (*model.Item, time.Time) {
	t.Helper()
	r := e.rreq(100 * mib)
	r.TaskMemory, r.TaskCPUs, r.TTL, r.SessionID = 500*mib, 2, ttl, session
	res := e.reserve(t, r)
	t0 := e.clk.Now()
	e.tick(t)
	it := e.wantState(t, res.ID, model.StateGranted, model.ReasonNone)
	if !it.ExpiresAt.Equal(t0.Add(ttl)) {
		t.Fatalf("expires %v, want %v", it.ExpiresAt, t0.Add(ttl))
	}
	return it, t0
}

// tickAt moves the clock to at and ticks.
func (e *env) tickAt(t *testing.T, at time.Time) {
	t.Helper()
	e.clk.Advance(at.Sub(e.clk.Now()))
	e.tick(t)
}

func wantBadState(t *testing.T, err error, text string) {
	t.Helper()
	if !errors.Is(err, ErrBadState) || !strings.Contains(err.Error(), text) {
		t.Fatalf("err %v, want ErrBadState containing %q", err, text)
	}
}

func TestReleaseFreesPool(t *testing.T) {
	e := newEnv(t, nil)
	res := e.reserve(t, e.rreq(gib))
	e.tick(t)
	e.wantState(t, res.ID, model.StateGranted, model.ReasonNone)
	task := e.submit(t, e.req(gib))
	e.tick(t)
	e.wantState(t, task.ID, model.StateQueued, model.ReasonNone)

	got, err := e.s.Release(e.ctx, res.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != model.StateReleased || got.Env != nil {
		t.Fatalf("released %s env %v", got.State, got.Env)
	}
	it := e.wantState(t, res.ID, model.StateReleased, model.ReasonNone)
	if it.Env != nil || it.EndedAt.IsZero() {
		t.Fatalf("stored env %v ended %v", it.Env, it.EndedAt)
	}
	e.tick(t)
	e.wantState(t, task.ID, model.StateRunning, model.ReasonNone)
	if ids := launchedIDs(e.ex); len(ids) != 1 || ids[0] != task.ID {
		t.Fatalf("launched %v, want only the task (no teardown)", ids)
	}
}

func TestReleaseRefusedWithNestedTasks(t *testing.T) {
	e := newEnv(t, nil)
	res := e.grantBudget(t, 100*mib, 500*mib, 2)
	nested := e.submit(t, e.within(res.ID, 100*mib))
	e.tick(t)
	e.wantState(t, nested.ID, model.StateRunning, model.ReasonNone)

	for _, force := range []bool{false, true} {
		_, err := e.s.Release(e.ctx, res.ID, force)
		wantBadState(t, err, "nested")
		e.wantState(t, res.ID, model.StateGranted, model.ReasonNone)
	}
	// A queued nested task holds the reservation too.
	e.finish(nested.ID, success)
	e.tick(t)
	queued := e.submit(t, e.within(res.ID, 100*mib))
	_, err := e.s.Release(e.ctx, res.ID, false)
	wantBadState(t, err, "nested")
	e.wantState(t, queued.ID, model.StateQueued, model.ReasonNone)
}

func TestReleaseQueuedCancels(t *testing.T) {
	e := newEnv(t, nil)
	res := e.reserve(t, e.rreq(gib))
	got, err := e.s.Release(e.ctx, res.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != model.StateCancelled {
		t.Fatalf("state %s, want CANCELLED", got.State)
	}
	e.wantState(t, res.ID, model.StateCancelled, model.ReasonCancelled)
}

func TestReleaseTwiceIdempotent(t *testing.T) {
	e := newEnv(t, nil)
	res := e.reserve(t, e.rreq(gib))
	e.tick(t)
	first, err := e.s.Release(e.ctx, res.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(time.Minute)
	second, err := e.s.Release(e.ctx, res.ID, false)
	if err != nil {
		t.Fatalf("second release: %v", err)
	}
	if second.State != model.StateReleased || !second.EndedAt.Equal(first.EndedAt) {
		t.Fatalf("second %s ended %v, want RELEASED ended %v", second.State, second.EndedAt, first.EndedAt)
	}
}

func TestReleaseTask(t *testing.T) {
	e := newEnv(t, nil)
	task := e.submit(t, e.req(100*mib))
	_, err := e.s.Release(e.ctx, task.ID, false)
	wantBadState(t, err, "aq cancel")
	e.wantState(t, task.ID, model.StateQueued, model.ReasonNone)
}

func TestCancelGrantedReservation(t *testing.T) {
	e := newEnv(t, nil)
	res := e.reserve(t, e.rreq(500*mib))
	e.tick(t)
	_, err := e.s.Cancel(e.ctx, res.ID, "x")
	wantBadState(t, err, "aq release")
	e.wantState(t, res.ID, model.StateGranted, model.ReasonNone)

	queued := e.reserve(t, e.rreq(600*mib))
	e.tick(t)
	got, err := e.s.Cancel(e.ctx, queued.ID, "x")
	if err != nil || got.State != model.StateCancelled {
		t.Fatalf("cancel queued: %v %v", got, err)
	}
	e.wantState(t, queued.ID, model.StateCancelled, model.ReasonCancelled)
	if got, err := e.s.Cancel(e.ctx, queued.ID, "again"); err != nil || got.State != model.StateCancelled {
		t.Fatalf("cancel terminal: %v %v", got, err)
	}
}

func TestExtendNotCumulative(t *testing.T) {
	e := newEnv(t, nil)
	res, t0 := e.grantTTL(t, time.Hour, "")
	e.clk.Advance(10 * time.Minute)
	for i := 0; i < 2; i++ {
		got, err := e.s.Extend(e.ctx, res.ID, 2*time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if want := t0.Add(2*time.Hour + 10*time.Minute); !got.ExpiresAt.Equal(want) {
			t.Fatalf("extend #%d: expires %v, want %v", i+1, got.ExpiresAt, want)
		}
	}
	got, err := e.s.Extend(e.ctx, res.ID, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if want := t0.Add(2*time.Hour + 10*time.Minute); !got.ExpiresAt.Equal(want) {
		t.Fatalf("shorter extend: expires %v, want %v", got.ExpiresAt, want)
	}
	before := e.get(t, res.ID)
	for _, ttl := range []time.Duration{25 * time.Hour, 0, -time.Minute} {
		if _, err := e.s.Extend(e.ctx, res.ID, ttl); !errors.Is(err, ErrRejected) {
			t.Fatalf("extend %s: err %v, want ErrRejected", ttl, err)
		}
	}
	if after := e.get(t, res.ID); !after.ExpiresAt.Equal(before.ExpiresAt) || !after.WarnAt.Equal(before.WarnAt) || after.State != before.State {
		t.Fatalf("rejected extend changed the item: %+v", after)
	}

	queued := e.reserve(t, e.rreq(gib))
	e.tick(t)
	e.wantState(t, queued.ID, model.StateQueued, model.ReasonNone)
	_, err = e.s.Extend(e.ctx, queued.ID, time.Hour)
	wantBadState(t, err, "QUEUED")
}

func TestExpiryWarning(t *testing.T) {
	e := newEnv(t, nil)
	res, t0 := e.grantTTL(t, time.Hour, "sess-1")
	e.tickAt(t, t0.Add(44*time.Minute))
	e.wantState(t, res.ID, model.StateGranted, model.ReasonNone)
	if evs := e.events(t, res.ID, model.EventReservationExpiring); len(evs) != 0 {
		t.Fatalf("early events %+v", evs)
	}

	e.tickAt(t, t0.Add(45*time.Minute))
	e.wantState(t, res.ID, model.StateExpiring, model.ReasonNone)
	evs := e.events(t, res.ID, model.EventReservationExpiring)
	if len(evs) != 2 {
		t.Fatalf("events %+v, want session + operator", evs)
	}
	aud := map[model.Audience]bool{}
	for _, ev := range evs {
		aud[ev.Audience] = true
	}
	if !aud[model.AudienceSession] || !aud[model.AudienceOperator] {
		t.Fatalf("audiences %v", aud)
	}
	e.tick(t)
	if n := len(e.events(t, res.ID, model.EventReservationExpiring)); n != 2 {
		t.Fatalf("%d expiring events after another tick, want 2", n)
	}

	got, err := e.s.Extend(e.ctx, res.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	it := e.wantState(t, res.ID, model.StateGranted, model.ReasonNone)
	if want := t0.Add(time.Hour + 30*time.Minute); !it.WarnAt.Equal(want) || !got.WarnAt.Equal(want) {
		t.Fatalf("warn at %v, want %v", it.WarnAt, want)
	}
}

func TestShortTTLNoWarning(t *testing.T) {
	e := newEnv(t, nil)
	res, t0 := e.grantTTL(t, 10*time.Minute, "sess-1")
	for _, d := range []time.Duration{time.Minute, 5 * time.Minute, 10*time.Minute - time.Second} {
		e.tickAt(t, t0.Add(d))
		e.wantState(t, res.ID, model.StateGranted, model.ReasonNone)
	}
	if evs := e.events(t, res.ID, model.EventReservationExpiring); len(evs) != 0 {
		t.Fatalf("events %+v, want none", evs)
	}
	// Expired with no nested tasks: due for teardown (a later task), not held.
	e.tickAt(t, t0.Add(10*time.Minute))
	it := e.get(t, res.ID)
	if it.TTLHeld || !it.ExpiresAt.Equal(t0.Add(10*time.Minute)) {
		t.Fatalf("held %v expires %v", it.TTLHeld, it.ExpiresAt)
	}
	if evs := e.events(t, res.ID, model.EventReservationHeldByTask); len(evs) != 0 {
		t.Fatalf("held events %+v", evs)
	}
}

func TestExpireLockedReturnsDue(t *testing.T) {
	e := newEnv(t, nil)
	due, t0 := e.grantTTL(t, time.Hour, "")
	held := e.grantBudget(t, 100*mib, 200*mib, 1)
	nested := e.submit(t, e.within(held.ID, 100*mib))
	e.tick(t)
	e.wantState(t, nested.ID, model.StateRunning, model.ReasonNone)
	later := e.grantBudget(t, 100*mib, 0, 0)
	if _, err := e.s.Extend(e.ctx, later.ID, 3*time.Hour); err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(t0.Add(2*time.Hour + time.Minute).Sub(e.clk.Now()))

	e.s.mu.Lock()
	items, err := e.st.ListActive(e.ctx)
	if err != nil {
		e.s.mu.Unlock()
		t.Fatal(err)
	}
	got := e.s.expireLocked(e.ctx, items, e.clk.Now())
	e.s.mu.Unlock()
	if len(got) != 1 || got[0].ID != due.ID {
		var ids []model.ItemID
		for _, it := range got {
			ids = append(ids, it.ID)
		}
		t.Fatalf("due %v, want only %s", ids, due.ID)
	}
	if it := e.get(t, held.ID); !it.TTLHeld {
		t.Fatal("reservation with a running nested task not held")
	}
}

func TestExpiryHeldByNestedTasks(t *testing.T) {
	e := newEnv(t, nil) // cancel_grace 10s
	res, t0 := e.grantTTL(t, time.Hour, "sess-1")
	e.clk.Advance(t0.Add(50 * time.Minute).Sub(e.clk.Now()))
	r := e.within(res.ID, 100*mib)
	r.Timeout = 30 * time.Minute
	nested, err := e.s.Submit(e.ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	e.tick(t)
	if it := e.wantState(t, nested.ID, model.StateRunning, model.ReasonNone); !it.StartedAt.Equal(t0.Add(50 * time.Minute)) {
		t.Fatalf("nested started %v", it.StartedAt)
	}

	e.tickAt(t, t0.Add(time.Hour))
	it := e.wantState(t, res.ID, model.StateExpiring, model.ReasonNone)
	if want := t0.Add(time.Hour + 20*time.Minute + 20*time.Second); !it.TTLHeld || !it.ExpiresAt.Equal(want) {
		t.Fatalf("held %v expires %v, want held until %v", it.TTLHeld, it.ExpiresAt, want)
	}
	evs := e.events(t, res.ID, model.EventReservationHeldByTask)
	if len(evs) != 1 || evs[0].Audience != model.AudienceOperator {
		t.Fatalf("held events %+v, want one for the operator", evs)
	}
	e.tickAt(t, t0.Add(time.Hour+time.Minute))
	if n := len(e.events(t, res.ID, model.EventReservationHeldByTask)); n != 1 {
		t.Fatalf("%d held events after another tick, want 1", n)
	}
	e.wantRejected(t, e.within(res.ID, 100*mib), "held past its TTL")

	e.finish(nested.ID, success)
	e.tick(t)
	e.wantState(t, nested.ID, model.StateSucceeded, model.ReasonNone)
	got, err := e.s.Extend(e.ctx, res.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if got.TTLHeld || got.State != model.StateGranted {
		t.Fatalf("after extend: held %v state %s", got.TTLHeld, got.State)
	}
	if it := e.get(t, res.ID); it.TTLHeld || it.State != model.StateGranted {
		t.Fatalf("stored: held %v state %s", it.TTLHeld, it.State)
	}
}

func TestHoldExtendsAgainWhenDeadlineMoves(t *testing.T) {
	e := newEnv(t, nil)
	res, t0 := e.grantTTL(t, time.Hour, "")
	e.clk.Advance(t0.Add(time.Hour - time.Minute).Sub(e.clk.Now()))
	r := e.within(res.ID, 100*mib)
	r.Timeout = 30 * time.Minute
	nested, err := e.s.Submit(e.ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	// Recovery keeps the nested task queued: its deadline counts from now.
	e.s.mu.Lock()
	e.s.recovering = true
	e.s.mu.Unlock()
	e.tickAt(t, t0.Add(time.Hour))
	e.wantState(t, nested.ID, model.StateQueued, model.ReasonNone)
	first := t0.Add(time.Hour + 30*time.Minute + 20*time.Second)
	if it := e.get(t, res.ID); !it.TTLHeld || !it.ExpiresAt.Equal(first) {
		t.Fatalf("held %v expires %v, want %v", it.TTLHeld, it.ExpiresAt, first)
	}
	// Before the held deadline nothing moves, however often the scheduler ticks.
	e.tickAt(t, t0.Add(time.Hour+time.Minute))
	if it := e.get(t, res.ID); !it.ExpiresAt.Equal(first) {
		t.Fatalf("expires moved to %v before the held deadline", it.ExpiresAt)
	}
	e.tickAt(t, first)
	if it := e.get(t, res.ID); !it.ExpiresAt.Equal(first.Add(30*time.Minute + 20*time.Second)) {
		t.Fatalf("expires %v after the held deadline", it.ExpiresAt)
	}
	if n := len(e.events(t, res.ID, model.EventReservationHeldByTask)); n != 2 {
		t.Fatalf("%d held events, want 2 (one per extension)", n)
	}
}
