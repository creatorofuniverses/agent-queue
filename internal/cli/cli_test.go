package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"resource-dispatcher/internal/config"
	"resource-dispatcher/internal/daemon"
	"resource-dispatcher/internal/model"
	"resource-dispatcher/internal/platform"
	"resource-dispatcher/internal/platform/fake"
	"resource-dispatcher/internal/protocol"
)

const gib = uint64(1) << 30

type harness struct {
	paths config.Paths
	ex    *fake.Executor
	vars  map[string]string
	cwd   string
}

// newHarness prepares paths and a fake platform; the daemon is started by startDaemon.
func newHarness(t *testing.T) *harness {
	t.Helper()
	// Short root: unix socket paths are limited to ~108 bytes.
	root, err := os.MkdirTemp("", "aqc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	h := &harness{
		paths: config.Paths{StateDir: filepath.Join(root, "s"), RuntimeDir: filepath.Join(root, "r"), ConfigFile: filepath.Join(root, "none.toml")},
		ex:    fake.NewExecutor(),
		cwd:   t.TempDir(),
	}
	h.vars = map[string]string{
		"AQ_STATE_DIR": h.paths.StateDir, "AQ_RUNTIME_DIR": h.paths.RuntimeDir, "AQ_CONFIG": h.paths.ConfigFile,
	}
	t.Chdir(h.cwd)
	return h
}

