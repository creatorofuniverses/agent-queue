package client

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestDefaultProfileSharedAcrossWorktrees(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		c := exec.Command("git", args...)
		c.Dir = repo
		if b, e := c.CombinedOutput(); e != nil {
			t.Fatal(string(b), e)
		}
	}
	git("init")
	git("-c", "user.name=test", "-c", "user.email=test@example.org", "commit", "--allow-empty", "-m", "init")
	wt := filepath.Join(t.TempDir(), "wt")
	git("worktree", "add", "--detach", wt)
	for _, dir := range []string{repo, wt} {
		if p := DefaultProfile(ctx, dir, "", "build"); p != repo+":build" {
			t.Fatal(p)
		}
	}
	if DefaultProfile(ctx, wt, "p", "build") != "p:build" || DefaultProfile(ctx, wt, "", "") != "" {
		t.Fatal("flags")
	}
	d := t.TempDir()
	if DefaultProfile(ctx, d, "", "build") != d+":build" {
		t.Fatal("fallback")
	}
}
func TestBuildSubmitMemoryOptionalWithProfile(t *testing.T) {
	o := SubmitOptions{Cwd: t.TempDir(), Name: "x", Timeout: "1m", CPUs: 1, Argv: []string{"true"}}
	for _, p := range []string{"", "p"} {
		o.Profile = p
		r, e := BuildSubmit(context.Background(), o, func(string) string { return "" })
		if e != nil || r.Limit != 0 || r.ProfileDerived != (p == "") || p != "" && r.Profile != p {
			t.Fatal(r, e)
		}
	}
	o.Profile = "bad\n"
	if _, e := BuildSubmit(context.Background(), o, func(string) string { return "" }); e == nil {
		t.Fatal("invalid")
	}
	o.Profile = ""
	o.Name = ""
	if _, e := BuildSubmit(context.Background(), o, func(string) string { return "" }); e == nil {
		t.Fatal("missing")
	}
}
