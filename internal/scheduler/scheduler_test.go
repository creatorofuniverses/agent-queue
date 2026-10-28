package scheduler

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"resource-dispatcher/internal/admission"
	"resource-dispatcher/internal/config"
	"resource-dispatcher/internal/model"
	"resource-dispatcher/internal/platform"
	"resource-dispatcher/internal/platform/fake"
	"resource-dispatcher/internal/store"
)

const (
	mib = uint64(1) << 20
	gib = uint64(1) << 30
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type env struct {
	s     *Scheduler
	cfg   config.Config
	st    *store.Store
	ex    *fake.Executor
	host  *fake.Host
	meter *fake.Meter // nil: Deps.Meter stays nil
	clk   *clock
	paths config.Paths
	cwd   string
	ctx   context.Context
}

// newEnv builds a scheduler over a real store, fake platform and a manual clock.
// Defaults: pool 1 GiB / 16 CPUs, a roomy host (64 GiB, floor 6.4 GiB), no pressure.
func newEnv(t *testing.T, mutate func(*config.Config)) *env {
	t.Helper()
	cfg := config.Default()
	cfg.PoolMemory = gib
	cfg.PoolCPUs = 16
	if mutate != nil {
		mutate(&cfg)
	}
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "aq.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &env{
		st:    st,
		ex:    fake.NewExecutor(),
		host:  fake.NewHost(),
		meter: fake.NewMeter(),
		clk:   &clock{t: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)},
		paths: config.Paths{StateDir: filepath.Join(dir, "state"), RuntimeDir: filepath.Join(dir, "run")},
		cwd:   t.TempDir(),
		ctx:   context.Background(),
	}
	e.host.Set(platform.MemInfo{Total: 64 * gib, Available: 64 * gib})
	e.host.SetDisk(100 * gib)
	e.cfg = cfg
	e.rebuild(t, e.ex, true)
	t.Cleanup(func() { e.s.bg.Wait() }) // before the temp dirs go away
	return e
}

// rebuild replaces the scheduler with one over x; with recover it also runs Recover,
// which a new scheduler needs before it admits anything.
func (e *env) rebuild(t *testing.T, x platform.Executor, recover bool) {
	t.Helper()
	if e.s != nil {
		e.s.bg.Wait()
	}
	var meter platform.StandMeter
	if e.meter != nil {
		meter = e.meter
	}
	e.s = New(Deps{Store: e.st, Exec: x, Host: e.host, Meter: meter, Cfg: e.cfg, Paths: e.paths, Now: e.clk.Now})
	if recover {
		if err := e.s.Recover(e.ctx); err != nil {
			t.Fatal(err)
		}
	}
}

// settle waits for background stops and temp dir removals.
func (e *env) settle() { e.s.bg.Wait() }

