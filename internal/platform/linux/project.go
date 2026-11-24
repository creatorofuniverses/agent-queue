package linux

import (
	"errors"
	"github.com/creatorofuniverses/agent-queue/internal/platform"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// ProjectRoot is the project a working directory belongs to, by the same rule
// as the profiles but without running git: walk up to the nearest .git. A .git
// directory makes its parent the root. A .git file ("gitdir: X", a worktree)
// is followed through X/commondir to the main repository, so every worktree
// of a repository is one project; without such a link the checkout itself is
// the root. With no repository above dir, dir is returned.
func ProjectRoot(dir string) string {
	for d := dir; ; d = filepath.Dir(d) {
		g := filepath.Join(d, ".git")
		if fi, err := os.Stat(g); err == nil {
			if fi.IsDir() {
				return d
			}
			return worktreeRoot(d, g)
		}
		if parent := filepath.Dir(d); parent == d {
			return dir
		}
	}
}

func worktreeRoot(checkout, gitFile string) string {
	b, err := os.ReadFile(gitFile)
	if err != nil {
		return checkout
	}
	gitdir, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir:")
	if !ok {
		return checkout
	}
	gitdir = strings.TrimSpace(gitdir)
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(checkout, gitdir)
	}
	c, err := os.ReadFile(filepath.Join(gitdir, "commondir"))
	if err != nil {
		return checkout
	}
	common := strings.TrimSpace(string(c))
	if !filepath.IsAbs(common) {
		common = filepath.Join(gitdir, common)
	}
	common = filepath.Clean(common)
	if filepath.Base(common) == ".git" {
		return filepath.Dir(common)
	}
	return checkout
}

// procIO reads the write counters of every process that shares a cgroup with
// an agent, and of the agent-tree processes an earlier pass reported while
// they live, breadth-first from the roots (processes whose parent is not in
// the set), so a consumer sees ancestors before descendants. A process that
// vanishes or cannot be read is skipped.
//
// The agent tree as seen here is an agent and what its processes start in
// their own cgroup. Remembered by PID and start time, its processes are
// read after the agent exits, with no agent left in the cgroup to select
// it, and so are the children they start there. A remembered process is
// forgotten once it is gone (no /proc/PID, or another start time under its
// PID) or after trackedMisses scans in a row without a reading; one that
// cannot be read for a while is kept, or nothing would select it again.
func (m *UsageMeter) procIO(procs map[int]*usageProc, agents []string) []platform.ProcIO {
	hosts := map[string]bool{}
	for _, p := range procs {
		if p.agent >= 0 && !p.inAQOrDocker && p.cgroup != "" {
			hosts[p.cgroup] = true
		}
	}
	prev, next := m.tracked, map[int]trackedProc{} // what the next pass remembers
	// miss keeps a remembered identity that this pass could not read.
	miss := func(pid int, start uint64) {
		n := 1
		if t, ok := prev[pid]; ok && t.start == start {
			n = t.misses + 1
		}
		if n < trackedMisses {
			next[pid] = trackedProc{start: start, misses: n}
		}
	}
	survivor := map[int]bool{} // tracked and still the same process
	for pid, t := range prev {
		p := procs[pid]
		if p != nil && p.inAQOrDocker {
			continue // counted as tasks or docker now
		}
		dir := filepath.Join(m.cfg.ProcRoot, strconv.Itoa(pid))
		s, ok := procStart(filepath.Join(dir, "stat"))
		switch {
		case ok && s != t.start:
			// another process under the PID
		case ok && p != nil:
			survivor[pid] = true
		case !ok && procGone(dir):
		default:
			miss(pid, t.start)
		}
	}
	in := map[int]bool{}
	var pids []int
	for pid, p := range procs {
		if !p.inAQOrDocker && (hosts[p.cgroup] || survivor[pid]) {
			in[pid] = true
			pids = append(pids, pid)
		}
	}
	// What survivors start in their cgroup, however deep.
	for grew := len(survivor) > 0; grew; {
		grew = false
		for pid, p := range procs {
			if pp := procs[p.ppid]; !in[pid] && in[p.ppid] && pp != nil && pp.cgroup == p.cgroup && !p.inAQOrDocker {
				in[pid] = true
				pids = append(pids, pid)
				grew = true
			}
		}
	}
	m.tracked = next
	if len(pids) == 0 {
		return nil
	}
	sort.Ints(pids)
	children := map[int][]int{}
	var queue []int
	for _, pid := range pids {
		if pp := procs[pid].ppid; in[pp] && pp != pid {
			children[pp] = append(children[pp], pid)
		} else {
			queue = append(queue, pid)
		}
	}
	var out []platform.ProcIO
	for seen := map[int]bool{}; len(queue) > 0; queue = queue[1:] {
		pid := queue[0]
		if seen[pid] {
			continue
		}
		seen[pid] = true
		queue = append(queue, children[pid]...)
		p := procs[pid]
		bytes, haveWrites := procWrites(filepath.Join(p.dir, "io"))
		start, ok := procStart(filepath.Join(p.dir, "stat"))
		if !ok {
			if t, known := prev[pid]; known && survivor[pid] {
				miss(pid, t.start)
			}
			continue
		}
		if !p.startKnown || start != p.start {
			continue
		}
		// Parents come first: a child of a tracked process in its cgroup is
		// in the tree too.
		_, parentTracked := next[p.ppid]
		pp := procs[p.ppid]
		remember := p.agent >= 0 || survivor[pid] || (parentTracked && pp != nil && pp.cgroup == p.cgroup)
		if !haveWrites {
			if remember {
				miss(pid, start)
			}
			continue
		}
		if remember {
			next[pid] = trackedProc{start: start}
		}
		e := platform.ProcIO{PID: pid, PPID: p.ppid, Start: start, Bytes: bytes, Cgroup: p.cgroup,
			Within: sourceKey(p.cgroup, m.manager, m.aqRels(), nil)}
		if p.agent >= 0 {
			e.Agent = agents[p.agent]
			e.Project = m.project(p.cwd)
		}
		out = append(out, e)
	}
	return out
}

