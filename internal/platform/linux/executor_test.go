package linux

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/creatorofuniverses/agent-queue/internal/model"
	"github.com/creatorofuniverses/agent-queue/internal/platform"
)

func testSpec() platform.LaunchSpec {
	return platform.LaunchSpec{
		ID:         "abc",
		Argv:       []string{"python3", "-m", "pytest", "a b"},
		Cwd:        "/work",
		Env:        map[string]string{"PATH": "/usr/bin", "AQ_JOB_ID": "abc"},
		Limit:      4 << 30,
		MemoryHigh: 3865470566,
		CPUs:       2,
		Timeout:    30 * time.Minute,
		KillGrace:  10 * time.Second,
		JobDir:     "/state/jobs/abc",
		TmpDir:     "/state/tmp/abc",
		MaxStdout:  100,
		MaxStderr:  200,
	}
}

func hasPair(args []string, a, b string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == a && args[i+1] == b {
			return true
		}
	}
	return false
}

func TestSystemdRunArgs(t *testing.T) {
	cfg := ExecutorConfig{SliceName: "aq.slice", UnitPrefix: "aq-", AqBinary: "/bin/aq"}
	args := SystemdRunArgs(cfg, testSpec())

	for _, f := range []string{"--user", "--quiet", "--collect", "--expand-environment=no"} {
		if !slices.Contains(args, f) {
			t.Errorf("missing %s in %q", f, args)
		}
	}
	for _, p := range [][2]string{
		{"--unit", "aq-abc"}, {"--slice", "aq.slice"},
		{"--setenv", "PATH=/usr/bin"}, {"--setenv", "AQ_JOB_ID=abc"},
		{"-p", "MemoryMax=4294967296"}, {"-p", "MemoryHigh=3865470566"}, {"-p", "MemorySwapMax=0"},
		{"-p", "CPUQuota=200%"}, {"-p", "KillMode=control-group"}, {"-p", "OOMPolicy=kill"},
		{"-p", "OOMScoreAdjust=900"}, {"-p", "RuntimeMaxSec=1800"}, {"-p", "TimeoutStopSec=10"}, {"-p", "TimeoutStopFailureMode=kill"},
		{"-p", "ExecStopPost=/bin/aq _record-result /state/jobs/abc"},
	} {
		if !hasPair(args, p[0], p[1]) {
			t.Errorf("missing %s %s in %q", p[0], p[1], args)
		}
	}
	// The unit (and so ExecStopPost) must not depend on the job's cwd still existing.
	if slices.Contains(args, "--working-directory") || slices.ContainsFunc(args, func(a string) bool {
		return strings.HasPrefix(a, "WorkingDirectory=")
	}) {
		t.Errorf("unit gets the job cwd as its working directory: %q", args)
	}
	for _, a := range args {
		if strings.HasPrefix(a, "Memory") && strings.Contains(a, "%") {
			t.Errorf("percent in %s", a)
		}
	}
	i := slices.Index(args, "--")
	if i < 0 {
		t.Fatalf("no -- in %q", args)
	}
	want := []string{"/bin/aq", "_exec", "--job-dir", "/state/jobs/abc", "--tmp-dir", "/state/tmp/abc", "--cwd", "/work",
		"--max-stdout", "100", "--max-stderr", "200", "--", "python3", "-m", "pytest", "a b"}
	if !slices.Equal(args[i+1:], want) {
		t.Fatalf("tail %q", args[i+1:])
	}
}

func TestSystemdRunArgsExtraAndRounding(t *testing.T) {
	cfg := ExecutorConfig{SliceName: "aq.slice", UnitPrefix: "aq-", AqBinary: "/bin/aq", ExtraProperties: []string{"MemoryMax=garbage"}}
	s := testSpec()
	s.Timeout = 1500 * time.Millisecond
	s.KillGrace = 0
	args := SystemdRunArgs(cfg, s)
	if !hasPair(args, "-p", "RuntimeMaxSec=2") || !hasPair(args, "-p", "TimeoutStopSec=1") {
		t.Fatalf("rounding: %q", args)
	}
	// extra properties come after the standard ones so they override them
	if !hasPair(args, "-p", "MemoryMax=garbage") || slices.Index(args, "MemoryMax=garbage") < slices.Index(args, "MemoryMax=4294967296") {
		t.Fatalf("extra: %q", args)
	}
}

