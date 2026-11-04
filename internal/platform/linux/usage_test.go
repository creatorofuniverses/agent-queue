package linux

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func usageFile(t *testing.T, root, path, body string) {
	t.Helper()
	p := filepath.Join(root, path)
	if e := os.MkdirAll(filepath.Dir(p), 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(p, []byte(body), 0600); e != nil {
		t.Fatal(e)
	}
}
func TestUsageMeter(t *testing.T) {
	root := t.TempDir()
	manager := "user.slice/user-1000.slice/user@1000.service"
	usageFile(t, root, "proc/self/cgroup", "0::/"+manager+"/app.slice/aq.service\n")
	proc := func(pid int, comm, argv, cg string, pss int) {
		base := fmt.Sprintf("proc/%d/", pid)
		usageFile(t, root, base+"comm", comm+"\n")
		usageFile(t, root, base+"cmdline", argv+"\x00")
		usageFile(t, root, base+"cgroup", "0::/"+cg+"\n")
		if pss >= 0 {
			usageFile(t, root, base+"smaps_rollup", fmt.Sprintf("Pss: %d kB\n", pss))
		} else {
			usageFile(t, root, base+"statm", "200 100 0\n")
		}
	}
	proc(1, "claude", "claude", manager+"/app.slice/x", 100*1024)
	proc(2, "claude", "claude", manager+"/app.slice/x", 200*1024)
	proc(3, "MainThread", "/opt/x/codex", manager+"/app.slice/x", 50*1024)
	proc(4, "claude", "claude", manager+"/aq.slice/aq-x.service", 500*1024)
	proc(5, "claude", "claude", "system.slice/docker-a.scope/x", 500*1024)
	proc(6, "claude", "claude", manager+"/app.slice/x", -1)
	for _, tc := range []struct {
		p             string
		cur, inactive int
	}{{manager + "/aq.slice", 300, 100}, {manager + "/aqtd.slice", 100, 20}, {"system.slice/docker-a.scope", 700, 100}, {"docker/b", 400, 100}} {
		usageFile(t, root, "cg/"+tc.p+"/memory.current", fmt.Sprint(tc.cur))
		usageFile(t, root, "cg/"+tc.p+"/memory.stat", fmt.Sprintf("inactive_file %d\n", tc.inactive))
	}
	m := NewUsageMeter(UsageMeterConfig{ProcRoot: filepath.Join(root, "proc"), CgroupRoot: filepath.Join(root, "cg"), Slices: []string{"aq.slice", "aqtd.slice"}})
	v, e := m.Usage(context.Background(), []string{"claude", "codex", "missing"})
	if e != nil || v.Tasks != 280 || v.Docker != 900 || len(v.Agents) != 3 || v.Agents[0].Bytes != 300<<20+uint64(100*os.Getpagesize()) || v.Agents[0].Procs != 3 || v.Agents[1].Procs != 1 || v.Agents[2].Bytes != 0 {
		t.Fatal(v, e)
	}
}
func TestUsageMeterVanishingProcess(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "42"), 0700)
	m := NewUsageMeter(UsageMeterConfig{ProcRoot: root, CgroupRoot: t.TempDir()})
	v, e := m.Usage(context.Background(), []string{"claude"})
	if e != nil || v.Agents[0].Procs != 0 {
		t.Fatal(v, e)
	}
}
func TestUsageMeterCountsWhatAgentsRun(t *testing.T) {
	root := t.TempDir()
	manager := "user.slice/user-1000.slice/user@1000.service"
	usageFile(t, root, "proc/self/cgroup", "0::/"+manager+"/app.slice/aq.service\n")
	proc := func(pid, ppid int, comm, cg string, pssMiB int) {
		base := fmt.Sprintf("proc/%d/", pid)
		usageFile(t, root, base+"comm", comm+"\n")
		usageFile(t, root, base+"cmdline", comm+"\x00")
		usageFile(t, root, base+"status", fmt.Sprintf("Name:\t%s\nPPid:\t%d\n", comm, ppid))
		usageFile(t, root, base+"cgroup", "0::/"+cg+"\n")
		usageFile(t, root, base+"smaps_rollup", fmt.Sprintf("Pss: %d kB\n", pssMiB*1024))
	}
	session := "user.slice/user-1000.slice/session-3.scope"
	proc(2, 1, "herder", session, 50)                             // runs the agents, is not one: other
	proc(10, 2, "claude", session, 400)                           // an agent
	proc(11, 10, "bash", session, 10)                             // what it runs directly
	proc(12, 11, "python", session, 500)                          // and that one's children
	proc(13, 10, "claude", session, 100)                          // a nested agent counts as an agent
	proc(14, 13, "node", session, 50)                             // what it runs goes to the nearest agent
	proc(15, 10, "python", manager+"/aq.slice/aq-x.service", 900) // already in aq: tasks
	proc(16, 2, "codex", session, 200)
	proc(17, 16, "cargo", session, 300)
	proc(18, 1, "sshd", "system.slice/sshd.service", 999)
	m := NewUsageMeter(UsageMeterConfig{ProcRoot: filepath.Join(root, "proc"), CgroupRoot: filepath.Join(root, "cg"), Slices: []string{"aq.slice"}})
	v, e := m.Usage(context.Background(), []string{"claude", "codex"})
	if e != nil {
		t.Fatal(e)
	}
	const mib = 1 << 20
	if a := v.Agents[0]; a.Bytes != 500*mib || a.Procs != 2 || a.Spawned != 560*mib || a.SpawnedProcs != 3 {
		t.Fatalf("claude %+v", a)
	}
	if a := v.Agents[1]; a.Bytes != 200*mib || a.Spawned != 300*mib || a.SpawnedProcs != 1 {
		t.Fatalf("codex %+v", a)
	}
}

