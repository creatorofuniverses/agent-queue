package scheduler

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"resource-dispatcher/internal/admission"
	"resource-dispatcher/internal/config"
	"resource-dispatcher/internal/model"
	"resource-dispatcher/internal/platform"
	"resource-dispatcher/internal/platform/fake"
)

// grantBudget reserves stand + a task budget and grants it with one Tick.
func (e *env) grantBudget(t *testing.T, stand, taskMem uint64, taskCPUs int) *model.Item {
	t.Helper()
	r := e.rreq(stand)
	r.TaskMemory, r.TaskCPUs = taskMem, taskCPUs
	res := e.reserve(t, r)
	e.tick(t)
	e.wantState(t, res.ID, model.StateGranted, model.ReasonNone)
	return res
}

func (e *env) within(parent model.ItemID, mem uint64) SubmitRequest {
	r := e.req(mem)
	r.ParentID = parent
	return r
}

func (e *env) wantRejected(t *testing.T, r SubmitRequest, text string) *model.Item {
	t.Helper()
	it, err := e.s.Submit(e.ctx, r)
	if !errors.Is(err, ErrRejected) || !strings.Contains(err.Error(), text) {
		t.Fatalf("submit: err %v, want ErrRejected containing %q", err, text)
	}
	if it == nil || it.State != model.StateRejected || !strings.Contains(it.Diagnostic, text) {
		t.Fatalf("item %+v, want REJECTED with %q", it, text)
	}
	e.wantState(t, it.ID, model.StateRejected, model.ReasonNone)
	return it
}

func TestWithinBypassesQueue(t *testing.T) {
	e := newEnv(t, nil)
	res := e.grantBudget(t, 100*mib, 500*mib, 2)
	big := e.submit(t, e.req(900*mib))
	e.tick(t)
	e.wantHead(t, big.ID, admission.BlockPoolMemory)

	nested := e.submit(t, e.within(res.ID, 400*mib))
	if nested.ParentID != res.ID {
		t.Fatalf("parent %q, want %s", nested.ParentID, res.ID)
	}
	e.tick(t)
	if !contains(launchedIDs(e.ex), nested.ID) {
		t.Fatalf("launched %v, want the nested task", launchedIDs(e.ex))
	}
	e.wantState(t, nested.ID, model.StateRunning, model.ReasonNone)
	e.wantState(t, big.ID, model.StateQueued, model.ReasonNone)
	e.wantHead(t, big.ID, admission.BlockPoolMemory)
}

func TestWithinBypassesPressure(t *testing.T) {
	e := newEnv(t, nil)
	res := e.grantBudget(t, 100*mib, 500*mib, 2)
	e.host.SetPressure(6)
	e.tick(t)
	e.clk.Advance(10 * time.Second)
	e.tick(t)

	plain := e.submit(t, e.req(100*mib))
	nested := e.submit(t, e.within(res.ID, 100*mib))
	e.tick(t)
	e.wantHead(t, plain.ID, admission.BlockPressure)
	e.wantState(t, plain.ID, model.StateQueued, model.ReasonNone)
	e.wantState(t, nested.ID, model.StateRunning, model.ReasonNone)
}

func TestWithinBypassesDiskAndLiveMemory(t *testing.T) {
	e := newEnv(t, nil)
	res := e.grantBudget(t, 100*mib, 500*mib, 2)
	e.host.SetDisk(0)
	e.available(0)
	nested := e.submit(t, e.within(res.ID, 100*mib))
	e.tick(t)
	e.wantState(t, nested.ID, model.StateRunning, model.ReasonNone)
}

// memErrHost fails every memory read.
type memErrHost struct{ *fake.Host }

func (memErrHost) Memory() (platform.MemInfo, error) {
	return platform.MemInfo{}, errors.New("meminfo unreadable")
}

func TestWithinStartsDespiteHostReadError(t *testing.T) {
	e := newEnv(t, nil)
	res := e.grantBudget(t, 100*mib, 500*mib, 1)
	e.s.bg.Wait()
	e.s = New(Deps{Store: e.st, Exec: e.ex, Host: memErrHost{e.host}, Meter: e.meter, Cfg: e.cfg, Paths: e.paths, Now: e.clk.Now})
	if err := e.s.Recover(e.ctx); err != nil { // the granted parent survives it
		t.Fatal(err)
	}
	plain := e.submit(t, e.req(100*mib))
	nested := e.submit(t, e.within(res.ID, 100*mib))
	if err := e.s.Tick(e.ctx); err == nil || !strings.Contains(err.Error(), "meminfo unreadable") {
		t.Fatalf("tick: %v, want the memory read error", err)
	}
	e.wantState(t, nested.ID, model.StateRunning, model.ReasonNone)
	e.wantState(t, plain.ID, model.StateQueued, model.ReasonNone)
}

