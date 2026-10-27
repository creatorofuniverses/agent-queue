package client

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// DetectProject returns the project (flag, else git root, else cwd) and git state.
func DetectProject(ctx context.Context, cwd, flagProject string) (proj, rev string, dirty bool) {
	gctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	proj = flagProject
	if proj == "" {
		proj = gitOut(gctx, cwd, "rev-parse", "--show-toplevel")
		if proj == "" {
			proj = cwd
		}
	}
	rev = gitOut(gctx, cwd, "rev-parse", "HEAD")
	dirty = rev != "" && gitOut(gctx, cwd, "status", "--porcelain") != ""
	return
}

// gitTimeout bounds all git calls of one submit.
var gitTimeout = 5 * time.Second

// gitOut runs git in dir and returns trimmed stdout; any error yields "".
// --no-optional-locks keeps status from taking index.lock.
func gitOut(ctx context.Context, dir string, args ...string) string {
	cmd := exec.CommandContext(ctx, "git", append([]string{"--no-optional-locks"}, args...)...)
	cmd.Dir = dir
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
