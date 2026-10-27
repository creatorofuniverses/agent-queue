package scheduler

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"resource-dispatcher/internal/admission"
	"resource-dispatcher/internal/config"
	"resource-dispatcher/internal/model"
	"resource-dispatcher/internal/platform"
	"resource-dispatcher/internal/store"
)

// floor16G is the reserve floor of a 16 GiB host (10 %).
const floor16G = 1717986918

// newResEnv is newEnv with pool 4 GiB / 16 CPUs and a 16 GiB host (floor 1.6 GiB).
func newResEnv(t *testing.T) *env {
	t.Helper()
	e := newEnv(t, func(c *config.Config) { c.PoolMemory = 4 * gib; c.PoolCPUs = 16 })
	e.host.Set(platform.MemInfo{Total: 16 * gib, Available: 16 * gib})
	return e
}

func (e *env) available(v uint64) { e.host.Set(platform.MemInfo{Total: 16 * gib, Available: v}) }

func (e *env) rreq(mem uint64) ReserveRequest {
	return ReserveRequest{Cwd: e.cwd, Memory: mem, TTL: 2 * time.Hour, Teardown: []byte("#!/bin/bash\ntrue\n")}
}

func (e *env) reserve(t *testing.T, r ReserveRequest) *model.Item {
	t.Helper()
	e.clk.Advance(time.Millisecond) // distinct created_at for FIFO
	it, err := e.s.Reserve(e.ctx, r)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if it.State != model.StateQueued || it.Kind != model.KindReservation {
		t.Fatalf("reserved %s %s", it.Kind, it.State)
	}
	return it
}

// release marks a reservation RELEASED directly in the store (Release is a later task).
func (e *env) release(t *testing.T, id model.ItemID) {
	t.Helper()
	it := e.get(t, id)
	it.State, it.EndedAt = model.StateReleased, e.clk.Now()
	if err := e.st.Update(e.ctx, it); err != nil {
		t.Fatal(err)
	}
}

func (e *env) wantHead(t *testing.T, id model.ItemID, b admission.Blocker) {
	t.Helper()
	hid, dec := e.s.HeadDecision()
	if hid != id || dec.Admit || dec.Blocker != b {
		t.Fatalf("head %s %+v, want %s blocked by %s", hid, dec, id, b)
	}
}

func (e *env) events(t *testing.T, id model.ItemID, kind model.EventKind) []model.Event {
	t.Helper()
	evs, err := e.st.Events(e.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	var out []model.Event
	for _, ev := range evs {
		if ev.Kind == kind {
			out = append(out, ev)
		}
	}
	return out
}

func jobDirs(t *testing.T, e *env) []string {
	t.Helper()
	ents, err := os.ReadDir(filepath.Join(e.paths.StateDir, "jobs"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	var out []string
	for _, d := range ents {
		out = append(out, d.Name())
	}
	return out
}

func TestReserveRejects(t *testing.T) {
	e := newResEnv(t)
	cases := map[string]func(r *ReserveRequest){
		"zero ttl":             func(r *ReserveRequest) { r.TTL = 0 },
		"ttl above max":        func(r *ReserveRequest) { r.TTL = 25 * time.Hour },
		"empty teardown":       func(r *ReserveRequest) { r.Teardown = nil },
		"teardown too large":   func(r *ReserveRequest) { r.Teardown = make([]byte, 64<<10+1) },
		"stand+budget > pool":  func(r *ReserveRequest) { r.Memory, r.TaskMemory, r.TaskCPUs = 3*gib, 2*gib, 1 },
		"budget without cpus":  func(r *ReserveRequest) { r.TaskMemory = gib },
		"bad docker project":   func(r *ReserveRequest) { r.DockerProject = "Bad Name" },
		"relative cwd":         func(r *ReserveRequest) { r.Cwd = "rel/dir" },
		"zero memory":          func(r *ReserveRequest) { r.Memory = 0 },
		"cpus without budget":  func(r *ReserveRequest) { r.TaskCPUs = 1 },
		"budget below 64 MiB":  func(r *ReserveRequest) { r.TaskMemory, r.TaskCPUs = 63*mib, 1 },
		"cpus above pool":      func(r *ReserveRequest) { r.CPUs, r.TaskMemory, r.TaskCPUs = 10, gib, 7 },
		"negative stand cpus":  func(r *ReserveRequest) { r.CPUs = -1 },
		"memory overflow pool": func(r *ReserveRequest) { r.Memory, r.TaskMemory, r.TaskCPUs = ^uint64(0), gib, 1 },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			r := e.rreq(gib)
			mut(&r)
			it, err := e.s.Reserve(e.ctx, r)
			if !errors.Is(err, ErrRejected) {
				t.Fatalf("err %v, want ErrRejected", err)
			}
			if it == nil || it.State != model.StateRejected || it.Diagnostic == "" {
				t.Fatalf("item %+v, want REJECTED with diagnostic", it)
			}
			e.wantState(t, it.ID, model.StateRejected, model.ReasonNone)
			if exists(t, e.paths.TeardownScript(it.ID)) {
				t.Fatal("teardown script written for a rejected reservation")
			}
		})
	}
	if d := jobDirs(t, e); len(d) != 0 {
		t.Fatalf("job dirs %v after rejections", d)
	}
}

func TestReserveCopiesTeardown(t *testing.T) {
	e := newResEnv(t)
	r := e.rreq(gib)
	r.Teardown = []byte("#!/bin/bash\necho bye\n")
	it := e.reserve(t, r)
	path := e.paths.TeardownScript(it.ID)
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "#!/bin/bash\necho bye\n" {
		t.Fatalf("script %q %v", got, err)
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("script mode %v %v", st.Mode(), err)
	}
	if st, err := os.Stat(filepath.Dir(path)); err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("job dir mode %v %v", st.Mode(), err)
	}
	if it.Limit != gib || it.Expected != gib || it.Timeout != 2*time.Hour || it.Cwd != e.cwd || it.Argv != nil {
		t.Fatalf("reservation fields %+v", it)
	}
}

