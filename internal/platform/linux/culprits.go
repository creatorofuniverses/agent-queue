package linux

import (
	"cmp"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/creatorofuniverses/agent-queue/internal/platform"
)

// MemorySnapshot reads who holds memory now: the n largest leaf cgroups, the n
// largest processes, each with the agent process it descends from, and the
// memory in use outside every cgroup. It reads procRoot (/proc) and cgroupRoot
// (/sys/fs/cgroup) only.
func MemorySnapshot(procRoot, cgroupRoot string, n int, agents []string) (platform.MemorySnapshot, error) {
	var v platform.MemorySnapshot
	f, err := os.Open(filepath.Join(procRoot, "meminfo"))
	if err != nil {
		return v, err
	}
	info, err := parseMeminfo(f)
	f.Close()
	if err != nil {
		return v, err
	}
	v.Available = info.Available
	inUse := info.Total - min(info.Free, info.Total)
	var charged uint64
	for _, k := range []string{"anon", "file", "kernel"} {
		c, err := statValue(filepath.Join(cgroupRoot, "memory.stat"), k)
		if err != nil {
			return v, err
		}
		charged += c
	}
	v.Uncharged = inUse - min(charged, inUse)

	_ = filepath.WalkDir(cgroupRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() || p == cgroupRoot || hasSubdir(p) {
			return nil
		}
		if b, err := readUint(filepath.Join(p, "memory.current")); err == nil {
			rel, _ := filepath.Rel(cgroupRoot, p)
			v.Cgroups = append(v.Cgroups, platform.CgroupMemory{Path: rel, Bytes: b})
		}
		return nil
	})
	slices.SortStableFunc(v.Cgroups, func(a, b platform.CgroupMemory) int { return cmp.Compare(b.Bytes, a.Bytes) })
	v.Cgroups = v.Cgroups[:min(n, len(v.Cgroups))]

	ents, err := os.ReadDir(procRoot)
	if err != nil {
		return v, err
	}
	type proc struct {
		name, exe string // exe: the base of argv[0], as the usage meter matches agents
		ppid      int
	}
	procs := map[int]proc{}
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		name, ppid, rss := procStatus(filepath.Join(procRoot, e.Name(), "status"))
		cmd, _ := os.ReadFile(filepath.Join(procRoot, e.Name(), "cmdline"))
		argv, _, _ := strings.Cut(string(cmd), "\x00")
		procs[pid] = proc{name, filepath.Base(argv), ppid}
		if rss == 0 {
			continue // a kernel thread, or gone
		}
		cg, _ := os.ReadFile(filepath.Join(procRoot, e.Name(), "cgroup"))
		path := ""
		for _, l := range strings.Split(string(cg), "\n") {
			if p, ok := strings.CutPrefix(l, "0::"); ok {
				path = strings.Trim(p, "/")
			}
		}
		v.Processes = append(v.Processes, platform.ProcessMemory{PID: pid, Name: name, Cgroup: path, RSS: rss})
	}
	slices.SortStableFunc(v.Processes, func(a, b platform.ProcessMemory) int { return cmp.Compare(b.RSS, a.RSS) })
	v.Processes = v.Processes[:min(n, len(v.Processes))]
	agent := func(p proc) string {
		for _, a := range agents {
			if p.name == a || p.exe == a {
				return a
			}
		}
		return ""
	}
	for i, p := range v.Processes {
		if agent(procs[p.PID]) != "" {
			continue // an agent itself
		}
		for pid, depth := procs[p.PID].ppid, 0; pid > 1 && depth < 64; pid, depth = procs[pid].ppid, depth+1 {
			if a := agent(procs[pid]); a != "" {
				v.Processes[i].Agent = a + " " + strconv.Itoa(pid)
				break
			}
		}
	}
	return v, nil
}

func hasSubdir(dir string) bool {
	ents, _ := os.ReadDir(dir)
	return slices.ContainsFunc(ents, fs.DirEntry.IsDir)
}

// procStatus is a process's name, parent and RSS in bytes from /proc/<pid>/status.
func procStatus(path string) (name string, ppid int, rss uint64) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", 0, 0
	}
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, "Name:"); ok {
			name = strings.TrimSpace(v)
		} else if v, ok := strings.CutPrefix(l, "PPid:"); ok {
			ppid, _ = strconv.Atoi(strings.TrimSpace(v))
		} else if v, ok := strings.CutPrefix(l, "VmRSS:"); ok {
			if f := strings.Fields(v); len(f) > 0 {
				n, _ := strconv.ParseUint(f[0], 10, 64)
				rss = n << 10
			}
		}
	}
	return name, ppid, rss
}
