package scheduler

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"resource-dispatcher/internal/admission"
	"resource-dispatcher/internal/config"
	"resource-dispatcher/internal/model"
	"resource-dispatcher/internal/platform"
)

var (
	exit0 = platform.ExitInfo{ServiceResult: "exit-code", ExitCode: "exited", ExitStatus: "0"}
	exit1 = platform.ExitInfo{ServiceResult: "exit-code", ExitCode: "exited", ExitStatus: "1"}
)

// grantStand reserves a 100M stand without a task budget for ttl (with a captured env)
// and grants it with one Tick; t0 is the grant time.
func (e *env) grantStand(t *testing.T, ttl time.Duration, mut func(*ReserveRequest)) (*model.Item, time.Time) {
	t.Helper()
	r := e.rreq(100 * mib)
	r.TTL = ttl
	r.Env = map[string]string{"PATH": "/usr/bin", "DOCKER_HOST": "unix:///run/docker.sock"}
	if mut != nil {
		mut(&r)
	}
	res := e.reserve(t, r)
	t0 := e.clk.Now()
	e.tick(t)
	e.wantState(t, res.ID, model.StateGranted, model.ReasonNone)
	return e.get(t, res.ID), t0
}

// tearDown moves the clock to res's expiry and ticks: res must be TEARING_DOWN.
func (e *env) tearDown(t *testing.T, res *model.Item) *model.Item {
	t.Helper()
	e.tickAt(t, res.ExpiresAt)
	return e.wantState(t, res.ID, model.StateTearingDown, model.ReasonNone)
}

// teardownSpecs are the launched specs of teardown units.
func teardownSpecs(e *env) []platform.LaunchSpec {
	var out []platform.LaunchSpec
	for _, s := range e.ex.Launched() {
		if model.IsTeardownUnitID(s.ID) {
			out = append(out, s)
		}
	}
	return out
}

func (e *env) wantEvent(t *testing.T, id model.ItemID, kind model.EventKind, text string) model.Event {
	t.Helper()
	evs := e.events(t, id, kind)
	if len(evs) != 1 || evs[0].Audience != model.AudienceOperator || !strings.Contains(evs[0].Detail, text) {
		t.Fatalf("%s events %+v, want one for the operator containing %q", kind, evs, text)
	}
	return evs[0]
}

// wantPoolBlocked submits a task for the whole pool and checks that it waits.
func (e *env) wantPoolBlocked(t *testing.T) *model.Item {
	t.Helper()
	task := e.submit(t, e.req(e.cfg.PoolMemory))
	e.tick(t)
	e.wantState(t, task.ID, model.StateQueued, model.ReasonNone)
	e.wantHead(t, task.ID, admission.BlockPoolMemory)
	return task
}

func writeFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestExpiryLaunchesTeardown(t *testing.T) {
	e := newEnv(t, nil)
	res, t0 := e.grantStand(t, time.Hour, nil)
	e.tickAt(t, t0.Add(time.Hour-time.Second))
	e.wantState(t, res.ID, model.StateExpiring, model.ReasonNone)
	if n := len(teardownSpecs(e)); n != 0 {
		t.Fatalf("%d teardown launches before expiry", n)
	}

	e.tickAt(t, t0.Add(time.Hour))
	it := e.wantState(t, res.ID, model.StateTearingDown, model.ReasonNone)
	if it.TeardownAttempt != 1 || it.TeardownLaunchError != "" {
		t.Fatalf("attempt %d launch error %q", it.TeardownAttempt, it.TeardownLaunchError)
	}
	specs := e.ex.Launched()
	if len(specs) != 1 {
		t.Fatalf("launched %d specs, want one teardown", len(specs))
	}
	s := specs[0]
	if s.ID != model.ItemID("teardown-"+string(res.ID)) ||
		len(s.Argv) != 2 || s.Argv[0] != "bash" || s.Argv[1] != e.paths.TeardownScript(res.ID) {
		t.Fatalf("spec id %s argv %q", s.ID, s.Argv)
	}
	if s.Env["AQ_ORIGINAL_CWD"] != e.cwd || s.Env["AQ_RESERVATION_ID"] != string(res.ID) ||
		s.Env["PATH"] != "/usr/bin" || s.Env["DOCKER_HOST"] != "unix:///run/docker.sock" {
		t.Fatalf("env %v", s.Env)
	}
	if s.Limit != 512*mib || s.MemoryHigh != 512*mib || s.CPUs != 1 || s.Timeout != 5*time.Minute ||
		s.KillGrace != e.cfg.CancelGrace {
		t.Fatalf("limits %+v", s)
	}
	if s.JobDir != e.paths.TeardownDir(res.ID, 1) || s.TmpDir != e.paths.TeardownTmpDir(res.ID, 1) || s.Cwd != e.cwd {
		t.Fatalf("dirs job %s tmp %s cwd %s", s.JobDir, s.TmpDir, s.Cwd)
	}
	if s.MaxStdout != e.cfg.Logs.MaxStdout || s.MaxStderr != e.cfg.Logs.MaxStderr {
		t.Fatalf("log caps %d %d", s.MaxStdout, s.MaxStderr)
	}
	if exists(t, filepath.Join(s.JobDir, "cwd")) {
		t.Fatal("fallback cwd created although the original exists")
	}
	e.wantPoolBlocked(t)
}

