//go:build integration

package linux

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"resource-dispatcher/internal/model"
	"resource-dispatcher/internal/platform"
)

const testPool = 1 << 30

func buildAq(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "aq")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/aq")
	cmd.Dir = "../../.."
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

func newTestExecutor(t *testing.T, extra ...string) *Executor {
	t.Helper()
	e, err := NewExecutor(ExecutorConfig{SliceName: "aq-test.slice", UnitPrefix: "aqtest-",
		AqBinary: buildAq(t), ExtraProperties: extra})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.EnsureSlice(t.Context(), testPool); err != nil {
		t.Fatal(err)
	}
	return e
}

// launch starts argv with MemoryHigh = limit, as with the default memory_high_ratio 1.0.
func launch(t *testing.T, e *Executor, limit uint64, timeout time.Duration, argv ...string) (platform.LaunchSpec, error) {
	t.Helper()
	d := t.TempDir()
	id := model.NewID()
	spec := platform.LaunchSpec{
		ID: id, Argv: argv, Cwd: d,
		Env:   map[string]string{"PATH": os.Getenv("PATH"), "HOME": os.Getenv("HOME")},
		Limit: limit, MemoryHigh: limit, CPUs: 1,
		Timeout: timeout, KillGrace: 2 * time.Second,
		JobDir: filepath.Join(d, "job"), TmpDir: filepath.Join(d, "tmp"),
		MaxStdout: 1 << 20, MaxStderr: 1 << 20,
	}
	unit := "aqtest-" + string(id) + ".service"
	t.Cleanup(func() {
		ctx := context.Background()
		_ = exec.CommandContext(ctx, "systemctl", "--user", "stop", unit).Run()
		_ = exec.CommandContext(ctx, "systemctl", "--user", "reset-failed", unit).Run()
	})
	_, err := e.Launch(t.Context(), spec)
	return spec, err
}

func waitResult(t *testing.T, jobDir string, timeout time.Duration) *platform.ExitInfo {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		info, err := (&Executor{}).ReadResult(jobDir)
		if err != nil {
			t.Fatal(err)
		}
		if info != nil {
			return info
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("no result.json in %s after %v", jobDir, timeout)
	return nil
}

func TestIntegExitCode(t *testing.T) {
	e := newTestExecutor(t)
	spec, err := launch(t, e, 64<<20, time.Minute, "sh", "-c", "exit 3")
	if err != nil {
		t.Fatal(err)
	}
	info := waitResult(t, spec.JobDir, 30*time.Second)
	if info.ServiceResult != "exit-code" || info.ExitStatus != "3" {
		t.Fatalf("%+v", info)
	}
}

func TestIntegOOM(t *testing.T) {
	e := newTestExecutor(t)
	spec, err := launch(t, e, 256<<20, time.Minute, "python3", "-c", "b=bytearray(1<<30)")
	if err != nil {
		t.Fatal(err)
	}
	info := waitResult(t, spec.JobDir, 60*time.Second)
	if info.ServiceResult != "oom-kill" || info.OwnOOMKills < 1 || info.OwnMaxEvents < 1 {
		t.Fatalf("%+v", info)
	}
}

func TestIntegChildrenSumOOM(t *testing.T) {
	e := newTestExecutor(t)
	spec, err := launch(t, e, 256<<20, time.Minute, "sh", "-c",
		`for i in 1 2 3 4; do python3 -c "import time;b=bytearray(100<<20);time.sleep(30)" & done; wait`)
	if err != nil {
		t.Fatal(err)
	}
	info := waitResult(t, spec.JobDir, 60*time.Second)
	if info.ServiceResult != "oom-kill" {
		t.Fatalf("%+v", info)
	}
}

func TestIntegTimeout(t *testing.T) {
	e := newTestExecutor(t)
	spec, err := launch(t, e, 64<<20, time.Second, "sleep", "30")
	if err != nil {
		t.Fatal(err)
	}
	info := waitResult(t, spec.JobDir, 20*time.Second)
	if info.ServiceResult != "timeout" {
		t.Fatalf("%+v", info)
	}
}

func TestIntegSetsidDescendantKilled(t *testing.T) {
	e := newTestExecutor(t)
	spec, err := launch(t, e, 64<<20, time.Minute, "sh", "-c", `setsid sh -c "sleep 300" & sleep 300`)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	time.Sleep(500 * time.Millisecond)
	if alive, err := e.Alive(ctx, spec.ID); err != nil || !alive {
		t.Fatalf("not alive before stop: %v %v", alive, err)
	}
	if err := e.Stop(ctx, spec.ID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(spec.KillGrace + 5*time.Second)
	for {
		alive, err := e.Alive(ctx, spec.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !alive {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("still alive after KillGrace+5s")
		}
		time.Sleep(100 * time.Millisecond)
	}
	ids, err := e.ListManaged(ctx)
	if err != nil || slices.Contains(ids, spec.ID) {
		t.Fatalf("ListManaged %v %v", ids, err)
	}
	if err := e.Stop(ctx, spec.ID); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
}

func TestIntegLimitsAppliedBeforeStart(t *testing.T) {
	e := newTestExecutor(t)
	const limit = 96 << 20
	spec, err := launch(t, e, limit, time.Minute, "sh", "-c",
		`cat "/sys/fs/cgroup$(sed -n 's/^0:://p' /proc/self/cgroup)/memory.max" /proc/self/oom_score_adj`)
	if err != nil {
		t.Fatal(err)
	}
	info := waitResult(t, spec.JobDir, 30*time.Second)
	b, _ := os.ReadFile(filepath.Join(spec.JobDir, "stdout.log"))
	if got := strings.Fields(string(b)); !slices.Equal(got, []string{strconv.Itoa(limit), "900"}) {
		t.Fatalf("stdout %q, result %+v", b, info)
	}
}

func TestIntegLaunchFailsWhenPropertyRejected(t *testing.T) {
	e := newTestExecutor(t, "MemoryMax=garbage")
	spec, err := launch(t, e, 64<<20, time.Minute, "sh", "-c", "echo ran")
	if err == nil {
		t.Fatal("Launch succeeded")
	}
	if !strings.Contains(err.Error(), "garbage") {
		t.Errorf("error lacks systemd-run stderr: %v", err)
	}
	time.Sleep(500 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(spec.JobDir, "stdout.log")); !os.IsNotExist(err) {
		t.Fatalf("stdout.log exists: %v", err)
	}
	out, _ := exec.Command("systemctl", "--user", "show", "-p", "LoadState", "--value",
		"aqtest-"+string(spec.ID)+".service").Output()
	if s := strings.TrimSpace(string(out)); s != "not-found" {
		t.Fatalf("unit LoadState %q", s)
	}
}
