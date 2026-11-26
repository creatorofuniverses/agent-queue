package tmpscan

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestClaudeProject(t *testing.T) {
	for path, want := range map[string]string{
		"/home/u/.codex/worktrees/x_y": "-home-u--codex-worktrees-x-y",
		"/home/u/проект":               "-home-u-------",
		"/home/u/😀x":                   "-home-u---x",
	} {
		if got := ClaudeProject(path); got != want {
			t.Errorf("ClaudeProject(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestScanLongClaudeProjectsStayLive(t *testing.T) {
	o := agedFixture(t)
	fakeProcess(t, o, 101, 0, "claude", "/"+strings.Repeat("a", 249))
	prefix := "-" + strings.Repeat("a", 199)
	for _, suffix := range []string{"abc123", "def456"} {
		scratchpad(t, o, prefix+"-"+suffix, "new", time.Hour)
	}
	scratchpad(t, o, prefix+"xyz", "unrelated", 30*time.Hour)
	r := scanFixture(t, o)
	for _, suffix := range []string{"abc123", "def456"} {
		if item := itemAt(t, r, scratchPath(o, prefix+"-"+suffix, "new")); item.State != "live" {
			t.Errorf("project %s: state = %s, want live", suffix, item.State)
		}
	}
	if item := itemAt(t, r, scratchPath(o, prefix+"xyz", "unrelated")); item.State != "orphan" {
		t.Errorf("unrelated project: state = %s, want orphan", item.State)
	}
}

func TestScanInUse(t *testing.T) {
	o := fixture(t)
	venv := filepath.Join(o.Root, "venv")
	file := filepath.Join(venv, "pyvenv.cfg")
	writeFile(t, file, "x")
	dir := fakeProcess(t, o, 101, 0, "claude", "/work")
	fd := filepath.Join(dir, "fd")
	makeDir(t, fd)
	must(t, os.Symlink(file, filepath.Join(fd, "3")))

	r := scanFixture(t, o)
	item := itemAt(t, r, "venv")
	if item.Kind != "venv" || item.State != "in_use" || item.Owner != (Owner{Kind: "process", PID: 101}) ||
		!slices.Equal(item.PIDs, []int{101}) {
		t.Errorf("expected venv in use by pid 101, got %+v", item)
	}
}

func TestScanUnixSocketPaths(t *testing.T) {
	o := unixSocketFixture(t)
	old := o.Now().Add(-30 * time.Hour)
	bound := filepath.Join(o.Root, "bound", "server socket.sock")
	unlinked := filepath.Join(o.Root, "unlinked", "server socket.sock")
	listenUnix(t, bound)
	listenUnix(t, unlinked)
	// A listener can keep its bound address after its filesystem entry is removed.
	must(t, os.Remove(unlinked))
	touch(t, bound, old)
	for _, name := range []string{"bound", "unlinked", "outside", "abstract"} {
		dir := filepath.Join(o.Root, name)
		makeDir(t, dir)
		touch(t, dir, old)
	}
	writeFile(t, filepath.Join(o.ProcRoot, "net", "unix"), fmt.Sprintf(
		"Num RefCount Protocol Flags Type St Inode Path\n"+
			"0000: 00000002 00000000 00010000 0001 01 100 %s\n"+
			"0000: 00000002 00000000 00010000 0001 01 101 %s\n"+
			"0000: 00000002 00000000 00010000 0001 01 102 %s\n"+
			"0000: 00000002 00000000 00010000 0001 01 103 @%s\n",
		bound, unlinked, filepath.Join(o.Root+"-outside", "outside"), filepath.Join(o.Root, "abstract")))

	r := scanFixture(t, o)
	for name, want := range map[string]State{
		"bound": "in_use", "unlinked": "in_use", "outside": "orphan", "abstract": "orphan",
	} {
		if item := itemAt(t, r, name); item.State != want || len(item.PIDs) != 0 {
			t.Errorf("%s: state = %q, PIDs = %v, want %q and no PIDs", name, item.State, item.PIDs, want)
		}
	}
}

func TestScanUnixSocketBackstop(t *testing.T) {
	o := unixSocketFixture(t)
	old := o.Now().Add(-30 * time.Hour)
	for _, name := range []string{"top.sock", "nested/sub/server.sock"} {
		path := filepath.Join(o.Root, name)
		listenUnix(t, path)
		touch(t, path, old)
	}
	touch(t, filepath.Join(o.Root, "nested", "sub"), old)
	touch(t, filepath.Join(o.Root, "nested"), old)

	r := scanFixture(t, o)
	for _, name := range []string{"top.sock", "nested"} {
		if item := itemAt(t, r, name); item.State != "in_use" || item.Partial {
			t.Errorf("%s: state = %q, partial = %v, want complete in_use item", name, item.State, item.Partial)
		}
	}
}

func unixSocketFixture(t *testing.T) Options {
	t.Helper()
	o := agedFixture(t)
	// Use a short root so a long TMPDIR cannot exceed the Unix socket path limit.
	root, err := os.MkdirTemp("/tmp", "ts")
	must(t, err)
	t.Cleanup(func() { must(t, os.RemoveAll(root)) })
	o.Root = root
	return o
}

func listenUnix(t *testing.T, path string) {
	t.Helper()
	makeDir(t, filepath.Dir(path))
	l, err := net.Listen("unix", path)
	must(t, err)
	t.Cleanup(func() { must(t, l.Close()) })
}

func TestScanUsesSelectedProcessReferences(t *testing.T) {
	o := fixture(t)
	o.Agents = []string{"agent-test"}
	o.TaskSlices = []string{"aq.slice"}
	for _, name := range []string{"fd", "maps with spaces", "cwd", "outside-selection", "foreign-process"} {
		makeDir(t, filepath.Join(o.Root, name))
	}
	fakeProcess(t, o, 101, 0, "agent-test", "/work")
	fakeProcess(t, o, 102, 101, "fish", "/work")
	child := fakeProcess(t, o, 103, 102, "helper", "/work")
	task := fakeProcess(t, o, 104, 0, "task", "/work")
	writeFile(t, filepath.Join(task, "cgroup"), "0::/user.slice/aq.slice/aq-task.service\n")
	direct := fakeProcess(t, o, 105, 0, "fish", filepath.Join(o.Root, "cwd"))
	unselected := fakeProcess(t, o, 106, 0, "claude", "/work") // Replaced by o.Agents.
	similarRoot := fakeProcess(t, o, 107, 0, "fish", o.Root+"-elsewhere")
	foreign := fakeProcess(t, o, 108, 101, "agent-test", filepath.Join(o.Root, "foreign-process"))
	writeFile(t, filepath.Join(foreign, "status"), fmt.Sprintf("PPid:\t101\nUid:\t%d\t%d\t%d\t%d\n",
		o.UID+1, o.UID+1, o.UID+1, o.UID+1))
	for dir, target := range map[string]string{
		child: "fd", direct: "fd", unselected: "outside-selection", similarRoot: "outside-selection",
	} {
		makeDir(t, filepath.Join(dir, "fd"))
		must(t, os.Symlink(filepath.Join(o.Root, target, "open")+" (deleted)", filepath.Join(dir, "fd", "3")))
	}
	writeFile(t, filepath.Join(task, "maps"), fmt.Sprintf(
		"00001000-00002000 r--p 00000000 08:02 1     %s (deleted)\n"+
			"00002000-00003000 rw-p 00000000 00:00 0     [heap]\n",
		filepath.Join(o.Root, "maps with spaces", "code.so")))
	for pid := 109; pid <= 114; pid++ {
		fakeProcess(t, o, pid, 0, "fish", filepath.Join(o.Root, "cwd"))
	}

	r := scanFixture(t, o)
	for name, want := range map[string][]int{
		"fd": {103, 105}, "maps with spaces": {104}, "cwd": {105, 109, 110, 111, 112},
		"outside-selection": nil, "foreign-process": nil,
	} {
		item := itemAt(t, r, name)
		state := State("recent")
		if len(want) > 0 {
			state = "in_use"
		}
		if item.State != state || !slices.Equal(item.PIDs, want) {
			t.Errorf("%s: got state %s and PIDs %v, want %s / %v", name, item.State, item.PIDs, state, want)
		}
	}
}

func TestScanScratchpadInUseKeepsProjectOwner(t *testing.T) {
	o := agedFixture(t)
	scratchpad(t, o, "-p-b", "s2", 30*time.Hour)
	rel := scratchPath(o, "-p-b", "s2")
	agent := fakeProcess(t, o, 101, 0, "claude", "/p/elsewhere")
	makeDir(t, filepath.Join(agent, "fd"))
	must(t, os.Symlink(filepath.Join(o.Root, rel, "pad.md"), filepath.Join(agent, "fd", "3")))

	item := itemAt(t, scanFixture(t, o), rel)
	if item.State != "in_use" || item.Owner != (Owner{Kind: "claude_project", Project: "-p-b", Session: "s2"}) ||
		!slices.Equal(item.PIDs, []int{101}) {
		t.Errorf("expected in-use scratchpad owned by project -p-b, got %+v", item)
	}
}

func TestScanInUsePathEndingInDeleted(t *testing.T) {
	o := fixture(t)
	file := filepath.Join(o.Root, "actual file (deleted)")
	writeFile(t, file, "x")
	agent := fakeProcess(t, o, 101, 0, "codex", "/work")
	makeDir(t, filepath.Join(agent, "fd"))
	must(t, os.Symlink(file, filepath.Join(agent, "fd", "3")))

	item := itemAt(t, scanFixture(t, o), "actual file (deleted)")
	if item.State != "in_use" || !slices.Equal(item.PIDs, []int{101}) {
		t.Errorf("expected matching literal filename, got %+v", item)
	}
}

// openclaude is among the default agent names: what an OpenClaude process holds is in use.
func TestScanHeldByOpenClaude(t *testing.T) {
	o := fixture(t)
	file := filepath.Join(o.Root, "held by openclaude")
	writeFile(t, file, "x")
	agent := fakeProcess(t, o, 101, 0, "openclaude", "/work")
	makeDir(t, filepath.Join(agent, "fd"))
	must(t, os.Symlink(file, filepath.Join(agent, "fd", "3")))

	if item := itemAt(t, scanFixture(t, o), "held by openclaude"); item.State != "in_use" || !slices.Equal(item.PIDs, []int{101}) {
		t.Errorf("OpenClaude's open file was not counted as in use: %+v", item)
	}
}

// opencode is among the default agent names too: what an OpenCode process holds is in use.
func TestScanHeldByOpenCode(t *testing.T) {
	o := fixture(t)
	file := filepath.Join(o.Root, "held by opencode")
	writeFile(t, file, "x")
	agent := fakeProcess(t, o, 101, 0, "opencode", "/work")
	makeDir(t, filepath.Join(agent, "fd"))
	must(t, os.Symlink(file, filepath.Join(agent, "fd", "3")))

	if item := itemAt(t, scanFixture(t, o), "held by opencode"); item.State != "in_use" || !slices.Equal(item.PIDs, []int{101}) {
		t.Errorf("OpenCode's open file was not counted as in use: %+v", item)
	}
}

func TestScanUnreadableCwdKeepsProjectsAlive(t *testing.T) {
	o := agedFixture(t)
	scratchpad(t, o, "-p-b", "s2", 30*time.Hour)
	// A process may vanish after status was read, leaving no readable cwd or stat.
	gone := filepath.Join(o.ProcRoot, "101")
	writeFile(t, filepath.Join(gone, "status"), fmt.Sprintf("Uid:\t%d\n", o.UID))
	rel := scratchPath(o, "-p-b", "s2")
	if item := itemAt(t, scanFixture(t, o), rel); item.State != "orphan" {
		t.Fatalf("vanished process kept a project alive: %+v", item)
	}

	dir := fakeProcess(t, o, 102, 0, "fish", "")
	// readlink on a directory fails with EINVAL even when tests run as root.
	must(t, os.Mkdir(filepath.Join(dir, "cwd"), 0000))
	if item := itemAt(t, scanFixture(t, o), rel); item.State != "candidate" {
		t.Errorf("unreadable cwd should keep projects alive, got %+v", item)
	}
}

func fakeProcess(t *testing.T, o Options, pid, ppid int, name, cwd string) string {
	t.Helper()
	dir := filepath.Join(o.ProcRoot, strconv.Itoa(pid))
	writeFile(t, filepath.Join(dir, "status"), fmt.Sprintf(
		"Name:\t%s\nPPid:\t%d\nUid:\t%d\t%d\t%d\t%d\n", name, ppid, o.UID, o.UID, o.UID, o.UID))
	writeFile(t, filepath.Join(dir, "comm"), name+"\n")
	writeFile(t, filepath.Join(dir, "stat"), fmt.Sprintf("%d (%s) S %d 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 1\n", pid, name, ppid))
	if cwd != "" {
		must(t, os.Symlink(cwd, filepath.Join(dir, "cwd")))
	}
	return dir
}

func TestScanUsesConfiguredTaskSliceAndAgentArgv0(t *testing.T) {
	o := agedFixture(t)
	o.TaskSlices = []string{"aqtest.slice"}
	for _, tc := range []struct {
		name    string
		pid     int
		cgroup  string
		cmdline string
		want    State
	}{
		{name: "task", pid: 201, cgroup: "0::/user.slice/aqtest.slice/aq-task.service\n", want: "in_use"},
		{name: "similar", pid: 202, cgroup: "0::/user.slice/notaqtest.slice/aq-task.service\n", want: "orphan"},
		{name: "agent", pid: 203, cmdline: "/opt/agents/claude\x00--resume\x00old\x00", want: "in_use"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(o.Root, tc.name, "open")
			writeFile(t, path, "in use")
			pidDir := fakeProcess(t, o, tc.pid, 0, "renamed", "/work")
			writeFile(t, filepath.Join(pidDir, "cgroup"), tc.cgroup)
			writeFile(t, filepath.Join(pidDir, "cmdline"), tc.cmdline)
			makeDir(t, filepath.Join(pidDir, "fd"))
			must(t, os.Symlink(path, filepath.Join(pidDir, "fd", "3")))
			r := scanFixture(t, o)
			if item := itemAt(t, r, tc.name); item.State != tc.want {
				t.Errorf("state = %s (PIDs %v), want %s", item.State, item.PIDs, tc.want)
			}
		})
	}
}