func TestDockerProjectExclusive(t *testing.T) {
	e := newResEnv(t)
	ra := e.rreq(gib)
	ra.DockerProject, ra.IdemKey = "p", "ka"
	a := e.reserve(t, ra)

	rb := e.rreq(gib)
	rb.DockerProject = "p"
	b, err := e.s.Reserve(e.ctx, rb)
	if !errors.Is(err, ErrRejected) || !strings.Contains(err.Error(), "already used") ||
		!strings.Contains(err.Error(), string(a.ID)) {
		t.Fatalf("queued A: err %v", err)
	}
	e.wantState(t, b.ID, model.StateRejected, model.ReasonNone)
	if exists(t, e.paths.JobDir(b.ID)) {
		t.Fatal("rejected reservation kept its job dir")
	}

	e.tick(t)
	e.wantState(t, a.ID, model.StateGranted, model.ReasonNone)
	if _, err := e.s.Reserve(e.ctx, rb); !errors.Is(err, ErrRejected) || !strings.Contains(err.Error(), "already used") {
		t.Fatalf("granted A: err %v", err)
	}

	// A repeated request of A itself is not a conflict.
	again, err := e.s.Reserve(e.ctx, ra)
	if err != nil || again.ID != a.ID {
		t.Fatalf("repeat of A: %+v %v", again, err)
	}

	e.release(t, a.ID)
	if b2 := e.reserve(t, rb); b2.ID == a.ID {
		t.Fatal("B got A's id")
	}

	// Concurrent reserves of one project: exactly one is accepted.
	var wg sync.WaitGroup
	var mu sync.Mutex
	accepted, rejected := 0, 0
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := e.rreq(64 * mib)
			r.DockerProject = "q"
			_, err := e.s.Reserve(e.ctx, r)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				accepted++
			case errors.Is(err, ErrRejected):
				rejected++
			default:
				t.Errorf("reserve: %v", err)
			}
		}()
	}
	wg.Wait()
	if accepted != 1 || rejected != 9 {
		t.Fatalf("accepted %d rejected %d, want 1/9", accepted, rejected)
	}
}