func TestWithinOverBudgetMemory(t *testing.T) {
	e := newEnv(t, nil)
	res := e.grantBudget(t, 100*mib, 500*mib, 2)
	running := e.submit(t, e.within(res.ID, 400*mib))
	e.tick(t)
	e.wantState(t, running.ID, model.StateRunning, model.ReasonNone)
	before := len(e.ex.Launched())

	e.wantRejected(t, e.within(res.ID, 200*mib), "task budget")
	e.tick(t)
	if n := len(e.ex.Launched()); n != before {
		t.Fatalf("launched %d, want %d", n, before)
	}
}

func TestWithinOverBudgetCPU(t *testing.T) {
	e := newEnv(t, nil)
	res := e.grantBudget(t, 100*mib, 500*mib, 2)
	r := e.within(res.ID, 100*mib)
	r.CPUs = 2
	running := e.submit(t, r)
	e.tick(t)
	e.wantState(t, running.ID, model.StateRunning, model.ReasonNone)

	it := e.wantRejected(t, e.within(res.ID, 100*mib), "task budget")
	if !strings.Contains(it.Diagnostic, "cpus") {
		t.Fatalf("diagnostic %q, want cpus", it.Diagnostic)
	}
}

// Final review F1: cpus near MaxInt must not wrap the budget check.
func TestWithinCPUOverflow(t *testing.T) {
	e := newEnv(t, nil)
	res := e.grantBudget(t, 100*mib, 500*mib, 2)
	running := e.submit(t, e.within(res.ID, 100*mib))
	e.tick(t)
	e.wantState(t, running.ID, model.StateRunning, model.ReasonNone)

	r := e.within(res.ID, 100*mib)
	r.CPUs = math.MaxInt
	e.wantRejected(t, r, "task budget")
}

func TestWithinLimitAboveBudget(t *testing.T) {
	e := newEnv(t, nil)
	res := e.grantBudget(t, 100*mib, 500*mib, 2)
	r := e.within(res.ID, 600*mib)
	r.Expected = 100 * mib
	e.wantRejected(t, r, "task budget")
}

func TestWithinRejectsOrdinaryValidation(t *testing.T) {
	e := newEnv(t, nil)
	res := e.grantBudget(t, 100*mib, 500*mib, 2)
	cases := map[string]func(r *SubmitRequest){
		"empty argv":       func(r *SubmitRequest) { r.Argv = nil },
		"zero timeout":     func(r *SubmitRequest) { r.Timeout = 0 },
		"expected > limit": func(r *SubmitRequest) { r.Expected = r.Limit + 1 },
		"below 64 MiB":     func(r *SubmitRequest) { r.Limit = 63 * mib },
		"zero cpus":        func(r *SubmitRequest) { r.CPUs = 0 },
		"missing cwd":      func(r *SubmitRequest) { r.Cwd = "/nonexistent/aq-within" },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			r := e.within(res.ID, 100*mib)
			mut(&r)
			if _, err := e.s.Submit(e.ctx, r); !errors.Is(err, ErrRejected) {
				t.Fatalf("err %v, want ErrRejected", err)
			}
		})
	}
}

func TestWithinParentChecks(t *testing.T) {
	e := newEnv(t, nil)
	granted := e.grantBudget(t, 100*mib, 200*mib, 1)

	queued := e.reserve(t, func() ReserveRequest {
		r := e.rreq(500 * mib) // 300M + 500M + 500M > 1G pool
		r.TaskMemory, r.TaskCPUs = 500*mib, 1
		return r
	}())
	e.tick(t)
	e.wantState(t, queued.ID, model.StateQueued, model.ReasonNone)
	e.wantRejected(t, e.within(queued.ID, 100*mib), "reservation "+string(queued.ID)+" is QUEUED, not granted")

	e.wantRejected(t, e.within("nope", 100*mib), "reservation nope not found")

	task := e.submit(t, e.req(64*mib))
	e.wantRejected(t, e.within(task.ID, 100*mib), string(task.ID)+" is not a reservation")

	held := e.get(t, granted.ID)
	held.TTLHeld = true
	if err := e.st.Update(e.ctx, held); err != nil {
		t.Fatal(err)
	}
	e.wantRejected(t, e.within(granted.ID, 100*mib), "reservation "+string(granted.ID)+" is held past its TTL; extend it first")

	e.release(t, granted.ID)
	e.wantRejected(t, e.within(granted.ID, 100*mib), "reservation "+string(granted.ID)+" is RELEASED, not granted")
}