func TestExecStopPostQuoting(t *testing.T) {
	cfg := ExecutorConfig{SliceName: "aq.slice", UnitPrefix: "aq-", AqBinary: "/opt/my bin/aq"}
	s := testSpec()
	s.JobDir = `/st/a$b"c`
	args := SystemdRunArgs(cfg, s)
	if !hasPair(args, "-p", `ExecStopPost="/opt/my bin/aq" _record-result "/st/a$$b\"c"`) {
		t.Fatalf("quoting: %q", args)
	}
}

func TestUserManagerCgroup(t *testing.T) {
	got := userManagerCgroup("0::/user.slice/user-1000.slice/user@1000.service/app.slice/aq.service\n", 1000)
	if got != "user.slice/user-1000.slice/user@1000.service" {
		t.Fatalf("got %q", got)
	}
	got = userManagerCgroup("0::/user.slice/user-1000.slice/session-2.scope\n", 1000)
	if got != "user.slice/user-1000.slice/user@1000.service" {
		t.Fatalf("fallback got %q", got)
	}
}

func TestSliceCgroupPath(t *testing.T) {
	for in, want := range map[string]string{
		"aq.slice":      "aq.slice",
		"aq-test.slice": "aq.slice/aq-test.slice",
		"a-b-c.slice":   "a.slice/a-b.slice/a-b-c.slice",
	} {
		if got := sliceCgroupPath(in); got != want {
			t.Errorf("%s: got %q want %q", in, got, want)
		}
	}
}

