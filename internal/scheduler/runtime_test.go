package scheduler

import (
	"testing"
	"time"

	"github.com/creatorofuniverses/agent-queue/internal/platform"
)

func TestRuntimeSurvivesRestart(t *testing.T) {
	e := newEnv(t, nil)
	r := e.req(100 * mib)
	r.Profile = "p"
	it := e.submit(t, r)
	e.tick(t)
	e.ex.SetCurrent(it.ID, 50*mib)
	for i := 0; i < 5; i++ {
		e.clk.Advance(time.Second)
		e.tick(t)
	}
	e.s.SetGuardActive(true) // still on at shutdown
	snap, err := e.s.SnapshotRuntime()
	if err != nil {
		t.Fatal(err)
	}
	e.rebuild(t, e.ex, true)
	if n, err := e.s.RestoreRuntime(e.ctx, snap); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	// The interval open at shutdown was closed by the snapshot: a kill long after
	// the restart is not blamed on the old guard episode.
	if e.s.guardAt(e.clk.Now().Add(time.Minute)) {
		t.Fatal("open guard interval survived the restart")
	}
	if !e.s.guardAt(e.clk.Now()) {
		t.Fatal("guard history lost")
	}
	for i := 0; i < 2; i++ {
		e.clk.Advance(time.Second)
		e.tick(t)
	}
	e.ex.Finish(it.ID, platform.ExitInfo{ServiceResult: "success", MemoryPeak: 60 * mib, EndedAt: e.clk.Now()})
	e.ex.SetAlive(it.ID, false)
	e.tick(t)
	rs, _ := e.st.ProfileRuns(e.ctx, "p", 10)
	if len(rs) != 1 || rs[0].Samples < 6 {
		t.Fatal("samples before the restart were lost", rs)
	}
}

func TestRestoreRuntimeSkipsFinishedTasks(t *testing.T) {
	e := newEnv(t, nil)
	r := e.req(100 * mib)
	r.Profile = "p"
	it := e.submit(t, r)
	e.tick(t)
	e.ex.SetCurrent(it.ID, 50*mib)
	e.clk.Advance(time.Second)
	e.tick(t)
	snap, _ := e.s.SnapshotRuntime()
	e.ex.Finish(it.ID, platform.ExitInfo{ServiceResult: "success", MemoryPeak: 60 * mib, EndedAt: e.clk.Now()})
	e.ex.SetAlive(it.ID, false)
	e.tick(t)
	e.rebuild(t, e.ex, true)
	if n, err := e.s.RestoreRuntime(e.ctx, snap); err != nil || n != 0 || len(e.s.samples) != 0 {
		t.Fatal(n, err, e.s.samples)
	}
}