// mkTmp creates the item's temp dir with a file in it, as the runner would.
func (e *env) mkTmp(t *testing.T, id model.ItemID) string {
	t.Helper()
	dir := e.paths.TmpDir(id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "scratch"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return err == nil
}

func (e *env) req(mem uint64) SubmitRequest {
	return SubmitRequest{Argv: []string{"true"}, Cwd: e.cwd, Limit: mem, CPUs: 1, Timeout: time.Hour}
}

func (e *env) submit(t *testing.T, r SubmitRequest) *model.Item {
	t.Helper()
	e.clk.Advance(time.Millisecond) // distinct created_at for FIFO
	it, err := e.s.Submit(e.ctx, r)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if it.State != model.StateQueued {
		t.Fatalf("submitted state %s", it.State)
	}
	return it
}

func (e *env) tick(t *testing.T) {
	t.Helper()
	if err := e.s.Tick(e.ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
}

func (e *env) get(t *testing.T, id model.ItemID) *model.Item {
	t.Helper()
	it, err := e.st.Get(e.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return it
}

func (e *env) wantState(t *testing.T, id model.ItemID, st model.State, r model.Reason) *model.Item {
	t.Helper()
	it := e.get(t, id)
	if it.State != st || it.Reason != r {
		t.Fatalf("item %s: state %s/%s, want %s/%s (diag %q)", id, it.State, it.Reason, st, r, it.Diagnostic)
	}
	return it
}

func launchedIDs(ex *fake.Executor) []model.ItemID {
	var out []model.ItemID
	for _, s := range ex.Launched() {
		out = append(out, s.ID)
	}
	return out
}

func contains(ids []model.ItemID, id model.ItemID) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

// finish makes the fake unit report a result and empties its cgroup.
func (e *env) finish(id model.ItemID, info platform.ExitInfo) {
	e.ex.Finish(id, info)
	e.ex.SetAlive(id, false)
}

var success = platform.ExitInfo{ServiceResult: "success", ExitCode: "exited", ExitStatus: "0"}

func TestPriorityOrder(t *testing.T) {
	e := newEnv(t, nil)
	first := e.submit(t, e.req(gib))
	e.tick(t)
	e.wantState(t, first.ID, model.StateRunning, model.ReasonNone)

	low := e.req(gib)
	low.Priority = model.PriorityLow
	lowIt := e.submit(t, low)
	high := e.req(gib)
	high.Priority = model.PriorityHigh
	highIt := e.submit(t, high)
	e.tick(t)
	if n := len(e.ex.Launched()); n != 1 {
		t.Fatalf("launched %d while pool is full", n)
	}

	e.finish(first.ID, success)
	e.tick(t)
	e.wantState(t, first.ID, model.StateSucceeded, model.ReasonNone)
	e.wantState(t, highIt.ID, model.StateRunning, model.ReasonNone)
	e.wantState(t, lowIt.ID, model.StateQueued, model.ReasonNone)
	if got := launchedIDs(e.ex); len(got) != 2 || got[1] != highIt.ID {
		t.Fatalf("launch order %v, want high second", got)
	}
}

func TestHeadOfLineBackfillDisabled(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.PoolMemory = 16 * gib; c.Scheduler.Backfill = false })
	running := e.submit(t, e.req(8*gib))
	e.tick(t)
	e.wantState(t, running.ID, model.StateRunning, model.ReasonNone)

	head := e.submit(t, e.req(10*gib))
	small := e.submit(t, e.req(gib))
	e.tick(t)
	e.wantState(t, head.ID, model.StateQueued, model.ReasonNone)
	e.wantState(t, small.ID, model.StateQueued, model.ReasonNone)
	id, dec := e.s.HeadDecision()
	if id != head.ID || dec.Admit || dec.Blocker != admission.BlockPoolMemory {
		t.Fatalf("head decision %s %+v, want %s pool_memory", id, dec, head.ID)
	}
}

func TestHeadDecisionEmptyQueue(t *testing.T) {
	e := newEnv(t, nil)
	e.tick(t)
	if id, dec := e.s.HeadDecision(); id != "" || !dec.Admit {
		t.Fatalf("empty queue: %q %+v", id, dec)
	}
}

func TestPoolNeverExceeded(t *testing.T) {
	e := newEnv(t, nil)
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := e.s.Submit(e.ctx, e.req(300*mib)); err != nil {
				errs <- err
			}
			if err := e.s.Tick(e.ctx); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	e.tick(t)

	all, err := e.st.List(e.ctx, store.ListFilter{IncludeTerminal: true})
	if err != nil {
		t.Fatal(err)
	}
	var running int
	var sum uint64
	for _, it := range all {
		switch it.State {
		case model.StateRunning:
			running++
			sum += it.Expected
		case model.StateQueued:
		default:
			t.Fatalf("unexpected state %s", it.State)
		}
	}
	if running != 3 || sum > gib || len(e.ex.Launched()) != 3 {
		t.Fatalf("running %d (sum %d, launched %d), want 3 within 1 GiB", running, sum, len(e.ex.Launched()))
	}
}

func TestRejectTooLargeAndInvalid(t *testing.T) {
	e := newEnv(t, nil)
	cases := map[string]func(r *SubmitRequest){
		"too large":        func(r *SubmitRequest) { r.Limit = 2 * gib },
		"zero memory":      func(r *SubmitRequest) { r.Limit = 0 },
		"below 64 MiB":     func(r *SubmitRequest) { r.Limit = 63 * mib },
		"relative cwd":     func(r *SubmitRequest) { r.Cwd = "rel/dir" },
		"missing cwd":      func(r *SubmitRequest) { r.Cwd = filepath.Join(e.cwd, "nope") },
		"empty argv":       func(r *SubmitRequest) { r.Argv = nil },
		"zero timeout":     func(r *SubmitRequest) { r.Timeout = 0 },
		"zero cpus":        func(r *SubmitRequest) { r.CPUs = 0 },
		"expected > limit": func(r *SubmitRequest) { r.Expected = r.Limit + 1 },
		"cpus exceed pool": func(r *SubmitRequest) { r.CPUs = 17 },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			r := e.req(256 * mib)
			mut(&r)
			it, err := e.s.Submit(e.ctx, r)
			if !errors.Is(err, ErrRejected) {
				t.Fatalf("err %v, want ErrRejected", err)
			}
			if it == nil || it.State != model.StateRejected || it.Diagnostic == "" {
				t.Fatalf("item %+v, want REJECTED with diagnostic", it)
			}
			e.wantState(t, it.ID, model.StateRejected, model.ReasonNone)
		})
	}
	e.tick(t)
	if n := len(e.ex.Launched()); n != 0 {
		t.Fatalf("launched %d rejected items", n)
	}
}