// Review Focus: the worktree is gone before expiry; the copy runs in an empty dir.
func TestTeardownCwdFallback(t *testing.T) {
	e := newEnv(t, nil)
	cwd := filepath.Join(t.TempDir(), "worktree")
	if err := os.Mkdir(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	res, _ := e.grantStand(t, time.Hour, func(r *ReserveRequest) { r.Cwd = cwd })
	if err := os.RemoveAll(cwd); err != nil {
		t.Fatal(err)
	}
	e.tearDown(t, res)
	s := teardownSpecs(e)[0]
	want := filepath.Join(e.paths.TeardownDir(res.ID, 1), "cwd")
	if s.Cwd != want || s.Env["AQ_ORIGINAL_CWD"] != cwd {
		t.Fatalf("cwd %s AQ_ORIGINAL_CWD %s, want %s and %s", s.Cwd, s.Env["AQ_ORIGINAL_CWD"], want, cwd)
	}
	ents, err := os.ReadDir(want)
	if err != nil || len(ents) != 0 {
		t.Fatalf("fallback cwd: %v %v, want an empty directory", ents, err)
	}
}

func TestTeardownSuccess(t *testing.T) {
	e := newEnv(t, nil)
	res, _ := e.grantStand(t, time.Hour, func(r *ReserveRequest) { r.Name = "web-stand" })
	e.tearDown(t, res)
	e.finish(model.TeardownUnitID(res.ID), exit0)
	e.tick(t)
	it := e.wantState(t, res.ID, model.StateTornDown, model.ReasonNone)
	if it.Env != nil || it.EndedAt.IsZero() {
		t.Fatalf("env %v ended %v", it.Env, it.EndedAt)
	}
	e.wantEvent(t, res.ID, model.EventTeardownSucceeded, "torn down forgotten stand web-stand")
	task := e.submit(t, e.req(gib))
	e.tick(t)
	e.wantState(t, task.ID, model.StateRunning, model.ReasonNone)
}

// §15: a failed teardown keeps the reservation in the accounting, with no automatic retry.
func TestTeardownFailureKeepsAccounting(t *testing.T) {
	e := newEnv(t, nil)
	res, _ := e.grantStand(t, time.Hour, nil)
	e.tearDown(t, res)
	e.finish(model.TeardownUnitID(res.ID), exit1)
	e.tick(t)
	it := e.wantState(t, res.ID, model.StateTeardownFailed, model.ReasonCommandFailed)
	if it.Env == nil {
		t.Fatal("env dropped from a non-terminal reservation")
	}
	stderr := filepath.Join(e.paths.TeardownDir(res.ID, 1), "stderr.log")
	ev := e.wantEvent(t, res.ID, model.EventTeardownFailed, stderr)
	if !strings.Contains(ev.Detail, "exit status 1") || !strings.Contains(it.Diagnostic, stderr) {
		t.Fatalf("detail %q diagnostic %q", ev.Detail, it.Diagnostic)
	}
	e.wantPoolBlocked(t)
	for range 10 {
		e.clk.Advance(time.Minute)
		e.tick(t)
	}
	e.wantState(t, res.ID, model.StateTeardownFailed, model.ReasonCommandFailed)
	if n := len(teardownSpecs(e)); n != 1 {
		t.Fatalf("%d teardown launches, want 1 (no automatic retry)", n)
	}
}

func TestTeardownTimeoutFails(t *testing.T) {
	e := newEnv(t, nil)
	res, _ := e.grantStand(t, time.Hour, nil)
	e.tearDown(t, res)
	e.finish(model.TeardownUnitID(res.ID), platform.ExitInfo{ServiceResult: "timeout", ExitCode: "killed", ExitStatus: "TERM"})
	e.tick(t)
	e.wantState(t, res.ID, model.StateTeardownFailed, model.ReasonTimeout)
	e.wantEvent(t, res.ID, model.EventTeardownFailed, "timed out")
}

func TestTeardownLaunchFailure(t *testing.T) {
	e := newEnv(t, nil)
	res, _ := e.grantStand(t, time.Hour, nil)
	e.ex.FailLaunch(errors.New("systemd-run: unit already exists"))
	e.tickAt(t, res.ExpiresAt)
	it := e.wantState(t, res.ID, model.StateTeardownFailed, model.ReasonLaunchFailed)
	if !strings.Contains(it.Diagnostic, "unit already exists") || it.TeardownAttempt != 1 {
		t.Fatalf("diagnostic %q attempt %d", it.Diagnostic, it.TeardownAttempt)
	}
	e.wantEvent(t, res.ID, model.EventTeardownFailed, "unit already exists")
	e.wantPoolBlocked(t)
}

func TestTeardownVanished(t *testing.T) {
	e := newEnv(t, nil) // cancel_grace 10s
	res, _ := e.grantStand(t, time.Hour, nil)
	e.tearDown(t, res)
	e.ex.SetAlive(model.TeardownUnitID(res.ID), false)
	e.tick(t)
	e.clk.Advance(e.cfg.CancelGrace - time.Second)
	e.tick(t)
	e.wantState(t, res.ID, model.StateTearingDown, model.ReasonNone)
	if n := len(e.events(t, res.ID, model.EventTeardownUnknown)); n != 0 {
		t.Fatalf("%d unknown events before cancel_grace", n)
	}
	e.clk.Advance(time.Second)
	e.tick(t)
	it := e.wantState(t, res.ID, model.StateTeardownUnknown, model.ReasonLost)
	if it.Env == nil {
		t.Fatal("env dropped from a non-terminal reservation")
	}
	e.wantEvent(t, res.ID, model.EventTeardownUnknown, string(res.ID))
	e.wantPoolBlocked(t)
}

// Codex r1: while Launch is in flight the reaper must not take the missing unit for a
// vanished one.
func TestTeardownLaunchInFlightNotReaped(t *testing.T) {
	e := newEnv(t, nil)
	res, _ := e.grantStand(t, time.Hour, nil)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	e.ex.SetBeforeLaunch(func(sp platform.LaunchSpec) {
		if model.IsTeardownUnitID(sp.ID) {
			close(entered)
			<-release
		}
	})
	e.clk.Advance(res.ExpiresAt.Sub(e.clk.Now()))
	done := make(chan error, 1)
	go func() { done <- e.s.Tick(e.ctx) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		unblock()
		t.Fatal("teardown Launch never started")
	}

	e.clk.Advance(3 * e.cfg.CancelGrace)
	e.tick(t)
	e.tick(t)
	e.wantState(t, res.ID, model.StateTearingDown, model.ReasonNone)
	if n := len(e.events(t, res.ID, model.EventTeardownUnknown)); n != 0 {
		t.Fatalf("%d unknown events while launching", n)
	}
	_, err := e.s.Release(e.ctx, res.ID, true)
	wantBadState(t, err, "TEARING_DOWN")

	unblock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := len(teardownSpecs(e)); n != 1 {
		t.Fatalf("%d teardown launches, want 1", n)
	}
	e.finish(model.TeardownUnitID(res.ID), exit0)
	e.tick(t)
	e.wantState(t, res.ID, model.StateTornDown, model.ReasonNone)
}

// launchErrorAlive starts res's teardown with a Launch error while the unit lives on.
func (e *env) launchErrorAlive(t *testing.T, res *model.Item) *model.Item {
	t.Helper()
	uid := model.TeardownUnitID(res.ID)
	e.ex.FailLaunch(errors.New("teardown unit started without its memory limit"))
	e.ex.SetAlive(uid, true)
	it := e.tearDown(t, res)
	if it.TeardownLaunchError == "" || !strings.Contains(it.Diagnostic, "without its memory limit") {
		t.Fatalf("launch error %q diagnostic %q", it.TeardownLaunchError, it.Diagnostic)
	}
	return it
}

// Codex r1: a Launch error with the unit alive stays TEARING_DOWN until the unit is gone.
func TestTeardownLaunchErrorWithLiveUnit(t *testing.T) {
	e := newEnv(t, nil)
	res, _ := e.grantStand(t, time.Hour, nil)
	uid := model.TeardownUnitID(res.ID)
	e.launchErrorAlive(t, res)
	e.settle()
	if !contains(e.ex.Stopped(), uid) {
		t.Fatalf("stopped %v, want %s", e.ex.Stopped(), uid)
	}
	_, err := e.s.Release(e.ctx, res.ID, true)
	wantBadState(t, err, "TEARING_DOWN")

	// The stop is repeated every cancel_grace while the unit lives.
	e.tick(t)
	e.settle()
	if n := len(e.ex.Stopped()); n != 1 {
		t.Fatalf("%d stops within cancel_grace, want 1", n)
	}
	e.clk.Advance(e.cfg.CancelGrace)
	e.tick(t)
	e.settle()
	if n := len(e.ex.Stopped()); n != 2 {
		t.Fatalf("%d stops after cancel_grace, want 2", n)
	}

	e.ex.SetAlive(uid, false)
	e.clk.Advance(e.cfg.CancelGrace)
	e.tick(t)
	it := e.wantState(t, res.ID, model.StateTeardownFailed, model.ReasonLaunchFailed)
	if !strings.Contains(it.Diagnostic, "without its memory limit") {
		t.Fatalf("diagnostic %q", it.Diagnostic)
	}
	e.wantEvent(t, res.ID, model.EventTeardownFailed, "without its memory limit")
}

// Codex r2: a successful result of an attempt whose launch failed is not trusted.
func TestTeardownLaunchErrorThenSuccessResult(t *testing.T) {
	e := newEnv(t, nil)
	res, _ := e.grantStand(t, time.Hour, nil)
	uid := model.TeardownUnitID(res.ID)
	e.launchErrorAlive(t, res)
	e.ex.Finish(uid, exit0) // result.json of attempt 1
	if r, _ := e.ex.ReadResult(e.paths.TeardownDir(res.ID, 1)); r == nil {
		t.Fatal("result not in the attempt's dir")
	}
	e.tick(t)
	e.wantState(t, res.ID, model.StateTearingDown, model.ReasonNone)
	e.ex.SetAlive(uid, false)
	e.tick(t)
	e.wantState(t, res.ID, model.StateTeardownFailed, model.ReasonLaunchFailed)
}

// Codex r2: a retry after a launch error starts clean.
func TestRetryAfterLaunchErrorSucceeds(t *testing.T) {
	e := newEnv(t, nil)
	res, _ := e.grantStand(t, time.Hour, nil)
	uid := model.TeardownUnitID(res.ID)
	e.launchErrorAlive(t, res)
	e.ex.SetAlive(uid, false)
	e.tick(t)
	e.wantState(t, res.ID, model.StateTeardownFailed, model.ReasonLaunchFailed)
	e.settle()

	e.ex.FailLaunch(nil)
	if _, err := e.s.RetryTeardown(e.ctx, res.ID); err != nil {
		t.Fatal(err)
	}
	e.tick(t)
	it := e.wantState(t, res.ID, model.StateTearingDown, model.ReasonNone)
	if it.TeardownAttempt != 2 || it.TeardownLaunchError != "" || it.Diagnostic != "" {
		t.Fatalf("attempt %d launch error %q diagnostic %q", it.TeardownAttempt, it.TeardownLaunchError, it.Diagnostic)
	}
	e.finish(uid, exit0)
	e.tick(t)
	if it := e.wantState(t, res.ID, model.StateTornDown, model.ReasonNone); it.TeardownLaunchError != "" {
		t.Fatalf("launch error %q", it.TeardownLaunchError)
	}
}

// Codex r3: a new attempt does not start while the previous attempt's Stop is in flight.
func TestRetryFencedByPreviousStop(t *testing.T) {
	e := newEnv(t, nil)
	res, _ := e.grantStand(t, time.Hour, nil)
	uid := model.TeardownUnitID(res.ID)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock) // before the env's cleanup waits for background stops
	e.ex.SetBeforeStop(func(id model.ItemID) {
		if id == uid {
			<-release
		}
	})
	e.launchErrorAlive(t, res)
	e.ex.SetAlive(uid, false)
	e.tick(t)
	e.wantState(t, res.ID, model.StateTeardownFailed, model.ReasonLaunchFailed)
	_, err := e.s.RetryTeardown(e.ctx, res.ID)
	wantBadState(t, err, "previous teardown attempt is still stopping")
	e.wantState(t, res.ID, model.StateTeardownFailed, model.ReasonLaunchFailed)

	unblock()
	e.settle()
	e.ex.FailLaunch(nil)
	if _, err := e.s.RetryTeardown(e.ctx, res.ID); err != nil {
		t.Fatal(err)
	}
	stops := len(e.ex.Stopped())
	e.tick(t)
	e.wantState(t, res.ID, model.StateTearingDown, model.ReasonNone)
	if n := len(teardownSpecs(e)); n != 1 { // attempt 1's Launch failed, so it is not recorded
		t.Fatalf("%d teardown launches recorded, want attempt 2 only", n)
	}
	for range 3 {
		e.clk.Advance(e.cfg.CancelGrace)
		e.tick(t)
		e.settle()
	}
	if n := len(e.ex.Stopped()); n != stops {
		t.Fatalf("stops grew from %d to %d after the second attempt started", stops, n)
	}
}

