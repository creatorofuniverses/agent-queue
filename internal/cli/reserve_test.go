package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-queue/internal/config"
	"agent-queue/internal/daemon"
	"agent-queue/internal/model"
	"agent-queue/internal/store"
)

func bigPool(c *config.Config) { c.PoolMemory = 8 * gib }

func (h *harness) script(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "td.sh")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func (h *harness) view(t *testing.T, args ...string) daemon.ItemView {
	t.Helper()
	code, out, errOut := h.run(args...)
	if code != 0 {
		t.Fatalf("%v: exit %d: %s", args, code, errOut)
	}
	var v daemon.ItemView
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestReserveParsesFlags(t *testing.T) {
	h := newHarness(t)
	h.startDaemonCfg(t, bigPool)
	td := h.script(t, "echo down\n")
	v := h.view(t, "reserve", "--memory", "2G", "--cpus", "1", "--task-memory", "1G", "--task-cpus", "2",
		"--ttl", "2h", "--teardown", td, "--docker-project", "proj", "--json")
	st := h.view(t, "status", "--json", string(v.ID))
	if st.MemoryLimit != 2147483648 || st.TaskMemory != 1073741824 || st.TaskCPUs != 2 || st.DockerProject != "proj" || st.Kind != model.KindReservation {
		t.Fatalf("view: %+v", st)
	}
	got, err := os.ReadFile(h.paths.TeardownScript(v.ID))
	if err != nil || string(got) != "echo down\n" {
		t.Fatalf("script %q %v", got, err)
	}
	code, out, _ := h.run("status", string(v.ID))
	if code != 0 || !strings.Contains(out, "docker:    proj") || !strings.Contains(out, "expires:") {
		t.Fatalf("status text: %s", out)
	}
}

func TestReserveUsageErrors(t *testing.T) {
	h := newHarness(t)
	h.startDaemonCfg(t, bigPool)
	td := h.script(t, "x")
	empty := h.script(t, "")
	big := h.script(t, strings.Repeat("a", 64<<10+1))
	cases := map[string][]string{
		"no memory":      {"--ttl", "1h", "--teardown", td},
		"no ttl":         {"--memory", "1G", "--teardown", td},
		"no teardown":    {"--memory", "1G", "--ttl", "1h"},
		"task-mem only":  {"--memory", "1G", "--ttl", "1h", "--teardown", td, "--task-memory", "1G"},
		"task-cpus only": {"--memory", "1G", "--ttl", "1h", "--teardown", td, "--task-cpus", "1"},
		"missing file":   {"--memory", "1G", "--ttl", "1h", "--teardown", td + ".nope"},
		"empty file":     {"--memory", "1G", "--ttl", "1h", "--teardown", empty},
		"too big":        {"--memory", "1G", "--ttl", "1h", "--teardown", big},
		"trailing args":  {"--memory", "1G", "--ttl", "1h", "--teardown", td, "extra"},
		"zero memory":    {"--memory", "0", "--ttl", "1h", "--teardown", td},
	}
	for name, a := range cases {
		code, _, _ := h.run(append([]string{"reserve"}, a...)...)
		if code != 64 {
			t.Errorf("%s: exit %d, want 64", name, code)
		}
	}
}

// Final review F5: JSON would silently replace invalid UTF-8 in the script.
func TestReserveRejectsNonUTF8Teardown(t *testing.T) {
	h := newHarness(t)
	h.startDaemonCfg(t, bigPool)
	td := h.script(t, "#!/bin/bash\necho \xff\xfe\n")
	code, _, errOut := h.run("reserve", "--memory", "1G", "--ttl", "1h", "--teardown", td)
	if code != 64 || !strings.Contains(errOut, "teardown script must be valid UTF-8") {
		t.Fatalf("exit %d stderr %q, want 64 with a UTF-8 message", code, errOut)
	}
}

func TestReserveCapturesDockerEnv(t *testing.T) {
	h := newHarness(t)
	h.startDaemonCfg(t, bigPool)
	h.vars["DOCKER_HOST"], h.vars["SECRET"] = "unix:///x.sock", "hunter2"
	v := h.view(t, "reserve", "--memory", "1G", "--ttl", "1h", "--teardown", h.script(t, "x"), "--json")
	st, err := store.Open(h.paths.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	it, err := st.Get(context.Background(), v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if it.Env["DOCKER_HOST"] != "unix:///x.sock" {
		t.Fatalf("env %v", it.Env)
	}
	if _, ok := it.Env["SECRET"]; ok {
		t.Fatal("SECRET captured")
	}
}

func TestReservePrintsQueuedBlocker(t *testing.T) {
	h := newHarness(t)
	h.startDaemon(t) // pool 1G
	td := h.script(t, "x")
	if code, out, e := h.run("reserve", "--memory", "768M", "--ttl", "1h", "--teardown", td); code != 0 || !strings.Contains(out, "GRANTED") {
		t.Fatalf("first: %d %s %s", code, out, e)
	}
	code, out, e := h.run("reserve", "--memory", "768M", "--ttl", "1h", "--teardown", td)
	f := strings.Fields(out)
	if code != 0 || len(f) != 4 || f[1] != "QUEUED" || f[2] != "position=1" || f[3] != "blocked_by=pool_memory" {
		t.Fatalf("second: %d %q %s", code, out, e)
	}
}

func TestSubmitWithin(t *testing.T) {
	h := newHarness(t)
	h.startDaemonCfg(t, bigPool)
	res := h.view(t, "reserve", "--memory", "1G", "--task-memory", "512M", "--task-cpus", "2", "--ttl", "1h",
		"--teardown", h.script(t, "x"), "--json")
	code, out, e := h.run("submit", "--within", string(res.ID), "--memory", "256M", "--timeout", "1m", "--", "true")
	if code != 0 {
		t.Fatalf("within: %d %s", code, e)
	}
	n := h.view(t, "status", "--json", strings.TrimSpace(out))
	deadline := time.Now().Add(5 * time.Second)
	for n.State != model.StateRunning && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		n = h.view(t, "status", "--json", string(n.ID))
	}
	if n.Within != res.ID || n.State != model.StateRunning {
		t.Fatalf("nested: %+v", n)
	}
	if code, _, _ := h.run("submit", "--within", string(res.ID), "--memory", "400M", "--timeout", "1m", "--", "true"); code != 5 {
		t.Fatalf("over budget: exit %d, want 5", code)
	}
}

func TestExtendReleaseTeardownCommands(t *testing.T) {
	h := newHarness(t)
	h.startDaemonCfg(t, bigPool)
	res := h.view(t, "reserve", "--memory", "1G", "--ttl", "1h", "--teardown", h.script(t, "x"), "--json")
	if code, _, e := h.run("extend", string(res.ID), "--ttl", "2h"); code != 0 || strings.Contains(e, "hint:") {
		t.Fatalf("extend: %d %s", code, e)
	}
	h.vars["CLAUDE_CODE_SESSION_ID"] = "c"
	if code, _, e := h.run("extend", string(res.ID), "--ttl", "2h"); code != 0 || !strings.Contains(e, "aq wait "+string(res.ID)+" --until expiring") {
		t.Fatalf("extend for a client-wait session: %d %s", code, e)
	}
	delete(h.vars, "CLAUDE_CODE_SESSION_ID")
	if code, _, _ := h.run("extend", string(res.ID)); code != 64 {
		t.Fatalf("extend without ttl: %d", code)
	}
	if code, _, _ := h.run("teardown", string(res.ID)); code != 64 {
		t.Fatalf("teardown without --retry: %d", code)
	}
	if code, out, e := h.run("release", string(res.ID)); code != 0 || !strings.Contains(out, "RELEASED") {
		t.Fatalf("release: %d %q %s", code, out, e)
	}
	task := h.view(t, "submit", "--memory", "64M", "--timeout", "1m", "--json", "--", "true")
	if code, _, _ := h.run("release", string(task.ID)); code != 1 {
		t.Fatalf("release task: exit %d, want 1", code)
	}
}

func TestWaitReservationExitCodes(t *testing.T) {
	cases := []struct {
		s    model.State
		want int
	}{
		{model.StateGranted, 0}, {model.StateExpiring, 0}, {model.StateCancelled, 4}, {model.StateRejected, 5},
		{model.StateReleased, 6}, {model.StateTornDown, 6}, {model.StateTeardownFailed, 6},
		{model.StateTearingDown, 6}, {model.StateTeardownUnknown, 6},
	}
	for _, c := range cases {
		if got := ExitCodeFor(daemon.ItemView{Kind: model.KindReservation, State: c.s}); got != c.want {
			t.Errorf("%s: exit %d, want %d", c.s, got, c.want)
		}
	}
}

func TestListShowsKindAndExpiry(t *testing.T) {
	h := newHarness(t)
	h.startDaemonCfg(t, bigPool)
	res := h.view(t, "reserve", "--memory", "1G", "--task-memory", "512M", "--task-cpus", "1", "--ttl", "1h",
		"--teardown", h.script(t, "x"), "--json")
	h.run("submit", "--within", string(res.ID), "--memory", "64M", "--timeout", "1m", "--", "true")
	code, out, _ := h.run("list")
	if code != 0 {
		t.Fatal(code)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	hdr := strings.Fields(lines[0])
	if hdr[2] != "KIND" || hdr[6] != "EXPIRES" {
		t.Fatalf("header %v", hdr)
	}
	var sawRes, sawNested bool
	for _, l := range lines[1:] {
		if strings.HasPrefix(l, string(res.ID)) {
			f := strings.Fields(l)
			sawRes = f[2] == "res" && strings.HasPrefix(f[6], "59m") || strings.HasPrefix(f[6], "1h")
		} else if strings.Contains(l, "(in "+string(res.ID)[:8]+")") {
			sawNested = strings.Contains(l, "task")
		}
	}
	if !sawRes || !sawNested {
		t.Fatalf("list:\n%s", out)
	}
}