func TestSubmitDefaultsAndIdempotency(t *testing.T) {
	e := newEnv(t, nil)
	r := e.req(512 * mib)
	r.IdemKey = "k"
	a := e.submit(t, r)
	if a.Expected != a.Limit || a.RequestHash == "" {
		t.Fatalf("expected %d limit %d hash %q", a.Expected, a.Limit, a.RequestHash)
	}
	b, err := e.s.Submit(e.ctx, r)
	if err != nil || b.ID != a.ID {
		t.Fatalf("same key: %v %v", b, err)
	}
	r.Argv = []string{"false"}
	if _, err := e.s.Submit(e.ctx, r); !errors.Is(err, store.ErrIdempotencyConflict) {
		t.Fatalf("different request: %v", err)
	}
}

func TestLaunchSpec(t *testing.T) {
	e := newEnv(t, func(c *config.Config) {
		c.PoolMemory = 8 * gib
		c.MemoryHighRatio = 0.9
	})
	r := e.req(4 * gib)
	r.CPUs = 2
	r.Env = map[string]string{"FOO": "bar"}
	it := e.submit(t, r)
	e.tick(t)
	l := e.ex.Launched()
	if len(l) != 1 {
		t.Fatalf("launched %d", len(l))
	}
	sp := l[0]
	if sp.MemoryHigh != 3865470566 || sp.Limit != 4*gib || sp.CPUs != 2 || sp.KillGrace != 10*time.Second {
		t.Fatalf("spec %+v", sp)
	}
	if sp.JobDir != e.paths.JobDir(it.ID) || sp.TmpDir != e.paths.TmpDir(it.ID) || sp.Cwd != e.cwd {
		t.Fatalf("dirs %q %q %q", sp.JobDir, sp.TmpDir, sp.Cwd)
	}
	if sp.Env["FOO"] != "bar" || sp.Env["AQ_JOB_ID"] != string(it.ID) || sp.Env["AQ_CPUS"] != "2" {
		t.Fatalf("env %v", sp.Env)
	}
	got := e.wantState(t, it.ID, model.StateRunning, model.ReasonNone)
	if !got.StartedAt.Equal(e.clk.Now()) || got.UnitName == "" || len(got.Env) != 1 {
		t.Fatalf("running item %+v", got)
	}
}

func TestCancelQueued(t *testing.T) {
	e := newEnv(t, nil)
	it := e.submit(t, e.req(gib))
	got, err := e.s.Cancel(e.ctx, it.ID, "changed my mind")
	if err != nil || got.State != model.StateCancelled || got.Reason != model.ReasonCancelled {
		t.Fatalf("cancel: %+v %v", got, err)
	}
	e.wantState(t, it.ID, model.StateCancelled, model.ReasonCancelled)
	e.tick(t)
	if n := len(e.ex.Launched()); n != 0 {
		t.Fatalf("cancelled item launched")
	}
}

func TestCancelRunning(t *testing.T) {
	e := newEnv(t, nil)
	it := e.submit(t, e.req(gib))
	e.tick(t)
	got, err := e.s.Cancel(e.ctx, it.ID, "stop")
	if err != nil || got.State != model.StateRunning || !got.CancelRequested || got.CancelReason != "stop" {
		t.Fatalf("cancel: %+v %v", got, err)
	}
	e.settle()
	if !contains(e.ex.Stopped(), it.ID) {
		t.Fatalf("Stop not called")
	}
	e.finish(it.ID, platform.ExitInfo{ServiceResult: "exit-code", ExitCode: "exited", ExitStatus: "143"})
	e.tick(t)
	e.wantState(t, it.ID, model.StateCancelled, model.ReasonCancelled)
}