func TestUsageProcIO(t *testing.T) {
	root := t.TempDir()
	manager := "user.slice/user-1000.slice/user@1000.service"
	usageFile(t, root, "proc/self/cgroup", "0::/"+manager+"/app.slice/aq.service\n")
	repo := filepath.Join(root, "r")
	if e := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); e != nil {
		t.Fatal(e)
	}
	session := "user.slice/user-1000.slice/session-3.scope"
	proc := func(pid, ppid int, comm, cg string, io string) {
		base := fmt.Sprintf("proc/%d/", pid)
		usageFile(t, root, base+"comm", comm+"\n")
		usageFile(t, root, base+"cmdline", comm+"\x00")
		usageFile(t, root, base+"status", fmt.Sprintf("Name:\t%s\nPPid:\t%d\n", comm, ppid))
		usageFile(t, root, base+"stat", fmt.Sprintf("%d (%s x) S %d 1 1 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 %d 0 0\n", pid, comm, ppid, 1000+pid))
		usageFile(t, root, base+"cgroup", "0::/"+cg+"\n")
		usageFile(t, root, base+"smaps_rollup", "Pss: 1 kB\n")
		if io != "" {
			usageFile(t, root, base+"io", io)
		}
		if e := os.Symlink(repo, filepath.Join(root, base, "cwd")); e != nil {
			t.Fatal(e)
		}
	}
	proc(10, 1, "claude", session, "rchar: 1\nwrite_bytes: 500\ncancelled_write_bytes: 100\n")
	proc(11, 10, "bash", session, "write_bytes: 70\ncancelled_write_bytes: 0\n")
	proc(12, 11, "python", session, "write_bytes: 5\ncancelled_write_bytes: 9\n")
	proc(13, 11, "gone", session, "") // io unreadable: skipped
	proc(20, 1, "stranger", session, "write_bytes: 3\ncancelled_write_bytes: 0\n")
	proc(30, 1, "other", "user.slice/user-1000.slice/session-4.scope", "write_bytes: 9\ncancelled_write_bytes: 0\n")
	m := NewUsageMeter(UsageMeterConfig{ProcRoot: filepath.Join(root, "proc"), CgroupRoot: filepath.Join(root, "cg")})
	v, e := m.Usage(context.Background(), []string{"claude"})
	if e != nil {
		t.Fatal(e)
	}
	idx := map[int]int{}
	for i, p := range v.IO {
		idx[p.PID] = i
	}
	for _, pid := range []int{10, 11, 12, 20} {
		if _, ok := idx[pid]; !ok {
			t.Fatalf("pid %d missing: %+v", pid, v.IO)
		}
	}
	for _, pid := range []int{13, 30} {
		if _, ok := idx[pid]; ok {
			t.Fatalf("pid %d must be absent: %+v", pid, v.IO)
		}
	}
	a := v.IO[idx[10]]
	if a.Agent != "claude" || a.Project != repo || a.Within != "session:session-3.scope" || a.Bytes != 400 || a.PPID != 1 || a.Start != 1010 {
		t.Errorf("agent entry = %+v", a)
	}
	if b := v.IO[idx[12]]; b.Agent != "" || b.Project != "" || b.Bytes != -4 || b.PPID != 11 || b.Within != "session:session-3.scope" {
		t.Errorf("descendant entry = %+v", b)
	}
	if !(idx[10] < idx[11] && idx[11] < idx[12]) {
		t.Errorf("order = %+v", v.IO)
	}
}