func TestWithinExpiringParentAccepted(t *testing.T) {
	e := newEnv(t, nil)
	res := e.grantBudget(t, 100*mib, 200*mib, 1)
	it := e.get(t, res.ID)
	it.State = model.StateExpiring
	if err := e.st.Update(e.ctx, it); err != nil {
		t.Fatal(err)
	}
	nested := e.submit(t, e.within(res.ID, 100*mib))
	e.tick(t)
	e.wantState(t, nested.ID, model.StateRunning, model.ReasonNone)
}

func TestWithinConcurrentBudget(t *testing.T) {
	e := newEnv(t, nil)
	res := e.grantBudget(t, 100*mib, 500*mib, 16)
	var wg sync.WaitGroup
	var mu sync.Mutex
	accepted, rejected := 0, 0
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := e.s.Submit(e.ctx, e.within(res.ID, 100*mib))
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				accepted++
			case errors.Is(err, ErrRejected):
				rejected++
			default:
				t.Errorf("submit: %v", err)
			}
		}()
	}
	wg.Wait()
	if accepted != 5 || rejected != 5 {
		t.Fatalf("accepted %d rejected %d, want 5/5", accepted, rejected)
	}
	active, err := e.st.ListActive(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	var sum uint64
	for _, it := range active {
		if it.ParentID == res.ID {
			sum += it.Expected
		}
	}
	if sum != 500*mib {
		t.Fatalf("expected sum %d, want 500M", sum)
	}
}

func TestTwoAgentsNoDeadlock(t *testing.T) {
	e := newEnv(t, nil)
	a := e.grantBudget(t, 200*mib, 300*mib, 1)
	b := e.grantBudget(t, 200*mib, 300*mib, 1)
	na := e.submit(t, e.within(a.ID, 300*mib))
	nb := e.submit(t, e.within(b.ID, 300*mib))
	e.tick(t)
	got := launchedIDs(e.ex)
	if !contains(got, na.ID) || !contains(got, nb.ID) {
		t.Fatalf("launched %v, want both nested tasks", got)
	}
}

func TestWithinFinishedFreesBudget(t *testing.T) {
	e := newEnv(t, nil)
	res := e.grantBudget(t, 100*mib, 500*mib, 1)
	first := e.submit(t, e.within(res.ID, 500*mib))
	e.tick(t)
	e.wantState(t, first.ID, model.StateRunning, model.ReasonNone)
	e.wantRejected(t, e.within(res.ID, 500*mib), "task budget")

	e.finish(first.ID, success)
	e.tick(t)
	e.wantState(t, first.ID, model.StateSucceeded, model.ReasonNone)
	second := e.submit(t, e.within(res.ID, 500*mib))
	e.tick(t)
	e.wantState(t, second.ID, model.StateRunning, model.ReasonNone)
}

func TestWithinNotAdmittedWhileRecovering(t *testing.T) {
	e := newEnv(t, nil)
	res := e.grantBudget(t, 100*mib, 500*mib, 1)
	e.rebuild(t, e.ex, false)
	nested := e.submit(t, e.within(res.ID, 100*mib))
	e.tick(t)
	e.wantState(t, nested.ID, model.StateQueued, model.ReasonNone)
	if contains(launchedIDs(e.ex), nested.ID) {
		t.Fatal("nested task launched while recovering")
	}
	// Recover keeps the granted parent and the queued nested task; then it starts.
	if err := e.s.Recover(e.ctx); err != nil {
		t.Fatal(err)
	}
	e.wantState(t, res.ID, model.StateGranted, model.ReasonNone)
	e.wantState(t, nested.ID, model.StateQueued, model.ReasonNone)
	e.tick(t)
	e.wantState(t, nested.ID, model.StateRunning, model.ReasonNone)
}