// trackedProc is a remembered agent-tree process: its start time, and how
// many passes in a row could not read it.
type trackedProc struct {
	start  uint64
	misses int
}

// trackedMisses is how many scans in a row a remembered process may go
// unread before it is forgotten.
const trackedMisses = 10

// procGone reports whether /proc/PID no longer exists: the process exited.
func procGone(dir string) bool {
	_, err := os.Stat(dir)
	return errors.Is(err, fs.ErrNotExist)
}

func (m *UsageMeter) aqRels() []string {
	rels := make([]string, 0, len(m.cfg.Slices))
	for _, s := range m.cfg.Slices {
		rels = append(rels, filepath.Join(m.manager, sliceCgroupPath(s)))
	}
	return rels
}

// project is the cached project root of a sampled working directory.
func (m *UsageMeter) project(cwd string) string {
	if cwd == "" {
		return ""
	}
	if r, ok := m.projects[cwd]; ok {
		return r
	}
	r := ProjectRoot(cwd)
	m.projects[cwd] = r
	return r
}

// procStart is starttime, field 22 of /proc/PID/stat. The command name in
// field 2 may hold spaces and parentheses, so fields count from the last ')'.
func procStart(path string) (uint64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0, false
	}
	f := strings.Fields(s[i+1:]) // f[0] is field 3
	if len(f) < 20 {
		return 0, false
	}
	n, err := strconv.ParseUint(f[19], 10, 64)
	return n, err == nil
}

// procWrites is write_bytes - cancelled_write_bytes from /proc/PID/io.
func procWrites(path string) (int64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	var w, c int64
	var haveW bool
	for _, l := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		switch k {
		case "write_bytes":
			w, err = strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			haveW = err == nil
		case "cancelled_write_bytes":
			c, _ = strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		}
	}
	return w - c, haveW
}