// The usage sampler and the disk meter must name the same source for a cgroup.
func TestSourceKeyMatchesDiskMeter(t *testing.T) {
	cfg := diskTree(t, diskIOTree())
	s, err := NewDiskMeter(cfg).Snapshot(context.Background())
	if err != nil || len(s.Cgroups) == 0 {
		t.Fatal(s, err)
	}
	containers, _ := cfg.Containers(context.Background())
	aq := []string{diskManager + "/aqtest.slice"}
	for _, c := range s.Cgroups {
		if got := sourceKey(c.ID, diskManager, aq, containers); got != c.Source {
			t.Errorf("sourceKey(%q) = %q, snapshot says %q", c.ID, got, c.Source)
		}
	}
}

// A process seen in an agent's tree keeps being read after the agent exits,
// even with no agent left in its cgroup, as long as it lives; so do the
// children it starts there. Strangers in the cgroup are not remembered, and
// a remembered process that exits is forgotten.
func TestUsageProcIOSurvivors(t *testing.T) {
	root := t.TempDir()
	manager := "user.slice/user-1000.slice/user@1000.service"
	usageFile(t, root, "proc/self/cgroup", "0::/"+manager+"/app.slice/aq.service\n")
	session := "user.slice/user-1000.slice/session-3.scope"
	proc := func(pid, ppid int, comm string, start int) {
		base := fmt.Sprintf("proc/%d/", pid)
		usageFile(t, root, base+"comm", comm+"\n")
		usageFile(t, root, base+"cmdline", comm+"\x00")
		usageFile(t, root, base+"status", fmt.Sprintf("Name:\t%s\nPPid:\t%d\n", comm, ppid))
		usageFile(t, root, base+"stat", fmt.Sprintf("%d (%s) S %d 1 1 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 %d 0 0\n", pid, comm, ppid, start))
		usageFile(t, root, base+"cgroup", "0::/"+session+"\n")
		usageFile(t, root, base+"io", "write_bytes: 7\ncancelled_write_bytes: 0\n")
	}
	gone := func(pid int) {
		if e := os.RemoveAll(filepath.Join(root, fmt.Sprintf("proc/%d", pid))); e != nil {
			t.Fatal(e)
		}
	}
	m := NewUsageMeter(UsageMeterConfig{ProcRoot: filepath.Join(root, "proc"), CgroupRoot: filepath.Join(root, "cg")})
	pids := func() []int {
		t.Helper()
		v, e := m.Usage(context.Background(), []string{"claude"})
		if e != nil {
			t.Fatal(e)
		}
		var out []int
		for _, p := range v.IO {
			out = append(out, p.PID)
		}
		slices.Sort(out)
		return out
	}
	proc(10, 1, "claude", 100)
	proc(11, 10, "writer", 110)
	proc(20, 1, "stranger", 120)
	if got := pids(); !slices.Equal(got, []int{10, 11, 20}) {
		t.Fatalf("with the agent: %v", got)
	}
	gone(10)
	proc(11, 1, "writer", 110) // orphaned
	proc(12, 11, "child", 130) // started after the agent left
	if got := pids(); !slices.Equal(got, []int{11, 12}) {
		t.Fatalf("after the agent exited: %v, want the writer and its child", got)
	}
	gone(11)
	proc(11, 1, "reused", 140) // the PID is someone else's now
	if got := pids(); !slices.Equal(got, []int{12}) {
		t.Fatalf("after the writer exited: %v, want only its child", got)
	}
	gone(12)
	if got := pids(); len(got) != 0 || len(m.tracked) != 0 {
		t.Fatalf("all gone: %v, tracked %v", got, m.tracked)
	}
}
