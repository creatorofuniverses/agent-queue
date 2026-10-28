package scheduler

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-queue/internal/admission"
	"agent-queue/internal/model"
	"agent-queue/internal/platform"
	"agent-queue/internal/platform/fake"
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

// registerTeardown makes the fake know a live teardown unit of attempt for reservation id.
func (e *env) registerTeardown(t *testing.T, id model.ItemID, attempt int) {
	t.Helper()
	spec := platform.LaunchSpec{ID: model.TeardownUnitID(id), JobDir: e.paths.TeardownDir(id, attempt)}
	if _, err := e.ex.Launch(e.ctx, spec); err != nil {
		t.Fatal(err)
	}
}

func TestRecoverTearingDownAlive(t *testing.T) {
	e := newEnv(t, nil)
	res, _ := e.grantStand(t, time.Hour, nil)
	uid := model.TeardownUnitID(res.ID)
	e.tearDown(t, res)
	e.ex.SetAlive(uid, true)
	launched := len(e.ex.Launched())

	e.rebuild(t, e.ex, true)
	e.settle()
	it := e.wantState(t, res.ID, model.StateTearingDown, model.ReasonNone)
	if it.TeardownAttempt != 1 {
		t.Fatalf("attempt %d, want 1", it.TeardownAttempt)
	}
	if s := e.ex.Stopped(); len(s) != 0 {
		t.Fatalf("stopped %v, want nothing", s)
	}
	if n := len(e.ex.Launched()); n != launched {
		t.Fatalf("launched %d, want %d", n, launched)
	}
	// The reaper goes on with the adopted unit.
	e.tick(t)
	e.wantState(t, res.ID, model.StateTearingDown, model.ReasonNone)
	e.finish(uid, exit0)
	e.tick(t)
	e.wantState(t, res.ID, model.StateTornDown, model.ReasonNone)
}

// §15: the teardown finished while the daemon was down.
func TestRecoverTearingDownResult(t *testing.T) {
	e := newEnv(t, nil)
	res, _ := e.grantStand(t, time.Hour, nil)
	uid := model.TeardownUnitID(res.ID)
	e.tearDown(t, res)
	e.finish(uid, exit0)
	if r, _ := e.ex.ReadResult(e.paths.TeardownDir(res.ID, 1)); r == nil {
		t.Fatal("result not in the attempt's dir")
	}
	tmp := e.paths.TeardownTmpDir(res.ID, 1)
	writeFile(t, filepath.Join(tmp, "f"))

	e.rebuild(t, e.ex, true)
	e.settle()
	it := e.wantState(t, res.ID, model.StateTornDown, model.ReasonNone)
	if it.ExitCode == nil || *it.ExitCode != 0 || it.Env != nil {
		t.Fatalf("exit code %v env %v", it.ExitCode, it.Env)
	}
	e.wantEvent(t, res.ID, model.EventTeardownSucceeded, "torn down")
	if exists(t, tmp) {
		t.Fatal("teardown temp dir kept")
	}
	task := e.submit(t, e.req(e.cfg.PoolMemory))
	e.tick(t)
	e.wantState(t, task.ID, model.StateRunning, model.ReasonNone)
}

// §15: neither the unit nor a result survived the restart.
func TestRecoverTearingDownGone(t *testing.T) {
	e := newEnv(t, nil)
	res, _ := e.grantStand(t, time.Hour, nil)
	e.tearDown(t, res)
	e.ex = fake.NewExecutor() // nothing of the previous daemon's units is left

	e.rebuild(t, e.ex, true)
	e.settle()
	it := e.wantState(t, res.ID, model.StateTeardownUnknown, model.ReasonLost)
	if !strings.Contains(it.Diagnostic, "without a result") {
		t.Fatalf("diagnostic %q", it.Diagnostic)
	}
	e.wantEvent(t, res.ID, model.EventTeardownUnknown, "may still be up")
	if l := e.ex.Launched(); len(l) != 0 {
		t.Fatalf("launched %v during Recover", l)
	}
	e.wantPoolBlocked(t)
	if l := e.ex.Launched(); len(l) != 0 {
		t.Fatalf("launched %v after Recover", l)
	}
}

func TestRecoverKeepsGranted(t *testing.T) {
	e := newEnv(t, nil)
	failed, _ := e.grantStand(t, time.Hour, nil)
	e.tearDown(t, failed)
	e.finish(model.TeardownUnitID(failed.ID), exit1)
	e.tick(t)
	e.wantState(t, failed.ID, model.StateTeardownFailed, model.ReasonCommandFailed)

	unknown, _ := e.grantStand(t, time.Hour, nil)
	e.tearDown(t, unknown)
	e.ex.SetAlive(model.TeardownUnitID(unknown.ID), false)
	e.tick(t)
	e.clk.Advance(e.cfg.CancelGrace)
	e.tick(t)
	e.wantState(t, unknown.ID, model.StateTeardownUnknown, model.ReasonLost)

	expiring, _ := e.grantStand(t, time.Hour, nil)
	e.tickAt(t, expiring.ExpiresAt.Add(-time.Second))
	e.wantState(t, expiring.ID, model.StateExpiring, model.ReasonNone)
	granted, _ := e.grantStand(t, 3*time.Hour, nil)
	queued := e.reserve(t, e.rreq(700*mib))
	e.tick(t)
	e.wantState(t, queued.ID, model.StateQueued, model.ReasonNone)

	ids := []model.ItemID{failed.ID, unknown.ID, expiring.ID, granted.ID, queued.ID}
	before := map[model.ItemID]*model.Item{}
	for _, id := range ids {
		before[id] = e.get(t, id)
	}
	launched := len(e.ex.Launched())
	e.rebuild(t, e.ex, true)
	e.settle()
	for _, id := range ids {
		b, a := before[id], e.get(t, id)
		if a.State != b.State || a.Reason != b.Reason || a.Diagnostic != b.Diagnostic ||
			!a.ExpiresAt.Equal(b.ExpiresAt) || a.TeardownAttempt != b.TeardownAttempt {
			t.Fatalf("%s changed by Recover: %s %s %q -> %s %s %q", id, b.State, b.Reason, b.Diagnostic, a.State, a.Reason, a.Diagnostic)
		}
	}
	if s := e.ex.Stopped(); len(s) != 0 {
		t.Fatalf("stopped %v", s)
	}
	if n := len(e.ex.Launched()); n != launched {
		t.Fatalf("launched %d, want %d", n, launched)
	}
}