func TestReserveIdempotentKey(t *testing.T) {
	e := newResEnv(t)
	r := e.rreq(gib)
	r.IdemKey = "k"
	a := e.reserve(t, r)
	b, err := e.s.Reserve(e.ctx, r)
	if err != nil || b.ID != a.ID {
		t.Fatalf("same key: %+v %v", b, err)
	}
	if d := jobDirs(t, e); len(d) != 1 || d[0] != string(a.ID) {
		t.Fatalf("job dirs %v, want only %s", d, a.ID)
	}
	r.Teardown = []byte("#!/bin/bash\necho other\n")
	if _, err := e.s.Reserve(e.ctx, r); !errors.Is(err, store.ErrIdempotencyConflict) {
		t.Fatalf("different script: %v", err)
	}
	if d := jobDirs(t, e); len(d) != 1 || d[0] != string(a.ID) {
		t.Fatalf("job dirs %v after conflict, want only %s", d, a.ID)
	}
}

func TestReservationCountsBothParts(t *testing.T) {
	e := newResEnv(t)
	r := e.rreq(gib)
	r.TaskMemory, r.TaskCPUs = gib, 1
	res := e.reserve(t, r)
	e.tick(t)
	e.wantState(t, res.ID, model.StateGranted, model.ReasonNone)

	big := e.submit(t, e.req(5*gib/2))
	e.tick(t)
	e.wantState(t, big.ID, model.StateQueued, model.ReasonNone)
	e.wantHead(t, big.ID, admission.BlockPoolMemory)

	if _, err := e.s.Cancel(e.ctx, big.ID, "test"); err != nil {
		t.Fatal(err)
	}
	fits := e.submit(t, e.req(2*gib))
	e.tick(t)
	e.wantState(t, fits.ID, model.StateRunning, model.ReasonNone)
}

func TestReservationCPUsCount(t *testing.T) {
	e := newResEnv(t)
	r := e.rreq(gib)
	r.CPUs, r.TaskMemory, r.TaskCPUs = 4, 256*mib, 8
	res := e.reserve(t, r)
	e.tick(t)
	e.wantState(t, res.ID, model.StateGranted, model.ReasonNone)

	q := e.req(256 * mib)
	q.CPUs = 5
	task := e.submit(t, q)
	e.tick(t)
	e.wantState(t, task.ID, model.StateQueued, model.ReasonNone)
	e.wantHead(t, task.ID, admission.BlockPoolCPU)
}

// standScenario grants A (2 GiB, docker project p unless empty) with 4.5 GiB available,
// lets a day pass and queues B (2 GiB).
func standScenario(t *testing.T, e *env, project string) (a, b *model.Item) {
	t.Helper()
	e.available(9 * gib / 2)
	ra := e.rreq(2 * gib)
	ra.TTL, ra.DockerProject = 24*time.Hour, project
	a = e.reserve(t, ra)
	e.tick(t)
	e.wantState(t, a.ID, model.StateGranted, model.ReasonNone)
	e.clk.Advance(24 * time.Hour)
	b = e.reserve(t, e.rreq(2*gib))
	e.tick(t)
	return a, b
}

func TestUnmeasuredStandNeverConsumed(t *testing.T) {
	e := newResEnv(t)
	_, b := standScenario(t, e, "")
	// 4.5 - 2 < 1.6 + 2
	e.wantState(t, b.ID, model.StateQueued, model.ReasonNone)
	e.wantHead(t, b.ID, admission.BlockLiveMemory)
}

func TestMeasuredStandCountsRemainder(t *testing.T) {
	e := newResEnv(t)
	e.meter.Set("p", 3*gib/2)
	a, b := standScenario(t, e, "p")
	// 4.5 - (2 - 1.5) >= 1.6 + 2
	e.wantState(t, b.ID, model.StateGranted, model.ReasonNone)
	if w := e.meter.Watched(); !slices.Contains(w, "p") {
		t.Fatalf("watched %v, want p", w)
	}
	if v, ok := e.s.StandUsage(a.ID); !ok || v != 3*gib/2 {
		t.Fatalf("stand usage %d %v", v, ok)
	}
	if _, ok := e.s.StandUsage(b.ID); ok {
		t.Fatal("stand usage for a reservation without a project")
	}
}

func TestMeterUnavailableIsConservative(t *testing.T) {
	t.Run("no measurement", func(t *testing.T) {
		e := newResEnv(t)
		e.meter.Set("p", 3*gib/2)
		e.meter.Unset("p")
		a, b := standScenario(t, e, "p")
		e.wantState(t, b.ID, model.StateQueued, model.ReasonNone)
		e.wantHead(t, b.ID, admission.BlockLiveMemory)
		if _, ok := e.s.StandUsage(a.ID); ok {
			t.Fatal("stand usage without a measurement")
		}
	})
	t.Run("nil meter", func(t *testing.T) {
		e := newResEnv(t)
		e.meter = nil
		e.rebuild(t, e.ex, true)
		_, b := standScenario(t, e, "p")
		e.wantState(t, b.ID, model.StateQueued, model.ReasonNone)
		e.wantHead(t, b.ID, admission.BlockLiveMemory)
	})
}

