package linux

import (
	"context"
	"errors"
	"github.com/creatorofuniverses/agent-queue/internal/platform"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

type UsageMeterConfig struct {
	ProcRoot, CgroupRoot string
	Slices               []string
}
type UsageMeter struct {
	cfg      UsageMeterConfig
	manager  string
	projects map[string]string // working directory -> project root; the sampler runs one pass at a time
	// tracked is the agent-tree processes the last pass reported, PID ->
	// start time: they are read while they live, with or without an agent
	// left in their cgroup.
	tracked map[int]uint64
}

func NewUsageMeter(cfg UsageMeterConfig) *UsageMeter {
	if cfg.ProcRoot == "" {
		cfg.ProcRoot = "/proc"
	}
	if cfg.CgroupRoot == "" {
		cfg.CgroupRoot = "/sys/fs/cgroup"
	}
	b, _ := os.ReadFile(filepath.Join(cfg.ProcRoot, "self/cgroup"))
	return &UsageMeter{cfg: cfg, manager: userManagerCgroup(string(b), os.Getuid()), projects: map[string]string{}}
}
func workingSet(dir string) (uint64, error) {
	cur, e := readUint(filepath.Join(dir, "memory.current"))
	if cgroupGone(e) {
		return 0, nil
	}
	if e != nil {
		return 0, e
	}
	inactive, e := statValue(filepath.Join(dir, "memory.stat"), "inactive_file")
	if cgroupGone(e) {
		return 0, nil
	}
	if e != nil {
		return 0, e
	}
	if inactive >= cur {
		return 0, nil
	}
	return cur - inactive, nil
}

// A cgroup removed between open and read returns ENODEV instead of ENOENT.
func cgroupGone(e error) bool        { return errors.Is(e, fs.ErrNotExist) || errors.Is(e, syscall.ENODEV) }
func withinCG(path, dir string) bool { return path == dir || strings.HasPrefix(path, dir+"/") }

// usageProc is what the single /proc pass learns about one process.
type usageProc struct {
	dir          string
	ppid, agent  int // agent indexes the agent list, -1 when the process is none
	cgroup       string
	inAQOrDocker bool
}

func (m *UsageMeter) Usage(ctx context.Context, agents []string) (platform.Usage, error) {
	v := platform.Usage{Agents: make([]platform.AgentUsage, len(agents))}
	var excluded []string
	for _, slice := range m.cfg.Slices {
		cg := filepath.Join(m.manager, sliceCgroupPath(slice))
		excluded = append(excluded, cg)
		n, e := workingSet(filepath.Join(m.cfg.CgroupRoot, cg))
		if e != nil {
			return v, e
		}
		v.Tasks += n
	}
	for _, glob := range []string{"system.slice/docker-*.scope", "docker/*"} {
		paths, e := filepath.Glob(filepath.Join(m.cfg.CgroupRoot, glob))
		if e != nil {
			return v, e
		}
		for _, p := range paths {
			n, e := workingSet(p)
			if e != nil {
				return v, e
			}
			v.Docker += n
			rel, _ := filepath.Rel(m.cfg.CgroupRoot, p)
			excluded = append(excluded, rel)
		}
	}
	for i, n := range agents {
		v.Agents[i].Name = n
	}
	ents, e := os.ReadDir(m.cfg.ProcRoot)
	if e != nil {
		return v, e
	}
	// One pass reads every process; PSS is read only for agents and what they run.
	procs := map[int]*usageProc{}
	for _, ent := range ents {
		if e := ctx.Err(); e != nil {
			return v, e
		}
		pid, e := strconv.Atoi(ent.Name())
		if e != nil || !ent.IsDir() {
			continue
		}
		dir := filepath.Join(m.cfg.ProcRoot, ent.Name())
		comm, e := os.ReadFile(filepath.Join(dir, "comm"))
		if e != nil {
			continue
		}
		cmd, _ := os.ReadFile(filepath.Join(dir, "cmdline"))
		argv, _, _ := strings.Cut(string(cmd), "\x00")
		p := &usageProc{dir: dir, agent: -1, ppid: parentPID(filepath.Join(dir, "status"))}
		for i, name := range agents {
			if strings.TrimSpace(string(comm)) == name || filepath.Base(argv) == name {
				p.agent = i
				break
			}
		}
		cg, e := os.ReadFile(filepath.Join(dir, "cgroup"))
		if e != nil {
			continue
		}
		for _, line := range strings.Split(string(cg), "\n") {
			if path, ok := strings.CutPrefix(line, "0::"); ok {
				path = strings.Trim(path, "/")
				p.cgroup = path
				p.inAQOrDocker = slices.ContainsFunc(excluded, func(x string) bool { return withinCG(path, x) })
			}
		}
		procs[pid] = p
	}
	// nearestAgent is the agent a process descends from, or -1.
	nearestAgent := func(pid int) int {
		for depth := 0; depth < 64 && pid > 1; depth++ {
			p := procs[pid]
			if p == nil {
				return -1
			}
			if p.agent >= 0 {
				return p.agent
			}
			pid = p.ppid
		}
		return -1
	}
	for _, p := range procs {
		if p.inAQOrDocker {
			continue // counted as tasks or docker
		}
		if p.agent >= 0 {
			v.Agents[p.agent].Bytes += processMemory(p.dir)
			v.Agents[p.agent].Procs++
		} else if a := nearestAgent(p.ppid); a >= 0 {
			v.Agents[a].Spawned += processMemory(p.dir)
			v.Agents[a].SpawnedProcs++
		}
	}
	v.IO = m.procIO(procs, agents)
	return v, nil
}

// processMemory is a process's PSS, or its RSS where smaps_rollup is not readable.
func processMemory(dir string) uint64 {
	if n, e := readPSS(filepath.Join(dir, "smaps_rollup")); e == nil {
		return n * 1024
	}
	b, err := os.ReadFile(filepath.Join(dir, "statm"))
	fields := strings.Fields(string(b))
	if err != nil || len(fields) < 2 {
		return 0
	}
	pages, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return 0
	}
	return pages * uint64(os.Getpagesize())
}

// parentPID reads PPid from /proc/<pid>/status; 0 when unknown.
func parentPID(status string) int {
	b, _ := os.ReadFile(status)
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, "PPid:"); ok {
			n, _ := strconv.Atoi(strings.TrimSpace(v))
			return n
		}
	}
	return 0
}

func readPSS(path string) (uint64, error) {
	b, e := os.ReadFile(path)
	if e != nil {
		return 0, e
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == "Pss:" {
			return strconv.ParseUint(f[1], 10, 64)
		}
	}
	return 0, fs.ErrNotExist
}