func TestRecoverExpiredDuringDowntime(t *testing.T) {
	e := newEnv(t, nil)
	res, _ := e.grantStand(t, time.Hour, nil)
	e.rebuild(t, e.ex, false)
	e.clk.Advance(res.ExpiresAt.Add(time.Minute).Sub(e.clk.Now()))
	if err := e.s.Recover(e.ctx); err != nil {
		t.Fatal(err)
	}
	e.wantState(t, res.ID, model.StateGranted, model.ReasonNone)
	if n := len(teardownSpecs(e)); n != 0 {
		t.Fatalf("%d teardown launches during Recover", n)
	}
	e.tick(t)
	e.wantState(t, res.ID, model.StateTearingDown, model.ReasonNone)
	if n := len(teardownSpecs(e)); n != 1 {
		t.Fatalf("%d teardown launches, want 1", n)
	}
}

func TestRecoverStopsOrphanTeardownUnit(t *testing.T) {
	e := newEnv(t, nil)
	res, _ := e.grantStand(t, time.Hour, nil)
	uid := model.TeardownUnitID(res.ID)
	e.tearDown(t, res)
	e.finish(uid, exit0)
	e.tick(t)
	e.wantState(t, res.ID, model.StateTornDown, model.ReasonNone)
	e.ex.SetAlive(uid, true) // a unit of a torn down reservation lingers
	stranger := model.NewID()
	e.registerTeardown(t, stranger, 1) // and one of a reservation the store does not know

	e.rebuild(t, e.ex, true)
	stopped := e.ex.Stopped()
	if !contains(stopped, uid) || !contains(stopped, model.TeardownUnitID(stranger)) {
		t.Fatalf("stopped %v, want %s and the stranger's unit", stopped, uid)
	}
	e.wantState(t, res.ID, model.StateTornDown, model.ReasonNone)
}

// Codex r2: an attempt whose launch failed is stopped by Recover and never trusted.
func TestRecoverTearingDownAfterLaunchError(t *testing.T) {
	e := newEnv(t, nil)
	res, _ := e.grantStand(t, time.Hour, nil)
	uid := model.TeardownUnitID(res.ID)
	e.launchErrorAlive(t, res)
	e.ex.FailLaunch(nil)
	e.settle()
	stops := len(e.ex.Stopped())

	e.rebuild(t, e.ex, true)
	e.settle()
	if s := e.ex.Stopped(); len(s) != stops+1 || s[len(s)-1] != uid {
		t.Fatalf("stopped %v, want one more stop of %s", s, uid)
	}
	it := e.wantState(t, res.ID, model.StateTearingDown, model.ReasonNone)
	if it.TeardownLaunchError == "" || it.TeardownAttempt != 1 {
		t.Fatalf("launch error %q attempt %d", it.TeardownLaunchError, it.TeardownAttempt)
	}
	// Recover's stop counts for the reaper's cancel_grace.
	e.tick(t)
	e.settle()
	if n := len(e.ex.Stopped()); n != stops+1 {
		t.Fatalf("%d stops within cancel_grace of Recover's, want %d", n, stops+1)
	}

	e.ex.SetAlive(uid, false)
	e.ex.Finish(uid, exit0)
	e.tick(t)
	e.wantState(t, res.ID, model.StateTeardownFailed, model.ReasonLaunchFailed)
	e.wantEvent(t, res.ID, model.EventTeardownFailed, "without its memory limit")
}

// The same with the unit already gone at restart: a result of that attempt is not trusted.
func TestRecoverLaunchErrorGoneFails(t *testing.T) {
	e := newEnv(t, nil)
	res, _ := e.grantStand(t, time.Hour, nil)
	uid := model.TeardownUnitID(res.ID)
	e.launchErrorAlive(t, res)
	e.ex.FailLaunch(nil)
	e.finish(uid, exit0)

	e.rebuild(t, e.ex, true)
	e.wantState(t, res.ID, model.StateTeardownFailed, model.ReasonLaunchFailed)
	e.wantEvent(t, res.ID, model.EventTeardownFailed, "without its memory limit")
}

func TestRecoverNestedTask(t *testing.T) {
	e := newEnv(t, nil)
	res := e.grantBudget(t, 100*mib, 500*mib, 2)
	nested := e.submit(t, e.within(res.ID, 400*mib))
	e.tick(t)
	e.wantState(t, nested.ID, model.StateRunning, model.ReasonNone)

	e.rebuild(t, e.ex, true)
	e.settle()
	e.wantState(t, nested.ID, model.StateRunning, model.ReasonNone)
	e.wantState(t, res.ID, model.StateGranted, model.ReasonNone)
	if s := e.ex.Stopped(); len(s) != 0 {
		t.Fatalf("stopped %v", s)
	}
	e.wantRejected(t, e.within(res.ID, 200*mib), "task budget")
	fits := e.submit(t, e.within(res.ID, 100*mib))
	e.tick(t)
	e.wantState(t, fits.ID, model.StateRunning, model.ReasonNone)
}