// A due reservation waits (EXPIRING) while a Stop of its teardown unit is in flight.
func TestTeardownWaitsForPreviousStop(t *testing.T) {
	e := newEnv(t, nil)
	res, _ := e.grantStand(t, time.Hour, nil)
	uid := model.TeardownUnitID(res.ID)
	e.s.mu.Lock()
	e.s.stopping[uid] = true
	e.s.mu.Unlock()
	e.tickAt(t, res.ExpiresAt)
	e.wantState(t, res.ID, model.StateExpiring, model.ReasonNone)
	if n := len(teardownSpecs(e)); n != 0 {
		t.Fatalf("%d teardown launches while stopping", n)
	}
	e.s.mu.Lock()
	delete(e.s.stopping, uid)
	e.s.mu.Unlock()
	e.tick(t)
	e.wantState(t, res.ID, model.StateTearingDown, model.ReasonNone)
}

// Codex r2, r3: each attempt has its own temp dir, removed once its processes are gone;
// the attempt's logs stay.
func TestTeardownTmpDirRemoved(t *testing.T) {
	e := newEnv(t, nil)
	res, _ := e.grantStand(t, time.Hour, nil)
	uid := model.TeardownUnitID(res.ID)
	stderr1 := filepath.Join(e.paths.TeardownDir(res.ID, 1), "stderr.log")

	// attempt 1: TEARDOWN_FAILED
	e.tearDown(t, res)
	tmp1 := e.paths.TeardownTmpDir(res.ID, 1)
	writeFile(t, filepath.Join(tmp1, "scratch"))
	writeFile(t, stderr1)
	e.finish(uid, exit1)
	e.tick(t)
	e.wantState(t, res.ID, model.StateTeardownFailed, model.ReasonCommandFailed)
	e.settle()
	if exists(t, tmp1) || !exists(t, stderr1) {
		t.Fatalf("after FAILED: tmp kept %v, stderr.log kept %v", exists(t, tmp1), exists(t, stderr1))
	}

	// attempt 2: TEARDOWN_UNKNOWN
	if _, err := e.s.RetryTeardown(e.ctx, res.ID); err != nil {
		t.Fatal(err)
	}
	e.tick(t)
	if s := teardownSpecs(e); len(s) != 2 || s[1].TmpDir != e.paths.TeardownTmpDir(res.ID, 2) {
		t.Fatalf("second spec %+v", s)
	}
	tmp2 := e.paths.TeardownTmpDir(res.ID, 2)
	writeFile(t, filepath.Join(tmp2, "scratch"))
	e.ex.SetAlive(uid, false)
	e.tick(t)
	e.clk.Advance(e.cfg.CancelGrace)
	e.tick(t)
	e.wantState(t, res.ID, model.StateTeardownUnknown, model.ReasonLost)
	e.settle()
	if exists(t, tmp2) {
		t.Fatal("after UNKNOWN: tmp kept")
	}

	// attempt 3: TORN_DOWN
	if _, err := e.s.RetryTeardown(e.ctx, res.ID); err != nil {
		t.Fatal(err)
	}
	e.tick(t)
	tmp3 := e.paths.TeardownTmpDir(res.ID, 3)
	writeFile(t, filepath.Join(tmp3, "scratch"))
	e.finish(uid, exit0)
	e.tick(t)
	e.wantState(t, res.ID, model.StateTornDown, model.ReasonNone)
	e.settle()
	if exists(t, tmp3) || !exists(t, stderr1) {
		t.Fatalf("after TORN_DOWN: tmp kept %v, stderr.log kept %v", exists(t, tmp3), exists(t, stderr1))
	}
}

