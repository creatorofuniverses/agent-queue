package linux

import (
	"agent-queue/internal/platform"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

type UsageMeterConfig struct {
	ProcRoot, CgroupRoot string
	Slices               []string
}
type UsageMeter struct {
	cfg     UsageMeterConfig
	manager string
}

func NewUsageMeter(cfg UsageMeterConfig) *UsageMeter {
	if cfg.ProcRoot == "" {
		cfg.ProcRoot = "/proc"
	}
	if cfg.CgroupRoot == "" {
		cfg.CgroupRoot = "/sys/fs/cgroup"
	}
	b, _ := os.ReadFile(filepath.Join(cfg.ProcRoot, "self/cgroup"))
	return &UsageMeter{cfg: cfg, manager: userManagerCgroup(string(b), os.Getuid())}
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
	for _, ent := range ents {
		if e := ctx.Err(); e != nil {
			return v, e
		}
		if _, e := strconv.Atoi(ent.Name()); e != nil || !ent.IsDir() {
			continue
		}
		dir := filepath.Join(m.cfg.ProcRoot, ent.Name())
		comm, e := os.ReadFile(filepath.Join(dir, "comm"))
		if e != nil {
			continue
		}
		cmd, _ := os.ReadFile(filepath.Join(dir, "cmdline"))
		argv, _, _ := strings.Cut(string(cmd), "\x00")
		idx := -1
		for i, name := range agents {
			if strings.TrimSpace(string(comm)) == name || filepath.Base(argv) == name {
				idx = i
				break
			}
		}
		if idx < 0 {
			continue
		}
		cg, e := os.ReadFile(filepath.Join(dir, "cgroup"))
		if e != nil {
			continue
		}
		skip := false
		for _, line := range strings.Split(string(cg), "\n") {
			if p, ok := strings.CutPrefix(line, "0::"); ok {
				p = strings.Trim(p, "/")
				for _, x := range excluded {
					if withinCG(p, x) {
						skip = true
						break
					}
				}
			}
		}
		if skip {
			continue
		}
		n, e := readPSS(filepath.Join(dir, "smaps_rollup"))
		if e == nil {
			n *= 1024
		} else {
			b, err := os.ReadFile(filepath.Join(dir, "statm"))
			fields := strings.Fields(string(b))
			if err != nil || len(fields) < 2 {
				continue
			}
			pages, err := strconv.ParseUint(fields[1], 10, 64)
			if err != nil {
				continue
			}
			n = pages * uint64(os.Getpagesize())
		}
		v.Agents[idx].Bytes += n
		v.Agents[idx].Procs++
	}
	return v, nil
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