func TestCancelIdempotent(t *testing.T) {
	e := newEnv(t, nil)
	q := e.submit(t, e.req(gib))
	for range 2 {
		got, err := e.s.Cancel(e.ctx, q.ID, "x")
		if err != nil || got.State != model.StateCancelled {
			t.Fatalf("cancel queued: %+v %v", got, err)
		}
	}

	done := e.submit(t, e.req(gib))
	e.tick(t)
	e.finish(done.ID, success)
	e.tick(t)
	before := e.wantState(t, done.ID, model.StateSucceeded, model.ReasonNone)
	got, err := e.s.Cancel(e.ctx, done.ID, "late")
	if err != nil || got.State != model.StateSucceeded || got.CancelRequested {
		t.Fatalf("cancel terminal: %+v %v", got, err)
	}
	if after := e.get(t, done.ID); after.CancelRequested || !after.EndedAt.Equal(before.EndedAt) {
		t.Fatalf("terminal item changed: %+v", after)
	}

	run := e.submit(t, e.req(gib))
	e.tick(t)
	for range 2 {
		got, err := e.s.Cancel(e.ctx, run.ID, "x")
		if err != nil || got.State != model.StateRunning || !got.CancelRequested {
			t.Fatalf("cancel running: %+v %v", got, err)
		}
	}
	if _, err := e.s.Cancel(e.ctx, "missing", "x"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cancel missing: %v", err)
	}
}

// The hook runs inside the first item's Launch, i.e. after both items were admitted
// (STARTING) but before the second one is launched. It also proves Launch is called
// without the scheduler mutex held: Cancel would deadlock otherwise.
func TestCancelBetweenAdmitAndLaunch(t *testing.T) {
	e := newEnv(t, nil)
	a := e.submit(t, e.req(256*mib))
	b := e.submit(t, e.req(256*mib))
	var hookErr error
	e.ex.SetBeforeLaunch(func(sp platform.LaunchSpec) {
		if sp.ID == a.ID {
			got, err := e.s.Cancel(e.ctx, b.ID, "race")
			if err == nil && (got.State != model.StateStarting || !got.CancelRequested) {
				err = errors.New("b not STARTING with cancel requested: " + string(got.State))
			}
			hookErr = err
		}
	})
	e.tick(t)
	if hookErr != nil {
		t.Fatal(hookErr)
	}
	if contains(launchedIDs(e.ex), b.ID) {
		t.Fatalf("cancelled item was launched")
	}
	e.wantState(t, b.ID, model.StateCancelled, model.ReasonCancelled)
	e.wantState(t, a.ID, model.StateRunning, model.ReasonNone)
}

func TestLaunchFailure(t *testing.T) {
	e := newEnv(t, nil)
	e.ex.FailLaunch(errors.New("systemd-run exploded"))
	it := e.submit(t, e.req(gib))
	tmp := e.mkTmp(t, it.ID)
	e.tick(t)
	got := e.wantState(t, it.ID, model.StateFailed, model.ReasonLaunchFailed)
	if !strings.Contains(got.Diagnostic, "exploded") || got.EndedAt.IsZero() {
		t.Fatalf("failed item %+v", got)
	}
	e.settle()
	if exists(t, tmp) {
		t.Fatalf("temp dir kept after launch failure")
	}

	e.ex.FailLaunch(nil)
	next := e.submit(t, e.req(gib))
	e.tick(t)
	e.wantState(t, next.ID, model.StateRunning, model.ReasonNone)
}

// systemd-run failed after a cancel arrived: the user asked for the end, so CANCELLED.
func TestLaunchErrorAfterCancelIsCancelled(t *testing.T) {
	e := newEnv(t, nil)
	e.ex.FailLaunch(errors.New("systemd-run exploded"))
	it := e.submit(t, e.req(gib))
	e.ex.SetBeforeLaunch(func(sp platform.LaunchSpec) {
		if _, err := e.s.Cancel(e.ctx, sp.ID, "user"); err != nil {
			t.Error(err)
		}
	})
	e.tick(t)
	got := e.wantState(t, it.ID, model.StateCancelled, model.ReasonCancelled)
	if !strings.Contains(got.Diagnostic, "exploded") {
		t.Fatalf("diagnostic %q", got.Diagnostic)
	}
}