func (h *harness) startDaemon(t *testing.T) {
	t.Helper()
	host := fake.NewHost()
	host.Set(platform.MemInfo{Total: 64 * gib, Available: 64 * gib})
	host.SetDisk(100 * gib)
	host.SetNumCPU(16)
	cfg := config.Default()
	cfg.PoolMemory = gib
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- daemon.Run(ctx, h.paths, cfg, h.ex, host) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("daemon did not stop")
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for protocol.Call(context.Background(), h.paths.Socket(), "ping", nil, nil) != nil {
		select {
		case err := <-done:
			t.Fatalf("daemon exited: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("daemon did not come up")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (h *harness) run(args ...string) (code int, stdout, stderr string) {
	var o, e bytes.Buffer
	code = Main(args, func(k string) string { return h.vars[k] }, &o, &e)
	return code, o.String(), e.String()
}

func (h *harness) launched(t *testing.T, n int) []platform.LaunchSpec {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if l := h.ex.Launched(); len(l) >= n {
			return l
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected %d launches, got %d", n, len(h.ex.Launched()))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSubmitParsesFlags(t *testing.T) {
	h := newHarness(t)
	h.startDaemon(t)
	code, out, errOut := h.run("submit", "--memory", "512M", "--cpus", "2", "--priority", "high", "--timeout", "30m",
		"--json", "--", "python3", "-m", "pytest", "-n", "2", "arg with space")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	var v daemon.ItemView
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatal(err)
	}
	if v.MemoryLimit != 512<<20 || v.CPUs != 2 || v.Priority != "high" || v.TimeoutSeconds != 1800 {
		t.Fatalf("view: %+v", v)
	}
	if v.Project != h.cwdResolved(t) {
		t.Errorf("project = %q, want cwd %q", v.Project, h.cwdResolved(t))
	}
	spec := h.launched(t, 1)[0]
	want := []string{"python3", "-m", "pytest", "-n", "2", "arg with space"}
	if !reflect.DeepEqual(spec.Argv, want) {
		t.Fatalf("argv = %q", spec.Argv)
	}
}

func (h *harness) cwdResolved(t *testing.T) string {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return wd
}

func TestSubmitPlainPrintsID(t *testing.T) {
	h := newHarness(t)
	h.startDaemon(t)
	code, out, errOut := h.run("submit", "--memory", "128M", "--timeout", "90s", "--key", "k1", "--", "true")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	id := strings.TrimSpace(out)
	if len(id) != 36 {
		t.Fatalf("stdout = %q", out)
	}
	// Same key, same request: same item.
	_, out2, _ := h.run("submit", "--memory", "128M", "--timeout", "90s", "--key", "k1", "--", "true")
	if strings.TrimSpace(out2) != id {
		t.Errorf("idempotent submit returned %q, want %q", out2, id)
	}
}

func TestSubmitCapturesOnlyAllowedEnv(t *testing.T) {
	h := newHarness(t)
	h.startDaemon(t)
	h.vars["PATH"], h.vars["HOME"], h.vars["LANG"] = "/bin", "/home/x", "C"
	h.vars["LC_ALL"], h.vars["VIRTUAL_ENV"], h.vars["SECRET"] = "C.UTF-8", "/venv", "hunter2"
	h.vars["LC_FOO"] = "x" // not a real locale category
	code, _, errOut := h.run("submit", "--memory", "128M", "--timeout", "1m", "--env", "FOO=a=b", "--env", "BAR=", "--", "true")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	got := map[string]string{}
	for k, v := range h.launched(t, 1)[0].Env {
		if k != "AQ_JOB_ID" && k != "AQ_CPUS" { // added by the scheduler
			got[k] = v
		}
	}
	want := map[string]string{"PATH": "/bin", "HOME": "/home/x", "LANG": "C", "LC_ALL": "C.UTF-8", "VIRTUAL_ENV": "/venv", "FOO": "a=b", "BAR": ""}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("env = %v, want %v", got, want)
	}
}

func TestWaitExitCodes(t *testing.T) {
	cases := []struct {
		v    daemon.ItemView
		want int
	}{
		{daemon.ItemView{State: model.StateSucceeded}, 0},
		{daemon.ItemView{State: model.StateFailed, Reason: model.ReasonCommandFailed}, 1},
		{daemon.ItemView{State: model.StateFailed, Reason: model.ReasonOOM}, 2},
		{daemon.ItemView{State: model.StateFailed, Reason: model.ReasonOOMGroup}, 2},
		{daemon.ItemView{State: model.StateFailed, Reason: model.ReasonOOMHostGuard}, 2},
		{daemon.ItemView{State: model.StateFailed, Reason: model.ReasonTimeout}, 3},
		{daemon.ItemView{State: model.StateCancelled, Reason: model.ReasonCancelled}, 4},
		{daemon.ItemView{State: model.StateFailed, Reason: model.ReasonLaunchFailed}, 5},
		{daemon.ItemView{State: model.StateFailed, Reason: model.ReasonLost}, 5},
		{daemon.ItemView{State: model.StateFailed, Reason: model.ReasonStorageFull}, 5},
		{daemon.ItemView{State: model.StateRejected}, 5},
	}
	for _, c := range cases {
		if got := ExitCodeFor(c.v); got != c.want {
			t.Errorf("%s/%s: exit %d, want %d", c.v.State, c.v.Reason, got, c.want)
		}
	}
}

func TestWaitEndToEnd(t *testing.T) {
	h := newHarness(t)
	h.startDaemon(t)
	_, out, _ := h.run("submit", "--memory", "128M", "--timeout", "1m", "--", "false")
	id := strings.TrimSpace(out)
	spec := h.launched(t, 1)[0]

	var mu sync.Mutex
	var code int
	var wout, werr string
	done := make(chan struct{})
	go func() {
		c, o, e := h.run("wait", id)
		mu.Lock()
		code, wout, werr = c, o, e
		mu.Unlock()
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	h.ex.Finish(spec.ID, platform.ExitInfo{ServiceResult: "exit-code", ExitCode: "exited", ExitStatus: "1", EndedAt: time.Now()})
	h.ex.SetAlive(spec.ID, false)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("wait did not return")
	}
	mu.Lock()
	defer mu.Unlock()
	if code != 1 || !strings.Contains(wout, "COMMAND_FAILED") || !strings.Contains(wout, "exit=1") {
		t.Fatalf("wait: code=%d out=%q err=%q", code, wout, werr)
	}

	// status, result, list, logs against the finished item.
	if c, o, e := h.run("status", id); c != 0 || !strings.Contains(o, "COMMAND_FAILED") {
		t.Errorf("status: %d %q %q", c, o, e)
	}
	if c, o, _ := h.run("result", id, "--json"); c != 0 || !strings.Contains(o, "stdout_path") {
		t.Errorf("result: %d %q", c, o)
	}
	if c, o, _ := h.run("list", "--all"); c != 0 || !strings.Contains(o, id) {
		t.Errorf("list: %d %q", c, o)
	}
	if c, _, e := h.run("logs", id, "--tail", "5"); c != 0 {
		t.Errorf("logs: %d %q", c, e)
	}
	if c, _, _ := h.run("logs", id, "--follow"); c != 0 {
		t.Errorf("logs --follow on finished item: %d", c)
	}
}

func TestCancel(t *testing.T) {
	h := newHarness(t)
	h.startDaemon(t)
	// Pool is 1 GiB: the second item queues behind the first.
	h.run("submit", "--memory", "1G", "--timeout", "1m", "--", "sleep", "9")
	_, out, _ := h.run("submit", "--memory", "512M", "--timeout", "1m", "--", "true")
	id := strings.TrimSpace(out)
	if c, o, e := h.run("cancel", id, "--reason", "test"); c != 0 || !strings.Contains(o, "CANCELLED") {
		t.Fatalf("cancel: %d %q %q", c, o, e)
	}
	if c, _, _ := h.run("wait", id); c != 4 {
		t.Errorf("wait cancelled: exit %d, want 4", c)
	}
}

func TestDaemonUnavailableExit69(t *testing.T) {
	h := newHarness(t)
	for _, args := range [][]string{
		{"status", "x"}, {"list"}, {"wait", "x"},
		{"submit", "--memory", "1G", "--timeout", "1m", "--", "true"},
	} {
		if code, _, errOut := h.run(args...); code != 69 || !strings.Contains(errOut, "daemon") {
			t.Errorf("%v: exit %d, stderr %q", args, code, errOut)
		}
	}
}

func TestUsageErrorExit64(t *testing.T) {
	h := newHarness(t)
	cases := [][]string{
		{},
		{"bogus"},
		{"submit"},
		{"submit", "--memory", "1G", "--timeout", "1m"},                 // no --
		{"submit", "--memory", "1G", "--", "true"},                      // no --timeout
		{"submit", "--timeout", "1m", "--", "true"},                     // no --memory
		{"submit", "--memory", "1G", "--timeout", "soon", "--", "true"}, // bad duration
		{"submit", "--memory", "1G", "--timeout", "1m", "--priority", "x", "--", "true"},
		{"submit", "--memory", "1G", "--timeout", "1m", "--env", "NOEQ", "--", "true"},
		{"status"},
		{"logs", "x", "--stream", "bad"},
	}
	for _, args := range cases {
		if code, _, _ := h.run(args...); code != 64 {
			t.Errorf("%v: exit %d, want 64", args, code)
		}
	}
}

func TestVersion(t *testing.T) {
	h := newHarness(t)
	if code, out, _ := h.run("version"); code != 0 || !strings.HasPrefix(out, "aq ") {
		t.Errorf("version: %d %q", code, out)
	}
}

// fakeSystem is a healthy machine; tests tweak it.
type fakeSystem struct {
	mu        sync.Mutex
	files     map[string]string
	exists    map[string]bool
	calls     []string
	systemd   string
	probeOut  string
	probeErr  error
	diskFree  uint64
	pingErr   error
	probeUnit string
}

func newFakeSystem() *fakeSystem {
	mgr := "/sys/fs/cgroup/user.slice/user-1000.slice/user@1000.service"
	return &fakeSystem{
		files: map[string]string{
			"/sys/fs/cgroup/cgroup.controllers": "cpuset cpu io memory pids",
			mgr + "/cgroup.controllers":         "cpu memory pids",
		},
		exists: map[string]bool{
			mgr + "/aq.slice": true,
			"/run/user/1000/systemd/user/aq-.service.d/99-aq-kill.conf": true,
			"/": true,
		},
		systemd:  "systemd 256 (256.7-1.fc41)\n+PAM",
		probeOut: "67108864\n",
		diskFree: 100 * gib,
	}
}

func (f *fakeSystem) system() doctorSystem {
	return doctorSystem{
		UID: 1000,
		ReadFile: func(p string) ([]byte, error) {
			if s, ok := f.files[p]; ok {
				return []byte(s), nil
			}
			return nil, os.ErrNotExist
		},
		Exists: func(p string) bool { return f.exists[p] },
		Run: func(_ context.Context, name string, args ...string) (string, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.calls = append(f.calls, name+" "+strings.Join(args, " "))
			switch {
			case name == "systemctl" && len(args) == 1 && args[0] == "--version":
				return f.systemd, nil
			case name == "systemd-run":
				return f.probeOut, f.probeErr
			}
			return "", nil
		},
		DiskFree:  func(string) (uint64, error) { return f.diskFree, nil },
		MemTotal:  func() (uint64, error) { return 64 * gib, nil },
		NumCPU:    func() int { return 16 },
		RandomHex: func() string { return "abcd1234" },
		Ping:      func(context.Context, string) error { return f.pingErr },
	}
}

func (f *fakeSystem) install(t *testing.T) {
	old := newDoctorSystem
	newDoctorSystem = f.system
	t.Cleanup(func() { newDoctorSystem = old })
}

type doctorOut struct {
	Checks []Check `json:"checks"`
}

func runDoctorJSON(t *testing.T, h *harness) (int, doctorOut) {
	t.Helper()
	h.vars["XDG_RUNTIME_DIR"] = "/run/user/1000"
	code, out, errOut := h.run("doctor", "--json")
	var d doctorOut
	if err := json.Unmarshal([]byte(out), &d); err != nil {
		t.Fatalf("doctor output %q (stderr %q): %v", out, errOut, err)
	}
	return code, d
}

func TestDoctorJSONShape(t *testing.T) {
	h := newHarness(t)
	f := newFakeSystem()
	f.install(t)
	code, d := runDoctorJSON(t, h)
	if code != 0 {
		t.Fatalf("exit %d: %+v", code, d)
	}
	var names []string
	for _, c := range d.Checks {
		names = append(names, c.Name)
		if !c.OK || c.Code != "" {
			t.Errorf("check %+v should be ok", c)
		}
	}
	want := []string{"cgroup_v2", "controllers", "slice", "probe", "config", "disk", "daemon", "systemd", "kill_dropin"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("names = %v", names)
	}
	// Probe unit uses the prefix and is stopped afterwards.
	joined := strings.Join(f.calls, "\n")
	if !strings.Contains(joined, "--unit=aq-probe-abcd1234") || !strings.Contains(joined, "MemoryMax=64M") ||
		!strings.Contains(joined, "stop aq-probe-abcd1234.service") {
		t.Errorf("calls:\n%s", joined)
	}
}

func TestDoctorFailures(t *testing.T) {
	tests := []struct {
		name string
		tune func(*fakeSystem)
		code string
	}{
		{"cgroup", func(f *fakeSystem) { delete(f.files, "/sys/fs/cgroup/cgroup.controllers") }, "CGROUP_V2_MISSING"},
		{"controller", func(f *fakeSystem) {
			f.files["/sys/fs/cgroup/user.slice/user-1000.slice/user@1000.service/cgroup.controllers"] = "cpu pids"
		}, "CONTROLLER_MISSING"},
		{"slice", func(f *fakeSystem) {
			delete(f.exists, "/sys/fs/cgroup/user.slice/user-1000.slice/user@1000.service/aq.slice")
		}, "SLICE_MISSING"},
		{"probe", func(f *fakeSystem) { f.probeOut = "max\n" }, "PROBE_FAILED"},
		{"probe-error", func(f *fakeSystem) { f.probeErr = os.ErrPermission }, "PROBE_FAILED"},
		{"disk", func(f *fakeSystem) { f.diskFree = 1 << 20 }, "DISK_LOW"},
		{"daemon", func(f *fakeSystem) { f.pingErr = protocol.ErrDaemonUnavailable }, "DAEMON_UNAVAILABLE"},
		{"systemd", func(f *fakeSystem) { f.systemd = "systemd 253 (253.1)" }, "SYSTEMD_TOO_OLD"},
		{"dropin", func(f *fakeSystem) {
			delete(f.exists, "/run/user/1000/systemd/user/aq-.service.d/99-aq-kill.conf")
		}, "KILL_DROPIN_MISSING"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			f := newFakeSystem()
			tc.tune(f)
			f.install(t)
			code, d := runDoctorJSON(t, h)
			if code != 1 {
				t.Errorf("exit %d, want 1", code)
			}
			var codes []string
			for _, c := range d.Checks {
				if !c.OK {
					codes = append(codes, c.Code)
				}
			}
			if !reflect.DeepEqual(codes, []string{tc.code}) {
				t.Errorf("failing codes = %v, want [%s]", codes, tc.code)
			}
			if tc.name == "probe" || tc.name == "probe-error" {
				if !strings.Contains(strings.Join(f.calls, "\n"), "stop aq-probe-abcd1234.service") {
					t.Error("probe unit not cleaned up")
				}
			}
		})
	}
}

func TestDoctorConfigInvalid(t *testing.T) {
	h := newHarness(t)
	if err := os.WriteFile(h.paths.ConfigFile, []byte(`pool_memory = "1T"`), 0o600); err != nil {
		t.Fatal(err)
	}
	newFakeSystem().install(t)
	code, d := runDoctorJSON(t, h)
	if code != 1 {
		t.Fatalf("exit %d", code)
	}
	for _, c := range d.Checks {
		if c.Name == "config" && (c.OK || c.Code != "CONFIG_INVALID") {
			t.Errorf("config check: %+v", c)
		}
	}
}

func TestSubmitEnvErrorDoesNotLeakValue(t *testing.T) {
	h := newHarness(t)
	for _, kv := range []string{"=SENTINELSECRET", "1BAD=SENTINELSECRET", "BAD KEY=SENTINELSECRET", "SENTINELSECRET"} {
		code, out, errOut := h.run("submit", "--memory", "1G", "--timeout", "1m", "--env", kv, "--", "true")
		if code != 64 {
			t.Errorf("%q: exit %d, want 64", kv, code)
		}
		if strings.Contains(out+errOut, "SENTINELSECRET") {
			t.Errorf("%q: output leaks the value: %q %q", kv, out, errOut)
		}
	}
}

func TestSubmitArgvEdgeCases(t *testing.T) {
	cases := [][]string{
		{"-x", "--flag"},
		{"sh", "-c", ""},
		{"cmd", "--", "arg"},
	}
	for _, argv := range cases {
		h := newHarness(t)
		h.startDaemon(t)
		args := append([]string{"submit", "--memory", "128M", "--timeout", "1m", "--"}, argv...)
		if code, _, errOut := h.run(args...); code != 0 {
			t.Fatalf("%q: exit %d: %s", argv, code, errOut)
		}
		if got := h.launched(t, 1)[0].Argv; !reflect.DeepEqual(got, argv) {
			t.Errorf("argv = %q, want %q", got, argv)
		}
	}
}

func TestDoctorSystemdUndeterminable(t *testing.T) {
	h := newHarness(t)
	f := newFakeSystem()
	f.systemd = "garbage"
	f.install(t)
	code, d := runDoctorJSON(t, h)
	if code != 1 {
		t.Errorf("exit %d", code)
	}
	for _, c := range d.Checks {
		if c.Name != "systemd" {
			continue
		}
		if c.OK || !strings.Contains(c.Detail, "cannot determine systemd version") || strings.Contains(c.Detail, "version 0") {
			t.Errorf("systemd check: %+v", c)
		}
	}
}

func TestDoctorSystemdOKDetail(t *testing.T) {
	h := newHarness(t)
	newFakeSystem().install(t)
	_, d := runDoctorJSON(t, h)
	for _, c := range d.Checks {
		if c.Name == "systemd" && c.Detail != "version 256" {
			t.Errorf("detail = %q", c.Detail)
		}
	}
}
