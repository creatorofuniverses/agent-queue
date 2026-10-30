package scheduler

import (
	"testing"
	"time"

	"github.com/creatorofuniverses/agent-queue/internal/config"
	"github.com/creatorofuniverses/agent-queue/internal/model"
	"github.com/creatorofuniverses/agent-queue/internal/platform"
)

// runNested writes a RUNNING nested task of parent to the store and its unit
// to the fake executor using cur bytes.
func (e *env) runNested(t *testing.T, parent model.ItemID, cur uint64) *model.Item {
	t.Helper()
	it := e.put(t, model.StateRunning, func(it *model.Item) {
		it.ParentID, it.Limit, it.Expected, it.StartedAt = parent, 2*gib, 2*gib, e.clk.Now()
	})
	e.register(t, it.ID)
	e.ex.SetCurrent(it.ID, cur)
	return it
}

func TestReleaseRecordsPeaks(t *testing.T) {
	e := newResEnv(t)
	r := e.rreq(gib)
	r.DockerProject, r.TaskMemory, r.TaskCPUs = "p", 3*gib, 2
	a := e.reserve(t, r)
	e.meter.Set("p", 300*mib)
	e.tick(t)
	e.wantState(t, a.ID, model.StateGranted, model.ReasonNone)

	one, two := e.runNested(t, a.ID, 300*mib), e.runNested(t, a.ID, 500*mib)
	e.meter.Set("p", 600*mib)
	e.tick(t) // nested tasks use 800 MiB at once
	e.meter.Set("p", 400*mib)
	e.ex.SetCurrent(two.ID, 100*mib)
	e.tick(t)

	// The unit's own peak is exact where sampling misses a spike.
	e.finish(one.ID, platform.ExitInfo{ServiceResult: "success", ExitCode: "exited", ExitStatus: "0", MemoryPeak: gib})
	e.finish(two.ID, success)
	e.tick(t)

	got, err := e.s.Release(e.ctx, a.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range []*model.Item{got, e.get(t, a.ID)} {
		if it.MemoryPeak != 600*mib || it.TaskMemoryPeak == nil || *it.TaskMemoryPeak != gib {
			t.Fatalf("stand peak %d, budget peak %v; want %d and %d", it.MemoryPeak, it.TaskMemoryPeak, 600*mib, gib)
		}
	}
}

func TestReleaseRecordsZeroBudgetPeak(t *testing.T) {
	e := newResEnv(t)
	r := e.rreq(gib)
	r.TaskMemory, r.TaskCPUs = gib, 1
	a := e.reserve(t, r)
	e.tick(t)
	if _, err := e.s.Release(e.ctx, a.ID, false); err != nil {
		t.Fatal(err)
	}
	it := e.get(t, a.ID)
	if it.TaskMemoryPeak == nil || *it.TaskMemoryPeak != 0 || it.MemoryPeak != 0 {
		t.Fatalf("stand peak %d, budget peak %v; want recorded zeros", it.MemoryPeak, it.TaskMemoryPeak)
	}
}

func TestReleaseReportsBudgetPeak(t *testing.T) {
	e := newResEnv(t)
	r := e.rreq(gib)
	r.DockerProject, r.TaskMemory, r.TaskCPUs = "p", 3*gib, 2
	a := e.reserve(t, r)
	e.meter.Set("p", 256*mib)
	e.tick(t)
	n := e.runNested(t, a.ID, 768*mib)
	e.tick(t)
	e.finish(n.ID, success)
	e.tick(t)
	got, err := e.s.Release(e.ctx, a.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	const want = "stand peak 256 MiB is 25% of declared 1.0 GiB (overestimated); " +
		"task budget peak 768 MiB is 25% of declared 3.0 GiB (overestimated)"
	if got.Diagnostic != want {
		t.Fatalf("diagnostic %q, want %q", got.Diagnostic, want)
	}
}

func TestExpiryRecordsPeaks(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.PoolMemory = 4 * gib })
	e.meter.Set("p", 70*mib)
	res, _ := e.grantStand(t, time.Hour, func(r *ReserveRequest) {
		r.DockerProject, r.TaskMemory, r.TaskCPUs = "p", gib, 1
	})
	n := e.runNested(t, res.ID, 200*mib)
	e.tick(t)
	e.finish(n.ID, success)
	e.tick(t)
	it := e.tearDown(t, res)
	if it.MemoryPeak != 70*mib || it.TaskMemoryPeak == nil || *it.TaskMemoryPeak != 200*mib {
		t.Fatalf("stand peak %d, budget peak %v; want %d and %d", it.MemoryPeak, it.TaskMemoryPeak, 70*mib, 200*mib)
	}
}