func TestTaskBudgetUnconsumedAlways(t *testing.T) {
	e := newResEnv(t)
	e.available(3 * gib)
	r := e.rreq(100 * mib)
	r.TaskMemory, r.TaskCPUs = gib, 1
	a := e.reserve(t, r)
	e.tick(t)
	e.wantState(t, a.ID, model.StateGranted, model.ReasonNone)

	// A nested task (submit --within is a later task): written to the store directly.
	nested := e.put(t, model.StateRunning, func(it *model.Item) {
		it.ParentID, it.Limit, it.Expected, it.StartedAt = a.ID, 500*mib, 500*mib, e.clk.Now()
	})
	e.register(t, nested.ID)
	e.ex.SetCurrent(nested.ID, 300*mib)

	// Unconsumed of A = 100M stand + (1G - 300M) budget = 824M; 3072 - 824 = 2248M.
	tooBig := e.reserve(t, e.rreq(700*mib)) // 2248 < 1638.4 + 700
	e.tick(t)
	e.wantState(t, tooBig.ID, model.StateQueued, model.ReasonNone)
	e.wantHead(t, tooBig.ID, admission.BlockLiveMemory)
	if _, err := e.s.Cancel(e.ctx, tooBig.ID, "test"); err != nil {
		t.Fatal(err)
	}

	fits := e.reserve(t, e.rreq(600*mib)) // 2248 >= 1638.4 + 600
	e.tick(t)
	e.wantState(t, fits.ID, model.StateGranted, model.ReasonNone)
	e.wantState(t, nested.ID, model.StateRunning, model.ReasonNone)
}

func TestStandOverDeclaredEventOnce(t *testing.T) {
	e := newResEnv(t)
	e.meter.Set("p", 5*gib/2)
	r := e.rreq(2 * gib)
	r.DockerProject, r.SessionID, r.ClientType = "p", "s1", "claude"
	a := e.reserve(t, r)
	e.tick(t)
	e.tick(t)
	evs := e.events(t, a.ID, model.EventStandOverDeclared)
	if len(evs) != 2 {
		t.Fatalf("events %+v, want 2", evs)
	}
	auds := []model.Audience{evs[0].Audience, evs[1].Audience}
	if !slices.Contains(auds, model.AudienceSession) || !slices.Contains(auds, model.AudienceOperator) {
		t.Fatalf("audiences %v", auds)
	}
	for _, ev := range evs {
		if ev.SessionID != "s1" || ev.ClientType != "claude" || ev.Detail == "" || !ev.CreatedAt.Equal(e.clk.Now()) {
			t.Fatalf("event %+v", ev)
		}
	}
	if !e.get(t, a.ID).OverDeclaredNotified {
		t.Fatal("OverDeclaredNotified not stored")
	}
	e.tick(t)
	if n := len(e.events(t, a.ID, model.EventStandOverDeclared)); n != 2 {
		t.Fatalf("events after a third tick: %d", n)
	}
}

func TestStandOverDeclaredNoSessionOperatorOnly(t *testing.T) {
	e := newResEnv(t)
	e.meter.Set("p", 5*gib/2)
	r := e.rreq(2 * gib)
	r.DockerProject = "p"
	a := e.reserve(t, r)
	e.tick(t)
	e.tick(t)
	evs := e.events(t, a.ID, model.EventStandOverDeclared)
	if len(evs) != 1 || evs[0].Audience != model.AudienceOperator {
		t.Fatalf("events %+v, want one operator event", evs)
	}
}

