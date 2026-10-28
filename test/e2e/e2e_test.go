//go:build integration

// Package e2e runs aq scenarios against the real systemd user manager.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creatorofuniverses/agent-queue/internal/config"
	"github.com/creatorofuniverses/agent-queue/internal/daemon"
	"github.com/creatorofuniverses/agent-queue/internal/model"
	"github.com/creatorofuniverses/agent-queue/internal/protocol"
)

const (
	sliceName         = "aqe2e.slice"
	teardownSliceName = "aqe2etd.slice"
	unitPrefix        = "aqe2e-"
	poolMemory        = 1 << 30
)

var aqBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "aqe2e-bin-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	aqBin = filepath.Join(dir, "aq")
	cmd := exec.Command("go", "build", "-o", aqBin, "./cmd/aq")
	cmd.Dir = "../.."
	if out, err := cmd.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "go build: %v\n%s", err, out)
		os.RemoveAll(dir)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// harness is one isolated aq installation: its own state, runtime dir, config and daemon.
type harness struct {
	t      *testing.T
	dir    string
	env    []string
	socket string
	logf   *os.File
	daemon *exec.Cmd
	exited chan struct{}
}

const baseConfig = `pool_memory = "1G"
pool_cpus = 4
cancel_grace = "3s"
slice_name = "` + sliceName + `"
unit_prefix = "` + unitPrefix + `"

[reservations]
expiry_warning = "2s"
teardown_timeout = "20s"
teardown_memory = "64M"
`

const testAgents = `
[[agents]]
name = "fakecodex"
identity_env = "FAKE_SESSION"
delivery = "push"
command = ["/usr/bin/false", "{session}", "{text}"]
[[agents]]
name = "claude"
identity_env = "CLAUDE_CODE_SESSION_ID"
delivery = "client-wait"
`

// newHarness writes the config (base plus extra TOML) and starts the daemon.
func newHarness(t *testing.T, extra string) *harness {
	t.Helper()
	// A short root keeps the Unix socket path well under the sun_path limit.
	dir, err := os.MkdirTemp("", "aqe2e-")
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, dir: dir}
	cfg := filepath.Join(dir, "config.toml")
	body := baseConfig + extra
	if !strings.Contains(extra, "[[agents]]") {
		body += testAgents
	}
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(cfg)
	if err != nil {
		os.RemoveAll(dir)
		t.Fatal(err)
	}
	identities := map[string]bool{"CODEX_THREAD_ID": true, "CLAUDE_CODE_SESSION_ID": true}
	for _, a := range loaded.Agents {
		identities[a.IdentityEnv] = true
	}
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		if !strings.HasPrefix(key, "AQ_") && !identities[key] {
			h.env = append(h.env, kv)
		}
	}
	h.env = append(h.env,
		"AQ_STATE_DIR="+filepath.Join(dir, "state"),
		"AQ_RUNTIME_DIR="+filepath.Join(dir, "run"),
		"AQ_CONFIG="+cfg)
	h.socket = filepath.Join(dir, "run", "aq.sock")
	if h.logf, err = os.Create(filepath.Join(dir, "daemon.log")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.cleanup)
	h.startDaemon()
	return h
}

