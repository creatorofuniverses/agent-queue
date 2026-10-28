package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func run(t *testing.T, maxOut uint64, script string) (ExecArgs, int) {
	t.Helper()
	d := t.TempDir()
	a := ExecArgs{JobDir: filepath.Join(d, "job"), TmpDir: filepath.Join(d, "tmp"),
		MaxStdout: maxOut, MaxStderr: 1 << 20, Argv: []string{"sh", "-c", script}}
	return a, Exec(a)
}

func TestExecExitCode(t *testing.T) {
	a, code := run(t, 1<<20, "exit 3")
	if code != 3 {
		t.Fatalf("code %d", code)
	}
	if _, err := os.Stat(filepath.Join(a.JobDir, "stdout.log")); err != nil {
		t.Fatal(err)
	}
}

func TestExecSignalCode(t *testing.T) {
	_, code := run(t, 1<<20, "kill -9 $$")
	if code != 137 {
		t.Fatalf("code %d", code)
	}
}

func TestExecCapsInfiniteStdout(t *testing.T) {
	start := time.Now()
	a, code := run(t, 1<<20, "yes | head -c 50000000; echo done >&2")
	if code != 0 {
		t.Fatalf("code %d", code)
	}
	if time.Since(start) > 30*time.Second {
		t.Fatal("too slow")
	}
	st, _ := os.Stat(filepath.Join(a.JobDir, "stdout.log"))
	if st.Size() != 1<<20 {
		t.Fatalf("size %d", st.Size())
	}
	e, _ := os.ReadFile(filepath.Join(a.JobDir, "stderr.log"))
	if !strings.Contains(string(e), "done") {
		t.Fatalf("stderr %q", e)
	}
	b, _ := os.ReadFile(filepath.Join(a.JobDir, "exec.json"))
	var m struct {
		LogsTruncated bool `json:"logs_truncated"`
	}
	if err := json.Unmarshal(b, &m); err != nil || !m.LogsTruncated {
		t.Fatalf("exec.json %s", b)
	}
}

func TestExecSetsTmpdir(t *testing.T) {
	a, _ := run(t, 1<<20, "echo $TMPDIR")
	b, _ := os.ReadFile(filepath.Join(a.JobDir, "stdout.log"))
	if strings.TrimSpace(string(b)) != a.TmpDir {
		t.Fatalf("got %q want %q", b, a.TmpDir)
	}
	if st, err := os.Stat(a.TmpDir); err != nil || !st.IsDir() || st.Mode().Perm() != 0o700 {
		t.Fatal("tmp dir missing or not private", st, err)
	}
}

func TestExecLaunchError(t *testing.T) {
	d := t.TempDir()
	a := ExecArgs{JobDir: d, TmpDir: filepath.Join(d, "tmp"), MaxStdout: 10, MaxStderr: 10, Argv: []string{"/nonexistent"}}
	if code := Exec(a); code != 127 {
		t.Fatalf("code %d", code)
	}
	b, _ := os.ReadFile(filepath.Join(d, "launch_error.txt"))
	if len(b) == 0 {
		t.Fatal("empty launch_error.txt")
	}
}

func TestParseExecArgs(t *testing.T) {
	a, err := ParseExecArgs([]string{"--job-dir", "J", "--tmp-dir", "T", "--cwd", "C", "--max-stdout", "5", "--max-stderr", "6", "--", "echo", "--x"})
	if err != nil || a.JobDir != "J" || a.TmpDir != "T" || a.Cwd != "C" || a.MaxStdout != 5 || a.MaxStderr != 6 || len(a.Argv) != 2 || a.Argv[1] != "--x" {
		t.Fatalf("%+v %v", a, err)
	}
	if _, err := ParseExecArgs([]string{"--job-dir", "J"}); err == nil {
		t.Fatal("want error")
	}
}

func TestExecDescendantHoldsPipe(t *testing.T) {
	start := time.Now()
	done := make(chan int, 1)
	go func() { _, c := run(t, 1<<20, "sleep 20 & exit 0"); done <- c }()
	select {
	case c := <-done:
		if c != 0 || time.Since(start) > 10*time.Second {
			t.Fatalf("code %d after %v", c, time.Since(start))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Exec hung on inherited pipe")
	}
}

// The command runs in Cwd; the supervisor itself does not change directory.
func TestExecRunsInCwd(t *testing.T) {
	d, cwd := t.TempDir(), t.TempDir()
	before, _ := os.Getwd()
	a := ExecArgs{JobDir: filepath.Join(d, "job"), TmpDir: filepath.Join(d, "tmp"), Cwd: cwd,
		MaxStdout: 1 << 20, MaxStderr: 1 << 20, Argv: []string{"sh", "-c", "pwd; rmdir \"$PWD\""}}
	if code := Exec(a); code != 0 {
		t.Fatalf("code %d", code)
	}
	b, _ := os.ReadFile(filepath.Join(a.JobDir, "stdout.log"))
	if strings.TrimSpace(string(b)) != cwd {
		t.Fatalf("pwd %q, want %q", b, cwd)
	}
	if after, _ := os.Getwd(); after != before {
		t.Fatalf("supervisor cwd changed to %q", after)
	}
}

// The root is re-checked at every launch: tmpfiles may have deleted it and
// someone else may have planted their own in its place.
func TestExecRefusesPlantedRoot(t *testing.T) {
	d := t.TempDir()
	root := filepath.Join(d, "aq-root")
	os.Symlink(t.TempDir(), root)
	a := ExecArgs{JobDir: filepath.Join(d, "job"), TmpDir: filepath.Join(root, "id"),
		MaxStdout: 10, MaxStderr: 10, Argv: []string{"true"}}
	if code := Exec(a); code != 127 {
		t.Fatalf("ran under a planted root: %d", code)
	}
	b, _ := os.ReadFile(filepath.Join(a.JobDir, "launch_error.txt"))
	if !strings.Contains(string(b), "untrusted") {
		t.Fatalf("launch error %q", b)
	}
}

func TestExecRecreatesRootPrivately(t *testing.T) {
	d := t.TempDir()
	root := filepath.Join(d, "aq-root")
	a := ExecArgs{JobDir: filepath.Join(d, "job"), TmpDir: filepath.Join(root, "id"),
		MaxStdout: 10, MaxStderr: 10, Argv: []string{"true"}}
	if code := Exec(a); code != 0 {
		t.Fatal(code)
	}
	if st, err := os.Lstat(root); err != nil || st.Mode().Perm() != 0o700 {
		t.Fatal("root not private", st, err)
	}
	// A task dir that is a symlink is refused too.
	os.RemoveAll(a.TmpDir)
	os.Symlink(t.TempDir(), a.TmpDir)
	if code := Exec(a); code != 127 {
		t.Fatalf("followed a planted task dir: %d", code)
	}
}
