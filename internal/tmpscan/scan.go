// Package tmpscan inventories temporary files and the processes using them.
package tmpscan

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

type State string // "aq_task" | "in_use" | "live" | "orphan" | "candidate" | "recent"
type Kind string  // "git" | "gocache" | "venv" | "claude_scratchpad" | "other"

type Owner struct {
	Kind    string `json:"kind"`              // "aq_task" | "claude_project" | "process" | "unknown"
	Project string `json:"project,omitempty"` // claude project dir name
	Session string `json:"session,omitempty"` // scratchpad session id, a label only
	PID     int    `json:"pid,omitempty"`
}

type Item struct {
	Path      string    `json:"path"`
	Dev       uint64    `json:"dev"`
	Ino       uint64    `json:"ino"`
	Bytes     uint64    `json:"bytes"`
	Inodes    uint64    `json:"inodes"`
	LastWrite time.Time `json:"last_write"`
	Owner     Owner     `json:"owner"`
	Kind      Kind      `json:"type"`
	State     State     `json:"state"`
	Partial   bool      `json:"partial"`
	Gits      []string  `json:"gits,omitempty"` // every .git below the root, relative
	PIDs      []int     `json:"pids,omitempty"` // up to 5 processes using it

	socketInUse    bool
	budgetExceeded bool
}

type Report struct {
	At             time.Time `json:"scanned_at"`
	Root           string    `json:"root"`
	Items          []Item    `json:"items"` // largest first
	OrphanBytes    uint64    `json:"orphan_bytes"`
	CandidateBytes uint64    `json:"candidate_bytes"`
	Complete       bool      `json:"complete"` // no item exceeded its time budget
	PartialItems   int       `json:"partial_items"`
}

type Options struct {
	Root, ProcRoot string
	UID            int
	OrphanAfter    time.Duration
	ItemBudget     time.Duration // per-item walk limit; default 5s
	Now            func() time.Time
	Agents         []string // process names; defaults to claude, openclaude, codex and opencode
	TaskSlices     []string // cgroup slice names for aq tasks and teardowns
}

// Scan walks the temporary directory and checks which items processes use.
func Scan(ctx context.Context, o Options) (Report, error) {
	return scan(ctx, o, os.Lstat)
}

func scan(ctx context.Context, o Options, lstat func(string) (fs.FileInfo, error)) (Report, error) {
	if err := ctx.Err(); err != nil {
		return Report{}, err
	}
	if o.Root == "" {
		o.Root = "/tmp"
	}
	root, err := filepath.Abs(o.Root)
	if err != nil {
		return Report{}, err
	}
	o.Root = root
	if o.ProcRoot == "" {
		o.ProcRoot = "/proc"
	}
	if o.OrphanAfter == 0 {
		o.OrphanAfter = 24 * time.Hour
	}
	if o.ItemBudget <= 0 {
		o.ItemBudget = 5 * time.Second
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if len(o.Agents) == 0 {
		o.Agents = []string{"claude", "openclaude", "codex", "opencode"}
	}

	rootInfo, err := lstat(o.Root)
	if err != nil {
		return Report{}, err
	}
	if !rootInfo.IsDir() {
		return Report{}, fmt.Errorf("scan root is not a directory: %s", o.Root)
	}
	rootDev := uint64(rootInfo.Sys().(*syscall.Stat_t).Dev)
	r := Report{At: o.Now(), Root: o.Root, Items: []Item{}, Complete: true}
	entries, err := os.ReadDir(o.Root)
	if err != nil {
		return Report{}, err
	}
	claudeDir := fmt.Sprintf("claude-%d", o.UID)
	aqPrefix := fmt.Sprintf("aq-%d-", o.UID)
	var partialClaudeRoot *Item
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return Report{}, err
		}
		path := filepath.Join(o.Root, entry.Name())
		info, err := lstat(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return Report{}, err
		}
		st := info.Sys().(*syscall.Stat_t)
		if uint64(st.Dev) != rootDev || int(st.Uid) != o.UID {
			continue
		}
		if entry.Name() == claudeDir && info.IsDir() {
			items, err := scanScratchpads(ctx, o, path, rootDev, lstat)
			if err != nil {
				if err := ctx.Err(); err != nil {
					return Report{}, err
				}
				item := newItem(path, info, Owner{Kind: "unknown"})
				item.Partial = true
				for _, scratchpad := range items {
					item.budgetExceeded = item.budgetExceeded || scratchpad.budgetExceeded
				}
				partialClaudeRoot = &item
				continue
			}
			r.Items = append(r.Items, items...)
			continue
		}
		owner := Owner{Kind: "unknown"}
		if strings.HasPrefix(entry.Name(), aqPrefix) {
			owner.Kind = "aq_task"
		}
		item, err := walkItem(ctx, o, path, info, owner)
		if err != nil {
			return Report{}, err
		}
		r.Items = append(r.Items, item)
	}

	projects, err := scanOwners(ctx, o, r.Items)
	if err != nil {
		return Report{}, err
	}
	if partialClaudeRoot != nil {
		// A process in one project cannot own the whole Claude directory.
		r.Items = append(r.Items, *partialClaudeRoot)
	}
	cutoff := r.At.Add(-o.OrphanAfter)
	for i := range r.Items {
		item := &r.Items[i]
		item.State = itemState(*item, projects, cutoff)
		r.Complete = r.Complete && !item.budgetExceeded
		if item.Partial {
			r.PartialItems++
		}
		switch item.State {
		case "orphan":
			r.OrphanBytes += item.Bytes
		case "candidate":
			r.CandidateBytes += item.Bytes
		}
	}
	sort.SliceStable(r.Items, func(i, j int) bool {
		return r.Items[i].Bytes > r.Items[j].Bytes
	})
	return r, nil
}

