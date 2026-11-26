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
	// A repository has a HEAD in .git.
	write("r/.git/HEAD", "ref: refs/heads/main\n")
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
	// An empty .git is no repository: git finds none there either, and the
	// Codex CLI sandbox leaves one behind under /tmp when bwrap is killed.
	mk("e/.git")
	if got, want := ProjectRoot(mk("e/a")), filepath.Join(root, "e/a"); got != want {
		t.Errorf("empty .git: %q, want %q", got, want)
	}
	// The walk goes on past it to a repository above.
	write("p/.git/HEAD", "ref: refs/heads/main\n")
	mk("p/e/.git")
	if got, want := ProjectRoot(mk("p/e/x")), filepath.Join(root, "p"); got != want {
		t.Errorf("empty .git below a repository: %q, want %q", got, want)
	}
}