// Launch failed but its cleanup Stop did not end the unit: the item must not go
// terminal (that would free resources still in use). It stays RUNNING with a cancel
// that the reaper keeps enforcing, and settles as LAUNCH_FAILED once the unit ends.
func TestLaunchErrorWithLiveUnitStaysRunning(t *testing.T) {
	for _, userCancel := range []bool{false, true} {
		e := newEnv(t, nil)
		e.ex.FailLaunch(errors.New("memory.max not applied; stop failed"))
		it := e.submit(t, e.req(gib))
		e.ex.SetBeforeLaunch(func(sp platform.LaunchSpec) {
			e.ex.SetAlive(sp.ID, true)
			if userCancel {
				if _, err := e.s.Cancel(e.ctx, sp.ID, "user"); err != nil {
					t.Error(err)
				}
			}
		})
		e.tick(t)
		e.ex.SetBeforeLaunch(nil)
		e.settle()
		got := e.wantState(t, it.ID, model.StateRunning, model.ReasonNone)
		if !got.CancelRequested || !strings.Contains(got.Diagnostic, "stop failed") {
			t.Fatalf("running item %+v", got)
		}
		if !contains(e.ex.Stopped(), it.ID) {
			t.Fatalf("unit not stopped again")
		}
		e.ex.FailLaunch(nil)
		next := e.submit(t, e.req(gib))
		e.tick(t)
		e.wantState(t, next.ID, model.StateQueued, model.ReasonNone) // resources still held

		e.finish(it.ID, platform.ExitInfo{ServiceResult: "exit-code", ExitStatus: "143"})
		e.tick(t)
		if userCancel {
			e.wantState(t, it.ID, model.StateCancelled, model.ReasonCancelled)
		} else {
			e.wantState(t, it.ID, model.StateFailed, model.ReasonLaunchFailed)
		}
		e.tick(t)
		e.wantState(t, next.ID, model.StateRunning, model.ReasonNone)
	}
}

// Env values may be secrets: kept only while the item can still launch.
func TestEnvClearedOnTerminal(t *testing.T) {
	e := newEnv(t, nil)
	secret := map[string]string{"TOKEN": "s3cret"}
	withEnv := func(mem uint64) SubmitRequest {
		r := e.req(mem)
		r.Env = secret
		return r
	}
	noEnv := func(id model.ItemID) {
		t.Helper()
		if it := e.get(t, id); it.Env != nil {
			t.Fatalf("item %s (%s) keeps env %v", id, it.State, it.Env)
		}
	}

	done := e.submit(t, withEnv(512*mib))
	cancelled := e.submit(t, withEnv(gib)) // queued behind done
	if e.get(t, done.ID).Env["TOKEN"] != "s3cret" {
		t.Fatal("env not stored for launch")
	}
	e.tick(t)
	if l := e.ex.Launched(); len(l) != 1 || l[0].Env["TOKEN"] != "s3cret" {
		t.Fatalf("launch spec env %+v", l)
	}
	if _, err := e.s.Cancel(e.ctx, cancelled.ID, "x"); err != nil {
		t.Fatal(err)
	}
	noEnv(cancelled.ID)
	e.finish(done.ID, success)
	e.tick(t)
	e.wantState(t, done.ID, model.StateSucceeded, model.ReasonNone)
	noEnv(done.ID)

	e.ex.FailLaunch(errors.New("boom"))
	failed := e.submit(t, withEnv(gib))
	e.tick(t)
	e.wantState(t, failed.ID, model.StateFailed, model.ReasonLaunchFailed)
	noEnv(failed.ID)

	bad := withEnv(gib)
	bad.Argv = nil
	rej, err := e.s.Submit(e.ctx, bad)
	if !errors.Is(err, ErrRejected) {
		t.Fatalf("submit: %v", err)
	}
	noEnv(rej.ID)
}