// Codex r1: at most teardown_concurrency teardowns at once, slots by ascending expires_at.
func TestTeardownConcurrencyLimit(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.Reservations.TeardownConcurrency = 2 })
	var ids [3]model.ItemID
	// queued in another order than they expire
	for i, ttl := range []time.Duration{time.Hour + 2*time.Second, time.Hour, time.Hour + time.Second} {
		r := e.rreq(100 * mib)
		r.TTL = ttl
		ids[i] = e.reserve(t, r).ID
	}
	t0 := e.clk.Now()
	e.tick(t)
	last, first, second := ids[0], ids[1], ids[2]

	e.tickAt(t, t0.Add(time.Hour+5*time.Second))
	e.wantState(t, first, model.StateTearingDown, model.ReasonNone)
	e.wantState(t, second, model.StateTearingDown, model.ReasonNone)
	e.wantState(t, last, model.StateExpiring, model.ReasonNone)
	if n := len(teardownSpecs(e)); n != 2 {
		t.Fatalf("%d teardown launches, want 2", n)
	}
	task := e.submit(t, e.req(800*mib)) // 3×100M still held: 300M + 800M > 1G
	e.tick(t)
	e.wantHead(t, task.ID, admission.BlockPoolMemory)
	e.wantState(t, last, model.StateExpiring, model.ReasonNone)

	e.finish(model.TeardownUnitID(first), exit0)
	e.tick(t)
	e.wantState(t, first, model.StateTornDown, model.ReasonNone)
	e.wantState(t, last, model.StateTearingDown, model.ReasonNone)
	if s := teardownSpecs(e); len(s) != 3 || s[2].ID != model.TeardownUnitID(last) {
		t.Fatalf("launches %v", launchedIDs(e.ex))
	}
}

