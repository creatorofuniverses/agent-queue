//go:build integration

package e2e

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-queue/internal/daemon"
	"agent-queue/internal/model"
)

// writeScript writes a teardown script into dir and returns its path.
func writeScript(t *testing.T, dir, body string) string {
	t.Helper()
	p := filepath.Join(dir, "td.sh")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// reserve runs `aq reserve --json <flags>` from dir and returns the reservation.
func (h *harness) reserve(dir string, flags ...string) daemon.ItemView {
	h.t.Helper()
	out, errOut, code := h.aqIn(dir, append([]string{"reserve", "--json"}, flags...)...)
	if code != 0 {
		h.t.Fatalf("aq reserve %v: exit %d: %s", flags, code, errOut)
	}
	var v daemon.ItemView
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		h.t.Fatalf("reserve output %q: %v", out, err)
	}
	return v
}

// itemOp runs an item command with --json (extend, release, teardown) and returns the item.
func (h *harness) itemOp(args ...string) daemon.ItemView {
	h.t.Helper()
	out, errOut, code := h.aq(append(args, "--json")...)
	if code != 0 {
		h.t.Fatalf("aq %v: exit %d: %s", args, code, errOut)
	}
	var v daemon.ItemView
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		h.t.Fatalf("aq %v output %q: %v", args, out, err)
	}
	return v
}

// teardownUnit is the systemd unit of a reservation's teardown.
func teardownUnit(id model.ItemID) string {
	return unitPrefix + string(model.TeardownUnitID(id)) + ".service"
}

// unitActive reports whether systemd considers the unit active (or activating).
func unitActive(unit string) bool {
	out, _ := exec.Command("systemctl", "--user", "is-active", unit).Output()
	s := strings.TrimSpace(string(out))
	return s == "active" || s == "activating" || s == "deactivating"
}

