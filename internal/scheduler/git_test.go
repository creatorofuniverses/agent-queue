package scheduler

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeGit puts a git on PATH that logs its arguments and hangs.
func fakeGit(t *testing.T) (argsFile string) {
	t.Helper()
	dir := t.TempDir()
	argsFile = filepath.Join(dir, "args")
	script := "#!/bin/sh\necho \"$@\" >> " + argsFile + "\nexec sleep 30\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	return argsFile
}

func TestGitStateBoundedAndLockFree(t *testing.T) {
	if gitTimeout != 5*time.Second {
		t.Fatalf("gitTimeout %v, want 5s", gitTimeout)
	}
	args := fakeGit(t)
	defer func(d time.Duration) { gitTimeout = d }(gitTimeout)
	gitTimeout = 300 * time.Millisecond

	start := time.Now()
	if _, _, ok := gitState(context.Background(), t.TempDir()); ok {
		t.Fatal("hung git reported ok")
	}
	if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("gitState took %v", el)
	}
	b, _ := os.ReadFile(args)
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if !strings.HasPrefix(l, "--no-optional-locks ") {
			t.Errorf("git called without --no-optional-locks: %q", l)
		}
	}
}
