package tmpscan

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"
)

func TestScanItemsAndKinds(t *testing.T) {
	o := fixture(t)
	for _, name := range []string{
		"gocache/README", "gocache/trim.txt", "venv/pyvenv.cfg", "wt/.git",
		"big/sub/repo/.git/config", "x.log",
	} {
		writeFile(t, filepath.Join(o.Root, name), "gitdir: /work/repo/.git")
	}
	aqName := fmt.Sprintf("aq-%d-aq", o.UID)
	makeDir(t, filepath.Join(o.Root, aqName))
	latest := o.Now().Add(-time.Hour)
	for _, path := range []string{"big", "big/sub", "big/sub/repo", "big/sub/repo/.git"} {
		touch(t, filepath.Join(o.Root, path), latest.Add(-time.Hour))
	}
	touch(t, filepath.Join(o.Root, "big/sub/repo/.git/config"), latest)

	r := scanFixture(t, o)
	want := map[string]Kind{
		"gocache": "gocache", "venv": "venv", "wt": "git", "big": "other", "x.log": "other",
		aqName: "other",
	}
	if r.Root != o.Root || !r.At.Equal(o.Now()) || !r.Complete || len(r.Items) != len(want) {
		t.Fatalf("unexpected report: %+v", r)
	}
	for i, item := range r.Items {
		if i > 0 && item.Bytes > r.Items[i-1].Bytes {
			t.Fatalf("items out of size order: %+v", r.Items)
		}
		name := filepath.Base(item.Path)
		if item.Kind != want[name] {
			t.Errorf("%s: kind = %q, want %q", name, item.Kind, want[name])
		}
		st := statAt(t, item.Path)
		if item.Dev != uint64(st.Dev) || item.Ino != uint64(st.Ino) {
			t.Errorf("%s: item has wrong device/inode: %+v", name, item)
		}
	}
	big := itemAt(t, r, "big")
	wantBytes := blocksAt(t, o, "big", "big/sub", "big/sub/repo", "big/sub/repo/.git", "big/sub/repo/.git/config")
	if big.Bytes != wantBytes || big.Inodes != 5 || !big.LastWrite.Equal(latest) ||
		!slices.Equal(big.Gits, []string{"sub/repo/.git"}) {
		t.Errorf("big: got %+v, want %d bytes, 5 inodes, mtime %v and nested .git", big, wantBytes, latest)
	}
	if wt := itemAt(t, r, "wt"); !slices.Equal(wt.Gits, []string{".git"}) {
		t.Errorf("worktree .git paths = %v", wt.Gits)
	}
	aq := itemAt(t, r, aqName)
	if aq.State != "aq_task" || aq.Owner.Kind != "aq_task" {
		t.Errorf("aq task: %+v", aq)
	}
}

func TestScanClaudeScratchpads(t *testing.T) {
	o := fixture(t)
	fakeProcess(t, o, 101, 0, "fish", "/p/a/sub")
	cases := []struct {
		project, session string
		age              time.Duration
		state            State
	}{
		{"-p-a", "s1", 30 * time.Hour, "candidate"},
		{"-p-a", "s4", time.Hour, "live"},
		{"-p-b", "s2", 30 * time.Hour, "orphan"},
		{"-p-b", "s5", time.Hour, "recent"},
	}
	for _, c := range cases {
		scratchpad(t, o, c.project, c.session, c.age)
	}
	r := scanFixture(t, o)
	if len(r.Items) != len(cases) {
		t.Fatalf("expected only session directories, got %+v", r.Items)
	}
	for _, c := range cases {
		item := itemAt(t, r, scratchPath(o, c.project, c.session))
		wantOwner := Owner{Kind: "claude_project", Project: c.project, Session: c.session}
		if item.Kind != "claude_scratchpad" || item.Owner != wantOwner || item.State != c.state {
			t.Errorf("%s: got %+v, want scratchpad, owner %+v, state %s", c.session, item, wantOwner, c.state)
		}
	}
}

func TestScanPartial(t *testing.T) {
	o := fixture(t)
	o.ItemBudget = time.Nanosecond
	for i := range 1000 {
		writeFile(t, filepath.Join(o.Root, "big", fmt.Sprint(i)), "x")
	}
	writeFile(t, filepath.Join(o.Root, "z.log"), "x")
	r := scanFixture(t, o)
	big := itemAt(t, r, "big")
	if r.Complete || !big.Partial || big.State != "in_use" {
		t.Errorf("expected incomplete report with partial in_use item, got %+v", r)
	}
	itemAt(t, r, "z.log")
}