func TestCleaningHoldsResources(t *testing.T) {
	e := newEnv(t, nil)
	a := e.submit(t, e.req(gib))
	e.tick(t)
	tmp := e.mkTmp(t, a.ID)
	jobFile := filepath.Join(e.paths.JobDir(a.ID), "stdout.log")
	if err := os.MkdirAll(filepath.Dir(jobFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jobFile, []byte("out"), 0o644); err != nil {
		t.Fatal(err)
	}
	b := e.submit(t, e.req(512*mib))
	e.ex.Finish(a.ID, success) // result written, cgroup still has processes
	e.tick(t)
	e.settle()
	e.wantState(t, a.ID, model.StateCleaning, model.ReasonNone)
	if !exists(t, tmp) {
		t.Fatalf("temp dir removed before cleanup was confirmed")
	}
	e.wantState(t, b.ID, model.StateQueued, model.ReasonNone)
	if id, dec := e.s.HeadDecision(); id != b.ID || dec.Blocker != admission.BlockPoolMemory {
		t.Fatalf("head %s %+v, want pool_memory", id, dec)
	}
	if contains(e.ex.Stopped(), a.ID) {
		t.Fatalf("stopped before 2x cancel_grace")
	}

	e.clk.Advance(20 * time.Second) // 2 x cancel_grace
	e.tick(t)
	e.settle()
	got := e.wantState(t, a.ID, model.StateCleaning, model.ReasonNone)
	if !contains(e.ex.Stopped(), a.ID) || got.Diagnostic == "" {
		t.Fatalf("lingering processes: stopped=%v diag=%q", e.ex.Stopped(), got.Diagnostic)
	}
	e.wantState(t, b.ID, model.StateQueued, model.ReasonNone)

	e.ex.SetAlive(a.ID, false)
	e.tick(t)
	e.wantState(t, a.ID, model.StateSucceeded, model.ReasonNone)
	e.wantState(t, b.ID, model.StateRunning, model.ReasonNone)
	e.settle()
	if exists(t, tmp) || !exists(t, jobFile) {
		t.Fatalf("after cleanup: tmp exists=%v, job file exists=%v; want tmp gone, job dir kept",
			exists(t, tmp), exists(t, jobFile))
	}
}

func TestRunningVanishedWithoutResultBecomesLost(t *testing.T) {
	e := newEnv(t, nil)
	a := e.submit(t, e.req(gib))
	e.tick(t)
	tmp := e.mkTmp(t, a.ID)
	next := e.submit(t, e.req(gib))
	waited := make(chan *model.Item, 1)
	go func() {
		it, _ := e.s.Wait(e.ctx, a.ID, WaitDone)
		waited <- it
	}()
	waitRegistered(t, e.s, a.ID)

	e.ex.SetAlive(a.ID, false) // unit gone, no result.json
	e.tick(t)
	e.clk.Advance(9 * time.Second)
	e.tick(t)
	e.wantState(t, a.ID, model.StateRunning, model.ReasonNone)

	// Processes seen again: the grace period restarts.
	e.ex.SetAlive(a.ID, true)
	e.tick(t)
	e.ex.SetAlive(a.ID, false)
	e.clk.Advance(time.Second)
	e.tick(t)
	e.clk.Advance(9 * time.Second)
	e.tick(t)
	e.wantState(t, a.ID, model.StateRunning, model.ReasonNone)
	e.wantState(t, next.ID, model.StateQueued, model.ReasonNone)

	e.clk.Advance(time.Second) // cancel_grace of continuous absence
	e.tick(t)
	got := e.wantState(t, a.ID, model.StateFailed, model.ReasonLost)
	if !strings.Contains(got.Diagnostic, "without result") || got.EndedAt.IsZero() {
		t.Fatalf("lost item %+v", got)
	}
	e.wantState(t, next.ID, model.StateRunning, model.ReasonNone)
	select {
	case it := <-waited:
		if it == nil || it.State != model.StateFailed {
			t.Fatalf("waiter got %+v", it)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("waiter not notified")
	}
	e.settle()
	if exists(t, tmp) {
		t.Fatalf("temp dir kept for LOST item")
	}
}

func TestStopRunsInBackground(t *testing.T) {
	e := newEnv(t, nil)
	release := make(chan struct{})
	hx := &hookExec{Executor: e.ex, onStop: func(model.ItemID) error {
		<-release
		return nil
	}}
	e.rebuild(t, hx, true)
	it := e.submit(t, e.req(gib))
	e.tick(t)

	// Both calls return while the first Stop is still blocked; only one Stop is in flight.
	for range 2 {
		got, err := e.s.Cancel(e.ctx, it.ID, "x")
		if err != nil || !got.CancelRequested {
			t.Fatalf("cancel: %+v %v", got, err)
		}
	}
	// Ticks are not blocked either, and do not stack another Stop.
	e.ex.Finish(it.ID, platform.ExitInfo{ServiceResult: "exit-code", ExitStatus: "143"})
	e.tick(t)
	e.clk.Advance(20 * time.Second)
	e.tick(t)
	e.wantState(t, it.ID, model.StateCleaning, model.ReasonNone)
	close(release)
	e.settle()
	if n := hx.stops(); n != 1 {
		t.Fatalf("%d Stop calls in flight, want 1", n)
	}

	// Finalization still comes from Tick.
	e.ex.SetAlive(it.ID, false)
	e.tick(t)
	e.wantState(t, it.ID, model.StateCancelled, model.ReasonCancelled)
}

func TestNewStartsRecovering(t *testing.T) {
	e := newEnv(t, nil)
	e.rebuild(t, e.ex, false)
	it := e.submit(t, e.req(gib))
	e.tick(t)
	e.wantState(t, it.ID, model.StateQueued, model.ReasonNone)
	if id, dec := e.s.HeadDecision(); id != it.ID || dec.Blocker != admission.BlockRecovering {
		t.Fatalf("before Recover: %s %+v, want recovering", id, dec)
	}
	if err := e.s.Recover(e.ctx); err != nil {
		t.Fatal(err)
	}
	e.tick(t)
	e.wantState(t, it.ID, model.StateRunning, model.ReasonNone)
}

// Cancel's Stop runs before the unit exists (a no-op for the real executor) and is
// still in flight when Launch returns, so start() cannot stop again. Tick must.
func TestCancelDuringLaunchStopsAfterUnitExists(t *testing.T) {
	e := newEnv(t, nil)
	release := make(chan struct{})
	hx := &hookExec{Executor: e.ex}
	hx.onStop = func(model.ItemID) error {
		if hx.stops() == 1 {
			<-release
		}
		return nil
	}
	e.rebuild(t, hx, true)
	it := e.submit(t, e.req(gib))
	e.ex.SetBeforeLaunch(func(sp platform.LaunchSpec) {
		if _, err := e.s.Cancel(e.ctx, sp.ID, "race"); err != nil {
			t.Error(err)
		}
	})
	e.tick(t)
	got := e.wantState(t, it.ID, model.StateRunning, model.ReasonNone)
	if !got.CancelRequested {
		t.Fatalf("cancel not recorded")
	}
	e.tick(t) // first Stop still in flight: no second one
	close(release)
	e.settle()
	e.tick(t) // within cancel_grace of the first Stop
	e.settle()
	if n := hx.stops(); n != 1 {
		t.Fatalf("%d stops before cancel_grace elapsed, want 1", n)
	}
	e.clk.Advance(10 * time.Second)
	e.tick(t)
	e.settle()
	if n := hx.stops(); n != 2 {
		t.Fatalf("%d stops after cancel_grace, want 2", n)
	}
	e.finish(it.ID, platform.ExitInfo{ServiceResult: "exit-code", ExitStatus: "143"})
	e.tick(t)
	e.wantState(t, it.ID, model.StateCancelled, model.ReasonCancelled)
}

func TestFailedCancelStopRetried(t *testing.T) {
	e := newEnv(t, nil)
	hx := &hookExec{Executor: e.ex}
	hx.onStop = func(model.ItemID) error {
		if hx.stops() == 1 {
			return errors.New("systemctl: connection refused")
		}
		return nil
	}
	e.rebuild(t, hx, true)
	it := e.submit(t, e.req(gib))
	e.tick(t)
	if _, err := e.s.Cancel(e.ctx, it.ID, "x"); err != nil {
		t.Fatal(err)
	}
	e.settle()
	if len(e.ex.Stopped()) != 0 {
		t.Fatalf("first Stop should have failed")
	}
	e.clk.Advance(10 * time.Second)
	e.tick(t)
	e.settle()
	if !contains(e.ex.Stopped(), it.ID) {
		t.Fatalf("failed Stop not retried")
	}
}

func TestLiveMemoryBlocks(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.PoolMemory = 16 * gib })
	e.host.Set(platform.MemInfo{Total: 31 * gib, Available: 3 * gib}) // floor 3.1 GiB
	it := e.submit(t, e.req(gib))
	e.tick(t)
	e.wantState(t, it.ID, model.StateQueued, model.ReasonNone)
	if id, dec := e.s.HeadDecision(); id != it.ID || dec.Blocker != admission.BlockLiveMemory {
		t.Fatalf("head %s %+v, want live_memory", id, dec)
	}
	e.host.Set(platform.MemInfo{Total: 31 * gib, Available: 8 * gib})
	e.tick(t)
	e.wantState(t, it.ID, model.StateRunning, model.ReasonNone)
}

