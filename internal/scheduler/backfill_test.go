package scheduler

import (
	"context"
	"errors"
	"github.com/creatorofuniverses/agent-queue/internal/admission"
	"github.com/creatorofuniverses/agent-queue/internal/config"
	"github.com/creatorofuniverses/agent-queue/internal/model"
	"github.com/creatorofuniverses/agent-queue/internal/platform"
	"github.com/creatorofuniverses/agent-queue/internal/store"
	"testing"
	"time"
)

func backfillEnv(t *testing.T) (*env, *model.Item, *model.Item) {
	e := newEnv(t, func(c *config.Config) { c.PoolMemory = 16 * gib })
	a := e.submit(t, e.req(8*gib))
	e.tick(t)
	b := e.submit(t, e.req(10*gib))
	return e, a, b
}
func TestBackfillByTime(t *testing.T) {
	e, _, head := backfillEnv(t)
	r := e.req(gib)
	r.Timeout = 10 * time.Minute
	small := e.submit(t, r)
	e.tick(t)
	e.wantState(t, small.ID, model.StateRunning, "")
	e.wantState(t, head.ID, model.StateQueued, "")
	if id, d := e.s.HeadDecision(); id != head.ID || d.Blocker != admission.BlockPoolMemory {
		t.Fatal(id, d)
	}
}
func TestBackfillByExtra(t *testing.T) {
	e, a, head := backfillEnv(t)
	r := e.req(gib)
	r.Timeout = 2 * time.Hour
	small := e.submit(t, r)
	r.Limit = 6 * gib
	other := e.submit(t, r)
	e.tick(t)
	e.wantState(t, small.ID, model.StateRunning, "")
	e.wantState(t, other.ID, model.StateQueued, "")
	e.ex.Finish(a.ID, platform.ExitInfo{ServiceResult: "success", EndedAt: e.clk.Now()})
	e.ex.SetAlive(a.ID, false)
	e.tick(t)
	e.wantState(t, head.ID, model.StateRunning, "")
}
func TestBackfillUsesProfileDuration(t *testing.T) {
	for _, history := range []bool{false, true} {
		e, a, _ := backfillEnv(t)
		a = e.get(t, a.ID)
		a.EstDuration = 5 * time.Minute
		e.st.Update(e.ctx, a)
		r := e.req(7 * gib)
		r.Timeout = 10 * time.Minute
		it := e.submit(t, r)
		if history {
			it.EstDuration = 3 * time.Minute
			e.st.Update(e.ctx, it)
		}
		e.tick(t)
		want := model.StateQueued
		if history {
			want = model.StateRunning
		}
		e.wantState(t, it.ID, want, "")
	}
}
func TestOverrunHolderShadowIsNow(t *testing.T) {
	e, a, _ := backfillEnv(t)
	a = e.get(t, a.ID)
	a.EstDuration = 5 * time.Minute
	e.st.Update(e.ctx, a)
	e.clk.Advance(10 * time.Minute)
	r := e.req(7 * gib)
	r.Timeout = time.Minute
	big := e.submit(t, r)
	r.Limit = gib
	small := e.submit(t, r)
	e.tick(t)
	e.wantState(t, big.ID, model.StateQueued, "")
	e.wantState(t, small.ID, model.StateRunning, "")
}
func TestNoBackfillWhenHeadUnreachable(t *testing.T) {
	e, a, _ := backfillEnv(t)
	e.ex.SetCurrent(a.ID, 8*gib)
	e.host.Set(platform.MemInfo{Total: 16 * gib, Available: 3 * gib})
	small := e.submit(t, e.req(gib))
	e.tick(t)
	e.wantState(t, small.ID, model.StateQueued, "")
}
func TestBackfillCleaningHolderNoPhantom(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.PoolMemory = 16 * gib })
	a := e.submit(t, e.req(8*gib))
	b := e.submit(t, e.req(4*gib))
	e.tick(t)
	e.ex.Finish(a.ID, platform.ExitInfo{ServiceResult: "success"})
	e.ex.SetCurrent(a.ID, 100*mib)
	e.ex.SetCurrent(b.ID, 4*gib)
	b = e.get(t, b.ID)
	b.EstDuration = 10 * time.Minute
	e.st.Update(e.ctx, b)
	e.host.Set(platform.MemInfo{Total: 16 * gib, Available: 5*gib + e.cfg.ReserveFloor(16*gib)})
	e.submit(t, e.req(9*gib))
	r := e.req(3 * gib)
	r.Timeout = 2 * time.Hour
	c := e.submit(t, r)
	e.tick(t)
	e.wantState(t, c.ID, model.StateQueued, "")
}
func TestReservationHolderIncludesTeardown(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.PoolMemory = 16 * gib })
	r := e.rreq(8 * gib)
	r.TTL = 10 * time.Minute
	a := e.reserve(t, r)
	e.tick(t)
	e.submit(t, e.req(10*gib))
	r.Memory = 7 * gib
	r.TTL = 9 * time.Minute
	bad := e.reserve(t, r)
	r.Memory = 5 * gib
	good := e.reserve(t, r)
	e.tick(t)
	e.wantState(t, bad.ID, model.StateQueued, "")
	e.wantState(t, good.ID, model.StateGranted, "")
	hs := e.s.last.holders
	found := false
	for _, h := range hs {
		if h.ID == a.ID {
			found = true
			if !h.End.Equal(e.get(t, a.ID).ExpiresAt.Add(5 * time.Minute)) {
				t.Fatal(h)
			}
		}
	}
	if !found {
		t.Fatal(hs)
	}
	a = e.get(t, a.ID)
	a.State = model.StateTearingDown
	a.TeardownStartedAt = e.clk.Now().Add(-2 * time.Minute)
	e.st.Update(e.ctx, a)
	e.tick(t)
	for _, h := range e.s.last.holders {
		if h.ID == a.ID && !h.End.Equal(e.clk.Now().Add(3*time.Minute)) {
			t.Fatal(h)
		}
	}
}
func TestNoBackfillOnPressure(t *testing.T) {
	e, _, _ := backfillEnv(t)
	e.host.SetPressure(10)
	e.tick(t)
	e.clk.Advance(11 * time.Second)
	it := e.submit(t, e.req(gib))
	e.tick(t)
	e.wantState(t, it.ID, model.StateQueued, "")
}
func TestLastPassCommitments(t *testing.T) {
	e := newResEnv(t)
	e.cfg.PoolMemory = 16 * gib
	e.rebuild(t, e.ex, true)
	a := e.submit(t, e.req(2*gib))
	rr := e.rreq(3 * gib)
	rr.TaskMemory = 4 * gib
	rr.TaskCPUs = 4
	r := e.reserve(t, rr)
	e.tick(t)
	nr := e.req(2 * gib)
	nr.ParentID = r.ID
	n := e.submit(t, nr)
	e.tick(t)
	e.ex.SetCurrent(a.ID, gib)
	e.ex.SetCurrent(n.ID, gib)
	e.tick(t)
	p := e.s.LastPass()
	c := p.Commitments
	if c.TasksExpected != 2*gib || c.TasksUnconsumed != gib || c.Stands != 3*gib || c.StandsUnconsumed != 3*gib || c.Budgets != 4*gib || c.BudgetsUnconsumed != 3*gib || c.Memory != 9*gib || p.Running != 2 || p.Held != 1 {
		t.Fatal(p)
	}
}
func TestExpectedStartForQueued(t *testing.T) {
	e, a, head := backfillEnv(t)
	e.tick(t)
	at, ok := e.s.ExpectedStart(head.ID)
	if !ok || !at.Equal(e.get(t, a.ID).StartedAt.Add(time.Hour+e.s.d.Cfg.CancelGrace)) {
		t.Fatal(at, ok)
	}
	e.host.SetPressure(10)
	e.tick(t)
	e.clk.Advance(11 * time.Second)
	e.tick(t)
	if _, ok = e.s.ExpectedStart(head.ID); ok {
		t.Fatal("pressure forecast")
	}
}
func TestBackfillDoesNotResetAdmissionAlert(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.PoolMemory = 16 * gib; c.Notify.AdmissionClosedAlert = time.Minute })
	a := e.submit(t, e.req(8*gib))
	e.tick(t)
	e.ex.SetCurrent(a.ID, 8*gib)
	e.host.Set(platform.MemInfo{Total: 16 * gib, Available: 4 * gib})
	e.submit(t, e.req(7*gib))
	for i := 0; i < 3; i++ {
		r := e.req(gib)
		r.Timeout = time.Second
		it := e.submit(t, r)
		e.tick(t)
		e.ex.Finish(it.ID, platform.ExitInfo{ServiceResult: "success"})
		e.ex.SetAlive(it.ID, false)
		e.clk.Advance(31 * time.Second)
	}
	e.tick(t)
	items, _ := e.st.List(e.ctx, store.ListFilter{IncludeTerminal: true})
	found := false
	for _, it := range items {
		evs, _ := e.st.Events(e.ctx, it.ID)
		for _, ev := range evs {
			if ev.Kind == model.EventAdmissionClosed {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("no alert")
	}
}
func TestAbortedPassClearsForecast(t *testing.T) {
	e, _, head := backfillEnv(t)
	e.tick(t)
	if _, ok := e.s.ExpectedStart(head.ID); !ok {
		t.Fatal("no forecast")
	}
	e.host.FailMemory(errors.New("meminfo"))
	e.s.Tick(e.ctx)
	if _, ok := e.s.ExpectedStart(head.ID); ok {
		t.Fatal("stale forecast after an aborted pass")
	}
	ctx, cancel := context.WithTimeout(e.ctx, 3*time.Second)
	done := make(chan struct{})
	go func() { defer close(done); e.s.Run(ctx) }()
	_, err := e.s.Estimate(ctx, EstimateRequest{Limit: gib, CPUs: 1, Timeout: time.Minute})
	cancel()
	<-done
	if err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("estimate from a stale pass", err)
	}
	e.host.FailMemory(nil)
	e.tick(t)
	if _, ok := e.s.ExpectedStart(head.ID); !ok {
		t.Fatal("forecast not restored")
	}
}
func TestReservationHolderWaitsForNestedTasks(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.PoolMemory = 16 * gib })
	res := e.grantBudget(t, gib, 2*gib, 1)
	nr := e.within(res.ID, gib)
	nr.Timeout = 3 * time.Hour
	n := e.submit(t, nr)
	e.tick(t)
	n = e.get(t, n.ID)
	want := n.StartedAt.Add(3*time.Hour + e.s.d.Cfg.CancelGrace + e.s.d.Cfg.Reservations.TeardownTimeout)
	if res = e.get(t, res.ID); !want.After(res.ExpiresAt) {
		t.Fatal("nested deadline must be past the TTL for this test", res.ExpiresAt, want)
	}
	for _, h := range e.s.last.holders {
		if h.ID == res.ID && !h.End.Equal(want) {
			t.Fatal(h.End, want)
		}
	}
}
