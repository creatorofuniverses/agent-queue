package scheduler

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"resource-dispatcher/internal/admission"
	"resource-dispatcher/internal/model"
	"resource-dispatcher/internal/platform"
	"resource-dispatcher/internal/platform/fake"
)

// hookExec wraps the fake executor: it records Stop/Alive/ReadResult calls in order,
// lets onStop act (or fail) inside Stop, and makes ReadResult fail for chosen job dirs.
type hookExec struct {
	*fake.Executor
	onStop  func(id model.ItemID) error // set before the scheduler uses it
	readErr map[string]error            // by job dir; set before use

	mu    sync.Mutex
	calls []string
}

func (x *hookExec) record(c string) int {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.calls = append(x.calls, c)
	n := 0
	for _, v := range x.calls {
		if strings.HasPrefix(v, "stop:") {
			n++
		}
	}
	return n
}

// stops is the number of Stop calls so far.
func (x *hookExec) stops() int { return x.record("") }

func (x *hookExec) Stop(ctx context.Context, id model.ItemID) error {
	x.record("stop:" + string(id))
	if x.onStop != nil {
		if err := x.onStop(id); err != nil {
			return err
		}
	}
	return x.Executor.Stop(ctx, id)
}

func (x *hookExec) Alive(ctx context.Context, id model.ItemID) (bool, error) {
	x.record("alive:" + string(id))
	return x.Executor.Alive(ctx, id)
}

func (x *hookExec) ReadResult(jobDir string) (*platform.ExitInfo, error) {
	x.record("result:" + filepath.Base(jobDir))
	if err := x.readErr[jobDir]; err != nil {
		return nil, err
	}
	return x.Executor.ReadResult(jobDir)
}

// aliveBeforeResult checks that every result read of id came after an Alive read of id.
func (x *hookExec) aliveBeforeResult(t *testing.T, id model.ItemID) {
	t.Helper()
	x.mu.Lock()
	defer x.mu.Unlock()
	seenAlive := false
	for _, c := range x.calls {
		switch c {
		case "alive:" + string(id):
			seenAlive = true
		case "result:" + string(id):
			if !seenAlive {
				t.Fatalf("result of %s read before Alive: %v", id, x.calls)
			}
			seenAlive = false
		}
	}
}

// put stores an item as a previous daemon left it.
func (e *env) put(t *testing.T, state model.State, mut func(*model.Item)) *model.Item {
	t.Helper()
	e.clk.Advance(time.Millisecond)
	it := &model.Item{
		ID: model.NewID(), Kind: model.KindTask, State: state, Argv: []string{"true"}, Cwd: e.cwd,
		Limit: 256 * mib, Expected: 256 * mib, CPUs: 1, Timeout: time.Hour, CreatedAt: e.clk.Now(),
	}
	it.JobDir = e.paths.JobDir(it.ID)
	if mut != nil {
		mut(it)
	}
	if _, _, err := e.st.InsertItem(e.ctx, it); err != nil {
		t.Fatal(err)
	}
	return it
}

// register makes the fake know a live unit for id (and its job dir).
func (e *env) register(t *testing.T, id model.ItemID) {
	t.Helper()
	if _, err := e.ex.Launch(e.ctx, platform.LaunchSpec{ID: id, JobDir: e.paths.JobDir(id)}); err != nil {
		t.Fatal(err)
	}
}

func TestRecoverAdoptsAndFinalizes(t *testing.T) {
	e := newEnv(t, nil)
	hx := &hookExec{Executor: e.ex}
	e.rebuild(t, hx, false)

	done, alive, lost := e.put(t, model.StateRunning, nil), e.put(t, model.StateRunning, nil), e.put(t, model.StateRunning, nil)
	starting := e.put(t, model.StateStarting, nil)
	queued := e.put(t, model.StateQueued, nil)
	e.register(t, done.ID)
	e.ex.Finish(done.ID, platform.ExitInfo{ServiceResult: "success", ExitCode: "exited", ExitStatus: "0", MemoryPeak: 1234})
	e.ex.SetAlive(done.ID, false)
	e.register(t, alive.ID)
	orphan := model.NewID()
	e.register(t, orphan)
	launchedBefore := len(e.ex.Launched())

	var during admission.Decision
	var duringID model.ItemID
	hx.onStop = func(model.ItemID) error {
		if err := e.s.Tick(e.ctx); err != nil {
			t.Error(err)
		}
		duringID, during = e.s.HeadDecision()
		return nil
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

func TestRecoverStartingAndUnreadableResult(t *testing.T) {
	e := newEnv(t, nil)
	hx := &hookExec{Executor: e.ex, readErr: map[string]error{}}
	e.rebuild(t, hx, false)

	adopted := e.put(t, model.StateStarting, nil)
	e.register(t, adopted.ID) // unit up, no result yet
	unreadable := e.put(t, model.StateStarting, nil)
	hx.readErr[unreadable.JobDir] = errors.New("result.json: unexpected end of JSON input")
	unreadableRun := e.put(t, model.StateRunning, nil)
	hx.readErr[unreadableRun.JobDir] = errors.New("result.json: bad")
	cancelled := e.put(t, model.StateStarting, func(it *model.Item) { it.CancelRequested, it.CancelReason = true, "x" })
	requeued := e.put(t, model.StateStarting, nil)

	if err := e.s.Recover(e.ctx); err != nil {
		t.Fatal(err)
	}
	got := e.wantState(t, adopted.ID, model.StateRunning, model.ReasonNone)
	if got.StartedAt.IsZero() {
		t.Fatalf("adopted item without StartedAt")
	}
	for _, it := range []*model.Item{unreadable, unreadableRun} {
		got := e.wantState(t, it.ID, model.StateFailed, model.ReasonLost)
		if !strings.Contains(got.Diagnostic, "unreadable result") {
			t.Fatalf("diagnostic %q", got.Diagnostic)
		}
	}
	e.wantState(t, cancelled.ID, model.StateCancelled, model.ReasonCancelled)
	e.wantState(t, requeued.ID, model.StateQueued, model.ReasonNone)
	for _, it := range []*model.Item{adopted, unreadable, unreadableRun, cancelled, requeued} {
		hx.aliveBeforeResult(t, it.ID)
	}
}

func TestRecoverAdoptedCancelRequestedIsStopped(t *testing.T) {
	e := newEnv(t, nil)
	hx := &hookExec{Executor: e.ex}
	e.rebuild(t, hx, false)
	it := e.put(t, model.StateRunning, func(it *model.Item) { it.CancelRequested, it.CancelReason = true, "x" })
	e.register(t, it.ID)
	if err := e.s.Recover(e.ctx); err != nil {
		t.Fatal(err)
	}
	e.settle()
	if n := len(e.ex.Stopped()); n != 0 {
		t.Fatalf("known item treated as orphan")
	}
	e.wantState(t, it.ID, model.StateRunning, model.ReasonNone)
	e.tick(t)
	e.settle()
	if !contains(e.ex.Stopped(), it.ID) {
		t.Fatalf("adopted item with cancel requested was not stopped")
	}
	e.finish(it.ID, platform.ExitInfo{ServiceResult: "exit-code", ExitStatus: "143"})
	e.tick(t)
	e.wantState(t, it.ID, model.StateCancelled, model.ReasonCancelled)
}
