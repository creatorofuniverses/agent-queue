//go:build integration

package linux

import (
	"context"
	"testing"
	"time"

	"resource-dispatcher/internal/config"
	"resource-dispatcher/internal/daemon"
	"resource-dispatcher/internal/model"
	"resource-dispatcher/internal/platform"
	"resource-dispatcher/internal/scheduler"
)

// Lowering the slice ceiling below a task's usage kills the task (attributed to the
// host guard), and the test process survives.
func TestIntegGuardKillsTaskNotHost(t *testing.T) {
	e := newTestExecutor(t)
	restoreSlice(t, e)

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

// restoreSlice resets the test slice ceiling when the test ends.
func restoreSlice(t *testing.T, e *Executor) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := e.EnsureSlice(ctx, testPool); err != nil {
			t.Errorf("restore slice ceiling: %v", err)
		}
	})
}

type staticHost struct{ mem platform.MemInfo }

func (h staticHost) Memory() (platform.MemInfo, error) { return h.mem, nil }
func (staticHost) PressureFullAvg10() (float64, error) { return 0, nil }
func (staticHost) DiskFree(string) (uint64, error)     { return 0, nil }
func (staticHost) KernelOOMKills() (uint64, error)     { return 0, nil }
func (staticHost) NumCPU() int                         { return 1 }

// Starting a sibling unit makes systemd reset the slice ceiling; the running guard keeps it lowered.
func TestIntegGuardHoldsCeilingAcrossSiblingStart(t *testing.T) {
	e := newTestExecutor(t)
	restoreSlice(t, e)
	cfg := config.Default()
	cfg.PoolMemory = testPool
	cfg.ReserveFloorPercent = 0
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		daemon.RunGuard(ctx, e, staticHost{platform.MemInfo{Total: 4 << 30, Available: 600 << 20}}, cfg, func(bool) {}, 100*time.Millisecond)
	}()
	t.Cleanup(func() { cancel(); <-done })
	const limit = 700 << 20 // 600 MiB available + small slice usage
	waitCeiling := func() uint64 {
		deadline := time.Now().Add(10 * time.Second)
		for {
			got, err := e.SliceCeiling()
			if err == nil && got < limit {
				return got
			}
			if time.Now().After(deadline) {
				t.Fatalf("ceiling %d (%v) not lowered", got, err)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	waitCeiling()
	if _, err := launch(t, e, 64<<20, time.Minute, "sleep", "30"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	waitCeiling()
}
