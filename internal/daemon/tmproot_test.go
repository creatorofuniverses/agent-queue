package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/creatorofuniverses/agent-queue/internal/config"
	"github.com/creatorofuniverses/agent-queue/internal/platform"
)

func fsAs(tmpfs bool, err error) func(string) (platform.FSUsage, error) {
	return func(string) (platform.FSUsage, error) { return platform.FSUsage{Tmpfs: tmpfs}, err }
}

func TestPlanTmpRoot(t *testing.T) {
	uid := os.Getuid()
	cfg := config.Default()

	// Auto mode: /tmp is used only when it is tmpfs.
	if p := PlanTmpRoot(cfg, uid, fsAs(false, nil), false); p.Root != "" || !strings.Contains(p.Reason, "not tmpfs") {
		t.Fatalf("disk /tmp: %+v", p)
	}
	if p := PlanTmpRoot(cfg, uid, fsAs(false, errors.New("boom")), false); p.Root != "" || !strings.Contains(p.Reason, "boom") {
		t.Fatalf("stat error: %+v", p)
	}

	// An explicit tmp_dir is used whatever its filesystem, as a private aq-<uid> dir.
	cfg.TmpDir = t.TempDir()
	want := filepath.Join(cfg.TmpDir, fmt.Sprintf("aq-%d-aq", uid))
	if p := PlanTmpRoot(cfg, uid, fsAs(false, nil), false); p.Root != want {
		t.Fatalf("plan only: %+v", p)
	}
	if _, err := os.Stat(want); !os.IsNotExist(err) {
		t.Fatal("planning created the dir")
	}
	p := PlanTmpRoot(cfg, uid, fsAs(false, nil), true)
	if st, err := os.Lstat(want); p.Root != want || err != nil || !st.IsDir() || st.Mode().Perm() != 0o700 {
		t.Fatalf("create: %+v %v %v", p, st, err)
	}
	os.Chmod(want, 0o755)
	if PlanTmpRoot(cfg, uid, fsAs(false, nil), true); mode(t, want) != 0o700 {
		t.Fatal("mode not restored")
	}

	// Another slice is another daemon: another root.
	other := cfg
	other.SliceName = "aqtest.slice"
	if p := PlanTmpRoot(other, uid, fsAs(false, nil), false); p.Root != filepath.Join(cfg.TmpDir, fmt.Sprintf("aq-%d-aqtest", uid)) {
		t.Fatalf("per slice: %+v", p)
	}

	// A planted symlink in a shared dir is refused, not followed.
	cfg.TmpDir = t.TempDir()
	os.Symlink(t.TempDir(), filepath.Join(cfg.TmpDir, fmt.Sprintf("aq-%d-aq", uid)))
	if p := PlanTmpRoot(cfg, uid, fsAs(true, nil), true); p.Root != "" || !strings.Contains(p.Reason, "not a directory") {
		t.Fatalf("symlink: %+v", p)
	}
	// Someone else's dir is refused too.
	cfg.TmpDir = t.TempDir()
	os.Mkdir(filepath.Join(cfg.TmpDir, fmt.Sprintf("aq-%d-aq", uid+1)), 0o700)
	if p := PlanTmpRoot(cfg, uid+1, fsAs(true, nil), true); p.Root != "" || !strings.Contains(p.Reason, "owned by") {
		t.Fatalf("foreign owner: %+v", p)
	}
}

// A tmpfs /tmp that is full (ENOSPC on the root) or otherwise refuses the
// root right now keeps the plan on tmpfs with the gate on: falling back would
// put every task's temp files on the SSD for the daemon's lifetime.
func TestPlanTmpRootKeepsTmpfsWhenRootCannotBeMadeYet(t *testing.T) {
	cfg := config.Default()
	cfg.TmpDir = t.TempDir()
	os.Chmod(cfg.TmpDir, 0o500) // stands in for ENOSPC: Mkdir fails, nothing planted
	t.Cleanup(func() { os.Chmod(cfg.TmpDir, 0o700) })
	want := filepath.Join(cfg.TmpDir, fmt.Sprintf("aq-%d-aq", os.Getuid()))
	p := PlanTmpRoot(cfg, os.Getuid(), fsAs(true, nil), true)
	if p.Root != want || p.Reason == "" {
		t.Fatalf("want a pending tmpfs root, got %+v", p)
	}
}

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return st.Mode().Perm()
}
