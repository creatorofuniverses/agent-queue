package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGitOutBoundedAndLockFree(t *testing.T) {
	if gitTimeout != 5*time.Second {
		t.Fatalf("gitTimeout %v, want 5s", gitTimeout)
	}
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	script := "#!/bin/sh\necho \"$@\" >> " + argsFile + "\nexec sleep 30\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	if out := gitOut(ctx, t.TempDir(), "rev-parse", "HEAD"); out != "" {
		t.Fatalf("out %q", out)
	}
	if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("gitOut took %v", el)
	}
	b, _ := os.ReadFile(argsFile)
	if got := strings.TrimSpace(string(b)); got != "--no-optional-locks rev-parse HEAD" {
		t.Fatalf("git args %q", got)
	}
}
