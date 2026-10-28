package scheduler

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"resource-dispatcher/internal/model"
	"resource-dispatcher/internal/platform"
	"resource-dispatcher/internal/store"
	"strings"
	"testing"
	"time"
)

func runProfiled(t *testing.T, e *env, name string, peak, cur uint64, dur time.Duration, st model.State, reason model.Reason) *model.Item {
	t.Helper()
	r := e.req(512 * mib)
	r.Profile = name
	it := e.submit(t, r)
	e.tick(t)
	e.ex.SetCurrent(it.ID, cur)
	for d := time.Duration(0); d < dur; d += time.Second {
		e.clk.Advance(time.Second)
		e.tick(t)
	}
	ei := platform.ExitInfo{ServiceResult: "success", ExitCode: "exited", ExitStatus: "0", MemoryPeak: peak, EndedAt: e.clk.Now()}
	if st == model.StateCancelled {
		it, _ = e.s.Cancel(e.ctx, it.ID, "test")
		e.settle()
	} else if reason == model.ReasonOOM {
		ei.OwnOOMEvents = 1
		ei.OwnOOMKills = 1
	} else if reason == model.ReasonCommandFailed {
		ei.ExitStatus = "1"
	}
	e.ex.Finish(it.ID, ei)
	e.ex.SetAlive(it.ID, false)
	e.tick(t)
	return e.get(t, it.ID)
}
func train(t *testing.T, e *env) {
	for i, p := range []uint64{150, 200, 180} {
		runProfiled(t, e, "p", p*mib, 100*mib, time.Duration(60+30*i)*time.Second, model.StateSucceeded, "")
	}
}
func TestSubmitMemoryFromProfile(t *testing.T) {
	e := newEnv(t, nil)
	train(t, e)
	r := e.req(0)
	r.Profile = "p"
	it := e.submit(t, r)
	if it.Limit != 240*mib || it.Expected != 109051903 || !it.MemoryFromProfile || it.EstDuration != 120*time.Second {
		t.Fatal(it)
	}
	r.Expected = 50 * mib
	it = e.submit(t, r)
	if it.Expected != 50*mib {
		t.Fatal(it)
	}
}
func TestSubmitProfileNoHistoryRejected(t *testing.T) {
	e := newEnv(t, nil)
	r := e.req(0)
	r.Profile = "new"
	it, err := e.s.Submit(e.ctx, r)
	if !errors.Is(err, ErrRejected) || it.Diagnostic != "memory not given and profile new has no usable runs; pass --memory" {
		t.Fatal(it, err)
	}
	r.Profile = ""
	it, err = e.s.Submit(e.ctx, r)
	if !errors.Is(err, ErrRejected) || it.Diagnostic != "memory limit is required" {
		t.Fatal(it, err)
	}
}
func TestProfileIgnoresUnusableRuns(t *testing.T) {
	e := newEnv(t, nil)
	for i := 0; i < 3; i++ {
		runProfiled(t, e, "p", 512*mib, 200*mib, time.Second, model.StateFailed, model.ReasonOOM)
	}
	runProfiled(t, e, "p", 400*mib, 100*mib, time.Second, model.StateCancelled, model.ReasonCancelled)
	r := e.req(0)
	r.Profile = "p"
	if _, err := e.s.Submit(e.ctx, r); !errors.Is(err, ErrRejected) {
		t.Fatal(err)
	}
	runProfiled(t, e, "p", 100*mib, 100*mib, time.Second, model.StateSucceeded, "")
	if it := e.submit(t, r); it.Limit != 120*mib {
		t.Fatal(it)
	}
}
func TestExplicitMemoryGetsDurationKeepsHash(t *testing.T) {
	e := newEnv(t, nil)
	train(t, e)
	r := e.req(gib)
	r.Profile = "p"
	r.Timeout = time.Minute
	r.IdemKey = "k"
	it := e.submit(t, r)
	if it.Expected != gib || it.MemoryFromProfile || it.EstDuration != time.Minute {
		t.Fatal(it)
	}
	runProfiled(t, e, "p", 300*mib, 100*mib, time.Second, model.StateSucceeded, "")
	again, err := e.s.Submit(e.ctx, r)
	if err != nil || again.ID != it.ID {
		t.Fatal(again, err)
	}
}
func TestDerivedProfileKeepsPrePlan4Hash(t *testing.T) {
	e := newEnv(t, nil)
	r := e.req(100 * mib)
	r.Name = "build"
	r.IdemKey = "k"
	r.Expected = r.Limit
	b, _ := json.Marshal(r)
	var old map[string]any
	json.Unmarshal(b, &old)
	delete(old, "profile") // marshaling the original struct preserves its field order
	sum := sha256.Sum256(b)
	it := &model.Item{ID: model.NewID(), Kind: model.KindTask, State: model.StateQueued, IdemKey: r.IdemKey, RequestHash: hex.EncodeToString(sum[:]), ClientType: r.ClientType, Project: r.Project, CreatedAt: e.clk.Now()}
	if _, _, err := e.st.InsertItem(e.ctx, it); err != nil {
		t.Fatal(err)
	}
	r.Profile = e.cwd + ":build"
	r.ProfileDerived = true
	got, err := e.s.Submit(e.ctx, r)
	if err != nil || got.ID != it.ID {
		t.Fatal(got, err)
	}
	r.ProfileDerived = false
	r.Profile = "x"
	if _, err = e.s.Submit(e.ctx, r); err == nil {
		t.Fatal("missing conflict")
	}
}
func TestSamplingOncePerSecond(t *testing.T) {
	e := newEnv(t, nil)
	r := e.req(100 * mib)
	r.Profile = "p"
	it := e.submit(t, r)
	e.tick(t)
	e.ex.SetCurrent(it.ID, 50*mib)
	for i := 0; i < 10; i++ {
		e.clk.Advance(200 * time.Millisecond)
		e.tick(t)
	}
	e.ex.Finish(it.ID, platform.ExitInfo{ServiceResult: "success", MemoryPeak: 60 * mib, EndedAt: e.clk.Now()})
	e.ex.SetAlive(it.ID, false)
	e.tick(t)
	rs, _ := e.st.ProfileRuns(e.ctx, "p", 10)
	if len(rs) != 1 || rs[0].Samples < 1 || rs[0].Samples > 3 {
		t.Fatal(rs)
	}
	it = e.submit(t, e.req(100*mib))
	e.tick(t)
	e.ex.SetCurrent(it.ID, 50*mib)
	e.clk.Advance(time.Second)
	e.tick(t)
	if len(e.s.samples) != 0 {
		t.Fatal(e.s.samples)
	}
}
func TestProfileRunRecordedOnce(t *testing.T) {
	e := newEnv(t, nil)
	it := runProfiled(t, e, "p", 100*mib, 50*mib, time.Second, model.StateSucceeded, "")
	e.s.mu.Lock()
	err := e.s.persistLocked(e.ctx, it)
	e.s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	r := e.req(100 * mib)
	r.Profile = "p"
	q := e.submit(t, r)
	e.s.Cancel(e.ctx, q.ID, "")
	r.Profile = "bad\n"
	e.s.Submit(e.ctx, r)
	rs, _ := e.st.ProfileRuns(e.ctx, "p", 20)
	if len(rs) != 1 {
		t.Fatal(rs)
	}
}
func TestProfileRunAfterRestart(t *testing.T) {
	for _, n := range []int{0, 3} {
		t.Run(string(rune('0'+n)), func(t *testing.T) {
			e := newEnv(t, nil)
			r := e.req(100 * mib)
			r.Profile = "p"
			it := e.submit(t, r)
			e.tick(t)
			e.ex.SetCurrent(it.ID, 50*mib)
			e.clk.Advance(time.Second)
			e.tick(t)
			e.rebuild(t, e.ex, true)
			for i := 0; i < n; i++ {
				e.clk.Advance(time.Second)
				e.tick(t)
			}
			e.ex.Finish(it.ID, platform.ExitInfo{ServiceResult: "success", MemoryPeak: 60 * mib, EndedAt: e.clk.Now()})
			e.ex.SetAlive(it.ID, false)
			e.tick(t)
			rs, _ := e.st.ProfileRuns(e.ctx, "p", 10)
			if len(rs) != 1 || rs[0].Samples != n || n == 0 && rs[0].Hist.Count() != 1 {
				t.Fatal(rs)
			}
		})
	}
}
func TestProfileRunCrashDuringLaunch(t *testing.T) {
	for _, alive := range []bool{false, true} {
		t.Run(strings.TrimSpace(map[bool]string{true: "adopt", false: "finish"}[alive]), func(t *testing.T) {
			e := newEnv(t, nil)
			r := e.req(100 * mib)
			r.Profile = "p"
			it := e.submit(t, r)
			e.tick(t)
			it = e.get(t, it.ID)
			t0 := it.AdmittedAt
			it.State = model.StateStarting
			it.StartedAt = time.Time{}
			e.st.Update(e.ctx, it)
			e.clk.Advance(40 * time.Second)
			if !alive {
				e.ex.Finish(it.ID, platform.ExitInfo{ServiceResult: "success", MemoryPeak: 80 * mib, EndedAt: t0.Add(40 * time.Second)})
				e.ex.SetAlive(it.ID, false)
			}
			e.rebuild(t, e.ex, true)
			if alive {
				it = e.get(t, it.ID)
				if !it.StartedAt.Equal(t0) {
					t.Fatal(it)
				}
				e.ex.Finish(it.ID, platform.ExitInfo{ServiceResult: "success", MemoryPeak: 80 * mib, EndedAt: t0.Add(time.Minute)})
				e.ex.SetAlive(it.ID, false)
				e.tick(t)
			}
			rs, _ := e.st.ProfileRuns(e.ctx, "p", 10)
			want := 40 * time.Second
			if alive {
				want = time.Minute
			}
			if len(rs) != 1 || rs[0].Duration != want || !rs[0].Usable() {
				t.Fatal(rs)
			}
		})
	}
}
func TestNestedTaskProfileLimitChecksBudget(t *testing.T) {
	e := newResEnv(t)
	runProfiled(t, e, "p", 200*mib, 100*mib, time.Second, model.StateSucceeded, "")
	rr := e.rreq(100 * mib)
	rr.TaskMemory = 256 * mib
	rr.TaskCPUs = 2
	res := e.reserve(t, rr)
	e.tick(t)
	r := e.req(0)
	r.Profile = "p"
	r.ParentID = res.ID
	e.submit(t, r)
	runProfiled(t, e, "large", 250*mib, 100*mib, time.Second, model.StateSucceeded, "")
	r.Profile = "large"
	it, err := e.s.Submit(e.ctx, r)
	if !errors.Is(err, ErrRejected) || !strings.Contains(it.Diagnostic, "task budget") {
		t.Fatal(it, err)
	}
}
func TestExtendCountsExtensions(t *testing.T) {
	e := newResEnv(t)
	it := e.reserve(t, e.rreq(100*mib))
	e.tick(t)
	for i := 1; i <= 2; i++ {
		got, err := e.s.Extend(e.ctx, it.ID, time.Hour)
		if err != nil || got.ExtendCount != i {
			t.Fatal(got, err)
		}
	}
	e.s.Extend(e.ctx, it.ID, 0)
	if e.get(t, it.ID).ExtendCount != 2 {
		t.Fatal("count")
	}
}
func TestProfileRunEndingBeforeStartIsUsable(t *testing.T) {
	e := newEnv(t, nil)
	r := e.req(512 * mib)
	r.Profile = "quick"
	it := e.submit(t, r)
	e.tick(t)
	// The runner stamps EndedAt before the daemon gets to stamp StartedAt.
	e.ex.Finish(it.ID, platform.ExitInfo{ServiceResult: "success", ExitCode: "exited", ExitStatus: "0", MemoryPeak: 100 * mib, EndedAt: e.clk.Now().Add(-50 * time.Millisecond)})
	e.ex.SetAlive(it.ID, false)
	e.tick(t)
	e.wantState(t, it.ID, model.StateSucceeded, "")
	if _, ok, err := e.s.ProfileEstimate(e.ctx, "quick"); err != nil || !ok {
		t.Fatal(ok, err)
	}
}
func TestProfileStoreErrorIsNotRejection(t *testing.T) {
	e := newEnv(t, nil)
	sqlExec(t, e.sqlDB(t), `DROP TABLE profile_runs`)
	r := e.req(0)
	r.Profile = "p"
	r.IdemKey = "k"
	if _, err := e.s.Submit(e.ctx, r); err == nil || errors.Is(err, ErrRejected) {
		t.Fatal(err)
	}
	its, _ := e.st.List(e.ctx, store.ListFilter{IncludeTerminal: true})
	if len(its) != 0 {
		t.Fatal("transient store error stored a row", its)
	}
}