// lines returns the lines of a marker file; a missing file has none.
func lines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	s := strings.TrimRight(string(b), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func eventually(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not reached in %s", what, timeout)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// §15: a granted reservation is extended and released; release never runs the teardown.
func TestE2EReserveExtendRelease(t *testing.T) {
	h := newHarness(t, "")
	marker := filepath.Join(h.dir, "marker")
	td := writeScript(t, h.dir, "echo x >> "+marker+"\n")

	r := h.reserve(h.dir, "--memory", "300M", "--ttl", "1m", "--teardown", td)
	if r.State != model.StateGranted || r.Kind != model.KindReservation || r.ExpiresAt == nil {
		t.Fatalf("reserve: %s kind=%s expires_at=%v, want GRANTED reservation with expires_at", r.State, r.Kind, r.ExpiresAt)
	}
	// The plain output is "<id> STATE".
	out, errOut, code := h.aq("reserve", "--memory", "64M", "--ttl", "1m", "--teardown", td)
	plain := strings.Fields(out)
	if code != 0 || len(plain) != 2 || plain[1] != string(model.StateGranted) {
		t.Fatalf("plain reserve: output %q exit %d (%s), want \"<id> GRANTED\"", out, code, errOut)
	}
	h.itemOp("release", plain[0])

	e := h.itemOp("extend", string(r.ID), "--ttl", "5m")
	if e.State != model.StateGranted || e.ExpiresAt == nil || !e.ExpiresAt.After(*r.ExpiresAt) {
		t.Fatalf("extend: %s expires_at %v, want GRANTED and later than %v", e.State, e.ExpiresAt, r.ExpiresAt)
	}
	if v := h.status(r.ID); v.ExpiresAt == nil || !v.ExpiresAt.Equal(*e.ExpiresAt) {
		t.Fatalf("status expires_at %v, extend returned %v", v.ExpiresAt, e.ExpiresAt)
	}

	if v := h.itemOp("release", string(r.ID)); v.State != model.StateReleased {
		t.Fatalf("release: %s/%s, want RELEASED", v.State, v.Reason)
	}
	if code := h.wait(r.ID); code != 6 {
		t.Fatalf("wait on a released reservation: exit %d, want 6", code)
	}
	time.Sleep(time.Second)
	if l := lines(t, marker); len(l) != 0 {
		t.Fatalf("teardown ran on release: marker %q", l)
	}
}

// §15 / Review Focus: the teardown runs from the daemon's copy after the script and the
// whole project directory are gone, in <attempt>/cwd with AQ_ORIGINAL_CWD set.
func TestE2EExpiryTeardownByCopy(t *testing.T) {
	h := newHarness(t, "")
	proj := filepath.Join(h.dir, "proj")
	if err := os.Mkdir(proj, 0o700); err != nil {
		t.Fatal(err)
	}
	proj, err := filepath.EvalSymlinks(proj)
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(h.dir, "marker")
	writeScript(t, proj, "echo \"$AQ_ORIGINAL_CWD\" > "+marker+"\npwd >> "+marker+"\n")

	r := h.reserve(proj, "--memory", "64M", "--ttl", "5s", "--teardown", "td.sh")
	if r.State != model.StateGranted {
		t.Fatalf("reserve: %s, want GRANTED", r.State)
	}
	if err := os.RemoveAll(proj); err != nil {
		t.Fatal(err)
	}

	v := h.waitState(r.ID, model.StateTornDown, 30*time.Second)
	if v.TeardownAttempt != 1 {
		t.Fatalf("teardown_attempt %d, want 1", v.TeardownAttempt)
	}
	state, err := filepath.EvalSymlinks(filepath.Join(h.dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{proj, filepath.Join(state, "jobs", string(r.ID), "teardown-1", "cwd")}
	if got := lines(t, marker); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("marker %q, want %q", got, want)
	}
}

// §15: a failed teardown keeps the reservation in the pool until release --force.
func TestE2ETeardownFailureKeepsAccounting(t *testing.T) {
	h := newHarness(t, "")
	td := writeScript(t, h.dir, "exit 3\n")

	r := h.reserve(h.dir, "--memory", "1G", "--ttl", "3s", "--teardown", td)
	if r.State != model.StateGranted {
		t.Fatalf("reserve: %s, want GRANTED", r.State)
	}
	v := h.waitState(r.ID, model.StateTeardownFailed, 30*time.Second)
	if v.ExitCode == nil || *v.ExitCode != 3 {
		t.Fatalf("teardown exit_code %v, want 3 (%s)", v.ExitCode, v.Diagnostic)
	}
	if code := h.wait(r.ID); code != 6 {
		t.Fatalf("wait on TEARDOWN_FAILED: exit %d, want 6", code)
	}

	task := h.submit(mem("256M"), "true")
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(250 * time.Millisecond) {
		vs, err := h.list()
		if err != nil {
			t.Fatal(err)
		}
		var tv *daemon.ItemView
		for i := range vs {
			if vs[i].ID == task {
				tv = &vs[i]
			}
		}
		if tv == nil || tv.State != model.StateQueued || tv.BlockedBy != "pool_memory" {
			t.Fatalf("task while the failed reservation holds the pool: %+v, want QUEUED blocked_by pool_memory", tv)
		}
	}

	if v := h.itemOp("release", string(r.ID), "--force"); v.State != model.StateReleased {
		t.Fatalf("release --force: %s, want RELEASED", v.State)
	}
	if code := h.wait(task); code != 0 {
		t.Fatalf("task after release --force: wait exit %d (%+v)", code, h.status(task))
	}
	if v := h.status(task); v.State != model.StateSucceeded {
		t.Fatalf("task %s/%s, want SUCCEEDED", v.State, v.Reason)
	}
}

// §15: two reservations filling the pool run their nested tasks without deadlock;
// a nested task over the budget is rejected at once.
func TestE2EWithinNoDeadlock(t *testing.T) {
	h := newHarness(t, "")
	td := writeScript(t, h.dir, "true\n")

	var res [2]daemon.ItemView
	for i := range res {
		res[i] = h.reserve(h.dir, "--memory", "200M", "--task-memory", "300M", "--task-cpus", "1",
			"--ttl", "5m", "--teardown", td)
		if res[i].State != model.StateGranted {
			t.Fatalf("reserve %d: %s, want GRANTED", i, res[i].State)
		}
	}
	var tasks [2]model.ItemID
	for i := range tasks {
		tasks[i] = h.submit(append(mem("300M"), "--within", string(res[i].ID)), "sh", "-c", "sleep 2")
	}
	h.waitState(tasks[0], model.StateRunning, 30*time.Second)

	start := time.Now()
	_, errOut, code := h.aq(append(append([]string{"submit"}, mem("300M")...), "--within", string(res[0].ID), "--", "true")...)
	if took := time.Since(start); code != 5 || took >= 2*time.Second {
		t.Fatalf("third nested submit: exit %d in %s (%s), want 5 in < 2s", code, took, errOut)
	}
	if !strings.Contains(errOut, "task budget") {
		t.Fatalf("third nested submit error %q, want it to mention the task budget", errOut)
	}
	if v := h.status(tasks[0]); v.State != model.StateRunning {
		t.Fatalf("first nested task %s after the rejection, want still RUNNING", v.State)
	}

	for _, id := range tasks {
		if code := h.wait(id); code != 0 {
			t.Fatalf("wait %s: exit %d (%+v)", id, code, h.status(id))
		}
		if v := h.status(id); v.State != model.StateSucceeded || v.Within == "" {
			t.Fatalf("%s: %s within=%q, want SUCCEEDED nested", id, v.State, v.Within)
		}
	}
	for _, r := range res {
		h.itemOp("release", string(r.ID))
	}
}

// §15: a nested task running past the TTL holds the reservation; it is torn down
// only after the task ends.
func TestE2EHeldByTask(t *testing.T) {
	h := newHarness(t, "")
	marker := filepath.Join(h.dir, "marker")
	td := writeScript(t, h.dir, "echo x >> "+marker+"\n")

	start := time.Now()
	r := h.reserve(h.dir, "--memory", "64M", "--task-memory", "64M", "--task-cpus", "1",
		"--ttl", "3s", "--teardown", td)
	if r.State != model.StateGranted {
		t.Fatalf("reserve: %s, want GRANTED", r.State)
	}
	task := h.submit([]string{"--memory", "64M", "--timeout", "30s", "--within", string(r.ID)}, "sleep", "8")
	h.waitState(task, model.StateRunning, 4*time.Second)

	time.Sleep(time.Until(start.Add(5 * time.Second)))
	v := h.status(r.ID)
	if v.State != model.StateExpiring || !v.TTLHeld {
		t.Fatalf("reservation at t+5s: %s ttl_held=%v, want EXPIRING held", v.State, v.TTLHeld)
	}
	if v.ExpiresAt == nil || !v.ExpiresAt.After(start.Add(8*time.Second)) {
		t.Fatalf("held expires_at %v, want past the nested task's end", v.ExpiresAt)
	}
	if l := lines(t, marker); len(l) != 0 {
		t.Fatalf("teardown ran under a running nested task: marker %q", l)
	}

	if code := h.wait(task); code != 0 {
		t.Fatalf("nested task: wait exit %d (%+v)", code, h.status(task))
	}
	h.waitState(r.ID, model.StateTornDown, 60*time.Second)
	if l := lines(t, marker); len(l) != 1 {
		t.Fatalf("marker %q, want one teardown run", l)
	}
}

// startKilledTeardown reserves with a 4s teardown that appends to marker, waits until
// the teardown unit runs and SIGKILLs the daemon.
func startKilledTeardown(t *testing.T, h *harness, marker string) model.ItemID {
	t.Helper()
	td := writeScript(t, h.dir, "sleep 4; echo x >> "+marker+"\n")
	r := h.reserve(h.dir, "--memory", "64M", "--ttl", "2s", "--teardown", td)
	if r.State != model.StateGranted {
		t.Fatalf("reserve: %s, want GRANTED", r.State)
	}
	h.waitState(r.ID, model.StateTearingDown, 30*time.Second)
	// TEARING_DOWN is recorded before the launch: kill only once the unit runs.
	eventually(t, 10*time.Second, "teardown unit active", func() bool { return unitActive(teardownUnit(r.ID)) })
	h.killDaemon()
	return r.ID
}

// §15: a teardown outlives a SIGKILLed daemon and its result is picked up after restart.
func TestE2EDaemonKilledDuringTeardown(t *testing.T) {
	h := newHarness(t, "")
	marker := filepath.Join(h.dir, "marker")
	id := startKilledTeardown(t, h, marker)

	time.Sleep(6 * time.Second)
	h.startDaemon()
	h.waitState(id, model.StateTornDown, 30*time.Second)
	if l := lines(t, marker); len(l) != 1 {
		t.Fatalf("marker %q, want exactly one line", l)
	}
}

// §15: a teardown that left no result is TEARDOWN_UNKNOWN after restart, is not
// retried by itself, and runs again on teardown --retry.
func TestE2ETeardownUnknownNoRetry(t *testing.T) {
	h := newHarness(t, "")
	marker := filepath.Join(h.dir, "marker")
	id := startKilledTeardown(t, h, marker)

	result := filepath.Join(h.dir, "state", "jobs", string(id), "teardown-1", "result.json")
	eventually(t, 30*time.Second, "teardown unit finished with a result", func() bool {
		_, err := os.Stat(result)
		return err == nil && !unitActive(teardownUnit(id))
	})
	if err := os.Remove(result); err != nil {
		t.Fatal(err)
	}
	h.startDaemon()

	h.waitState(id, model.StateTeardownUnknown, 30*time.Second)
	if code := h.wait(id); code != 6 {
		t.Fatalf("wait on TEARDOWN_UNKNOWN: exit %d, want 6", code)
	}
	time.Sleep(10 * time.Second)
	if v := h.status(id); v.State != model.StateTeardownUnknown || v.TeardownAttempt != 1 {
		t.Fatalf("10s after restart: %s attempt %d, want TEARDOWN_UNKNOWN attempt 1", v.State, v.TeardownAttempt)
	}
	if l := lines(t, marker); len(l) != 1 {
		t.Fatalf("marker %q 10s after restart, want one line (no automatic retry)", l)
	}

	h.itemOp("teardown", "--retry", string(id))
	v := h.waitState(id, model.StateTornDown, 30*time.Second)
	if v.TeardownAttempt != 2 {
		t.Fatalf("teardown_attempt %d after retry, want 2", v.TeardownAttempt)
	}
	if l := lines(t, marker); len(l) != 2 {
		t.Fatalf("marker %q after retry, want two lines", l)
	}
}
