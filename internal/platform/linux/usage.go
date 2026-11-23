package linux

import (
	"cmp"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/creatorofuniverses/agent-queue/internal/platform"
)

type UsageMeterConfig struct {
	ProcRoot, CgroupRoot string
	Slices               []string
}
type UsageMeter struct {
	cfg      UsageMeterConfig
	manager  string
	projects map[string]string // working directory -> project root; the sampler runs one pass at a time
	// tracked is the agent-tree processes the last pass reported or could
	// not read, by PID: they are read while they live, with or without an
	// agent left in their cgroup.
	tracked map[int]trackedProc
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
	swapBytes    uint64
	cmdline      string // retained for agents so session metadata needs no second read
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
		cmdline := string(cmd)
		argv, _, _ := strings.Cut(cmdline, "\x00")
		ppid, swapBytes := parentPID(filepath.Join(dir, "status"))
		p := &usageProc{dir: dir, agent: -1, ppid: ppid, swapBytes: swapBytes}
		for i, name := range agents {
			if strings.TrimSpace(string(comm)) == name || filepath.Base(argv) == name {
				p.agent = i
				p.cmdline = cmdline
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
	// rootAgent includes the process itself and looks up to 64 parents,
	// using the same ancestry boundary as nearestAgent.
	rootAgent := func(pid int) int {
		root := -1
		for depth := 0; depth <= 64; depth++ {
			p := procs[pid]
			if p == nil {
				break
			}
			if p.agent >= 0 {
				root = pid
			}
			pid = p.ppid
			if pid <= 1 {
				break
			}
		}
		return root
	}
	sessionIndex := map[int]int{}
	for pid, p := range procs {
		if p.inAQOrDocker {
			continue // counted as tasks or docker
		}
		root := rootAgent(pid)
		if root < 0 {
			continue
		}
		i, ok := sessionIndex[root]
		if !ok {
			i = len(v.Sessions)
			sessionIndex[root] = i
			v.Sessions = append(v.Sessions, platform.SessionUsage{PID: root, Client: agents[procs[root].agent]})
		}
		s := &v.Sessions[i]
		memory := processMemory(p.dir)
		if p.agent >= 0 {
			v.Agents[p.agent].Bytes += memory
			v.Agents[p.agent].Procs++
			s.AgentBytes += memory
		} else {
			// The ancestry walk found a root, so there is also a nearest agent.
			a := nearestAgent(p.ppid)
			v.Agents[a].Spawned += memory
			v.Agents[a].SpawnedProcs++
			s.RunBytes += memory
		}
		s.SwapBytes += p.swapBytes
	}
	if len(v.Sessions) > 0 {
		bootTime := procBootTime(filepath.Join(m.cfg.ProcRoot, "stat"))
		for i := range v.Sessions {
			s := &v.Sessions[i]
			p := procs[s.PID]
			if start, ok := procStart(filepath.Join(p.dir, "stat")); ok {
				s.Start = start
				if !bootTime.IsZero() {
					// /proc reports starttime in Linux user clock ticks (USER_HZ).
					const ticksPerSecond = 100
					s.StartedAt = bootTime.Add(time.Duration(start) * (time.Second / ticksPerSecond))
				}
			}
			s.Cwd, _ = os.Readlink(filepath.Join(p.dir, "cwd"))
			if p.cmdline != "" {
				s.Cmdline = strings.Split(strings.TrimSuffix(p.cmdline, "\x00"), "\x00")
			}
		}
		slices.SortFunc(v.Sessions, func(a, b platform.SessionUsage) int {
			if order := cmp.Compare(b.AgentBytes+b.RunBytes, a.AgentBytes+a.RunBytes); order != 0 {
				return order
			}
			return cmp.Compare(a.PID, b.PID)
		})
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

// parentPID reads PPid and VmSwap from /proc/PID/status; zero when unknown.
func parentPID(status string) (pid int, swapBytes uint64) {
	b, _ := os.ReadFile(status)
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, "PPid:"); ok {
			pid, _ = strconv.Atoi(strings.TrimSpace(v))
		} else if v, ok := strings.CutPrefix(l, "VmSwap:"); ok {
			f := strings.Fields(v)
			if len(f) == 2 && f[1] == "kB" {
				n, e := strconv.ParseUint(f[0], 10, 64)
				if e == nil {
					swapBytes = n * 1024
				}
			}
		}
	}
	return pid, swapBytes
}

// procBootTime reads btime, the boot time in Unix seconds, from /proc/stat.
func procBootTime(path string) time.Time {
	b, _ := os.ReadFile(path)
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "btime "); ok {
			if seconds, e := strconv.ParseInt(strings.TrimSpace(v), 10, 64); e == nil {
				return time.Unix(seconds, 0).UTC()
			}
			break
		}
	}
	return time.Time{}
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