func TestPressureBlocks(t *testing.T) {
	e := newEnv(t, nil)
	e.host.SetPressure(6)
	e.tick(t)
	e.clk.Advance(10 * time.Second)
	e.tick(t)
	if closed, avg := e.s.Pressure(); !closed || avg != 6 {
		t.Fatalf("pressure %v %v, want closed at 6", closed, avg)
	}
	it := e.submit(t, e.req(gib))
	e.tick(t)
	e.wantState(t, it.ID, model.StateQueued, model.ReasonNone)
	if _, dec := e.s.HeadDecision(); dec.Blocker != admission.BlockPressure {
		t.Fatalf("decision %+v, want pressure", dec)
	}

	e.host.SetPressure(0.5)
	e.tick(t)
	e.clk.Advance(29 * time.Second)
	e.tick(t)
	e.wantState(t, it.ID, model.StateQueued, model.ReasonNone)
	e.clk.Advance(time.Second)
	e.tick(t)
	e.wantState(t, it.ID, model.StateRunning, model.ReasonNone)
	if closed, _ := e.s.Pressure(); closed {
		t.Fatalf("gate still closed")
	}
}

func TestUnconsumedCountsRecentStarts(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.PoolMemory = 16 * gib })
	e.host.Set(platform.MemInfo{Total: 30 * gib, Available: 8 * gib}) // floor 3 GiB
	a := e.submit(t, e.req(3*gib))
	b := e.submit(t, e.req(3*gib))
	e.tick(t) // both heads evaluated in one Tick: 8-3 < 3+3
	e.wantState(t, a.ID, model.StateRunning, model.ReasonNone)
	e.wantState(t, b.ID, model.StateQueued, model.ReasonNone)
	if _, dec := e.s.HeadDecision(); dec.Blocker != admission.BlockLiveMemory {
		t.Fatalf("decision %+v, want live_memory", dec)
	}

	e.ex.SetCurrent(a.ID, 0)
	e.tick(t) // a started but consumed nothing yet: still unconsumed
	e.wantState(t, b.ID, model.StateQueued, model.ReasonNone)

	e.ex.SetCurrent(a.ID, 3*gib)
	e.tick(t)
	e.wantState(t, b.ID, model.StateRunning, model.ReasonNone)
}