func (h *harness) startDaemon() {
	h.t.Helper()
	cmd := exec.Command(aqBin, "daemon")
	cmd.Env = h.env
	cmd.Dir = h.dir
	cmd.Stdout, cmd.Stderr = h.logf, h.logf
	if err := cmd.Start(); err != nil {
		h.t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { cmd.Wait(); close(exited) }()
	h.daemon, h.exited = cmd, exited

	deadline := time.Now().Add(30 * time.Second)
	for {
		if err := h.ping(); err == nil {
			return
		}
		select {
		case <-exited:
			h.t.Fatalf("daemon exited during startup:\n%s", h.daemonLog())
		default:
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("daemon did not answer ping in 30s:\n%s", h.daemonLog())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (h *harness) ping() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var res struct {
		Recovering bool `json:"recovering"`
	}
	if err := protocol.Call(ctx, h.socket, "ping", nil, &res); err != nil {
		return err
	}
	if res.Recovering {
		return errors.New("recovering")
	}
	return nil
}

// killDaemon sends SIGKILL and waits for the process to be gone.
func (h *harness) killDaemon() {
	h.t.Helper()
	if err := h.daemon.Process.Signal(syscall.SIGKILL); err != nil {
		h.t.Fatal(err)
	}
	select {
	case <-h.exited:
	case <-time.After(10 * time.Second):
		h.t.Fatal("daemon did not die after SIGKILL")
	}
}

func (h *harness) daemonLog() string {
	b, _ := os.ReadFile(filepath.Join(h.dir, "daemon.log"))
	return string(b)
}

func (h *harness) cleanup() {
	if h.daemon != nil {
		select {
		case <-h.exited:
		default:
			h.daemon.Process.Signal(syscall.SIGTERM)
			select {
			case <-h.exited:
			case <-time.After(15 * time.Second):
				h.daemon.Process.Kill()
				<-h.exited
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	systemctl := func(args ...string) {
		_ = exec.CommandContext(ctx, "systemctl", append([]string{"--user"}, args...)...).Run()
	}
	systemctl("stop", unitPrefix+"*.service")
	systemctl("reset-failed", unitPrefix+"*.service")
	systemctl("stop", sliceName, teardownSliceName)
	if h.t.Failed() {
		h.t.Logf("daemon log:\n%s", h.daemonLog())
	}
	h.logf.Close()
	os.RemoveAll(h.dir)
}

// aq runs the client and returns stdout, stderr and the exit code (-1 if it
// could not run). Safe to call from any goroutine.
func (h *harness) aq(args ...string) (string, string, int) {
	return h.aqEnv(nil, args...)
}

// aqIn is aq run from directory dir.
func (h *harness) aqEnv(vars map[string]string, args ...string) (string, string, int) {
	return h.aqWithEnvIn(h.dir, vars, args...)
}
func (h *harness) aqIn(dir string, args ...string) (string, string, int) {
	return h.aqWithEnvIn(dir, nil, args...)
}
func (h *harness) aqWithEnvIn(dir string, vars map[string]string, args ...string) (string, string, int) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, aqBin, args...)
	for _, kv := range h.env {
		key, _, _ := strings.Cut(kv, "=")
		if _, overridden := vars[key]; !overridden {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	for k, v := range vars {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Dir = dir
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return out.String(), errb.String(), 0
	case errors.As(err, &ee):
		return out.String(), errb.String(), ee.ExitCode()
	}
	return out.String(), fmt.Sprintf("aq %v: %v", args, err), -1
}

// trySubmit runs `aq submit --json <flags> -- argv` and returns the item ID.
// Safe to call from any goroutine.
func (h *harness) trySubmit(flags []string, argv ...string) (model.ItemID, error) {
	args := append(append([]string{"submit", "--json"}, flags...), "--")
	out, errOut, code := h.aq(append(args, argv...)...)
	if code != 0 {
		return "", fmt.Errorf("aq submit %v: exit %d: %s", argv, code, errOut)
	}
	var v daemon.ItemView
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return "", fmt.Errorf("submit output %q: %v", out, err)
	}
	return v.ID, nil
}

func (h *harness) submit(flags []string, argv ...string) model.ItemID {
	h.t.Helper()
	id, err := h.trySubmit(flags, argv...)
	if err != nil {
		h.t.Fatal(err)
	}
	return id
}

func (h *harness) status(id model.ItemID) daemon.ItemView {
	h.t.Helper()
	out, errOut, code := h.aq("status", "--json", string(id))
	if code != 0 {
		h.t.Fatalf("aq status %s: exit %d: %s", id, code, errOut)
	}
	var v daemon.ItemView
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		h.t.Fatalf("status output %q: %v", out, err)
	}
	return v
}

func (h *harness) list() ([]daemon.ItemView, error) {
	out, errOut, code := h.aq("list", "--json")
	if code != 0 {
		return nil, fmt.Errorf("aq list: exit %d: %s", code, errOut)
	}
	var vs []daemon.ItemView
	err := json.Unmarshal([]byte(out), &vs)
	return vs, err
}

// wait runs `aq wait` and returns its exit code.
func (h *harness) wait(id model.ItemID) int {
	h.t.Helper()
	_, errOut, code := h.aq("wait", string(id))
	if code == -1 {
		h.t.Fatal(errOut)
	}
	return code
}

// waitState polls status until the item reaches state.
func (h *harness) waitState(id model.ItemID, state model.State, timeout time.Duration) daemon.ItemView {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		v := h.status(id)
		if v.State == state {
			return v
		}
		if v.State.Terminal() || time.Now().After(deadline) {
			h.t.Fatalf("item %s is %s/%s, want %s", id, v.State, v.Reason, state)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func mem(m string) []string { return []string{"--memory", m, "--timeout", "2m"} }

// A01/A03: parallel submits all succeed and running memory never exceeds the pool.
func TestE2EParallelSubmits(t *testing.T) {
	h := newHarness(t, "")

	stop := make(chan struct{})
	var (
		maxSum  uint64
		pollErr error
		polled  sync.WaitGroup
	)
	polled.Add(1)
	go func() {
		defer polled.Done()
		tick := time.NewTicker(200 * time.Millisecond)
		defer tick.Stop()
		for {
			vs, err := h.list()
			if err != nil {
				pollErr = err
				return
			}
			var sum uint64
			for _, v := range vs {
				if v.State == model.StateRunning {
					sum += v.MemoryLimit
				}
			}
			maxSum = max(maxSum, sum)
			select {
			case <-stop:
				return
			case <-tick.C:
			}
		}
	}()

	ids := make([]model.ItemID, 10)
	errs := make([]error, len(ids))
	var wg sync.WaitGroup
	for i := range ids {
		wg.Go(func() { ids[i], errs[i] = h.trySubmit(mem("300M"), "sleep", "2") })
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		close(stop)
		polled.Wait()
		t.Fatal(err)
	}
	for _, id := range ids {
		if code := h.wait(id); code != 0 {
			t.Errorf("wait %s: exit %d (%+v)", id, code, h.status(id))
		}
	}
	close(stop)
	polled.Wait()
	if pollErr != nil {
		t.Fatal(pollErr)
	}
	for _, id := range ids {
		if v := h.status(id); v.State != model.StateSucceeded {
			t.Errorf("%s: %s/%s", id, v.State, v.Reason)
		}
	}
	if maxSum > poolMemory {
		t.Fatalf("running memory reached %d > pool %d", maxSum, poolMemory)
	}
	// Three 300M tasks fit the 1G pool; the poll must have seen them run concurrently.
	if maxSum < 2*300<<20 {
		t.Fatalf("max running memory %d: tasks never ran concurrently", maxSum)
	}
}

// A05: 100 concurrent submits with one key create one item that runs once.
func TestE2ESameKey100(t *testing.T) {
	h := newHarness(t, "")
	file := filepath.Join(h.dir, "out.txt")

	ids := make([]string, 100)
	var wg sync.WaitGroup
	for i := range ids {
		wg.Go(func() {
			out, errOut, code := h.aq("submit", "--key", "k", "--memory", "64M", "--timeout", "1m",
				"--", "sh", "-c", "echo x >> "+file)
			if code != 0 {
				t.Errorf("submit %d: exit %d: %s", i, code, errOut)
			}
			ids[i] = strings.TrimSpace(out)
		})
	}
	wg.Wait()
	for i, id := range ids {
		if id != ids[0] || id == "" {
			t.Fatalf("submit %d returned %q, submit 0 returned %q", i, id, ids[0])
		}
	}
	if code := h.wait(model.ItemID(ids[0])); code != 0 {
		t.Fatalf("wait: exit %d", code)
	}
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "x\n" {
		t.Fatalf("file content %q, want exactly one line", b)
	}
}

// A07: a task over its limit is OOM-killed, the daemon survives.
func TestE2EOOM(t *testing.T) {
	h := newHarness(t, "")
	id := h.submit(mem("256M"), "python3", "-c", "b=bytearray(1<<30)")
	if code := h.wait(id); code != 2 {
		t.Fatalf("wait exit %d, want 2 (%+v)", code, h.status(id))
	}
	if v := h.status(id); v.State != model.StateFailed || v.Reason != model.ReasonOOM {
		t.Fatalf("%s/%s, want FAILED/OOM", v.State, v.Reason)
	}
	if err := h.ping(); err != nil {
		t.Fatalf("daemon after OOM: %v", err)
	}
}

// A09/A10: cancelling a running task kills its whole tree, setsid descendants included.
func TestE2ECancelRunningKillsTree(t *testing.T) {
	h := newHarness(t, "")
	marker := "aqe2e-marker-" + string(model.NewID())
	id := h.submit(mem("64M"), "sh", "-c", `setsid sh -c "sleep 300; : `+marker+`" & sleep 300`)
	h.waitState(id, model.StateRunning, 30*time.Second)

	deadline := time.Now().Add(10 * time.Second)
	for !pgrep(t, marker) {
		if time.Now().After(deadline) {
			t.Fatal("setsid descendant never appeared")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if _, errOut, code := h.aq("cancel", string(id)); code != 0 {
		t.Fatalf("cancel: exit %d: %s", code, errOut)
	}
	if code := h.wait(id); code != 4 {
		t.Fatalf("wait exit %d, want 4", code)
	}
	if v := h.status(id); v.State != model.StateCancelled {
		t.Fatalf("state %s/%s, want CANCELLED", v.State, v.Reason)
	}
	if pgrep(t, marker) {
		t.Fatal("task processes survived cancel")
	}
}

// pgrep reports whether a process whose command line contains pattern exists.
func pgrep(t *testing.T, pattern string) bool {
	t.Helper()
	err := exec.Command("pgrep", "-f", pattern).Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return true
	case errors.As(err, &ee) && ee.ExitCode() == 1:
		return false
	}
	t.Fatalf("pgrep: %v", err)
	return false
}

// A12: a task outlives a SIGKILLed daemon and its result is picked up after restart.
func TestE2EDaemonKilledTaskFinishesAlone(t *testing.T) {
	h := newHarness(t, "")
	start := time.Now()
	id := h.submit(mem("64M"), "sh", "-c", "sleep 3; exit 0")
	h.waitState(id, model.StateRunning, 30*time.Second)
	time.Sleep(time.Second - time.Since(start))
	h.killDaemon()
	time.Sleep(5 * time.Second)
	h.startDaemon()

	if code := h.wait(id); code != 0 {
		t.Fatalf("wait exit %d, want 0 (%+v)", code, h.status(id))
	}
	v := h.status(id)
	if v.State != model.StateSucceeded || v.MemoryPeak == 0 {
		t.Fatalf("%s/%s memory_peak=%d, want SUCCEEDED with memory_peak > 0", v.State, v.Reason, v.MemoryPeak)
	}
}

// A27: a failing command makes `aq wait` exit 1 with the command's exit code recorded.
func TestE2ECommandFailsWaitNonzero(t *testing.T) {
	h := newHarness(t, "")
	id := h.submit(mem("64M"), "sh", "-c", "exit 7")
	if code := h.wait(id); code != 1 {
		t.Fatalf("wait exit %d, want 1", code)
	}
	v := h.status(id)
	if v.Reason != model.ReasonCommandFailed || v.ExitCode == nil || *v.ExitCode != 7 {
		t.Fatalf("reason %s exit_code %v, want COMMAND_FAILED 7", v.Reason, v.ExitCode)
	}
}

// A24: endless stdout is capped on disk and the task ends by timeout.
func TestE2EInfiniteStdout(t *testing.T) {
	h := newHarness(t, "[logs]\nmax_stdout = \"1M\"\n")
	id := h.submit([]string{"--memory", "64M", "--timeout", "5s"}, "yes")
	if code := h.wait(id); code != 3 {
		t.Fatalf("wait exit %d, want 3 (%+v)", code, h.status(id))
	}
	out, errOut, code := h.aq("result", "--json", string(id))
	if code != 0 {
		t.Fatalf("result: exit %d: %s", code, errOut)
	}
	var r daemon.ResultView
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatal(err)
	}
	if r.Reason != model.ReasonTimeout {
		t.Fatalf("reason %s, want TIMEOUT", r.Reason)
	}
	fi, err := os.Stat(r.StdoutPath)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() == 0 || fi.Size() > 1<<20 {
		t.Fatalf("stdout.log is %d bytes, want 1..1MiB", fi.Size())
	}
}

// A14: with the pool busy, a later high-priority item starts before an earlier low one.
func TestE2EPriority(t *testing.T) {
	h := newHarness(t, "")
	blocker := h.submit(mem("1G"), "sleep", "3")
	h.waitState(blocker, model.StateRunning, 30*time.Second)
	low := h.submit(append(mem("1G"), "--priority", "low"), "sleep", "1")
	high := h.submit(append(mem("1G"), "--priority", "high"), "sleep", "1")
	for _, id := range []model.ItemID{blocker, low, high} {
		if code := h.wait(id); code != 0 {
			t.Fatalf("wait %s: exit %d", id, code)
		}
	}
	lv, hv := h.status(low), h.status(high)
	if lv.StartedAt == nil || hv.StartedAt == nil {
		t.Fatalf("missing started_at: low %v high %v", lv.StartedAt, hv.StartedAt)
	}
	if !hv.StartedAt.Before(*lv.StartedAt) {
		t.Fatalf("high started %v, low started %v; want high first", hv.StartedAt, lv.StartedAt)
	}
}