func TestScanForeignUIDAndSymlinks(t *testing.T) {
	o := fixture(t)
	foreign := filepath.Join(o.Root, "foreign")
	writeFile(t, foreign, "x")
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "must-not-count"), "x")
	link := filepath.Join(o.Root, "link")
	must(t, os.Symlink(outside, link))

	// Use a real Lstat with an overridden uid because an ordinary user cannot chown.
	r, err := scan(context.Background(), o, func(path string) (fs.FileInfo, error) {
		info, err := os.Lstat(path)
		if path != foreign || err != nil {
			return info, err
		}
		st := *info.Sys().(*syscall.Stat_t)
		st.Uid++
		return fileInfoStat{FileInfo: info, stat: st}, nil
	})
	must(t, err)
	item := itemAt(t, r, "link")
	if len(r.Items) != 1 || item.Kind != "other" || item.Bytes != blocksAt(t, o, "link") || item.Inodes != 1 {
		t.Errorf("expected only the symlink inode, got %+v", r.Items)
	}
}

func TestScanNoClaudeDir(t *testing.T) {
	o := fixture(t)
	writeFile(t, filepath.Join(o.Root, "x.log"), "x")
	r := scanFixture(t, o)
	if item := itemAt(t, r, "x.log"); item.State != "recent" || item.Owner.Kind != "unknown" {
		t.Errorf("expected a recent file with unknown owner, got %+v", item)
	}
}

func TestOrphanAndCandidateTotals(t *testing.T) {
	o := fixture(t)
	fakeProcess(t, o, 101, 0, "fish", "/p/alive")
	scratchpad(t, o, "-p-alive", "old", 30*time.Hour)
	scratchpad(t, o, "-p-alive", "new", time.Hour)
	scratchpad(t, o, "-p-dead", "old", 30*time.Hour)
	scratchpad(t, o, "-p-dead", "new", time.Hour)
	writeFile(t, filepath.Join(o.Root, "old.log"), "old")
	touch(t, filepath.Join(o.Root, "old.log"), o.Now().Add(-30*time.Hour))

	r := scanFixture(t, o)
	orphan := scratchPath(o, "-p-dead", "old")
	candidate := scratchPath(o, "-p-alive", "old")
	wantOrphan := blocksAt(t, o, orphan, filepath.Join(orphan, "pad.md"), "old.log")
	wantCandidate := blocksAt(t, o, candidate, filepath.Join(candidate, "pad.md"))
	if r.OrphanBytes != wantOrphan || r.CandidateBytes != wantCandidate {
		t.Errorf("orphan/candidate bytes = %d/%d, want %d/%d", r.OrphanBytes, r.CandidateBytes, wantOrphan, wantCandidate)
	}
}

func TestScanCancellation(t *testing.T) {
	o := fixture(t)
	writeFile(t, filepath.Join(o.Root, "x.log"), "x")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Scan(ctx, o); !errors.Is(err, context.Canceled) {
		t.Errorf("Scan error = %v, want context.Canceled", err)
	}
}

type fileInfoStat struct {
	fs.FileInfo
	stat syscall.Stat_t
}

func (f fileInfoStat) Sys() any { return &f.stat }

func fixture(t *testing.T) Options {
	t.Helper()
	now := time.Now()
	return Options{
		Root: t.TempDir(), ProcRoot: t.TempDir(), UID: os.Getuid(),
		OrphanAfter: 24 * time.Hour, Now: func() time.Time { return now },
	}
}

func scanFixture(t *testing.T, o Options) Report {
	t.Helper()
	r, err := Scan(context.Background(), o)
	must(t, err)
	return r
}

func itemAt(t *testing.T, r Report, rel string) Item {
	t.Helper()
	for _, item := range r.Items {
		if item.Path == filepath.Join(r.Root, rel) {
			return item
		}
	}
	t.Fatalf("missing item %s in %+v", rel, r.Items)
	return Item{}
}

func scratchPath(o Options, project, session string) string {
	return filepath.Join(fmt.Sprintf("claude-%d", o.UID), project, session)
}

func scratchpad(t *testing.T, o Options, project, session string, age time.Duration) {
	t.Helper()
	dir := filepath.Join(o.Root, scratchPath(o, project, session))
	file := filepath.Join(dir, "pad.md")
	writeFile(t, file, "text")
	stamp := o.Now().Add(-age)
	touch(t, file, stamp)
	touch(t, dir, stamp)
}

func blocksAt(t *testing.T, o Options, paths ...string) uint64 {
	t.Helper()
	var total uint64
	for _, path := range paths {
		total += uint64(statAt(t, filepath.Join(o.Root, path)).Blocks) * 512
	}
	return total
}

func statAt(t *testing.T, path string) *syscall.Stat_t {
	t.Helper()
	info, err := os.Lstat(path)
	must(t, err)
	return info.Sys().(*syscall.Stat_t)
}

func touch(t *testing.T, path string, stamp time.Time) {
	t.Helper()
	must(t, os.Chtimes(path, stamp, stamp))
}

func makeDir(t *testing.T, path string) {
	t.Helper()
	must(t, os.MkdirAll(path, 0700))
}

func writeFile(t *testing.T, path, text string) {
	t.Helper()
	makeDir(t, filepath.Dir(path))
	must(t, os.WriteFile(path, []byte(text), 0600))
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
