package linux

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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
