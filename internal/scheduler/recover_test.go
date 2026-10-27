package scheduler

import (
	"context"
	"testing"
	"time"

	"resource-dispatcher/internal/admission"
	"resource-dispatcher/internal/model"
	"resource-dispatcher/internal/platform"
	"resource-dispatcher/internal/platform/fake"
)

// stopHookExec lets a test act while Recover stops orphans (mutex released, still recovering).
type stopHookExec struct {
	*fake.Executor
	onStop func(id model.ItemID)
}

func (x *stopHookExec) Stop(ctx context.Context, id model.ItemID) error {
	if x.onStop != nil {
		x.onStop(id)
	}
	return x.Executor.Stop(ctx, id)
}

func TestRecoverAdoptsAndFinalizes(t *testing.T) {
	e := newEnv(t, nil)
	hx := &stopHookExec{Executor: e.ex}
	e.s = New(Deps{Store: e.st, Exec: hx, Host: e.host, Cfg: e.s.d.Cfg, Paths: e.paths, Now: e.clk.Now})

	// Items as a previous daemon left them; the fake learns their job dirs via Launch.
	put := func(state model.State) *model.Item {
		e.clk.Advance(time.Millisecond)
		it := &model.Item{
			ID: model.NewID(), Kind: model.KindTask, State: state, Argv: []string{"true"}, Cwd: e.cwd,
			Limit: 256 * mib, Expected: 256 * mib, CPUs: 1, Timeout: time.Hour, CreatedAt: e.clk.Now(),
		}
		it.JobDir = e.paths.JobDir(it.ID)
		if _, _, err := e.st.InsertItem(e.ctx, it); err != nil {
			t.Fatal(err)
		}
		return it
	}
	register := func(id model.ItemID) {
		if _, err := e.ex.Launch(e.ctx, platform.LaunchSpec{ID: id, JobDir: e.paths.JobDir(id)}); err != nil {
			t.Fatal(err)
		}
	}
	done, alive, lost := put(model.StateRunning), put(model.StateRunning), put(model.StateRunning)
	starting := put(model.StateStarting)
	queued := put(model.StateQueued)
	register(done.ID)
	e.ex.Finish(done.ID, platform.ExitInfo{ServiceResult: "success", ExitCode: "exited", ExitStatus: "0", MemoryPeak: 1234})
	e.ex.SetAlive(done.ID, false)
	register(alive.ID)
	orphan := model.NewID()
	register(orphan)
	launchedBefore := len(e.ex.Launched())

	var during admission.Decision
	var duringID model.ItemID
	hx.onStop = func(model.ItemID) {
		if err := e.s.Tick(e.ctx); err != nil {
			t.Error(err)
		}
		duringID, during = e.s.HeadDecision()
	}
	if err := e.s.Recover(e.ctx); err != nil {
		t.Fatal(err)
	}

	got := e.wantState(t, done.ID, model.StateSucceeded, model.ReasonNone)
	if got.ExitCode == nil || *got.ExitCode != 0 || got.MemoryPeak != 1234 {
		t.Fatalf("finalized from result.json: %+v", got)
	}
	e.wantState(t, alive.ID, model.StateRunning, model.ReasonNone)
	e.wantState(t, lost.ID, model.StateFailed, model.ReasonLost)
	e.wantState(t, starting.ID, model.StateQueued, model.ReasonNone)
	e.wantState(t, queued.ID, model.StateQueued, model.ReasonNone)

	stopped := e.ex.Stopped()
	if len(stopped) != 1 || stopped[0] != orphan {
		t.Fatalf("stopped %v, want only orphan %s", stopped, orphan)
	}
	if during.Admit || during.Blocker != admission.BlockRecovering || duringID != starting.ID {
		t.Fatalf("decision during Recover: %s %+v, want recovering for %s", duringID, during, starting.ID)
	}
	if n := len(e.ex.Launched()); n != launchedBefore {
		t.Fatalf("launched during Recover")
	}

	// Admission reopens afterwards.
	e.tick(t)
	e.wantState(t, starting.ID, model.StateRunning, model.ReasonNone)
	e.wantState(t, queued.ID, model.StateRunning, model.ReasonNone)
}