// Codex r1, ruling P2: the unused part of a running teardown's memory is unconsumed
// (all of it while its Launch is in flight); the tearing-down stand still counts.
func TestTeardownUnconsumedCountsLive(t *testing.T) {
	e := newResEnv(t) // floor 1.6G, teardown_memory 512M
	e.available(3 * gib)
	res, _ := e.grantStand(t, time.Hour, nil)
	uid := model.TeardownUnitID(res.ID)
	var b *model.Item
	e.ex.SetBeforeLaunch(func(sp platform.LaunchSpec) {
		if sp.ID != uid {
			return
		}
		e.ex.SetCurrent(uid, 512*mib) // not read while launching: the whole 512M counts
		b = e.reserve(t, e.rreq(gib))
		e.tick(t)
		e.wantState(t, b.ID, model.StateQueued, model.ReasonNone)
		e.wantHead(t, b.ID, admission.BlockLiveMemory)
	})
	e.tearDown(t, res)
	e.ex.SetBeforeLaunch(nil)

	// 3072 − 100 − 512 = 2460 < 1638.4 + 1024
	e.ex.SetCurrent(uid, 0)
	e.tick(t)
	e.wantState(t, b.ID, model.StateQueued, model.ReasonNone)
	e.wantHead(t, b.ID, admission.BlockLiveMemory)

	// 3072 − 100 − 0 = 2972 ≥ 2662.4
	e.ex.SetCurrent(uid, 512*mib)
	e.tick(t)
	e.wantState(t, b.ID, model.StateGranted, model.ReasonNone)
}