func scanScratchpads(ctx context.Context, o Options, claudeDir string, dev uint64, lstat func(string) (fs.FileInfo, error)) ([]Item, error) {
	projects, err := os.ReadDir(claudeDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var items []Item
	for _, project := range projects {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !project.IsDir() {
			continue
		}
		path := filepath.Join(claudeDir, project.Name())
		info, err := lstat(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return items, err
		}
		projectStat := info.Sys().(*syscall.Stat_t)
		if !info.IsDir() || uint64(projectStat.Dev) != dev {
			continue
		}
		sessions, err := os.ReadDir(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		partial := err != nil
		for _, session := range sessions {
			if !session.IsDir() {
				continue
			}
			sessionPath := filepath.Join(path, session.Name())
			sessionInfo, err := lstat(sessionPath)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				partial = true
				continue
			}
			st := sessionInfo.Sys().(*syscall.Stat_t)
			if !sessionInfo.IsDir() || uint64(st.Dev) != dev || int(st.Uid) != o.UID {
				continue
			}
			owner := Owner{Kind: "claude_project", Project: project.Name(), Session: session.Name()}
			item, err := walkItem(ctx, o, sessionPath, sessionInfo, owner)
			if err != nil {
				return nil, err
			}
			items = append(items, item)
		}
		if partial && int(projectStat.Uid) == o.UID {
			item := newItem(path, info, Owner{Kind: "claude_project", Project: project.Name()})
			item.Partial = true
			items = append(items, item)
		}
	}
	return items, nil
}

func lastWrite(st *syscall.Stat_t) time.Time {
	mtime := time.Unix(st.Mtim.Unix())
	ctime := time.Unix(st.Ctim.Unix())
	if ctime.After(mtime) {
		return ctime
	}
	return mtime
}

func newItem(path string, root fs.FileInfo, owner Owner) Item {
	st := root.Sys().(*syscall.Stat_t)
	item := Item{
		Path: path, Dev: uint64(st.Dev), Ino: uint64(st.Ino), Bytes: uint64(st.Blocks) * 512,
		Inodes: 1, LastWrite: lastWrite(st), Kind: "other", Owner: owner,
	}
	if owner.Kind == "claude_project" {
		item.Kind = "claude_scratchpad"
	}
	// Socket entries can belong to a server in a different network namespace.
	item.socketInUse = root.Mode()&fs.ModeSocket != 0
	return item
}

func walkItem(ctx context.Context, o Options, path string, root fs.FileInfo, owner Owner) (Item, error) {
	deadline := time.Now().Add(o.ItemBudget)
	item := newItem(path, root, owner)
	if !root.IsDir() {
		return item, ctx.Err()
	}
	if owner.Kind != "claude_project" {
		item.Kind = rootKind(path, item.Dev)
	}
	err := filepath.WalkDir(path, func(name string, entry fs.DirEntry, err error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !time.Now().Before(deadline) {
			return fs.SkipAll
		}
		if err != nil {
			item.Partial = true
			return nil
		}
		if name == path {
			return nil // Root was counted by the item's Lstat.
		}
		info, err := entry.Info()
		if err != nil {
			item.Partial = true
			return nil
		}
		st := info.Sys().(*syscall.Stat_t)
		if uint64(st.Dev) != item.Dev {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		item.Bytes += uint64(st.Blocks) * 512
		item.Inodes++
		item.socketInUse = item.socketInUse || info.Mode()&fs.ModeSocket != 0
		if at := lastWrite(st); at.After(item.LastWrite) {
			item.LastWrite = at
		}
		if entry.Name() == ".git" {
			item.Gits = append(item.Gits, name[len(path)+1:])
		}
		return nil
	})
	// The final filesystem call can finish after the deadline.
	item.budgetExceeded = !time.Now().Before(deadline)
	item.Partial = item.Partial || item.budgetExceeded
	return item, err
}

func rootKind(path string, dev uint64) Kind {
	marker := func(name string, allowDir bool) bool {
		info, err := os.Lstat(filepath.Join(path, name))
		return err == nil && uint64(info.Sys().(*syscall.Stat_t).Dev) == dev &&
			(info.Mode().IsRegular() || allowDir && info.IsDir())
	}
	switch {
	case marker(".git", true):
		return "git"
	case marker("README", false) && marker("trim.txt", false):
		return "gocache"
	case marker("pyvenv.cfg", false):
		return "venv"
	default:
		return "other"
	}
}
