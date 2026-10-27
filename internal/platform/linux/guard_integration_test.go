//go:build integration

package linux

import (
	"testing"
	"time"

	"resource-dispatcher/internal/model"
	"resource-dispatcher/internal/scheduler"
)

// Lowering the slice ceiling below a task's usage kills the task (attributed to the
// host guard), and the test process survives.
func TestIntegGuardKillsTaskNotHost(t *testing.T) {
	e := newTestExecutor(t)
	t.Cleanup(func() { _ = e.EnsureSlice(t.Context(), testPool) }) // restores the ceiling

	spec, err := launch(t, e, 512<<20, time.Minute, "python3", "-c",
		"import time;b=bytearray(300<<20);b[::4096]=b'x'*len(b[::4096]);time.sleep(60)")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		cur, err := e.MemoryCurrent(spec.ID)
		if err != nil {
			t.Fatal(err)
		}
		if cur >= 280<<20 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("task never reached 280 MiB (at %d)", cur)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err := e.SetSliceCeiling(200<<20, 200<<20); err != nil {
		t.Fatal(err)
	}
	info := waitResult(t, spec.JobDir, 60*time.Second)
	if info.ServiceResult != "oom-kill" {
		t.Fatalf("%+v", info)
	}
	state, reason := scheduler.MapOutcome(*info, false, true)
	if state != model.StateFailed || reason != model.ReasonOOMHostGuard {
		t.Fatalf("outcome %v %v for %+v", state, reason, info)
	}
}