func TestRetryTeardown(t *testing.T) {
	e := newEnv(t, nil)
	res, _ := e.grantStand(t, time.Hour, nil)
	other, _ := e.grantStand(t, 2*time.Hour, nil)
	uid := model.TeardownUnitID(res.ID)
	_, err := e.s.RetryTeardown(e.ctx, other.ID)
	wantBadState(t, err, "GRANTED")

	e.tearDown(t, res)
	_, err = e.s.RetryTeardown(e.ctx, res.ID)
	wantBadState(t, err, "TEARING_DOWN")
	e.finish(uid, exit1)
	e.tick(t)
	e.wantState(t, res.ID, model.StateTeardownFailed, model.ReasonCommandFailed)

	e.ex.SetAlive(uid, true)
	_, err = e.s.RetryTeardown(e.ctx, res.ID)
	wantBadState(t, err, "still running")
	e.ex.SetAlive(uid, false)

	got, err := e.s.RetryTeardown(e.ctx, res.ID)
	if err != nil {
		t.Fatal(err)
	}
	now := e.clk.Now()
	it := e.get(t, res.ID)
	for _, x := range []*model.Item{got, it} {
		if x.State != model.StateExpiring || !x.ExpiresAt.Equal(now) || !x.WarnAt.IsZero() || x.TTLHeld {
			t.Fatalf("after retry: %s expires %v warn %v held %v", x.State, x.ExpiresAt, x.WarnAt, x.TTLHeld)
		}
	}
	e.tick(t)
	it = e.wantState(t, res.ID, model.StateTearingDown, model.ReasonNone)
	specs := teardownSpecs(e)
	if it.TeardownAttempt != 2 || len(specs) != 2 || specs[1].JobDir != e.paths.TeardownDir(res.ID, 2) {
		t.Fatalf("attempt %d specs %+v", it.TeardownAttempt, specs)
	}

	task := e.submit(t, e.req(64*mib))
	_, err = e.s.RetryTeardown(e.ctx, task.ID)
	wantBadState(t, err, "not a reservation")
}

