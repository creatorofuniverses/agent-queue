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
// largest processes and the memory in use outside every cgroup. It reads
// procRoot (/proc) and cgroupRoot (/sys/fs/cgroup) only.
func MemorySnapshot(procRoot, cgroupRoot string, n int) (platform.MemorySnapshot, error) {
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
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		name, rss := procStatus(filepath.Join(procRoot, e.Name(), "status"))
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
	return v, nil
}

func hasSubdir(dir string) bool {
	ents, _ := os.ReadDir(dir)
	return slices.ContainsFunc(ents, fs.DirEntry.IsDir)
}

// procStatus is a process's name and RSS in bytes from /proc/<pid>/status.
func procStatus(path string) (name string, rss uint64) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", 0
	}
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, "Name:"); ok {
			name = strings.TrimSpace(v)
		} else if v, ok := strings.CutPrefix(l, "VmRSS:"); ok {
			if f := strings.Fields(v); len(f) > 0 {
				n, _ := strconv.ParseUint(f[0], 10, 64)
				rss = n << 10
			}
		}
	}
	return name, rss
}
