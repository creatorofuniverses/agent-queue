package linux

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProjectRoot(t *testing.T) {
	root := t.TempDir()
	mk := func(rel string) string {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	write := func(rel, body string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk("r/.git")
	deep := mk("r/a/b")
	if got, want := ProjectRoot(deep), filepath.Join(root, "r"); got != want {
		t.Errorf("repo: %q, want %q", got, want)
	}
	// A worktree: .git is a file pointing into the main repo's worktrees dir.
	write("w/.git", "gitdir: "+filepath.Join(root, "r/.git/worktrees/w")+"\n")
	write("r/.git/worktrees/w/commondir", "../..\n")
	sub := mk("w/src")
	if got, want := ProjectRoot(sub), filepath.Join(root, "r"); got != want {
		t.Errorf("worktree: %q, want %q", got, want)
	}
	// A relative gitdir whose common dir is not a .git: the checkout itself.
	write("s/.git", "gitdir: ../store/gd\n")
	write("store/gd/commondir", ".\n")
	if got, want := ProjectRoot(mk("s/x")), filepath.Join(root, "s"); got != want {
		t.Errorf("odd common dir: %q, want %q", got, want)
	}
	// No repository: the directory itself.
	plain := mk("plain/dir")
	if got := ProjectRoot(plain); got != plain {
		t.Errorf("plain: %q, want %q", got, plain)
	}
}