func TestForceRelease(t *testing.T) {
	e := newEnv(t, nil)
	unknown, _ := e.grantStand(t, time.Hour, nil)
	failed, _ := e.grantStand(t, time.Hour, nil)

	e.tickAt(t, failed.ExpiresAt)
	e.wantState(t, unknown.ID, model.StateTearingDown, model.ReasonNone)
	_, err := e.s.Release(e.ctx, unknown.ID, true)
	wantBadState(t, err, "TEARING_DOWN")

	e.finish(model.TeardownUnitID(failed.ID), exit1)
	e.ex.SetAlive(model.TeardownUnitID(unknown.ID), false)
	e.tick(t)
	e.clk.Advance(e.cfg.CancelGrace)
	e.tick(t)
	e.wantState(t, unknown.ID, model.StateTeardownUnknown, model.ReasonLost)
	e.wantState(t, failed.ID, model.StateTeardownFailed, model.ReasonCommandFailed)

	_, err = e.s.Release(e.ctx, failed.ID, false)
	wantBadState(t, err, "TEARDOWN_FAILED")
	wantBadState(t, err, "use aq teardown --retry or aq release --force")
	_, err = e.s.Release(e.ctx, unknown.ID, false)
	wantBadState(t, err, "use aq teardown --retry or aq release --force")
	e.wantState(t, failed.ID, model.StateTeardownFailed, model.ReasonCommandFailed)

	for _, id := range []model.ItemID{unknown.ID, failed.ID} {
		got, err := e.s.Release(e.ctx, id, true)
		if err != nil {
			t.Fatal(err)
		}
		it := e.wantState(t, id, model.StateReleased, model.ReasonNone)
		for _, x := range []*model.Item{got, it} {
			if x.Diagnostic != "released by operator (--force)" || x.Env != nil {
				t.Fatalf("released: diagnostic %q env %v", x.Diagnostic, x.Env)
			}
		}
	}
	task := e.submit(t, e.req(gib))
	e.tick(t)
	e.wantState(t, task.ID, model.StateRunning, model.ReasonNone)
}

// Final review F3: --force does not release a stand whose teardown unit still runs or
// is still being stopped, as RetryTeardown does not retry it.
func TestForceReleaseFencedByTeardownUnit(t *testing.T) {
	e := newEnv(t, nil)
	res, _ := e.grantStand(t, time.Hour, nil)
	uid := model.TeardownUnitID(res.ID)
	e.tearDown(t, res)
	e.ex.SetAlive(uid, false)
	e.tick(t)
	e.clk.Advance(e.cfg.CancelGrace)
	e.tick(t)
	e.wantState(t, res.ID, model.StateTeardownUnknown, model.ReasonLost)

	e.ex.SetAlive(uid, true)
	_, err := e.s.Release(e.ctx, res.ID, true)
	wantBadState(t, err, "still running")
	e.wantState(t, res.ID, model.StateTeardownUnknown, model.ReasonLost)
	e.ex.SetAlive(uid, false)

	e.s.mu.Lock()
	e.s.stopping[uid] = true
	e.s.mu.Unlock()
	_, err = e.s.Release(e.ctx, res.ID, true)
	wantBadState(t, err, "still stopping")
	e.wantState(t, res.ID, model.StateTeardownUnknown, model.ReasonLost)
	e.s.mu.Lock()
	delete(e.s.stopping, uid)
	e.s.mu.Unlock()

	if _, err := e.s.Release(e.ctx, res.ID, true); err != nil {
		t.Fatal(err)
	}
	e.wantState(t, res.ID, model.StateReleased, model.ReasonNone)
}

// The teardown unit is not a task: Tick never stops a healthy one.
func TestTeardownNotStoppedAsTaskOrphan(t *testing.T) {
	e := newEnv(t, nil)
	res, _ := e.grantStand(t, time.Hour, nil)
	e.tearDown(t, res)
	for range 5 {
		e.clk.Advance(e.cfg.CancelGrace)
		e.tick(t)
	}
	e.settle()
	if s := e.ex.Stopped(); len(s) != 0 {
		t.Fatalf("stopped %v", s)
	}
	e.wantState(t, res.ID, model.StateTearingDown, model.ReasonNone)
}