func TestWithinParentGoneCancelsNested(t *testing.T) {
	e := newEnv(t, nil)
	res := e.grantBudget(t, 100*mib, 500*mib, 1)
	nested := e.submit(t, e.within(res.ID, 100*mib))
	e.release(t, res.ID) // not reachable through Release with a nested task; must not launch
	e.tick(t)
	it := e.wantState(t, nested.ID, model.StateCancelled, model.ReasonCancelled)
	if want := "reservation " + string(res.ID) + " is RELEASED"; it.Diagnostic != want {
		t.Fatalf("diagnostic %q, want %q", it.Diagnostic, want)
	}
	if contains(launchedIDs(e.ex), nested.ID) {
		t.Fatal("nested task of a released reservation launched")
	}
}

func TestWithinIdempotentRepeat(t *testing.T) {
	e := newEnv(t, nil)
	res := e.grantBudget(t, 100*mib, 500*mib, 1)
	r := e.within(res.ID, 500*mib)
	r.IdemKey = "k"
	first := e.submit(t, r)
	again, err := e.s.Submit(e.ctx, r) // its own budget use is not counted twice
	if err != nil || again.ID != first.ID {
		t.Fatalf("repeat: %+v %v", again, err)
	}
}

// plan1SubmitRequest is SubmitRequest as plan 1 shipped it, without ParentID.
type plan1SubmitRequest struct {
	Name       string            `json:"name"`
	Argv       []string          `json:"argv"`
	Cwd        string            `json:"cwd"`
	Env        map[string]string `json:"env"`
	Limit      uint64            `json:"limit"`
	Expected   uint64            `json:"expected"`
	CPUs       int               `json:"cpus"`
	Timeout    time.Duration     `json:"timeout"`
	Priority   model.Priority    `json:"priority"`
	SessionID  string            `json:"session_id"`
	ClientType string            `json:"client_type"`
	Project    string            `json:"project"`
	IdemKey    string            `json:"-"`
	GitRev     string            `json:"git_rev"`
	GitDirty   bool              `json:"git_dirty"`
}

func TestRequestHashCompatibleWithPlan1(t *testing.T) {
	e := newEnv(t, nil)
	r := e.req(256 * mib)
	r.Expected, r.IdemKey, r.ClientType, r.Project = 128*mib, "k", "claude", "/p"
	r.Env = map[string]string{"PATH": "/bin"}
	old := plan1SubmitRequest{
		Name: r.Name, Argv: r.Argv, Cwd: r.Cwd, Env: r.Env, Limit: r.Limit, Expected: r.Expected,
		CPUs: r.CPUs, Timeout: r.Timeout, Priority: r.Priority, SessionID: r.SessionID,
		ClientType: r.ClientType, Project: r.Project, IdemKey: r.IdemKey, GitRev: r.GitRev, GitDirty: r.GitDirty,
	}
	canon, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(canon)
	stored := e.put(t, model.StateQueued, func(it *model.Item) {
		it.IdemKey, it.ClientType, it.Project = "k", "claude", "/p"
		it.RequestHash = hex.EncodeToString(sum[:])
	})
	got, err := e.s.Submit(e.ctx, r)
	if err != nil || got.ID != stored.ID {
		t.Fatalf("submit: %+v %v, want the plan 1 item %s", got, err, stored.ID)
	}
	if active, _ := e.st.ListActive(e.ctx); len(active) != 1 || !slices.ContainsFunc(active, func(it *model.Item) bool { return it.ID == stored.ID }) {
		t.Fatalf("active %v", active)
	}
}

// Ruling T5a: a reservation past its TTL (here waiting for a teardown slot) takes no new
// nested tasks, or a busy teardown queue would let them hold it forever.
func TestWithinRefusedWhenDue(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.Reservations.TeardownConcurrency = 1 })
	busy := e.grantBudget(t, 100*mib, 0, 0)      // expires first and takes the only slot
	res := e.grantBudget(t, 100*mib, 200*mib, 1) // TTL 2h
	e.tickAt(t, e.get(t, res.ID).ExpiresAt)
	e.wantState(t, busy.ID, model.StateTearingDown, model.ReasonNone)
	if it := e.wantState(t, res.ID, model.StateExpiring, model.ReasonNone); it.TTLHeld {
		t.Fatal("due reservation without nested tasks is held")
	}
	e.wantRejected(t, e.within(res.ID, 100*mib), "reservation "+string(res.ID)+" is past its TTL; extend it first")

	if _, err := e.s.Extend(e.ctx, res.ID, time.Hour); err != nil {
		t.Fatal(err)
	}
	nested := e.submit(t, e.within(res.ID, 100*mib))
	e.tick(t)
	e.wantState(t, nested.ID, model.StateRunning, model.ReasonNone)
}
