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
