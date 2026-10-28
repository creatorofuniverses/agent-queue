package client

import (
	"context"
	"strings"
)

// DefaultProfile shares history between worktrees of the same repository.
func DefaultProfile(ctx context.Context, cwd, flagProject, name string) string {
	if name == "" {
		return ""
	}
	root := flagProject
	if root == "" {
		gctx, cancel := context.WithTimeout(ctx, gitTimeout)
		defer cancel()
		common := gitOut(gctx, cwd, "rev-parse", "--path-format=absolute", "--git-common-dir")
		if strings.HasSuffix(common, "/.git") {
			root = strings.TrimSuffix(common, "/.git")
		}
		if root == "" {
			root = gitOut(gctx, cwd, "rev-parse", "--show-toplevel")
		}
		if root == "" {
			root = cwd
		}
	}
	return root + ":" + name
}