func TestGrantNotifiesOnlyIfWaited(t *testing.T) {
	e := newResEnv(t)
	ra := e.rreq(gib)
	ra.SessionID = "s1"
	a := e.reserve(t, ra)
	e.tick(t)
	e.wantState(t, a.ID, model.StateGranted, model.ReasonNone)
	if evs, _ := e.st.Events(e.ctx, a.ID); len(evs) != 0 {
		t.Fatalf("events for an immediate grant: %+v", evs)
	}
	if e.get(t, a.ID).NotifyGrant {
		t.Fatal("NotifyGrant on an immediate grant")
	}

	rb := e.rreq(7 * gib / 2)
	rb.SessionID = "s1"
	b := e.reserve(t, rb)
	e.tick(t)
	e.wantState(t, b.ID, model.StateQueued, model.ReasonNone)
	if !e.get(t, b.ID).NotifyGrant {
		t.Fatal("NotifyGrant not set after waiting a pass")
	}
	e.release(t, a.ID)
	e.tick(t)
	e.wantState(t, b.ID, model.StateGranted, model.ReasonNone)
	evs, err := e.st.Events(e.ctx, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Kind != model.EventReservationGranted || evs[0].Audience != model.AudienceSession ||
		evs[0].SessionID != "s1" {
		t.Fatalf("events %+v, want one session reservation_granted", evs)
	}
}

func TestGrantSetsExpiry(t *testing.T) {
	e := newResEnv(t)
	long := e.reserve(t, e.rreq(gib))
	r := e.rreq(gib)
	r.TTL = 10 * time.Minute
	short := e.reserve(t, r)
	e.tick(t)
	t0 := e.clk.Now()
	it := e.wantState(t, long.ID, model.StateGranted, model.ReasonNone)
	if !it.ExpiresAt.Equal(t0.Add(2*time.Hour)) || !it.WarnAt.Equal(t0.Add(time.Hour+45*time.Minute)) ||
		!it.AdmittedAt.Equal(t0) || !it.StartedAt.Equal(t0) {
		t.Fatalf("expires %v warn %v admitted %v started %v (t0 %v)", it.ExpiresAt, it.WarnAt, it.AdmittedAt, it.StartedAt, t0)
	}
	it = e.wantState(t, short.ID, model.StateGranted, model.ReasonNone)
	if !it.ExpiresAt.Equal(t0.Add(10*time.Minute)) || !it.WarnAt.IsZero() {
		t.Fatalf("short: expires %v warn %v", it.ExpiresAt, it.WarnAt)
	}
	if n := len(e.ex.Launched()); n != 0 {
		t.Fatalf("a reservation grant launched %d units", n)
	}
}

func TestWaitReturnsOnGrant(t *testing.T) {
	e := newResEnv(t)
	blocker := e.reserve(t, e.rreq(4*gib))
	e.tick(t)
	e.wantState(t, blocker.ID, model.StateGranted, model.ReasonNone)
	it := e.reserve(t, e.rreq(gib))
	e.tick(t)

	type res struct {
		it  *model.Item
		err error
	}
	ch := make(chan res, 1)
	go func() {
		got, err := e.s.Wait(e.ctx, it.ID)
		ch <- res{got, err}
	}()
	waitRegistered(t, e.s, it.ID)
	e.release(t, blocker.ID)
	e.tick(t)
	select {
	case r := <-ch:
		if r.err != nil || r.it.State != model.StateGranted {
			t.Fatalf("wait: %+v %v", r.it, r.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Wait did not return after the grant")
	}
	// Already granted: returns at once.
	if got, err := e.s.Wait(e.ctx, it.ID); err != nil || got.State != model.StateGranted {
		t.Fatalf("wait granted: %+v %v", got, err)
	}
}

func TestAfterNextPass(t *testing.T) {
	e := newResEnv(t)
	it := e.reserve(t, e.rreq(gib))
	<-e.s.kick // drop Reserve's kick: there is no Run loop here

	done := make(chan error, 1)
	go func() { done <- e.s.AfterNextPass(e.ctx) }()
	deadline := time.Now().Add(10 * time.Second)
	for len(e.s.kick) == 0 { // AfterNextPass kicks after it has subscribed
		if time.Now().After(deadline) {
			t.Fatal("AfterNextPass never kicked")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case err := <-done:
		t.Fatalf("AfterNextPass returned %v before a pass", err)
	case <-time.After(50 * time.Millisecond):
	}
	e.tick(t)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("AfterNextPass did not return after a pass")
	}
	e.wantState(t, it.ID, model.StateGranted, model.ReasonNone)

	ctx, cancel := context.WithCancel(e.ctx)
	cancel()
	if err := e.s.AfterNextPass(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled ctx: %v", err)
	}
}
