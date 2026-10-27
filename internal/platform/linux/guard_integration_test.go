//go:build integration

package linux

import (
	"context"
	"os"
	"path/filepath"
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

// A task whose page cache keeps hitting its own memory.max (memory.events "max" > 0,
// no OOM) and that the host guard then kills is a guard kill, not an own-limit OOM.
func TestIntegGuardKillAfterPageCacheReclaimIsNotOwnOOM(t *testing.T) {
	e := newTestExecutor(t)
	restoreSlice(t, e)
	// Page cache must be disk-backed: /tmp is tmpfs, whose pages count as unreclaimable shmem.
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	disk, err := os.MkdirTemp(cache, "aqtest-pagecache-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(disk) })

	const script = `
import os, sys, time
os.chdir(sys.argv[1])
b = bytearray(96 << 20)
b[::4096] = b'x' * len(b[::4096])
blk = bytes(1 << 20)
with open('big', 'wb') as f:
    for i in range(256):
        f.write(blk)
        if i % 16 == 15:
            os.fsync(f.fileno())  # bound dirty pages: they cannot be reclaimed until written back
open('ready', 'w').close()
while True:
    with open('big', 'rb') as f:
        while f.read(1 << 20):
            pass
    time.sleep(0.2)
`
	spec, err := launch(t, e, 192<<20, 2*time.Minute, "python3", "-c", script, disk)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		_, rerr := os.Stat(filepath.Join(disk, "ready"))
		maxEv, _ := readEvent(filepath.Join(e.unitDir(spec.ID), "memory.events"), "max")
		if rerr == nil && maxEv > 0 {
			break
		}
		if time.Now().After(deadline) {
			logs, _ := os.ReadFile(filepath.Join(spec.JobDir, "stderr.log"))
			t.Fatalf("page cache never hit the unit limit (ready: %v, max events %d): %s", rerr, maxEv, logs)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err := e.SetSliceCeiling(64<<20, 64<<20); err != nil {
		t.Fatal(err)
	}
	info := waitResult(t, spec.JobDir, 60*time.Second)
	if info.ServiceResult != "oom-kill" {
		t.Fatalf("%+v", info)
	}
	if state, reason := scheduler.MapOutcome(*info, false, true); state != model.StateFailed || reason != model.ReasonOOMHostGuard {
		t.Fatalf("guard active: outcome %v %v for %+v", state, reason, info)
	}
	if _, reason := scheduler.MapOutcome(*info, false, false); reason == model.ReasonOOM {
		t.Fatalf("guard inactive: classified as own OOM for %+v", info)
	}
}
