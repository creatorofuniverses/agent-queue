package tmpscan

import (
	"bufio"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// ClaudeProject encodes a working directory as a Claude project directory name.
func ClaudeProject(dir string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case 'A' <= r && r <= 'Z', 'a' <= r && r <= 'z', '0' <= r && r <= '9':
			return r
		default:
			return '-'
		}
	}, dir)
}

type projectUsage struct {
	alive         map[string]bool
	cwdUnreadable bool
}

type scanProcess struct {
	pid int
	dir string
	cwd string
}

func scanOwners(ctx context.Context, o Options, items []Item) (projectUsage, error) {
	projects := projectUsage{alive: make(map[string]bool)}
	entries, err := os.ReadDir(o.ProcRoot)
	if err != nil {
		return projects, err
	}
	var procs []scanProcess
	var treeRoots []int
	children := make(map[int][]int)
	cwds := make(map[string]bool)
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return projects, err
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 || !entry.IsDir() {
			continue
		}
		dir := filepath.Join(o.ProcRoot, entry.Name())
		uid, ppid := processIDs(dir)
		if uid < 0 {
			continue
		}
		children[ppid] = append(children[ppid], pid)
		if inAgentOrTask(dir, o.Agents) {
			treeRoots = append(treeRoots, pid)
		}
		if uid != o.UID {
			continue
		}
		cwd, err := os.Readlink(filepath.Join(dir, "cwd"))
		switch {
		case err == nil:
			cwd = procPath(cwd)
			if cwd != "" {
				cwds[cwd] = true
			}
		case errors.Is(err, fs.ErrNotExist):
			continue
		default:
			if _, err := os.ReadFile(filepath.Join(dir, "stat")); err != nil {
				continue
			}
			projects.cwdUnreadable = true
		}
		procs = append(procs, scanProcess{pid: pid, dir: dir, cwd: cwd})
	}

	for cwd := range cwds {
		for dir := cwd; ; dir = filepath.Dir(dir) {
			projects.alive[ClaudeProject(dir)] = true
			if dir == "/" {
				break
			}
		}
	}

	selected := make(map[int]bool)
	for len(treeRoots) != 0 {
		pid := treeRoots[0]
		treeRoots = treeRoots[1:]
		if selected[pid] {
			continue
		}
		selected[pid] = true
		treeRoots = append(treeRoots, children[pid]...)
	}

	byPath := make(map[string]*Item, len(items))
	for i := range items {
		byPath[items[i].Path] = &items[i]
	}
	rootPrefix := strings.TrimSuffix(o.Root, "/") + "/"
	// Keep the first five PIDs in numeric order regardless of procfs order.
	slices.SortFunc(procs, func(a, b scanProcess) int { return a.pid - b.pid })
	for _, p := range procs {
		if err := ctx.Err(); err != nil {
			return projects, err
		}
		record := func(path string) {
			for dir := path; dir != o.Root && strings.HasPrefix(dir, rootPrefix); dir = filepath.Dir(dir) {
				if item := byPath[dir]; item != nil {
					if len(item.PIDs) < 5 && !slices.Contains(item.PIDs, p.pid) {
						item.PIDs = append(item.PIDs, p.pid)
						if item.Owner.Kind == "unknown" {
							item.Owner = Owner{Kind: "process", PID: p.pid}
						}
					}
					return
				}
			}
		}
		record(p.cwd)
		if selected[p.pid] || p.cwd == o.Root || strings.HasPrefix(p.cwd, rootPrefix) {
			readProcessPaths(p.dir, record)
		}
	}
	return projects, ctx.Err()
}

func processIDs(procDir string) (uid, ppid int) {
	uid = -1
	status, err := os.ReadFile(filepath.Join(procDir, "status"))
	if err != nil {
		return uid, ppid
	}
	for line := range strings.SplitSeq(string(status), "\n") {
		key, rest, ok := strings.Cut(line, ":")
		if !ok || key != "Uid" && key != "PPid" {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		n, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		if key == "Uid" {
			uid = n
		} else {
			ppid = n
		}
	}
	return uid, ppid
}

func inAgentOrTask(procDir string, agents []string) bool {
	comm, _ := os.ReadFile(filepath.Join(procDir, "comm"))
	if slices.Contains(agents, strings.TrimSuffix(string(comm), "\n")) {
		return true
	}
	cgroup, _ := os.ReadFile(filepath.Join(procDir, "cgroup"))
	return strings.Contains(string(cgroup), "/aq.slice/")
}

func readProcessPaths(procDir string, record func(string)) {
	fds := filepath.Join(procDir, "fd")
	entries, _ := os.ReadDir(fds)
	for _, entry := range entries {
		if path, err := os.Readlink(filepath.Join(fds, entry.Name())); err == nil {
			record(procPath(path))
		}
	}

	f, err := os.Open(filepath.Join(procDir, "maps"))
	if err != nil {
		return
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		path := s.Text()
		// After the five fixed fields the rest of the line is the pathname,
		// which can include spaces.
		for range 5 {
			i := strings.IndexAny(path, " \t")
			if i < 0 {
				path = ""
				break
			}
			path = strings.TrimLeft(path[i:], " \t")
		}
		record(procPath(path))
	}
}

func procPath(path string) string {
	if !filepath.IsAbs(path) {
		return ""
	}
	if original, deleted := strings.CutSuffix(path, " (deleted)"); deleted {
		// Procfs appends this suffix to unlinked paths. It can also be
		// part of a real filename.
		if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
			path = original
		}
	}
	return filepath.Clean(path)
}

func itemState(item Item, projects projectUsage, cutoff time.Time) State {
	recent := !item.LastWrite.Before(cutoff)
	switch {
	case item.Owner.Kind == "aq_task":
		return "aq_task"
	case item.Partial || len(item.PIDs) != 0:
		return "in_use"
	case item.Owner.Kind == "claude_project" && (projects.cwdUnreadable || projects.alive[item.Owner.Project]):
		if recent {
			return "live"
		}
		return "candidate"
	case recent:
		return "recent"
	default:
		return "orphan"
	}
}