// fakeCgroup builds a cgroupfs-like tree for the cgroup-reading methods.
func fakeCgroup(t *testing.T) (*Executor, string) {
	t.Helper()
	root := t.TempDir()
	e := newExecutor(ExecutorConfig{SliceName: "aqtest.slice", TeardownSlice: "aqtesttd.slice", UnitPrefix: "aqtest-", AqBinary: "/bin/aq", CgroupRoot: root},
		"user.slice/user-1000.slice/user@1000.service")
	slice := e.sliceDir()
	write := func(rel, s string) {
		p := filepath.Join(slice, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("memory.current", "1234\n")
	write("memory.events", "low 0\nhigh 0\nmax 4\noom 2\noom_kill 3\noom_group_kill 0\n")
	write("memory.max", "max\n")
	write("memory.high", "max\n")
	write("aqtest-one.service/cgroup.procs", "42\n")
	write("aqtest-one.service/cgroup.events", "populated 1\nfrozen 0\n")
	write("aqtest-one.service/memory.current", "777\n")
	write("aqtest-two.service/cgroup.procs", "")
	write("aqtest-two.service/cgroup.events", "populated 0\nfrozen 0\n")
	// processes only in a sub-cgroup the task created
	write("aqtest-nested.service/cgroup.procs", "")
	write("aqtest-nested.service/cgroup.events", "populated 1\nfrozen 0\n")
	write("aqtest-nested.service/inner/cgroup.procs", "43\n")
	write("aqtest-nested.service/inner/cgroup.events", "populated 1\nfrozen 0\n")
	write("other.service/cgroup.procs", "1\n")
	return e, slice
}

func TestCgroupReads(t *testing.T) {
	e, slice := fakeCgroup(t)
	if want := "user.slice/user-1000.slice/user@1000.service/aqtest.slice"; !strings.HasSuffix(slice, want) {
		t.Fatalf("slice dir %s", slice)
	}
	if v, err := e.SliceCurrent(); err != nil || v != 1234 {
		t.Fatalf("SliceCurrent %d %v", v, err)
	}
	if v, err := e.SliceOOMKills(); err != nil || v != 3 {
		t.Fatalf("SliceOOMKills %d %v", v, err)
	}
	if v, err := e.MemoryCurrent("one"); err != nil || v != 777 {
		t.Fatalf("MemoryCurrent %d %v", v, err)
	}
	if v, err := e.MemoryCurrent("missing"); err != nil || v != 0 {
		t.Fatalf("MemoryCurrent missing %d %v", v, err)
	}
	for id, want := range map[model.ItemID]bool{"one": true, "two": false, "nested": true, "missing": false} {
		if got, err := e.Alive(t.Context(), id); err != nil || got != want {
			t.Errorf("Alive(%s) = %v %v", id, got, err)
		}
	}
	ids, err := e.ListManaged(t.Context())
	slices.Sort(ids)
	if err != nil || !slices.Equal(ids, []model.ItemID{"nested", "one", "two"}) {
		t.Fatalf("ListManaged %v %v", ids, err)
	}
	if err := e.SetSliceCeiling(1000, 900); err != nil {
		t.Fatal(err)
	}
	for f, want := range map[string]string{"memory.max": "1000", "memory.high": "900"} {
		b, _ := os.ReadFile(filepath.Join(slice, f))
		if strings.TrimSpace(string(b)) != want {
			t.Errorf("%s = %q", f, b)
		}
	}
}

func TestListManagedNoSlice(t *testing.T) {
	e := newExecutor(ExecutorConfig{SliceName: "aq.slice", UnitPrefix: "aq-", CgroupRoot: t.TempDir()}, "u")
	if ids, err := e.ListManaged(t.Context()); err != nil || len(ids) != 0 {
		t.Fatalf("%v %v", ids, err)
	}
}

func TestCapabilitiesAllTrue(t *testing.T) {
	e := newExecutor(ExecutorConfig{}, "u")
	if c := e.Capabilities(); !c.HardMemoryLimit || !c.TreeKill || !c.OOMAttribution || !c.SurvivesDaemonRestart {
		t.Fatalf("%+v", c)
	}
}

func TestSystemdRunArgsClampsMemoryHigh(t *testing.T) {
	cfg := ExecutorConfig{SliceName: "aq.slice", UnitPrefix: "aq-", AqBinary: "/bin/aq"}
	for _, high := range []uint64{0, 5 << 30} {
		s := testSpec()
		s.MemoryHigh = high
		if args := SystemdRunArgs(cfg, s); !hasPair(args, "-p", "MemoryHigh=4294967296") {
			t.Errorf("high %d: %q", high, args)
		}
	}
}

// fakeRunner records commands instead of running them.
type fakeRunner struct {
	calls   [][]string
	ctxErrs []error
	fail    map[string]error // by command name
}

func (f *fakeRunner) run(ctx context.Context, name string, args ...string) error {
	f.calls = append(f.calls, append([]string{name}, args...))
	f.ctxErrs = append(f.ctxErrs, ctx.Err())
	return f.fail[name]
}

func (f *fakeRunner) ran(name, arg string) bool {
	for _, c := range f.calls {
		if c[0] == name && slices.Contains(c, arg) {
			return true
		}
	}
	return false
}

func launchSpecIn(t *testing.T, id model.ItemID) platform.LaunchSpec {
	s := testSpec()
	s.ID = id
	s.JobDir = filepath.Join(t.TempDir(), "job")
	return s
}

func TestLaunchRejectsBadEnvWithoutLeakingValue(t *testing.T) {
	const sentinel = "SUPERSECRETVALUE"
	for _, env := range []map[string]string{
		{"BAD-NAME": sentinel},
		{"1X": sentinel},
		{"OK": sentinel + "\xff"},
		{"OK": sentinel + "\x00"},
	} {
		e, _ := fakeCgroup(t)
		f := &fakeRunner{}
		e.runCmd = f.run
		s := launchSpecIn(t, "one")
		s.Env = env
		_, err := e.Launch(t.Context(), s)
		if err == nil || strings.Contains(err.Error(), sentinel) {
			t.Errorf("env %q: err %v", env, err)
		}
		if f.ran("systemd-run", "--user") {
			t.Errorf("env %q: systemd-run was called", env)
		}
	}
}

func TestRedactEnv(t *testing.T) {
	stderr := "Cannot assign environment variable BAD=SUPERSECRETVALUE: Invalid argument\nother SECRET2VAL here"
	got := redactEnv(stderr, map[string]string{"X": "SECRET2VAL", "Y": "ab"})
	if strings.Contains(got, "SUPERSECRETVALUE") || strings.Contains(got, "SECRET2VAL") || !strings.Contains(got, "Invalid argument") {
		t.Fatalf("got %q", got)
	}
}

func TestLaunchVerifyFailStopsUnit(t *testing.T) {
	e, slice := fakeCgroup(t)
	// unit cgroup exists but memory.max says "max": the limit is not in place
	if err := os.WriteFile(filepath.Join(slice, "aqtest-one.service", "memory.max"), []byte("max\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := &fakeRunner{}
	e.runCmd = f.run
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := e.Launch(ctx, launchSpecIn(t, "one"))
	if err == nil || !strings.Contains(err.Error(), "memory.max") {
		t.Fatalf("err %v", err)
	}
	if !f.ran("systemctl", "aqtest-one.service") || !f.ran("systemctl", "stop") {
		t.Fatalf("unit not stopped: %q", f.calls)
	}
	for i, ce := range f.ctxErrs {
		if ce != nil {
			t.Errorf("call %q ran with done ctx: %v", f.calls[i], ce)
		}
	}
}

func TestLaunchSystemdRunErrorStopsUnit(t *testing.T) {
	e, _ := fakeCgroup(t)
	f := &fakeRunner{fail: map[string]error{"systemd-run": errors.New("signal: killed")}}
	e.runCmd = f.run
	if _, err := e.Launch(t.Context(), launchSpecIn(t, "one")); err == nil {
		t.Fatal("want error")
	}
	if !f.ran("systemctl", "aqtest-one.service") {
		t.Fatalf("unit not stopped: %q", f.calls)
	}
}

func TestEnsureSliceWritesKillDropIn(t *testing.T) {
	rt := t.TempDir()
	e := newExecutor(ExecutorConfig{SliceName: "aqtest.slice", UnitPrefix: "aqtest-", RuntimeDir: rt}, "u")
	f := &fakeRunner{}
	e.runCmd = f.run
	if err := e.EnsureSlice(t.Context(), 1<<30); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(rt, "systemd", "user", "aqtest-.service.d")
	path := filepath.Join(dir, "99-aq-kill.conf")
	b, err := os.ReadFile(path)
	if err != nil || string(b) != "[Service]\nTimeoutStopFailureMode=kill\n" {
		t.Fatalf("drop-in %q %v", b, err)
	}
	if st, _ := os.Stat(dir); st.Mode().Perm() != 0o700 {
		t.Errorf("dir mode %v", st.Mode().Perm())
	}
	st1, _ := os.Stat(path)
	if st1.Mode().Perm() != 0o644 {
		t.Errorf("file mode %v", st1.Mode().Perm())
	}
	if !f.ran("systemctl", "start") || !f.ran("systemctl", "set-property") {
		t.Fatalf("calls %q", f.calls)
	}

	// unchanged content is not rewritten; different content is replaced
	if err := e.EnsureSlice(t.Context(), 1<<30); err != nil {
		t.Fatal(err)
	}
	if st2, _ := os.Stat(path); !os.SameFile(st1, st2) || !st2.ModTime().Equal(st1.ModTime()) {
		t.Error("identical drop-in was rewritten")
	}
	if err := os.WriteFile(path, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := e.EnsureSlice(t.Context(), 1<<30); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != "[Service]\nTimeoutStopFailureMode=kill\n" {
		t.Fatalf("stale drop-in kept: %q", b)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 1 {
		t.Fatalf("leftover temp files: %v", ents)
	}
}

func TestRuntimeDirDefaultsToXDG(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/4242")
	if e := newExecutor(ExecutorConfig{}, "u"); e.cfg.RuntimeDir != "/run/user/4242" {
		t.Fatalf("got %q", e.cfg.RuntimeDir)
	}
}

func TestSystemdRunArgsTeardownSlice(t *testing.T) {
	cfg := ExecutorConfig{SliceName: "aqtest.slice", TeardownSlice: "aqtesttd.slice", UnitPrefix: "aqtest-", AqBinary: "/bin/aq"}
	id := model.NewID()
	s := testSpec()
	s.ID = model.TeardownUnitID(id)
	args := SystemdRunArgs(cfg, s)
	if !hasPair(args, "--slice", "aqtesttd.slice") || !hasPair(args, "--unit", "aqtest-teardown-"+string(id)) {
		t.Fatalf("teardown unit: %q", args)
	}
	// Final review F4: the kernel must not prefer the script that frees the stand.
	if !hasPair(args, "-p", "OOMScoreAdjust=0") || hasPair(args, "-p", "OOMScoreAdjust=900") {
		t.Fatalf("teardown unit OOMScoreAdjust: %q", args)
	}
	s.ID = id
	args = SystemdRunArgs(cfg, s)
	if !hasPair(args, "--slice", "aqtest.slice") || !hasPair(args, "--unit", "aqtest-"+string(id)) {
		t.Fatalf("task unit: %q", args)
	}
	if !hasPair(args, "-p", "OOMScoreAdjust=900") || hasPair(args, "-p", "OOMScoreAdjust=0") {
		t.Fatalf("task unit OOMScoreAdjust: %q", args)
	}
}

func TestNewExecutorNeedsSeparateTeardownSlice(t *testing.T) {
	for _, td := range []string{"", "aqtest.slice"} {
		_, err := NewExecutor(ExecutorConfig{SliceName: "aqtest.slice", TeardownSlice: td, UnitPrefix: "aqtest-", AqBinary: "/bin/aq"})
		if err == nil || !strings.Contains(err.Error(), "TeardownSlice") {
			t.Errorf("TeardownSlice %q: err %v", td, err)
		}
	}
}

// Teardown units live in their own slice: liveness, memory and listing read it there,
// while the guard's slice reads stay on the task slice.
func TestTeardownCgroupReads(t *testing.T) {
	e, slice := fakeCgroup(t)
	td := filepath.Join(filepath.Dir(slice), "aqtesttd.slice")
	write := func(rel, s string) {
		p := filepath.Join(td, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("memory.current", "999999\n")
	write("aqtest-teardown-r1.service/cgroup.events", "populated 1\nfrozen 0\n")
	write("aqtest-teardown-r1.service/memory.current", "555\n")
	// a task-shaped id in the task slice must not be looked up in the teardown slice
	write("aqtest-one.service/cgroup.events", "populated 0\nfrozen 0\n")

	if v, err := e.MemoryCurrent("teardown-r1"); err != nil || v != 555 {
		t.Fatalf("MemoryCurrent %d %v", v, err)
	}
	if ok, err := e.Alive(t.Context(), "teardown-r1"); err != nil || !ok {
		t.Fatalf("Alive teardown %v %v", ok, err)
	}
	if ok, err := e.Alive(t.Context(), "one"); err != nil || !ok {
		t.Fatalf("Alive task %v %v (read from the wrong slice?)", ok, err)
	}
	ids, err := e.ListManaged(t.Context())
	slices.Sort(ids)
	if err != nil || !slices.Equal(ids, []model.ItemID{"nested", "one", "teardown-r1", "two"}) {
		t.Fatalf("ListManaged %v %v", ids, err)
	}
	if v, err := e.SliceCurrent(); err != nil || v != 1234 {
		t.Fatalf("SliceCurrent %d %v: must read the task slice", v, err)
	}
}

func TestEnsureTeardownSlice(t *testing.T) {
	rt := t.TempDir()
	e := newExecutor(ExecutorConfig{SliceName: "aqtest.slice", TeardownSlice: "aqtesttd.slice", UnitPrefix: "aqtest-", RuntimeDir: rt}, "u")
	f := &fakeRunner{}
	e.runCmd = f.run
	if err := e.EnsureTeardownSlice(t.Context(), 1<<30); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"systemctl", "--user", "start", "aqtesttd.slice"},
		{"systemctl", "--user", "set-property", "--runtime", "aqtesttd.slice", "MemoryMax=1073741824", "MemorySwapMax=0"},
	}
	if len(f.calls) != len(want) {
		t.Fatalf("calls %q", f.calls)
	}
	for i := range want {
		if !slices.Equal(f.calls[i], want[i]) {
			t.Fatalf("call %d %q, want %q", i, f.calls[i], want[i])
		}
	}
}