func TestGuardActiveFlag(t *testing.T) {
	e := newEnv(t, nil)
	if e.s.GuardActive() {
		t.Fatal("guard active by default")
	}
	e.s.SetGuardActive(true)
	it := e.submit(t, e.req(gib))
	e.tick(t)
	e.finish(it.ID, platform.ExitInfo{ServiceResult: "oom-kill", ExitCode: "killed", ExitStatus: "KILL", OwnOOMKills: 1})
	e.tick(t)
	e.wantState(t, it.ID, model.StateFailed, model.ReasonOOMHostGuard)
}

// waitRegistered spins until a Wait for id has subscribed.
func waitRegistered(t *testing.T, s *Scheduler, id model.ItemID) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		_, ok := s.waiters[id]
		s.mu.Unlock()
		if ok {
			return
		}
		runtime.Gosched()
	}
	t.Fatal("Wait never subscribed")
}

func TestWaitReturnsOnTerminal(t *testing.T) {
	e := newEnv(t, nil)
	it := e.submit(t, e.req(gib))
	e.tick(t)

	type res struct {
		it  *model.Item
		err error
	}
	ch := make(chan res, 1)
	go func() {
		got, err := e.s.Wait(e.ctx, it.ID, WaitDone)
		ch <- res{got, err}
	}()
	waitRegistered(t, e.s, it.ID)
	e.finish(it.ID, platform.ExitInfo{ServiceResult: "exit-code", ExitCode: "exited", ExitStatus: "3"})
	e.tick(t)
	select {
	case r := <-ch:
		if r.err != nil || r.it.State != model.StateFailed || r.it.Reason != model.ReasonCommandFailed ||
			r.it.ExitCode == nil || *r.it.ExitCode != 3 {
			t.Fatalf("wait: %+v %v", r.it, r.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Wait did not return after finalization")
	}

	// Already terminal: returns at once.
	if got, err := e.s.Wait(e.ctx, it.ID, WaitDone); err != nil || got.State != model.StateFailed {
		t.Fatalf("wait terminal: %+v %v", got, err)
	}

	// Context cancellation.
	other := e.submit(t, e.req(gib))
	ctx, cancel := context.WithCancel(e.ctx)
	go func() {
		got, err := e.s.Wait(ctx, other.ID, WaitDone)
		ch <- res{got, err}
	}()
	waitRegistered(t, e.s, other.ID)
	cancel()
	select {
	case r := <-ch:
		if !errors.Is(r.err, context.Canceled) {
			t.Fatalf("wait after cancel: %+v %v", r.it, r.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Wait ignored ctx cancellation")
	}
}

func TestRunTicksOnKick(t *testing.T) {
	e := newEnv(t, nil)
	launched := make(chan model.ItemID, 1)
	e.ex.SetBeforeLaunch(func(sp platform.LaunchSpec) { launched <- sp.ID })
	ctx, cancel := context.WithCancel(e.ctx)
	done := make(chan error, 1)
	go func() { done <- e.s.Run(ctx) }()
	it := e.submit(t, e.req(gib)) // Submit kicks the loop
	select {
	case id := <-launched:
		if id != it.ID {
			t.Fatalf("launched %s", id)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not admit the submitted item")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned %v", err)
	}
}
