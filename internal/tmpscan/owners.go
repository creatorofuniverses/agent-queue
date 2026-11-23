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
	"unicode/utf16"

	"github.com/creatorofuniverses/agent-queue/internal/platform"
)

const claudeProjectLimit = 200

// ClaudeProject encodes a working directory before Claude's length limit and
// hash suffix are applied. The replacement works on UTF-16 code units.
func ClaudeProject(dir string) string {
	var encoded strings.Builder
	encoded.Grow(len(dir))
	for _, r := range dir {
		switch {
		case 'A' <= r && r <= 'Z', 'a' <= r && r <= 'z', '0' <= r && r <= '9':
			encoded.WriteByte(byte(r))
		case utf16.RuneLen(r) == 2:
			encoded.WriteString("--")
		default:
			encoded.WriteByte('-')
		}
	}
	return encoded.String()
}

type projectUsage struct {
	alive         map[string]bool
	prefixes      map[string]bool
	cwdUnreadable bool
}

func (p projectUsage) isAlive(name string) bool {
	return p.cwdUnreadable || p.alive[name] ||
		len(name) > claudeProjectLimit && name[claudeProjectLimit] == '-' && p.prefixes[name[:claudeProjectLimit]]
}

type scanProcess struct {
	pid int
	dir string
	cwd string
}

func scanOwners(ctx context.Context, o Options, items []Item) (projectUsage, error) {
	projects := projectUsage{alive: make(map[string]bool), prefixes: make(map[string]bool)}
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
		if inAgentOrTask(dir, o.Agents, o.TaskSlices) {
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
			name := ClaudeProject(dir)
			if len(name) > claudeProjectLimit {
				// Any hash suffix could belong to this live working directory.
				projects.prefixes[name[:claudeProjectLimit]] = true
			} else {
				projects.alive[name] = true
			}
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
	findItem := func(path string) *Item {
		for dir := path; dir != o.Root && strings.HasPrefix(dir, rootPrefix); dir = filepath.Dir(dir) {
			if item := byPath[dir]; item != nil {
				return item
			}
		}
		return nil
	}
	// Keep the first five PIDs in numeric order regardless of procfs order.
	slices.SortFunc(procs, func(a, b scanProcess) int { return a.pid - b.pid })
	for _, p := range procs {
		if err := ctx.Err(); err != nil {
			return projects, err
		}
		record := func(path string) {
			item := findItem(path)
			if item != nil && len(item.PIDs) < 5 && !slices.Contains(item.PIDs, p.pid) {
				item.PIDs = append(item.PIDs, p.pid)
				if item.Owner.Kind == "unknown" {
					item.Owner = Owner{Kind: "process", PID: p.pid}
				}
			}
		}
		record(p.cwd)
		if selected[p.pid] || p.cwd == o.Root || strings.HasPrefix(p.cwd, rootPrefix) {
			readProcessPaths(p.dir, record)
		}
	}
	// Process fds expose socket inodes. The namespace's socket table supplies bound paths.
	readProcPaths(filepath.Join(o.ProcRoot, "net", "unix"), 7, func(path string) {
		if filepath.IsAbs(path) {
			if item := findItem(filepath.Clean(path)); item != nil {
				item.socketInUse = true
			}
		}
	})
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

func inAgentOrTask(procDir string, agents, taskSlices []string) bool {
	comm, err := os.ReadFile(filepath.Join(procDir, "comm"))
	if err == nil {
		cmdline, _ := os.ReadFile(filepath.Join(procDir, "cmdline"))
		if platform.AgentIndex(agents, string(comm), string(cmdline)) >= 0 {
			return true
		}
	}
	cgroup, _ := os.ReadFile(filepath.Join(procDir, "cgroup"))
	for line := range strings.SplitSeq(string(cgroup), "\n") {
		if path, ok := strings.CutPrefix(line, "0::"); ok {
			for component := range strings.SplitSeq(path, "/") {
				if slices.Contains(taskSlices, component) {
					return true
				}
			}
		}
	}
	return false
}

func readProcessPaths(procDir string, record func(string)) {
	fds := filepath.Join(procDir, "fd")
	entries, _ := os.ReadDir(fds)
	for _, entry := range entries {
		if path, err := os.Readlink(filepath.Join(fds, entry.Name())); err == nil {
			record(procPath(path))
		}
	}
	readProcPaths(filepath.Join(procDir, "maps"), 5, func(path string) {
		record(procPath(path))
	})
}

func readProcPaths(file string, fields int, record func(string)) {
	f, err := os.Open(file)
	if err != nil {
		return
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		path := s.Text()
		// The pathname follows the fixed fields and can include spaces.
		for range fields {
			i := strings.IndexAny(path, " \t")
			if i < 0 {
				path = ""
				break
			}
			path = strings.TrimLeft(path[i:], " \t")
		}
		record(path)
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
	case item.Partial || item.socketInUse || len(item.PIDs) != 0:
		return "in_use"
	case item.Owner.Kind == "claude_project" && projects.isAlive(item.Owner.Project):
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
