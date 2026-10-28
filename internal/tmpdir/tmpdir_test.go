package tmpdir

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureRootCreatesPrivateDir(t *testing.T) {
	root := filepath.Join(t.TempDir(), "aq-root")
	if err := EnsureRoot(root, os.Getuid()); err != nil {
		t.Fatal(err)
	}
	if m := perm(t, root); m != 0o700 {
		t.Fatalf("mode %o", m)
	}
	// Reuse tightens a loosened mode.
	os.Chmod(root, 0o777)
	if err := EnsureRoot(root, os.Getuid()); err != nil || perm(t, root) != 0o700 {
		t.Fatal("mode not restored", err)
	}
}

func TestEnsureRootMakesOnlyTheRoot(t *testing.T) {
	// The parent is not ours to create: MkdirAll would build a path an attacker chose.
	root := filepath.Join(t.TempDir(), "missing", "aq-root")
	if err := EnsureRoot(root, os.Getuid()); err == nil {
		t.Fatal("created a missing parent")
	}
}

func TestEnsureRootRefusesPlantedRoots(t *testing.T) {
	base := t.TempDir()
	link := filepath.Join(base, "link")
	os.Symlink(t.TempDir(), link)
	if err := EnsureRoot(link, os.Getuid()); !errors.Is(err, ErrUntrusted) {
		t.Fatalf("symlink: %v", err)
	}
	file := filepath.Join(base, "file")
	os.WriteFile(file, nil, 0o600)
	if err := EnsureRoot(file, os.Getuid()); !errors.Is(err, ErrUntrusted) {
		t.Fatalf("file: %v", err)
	}
	dir := filepath.Join(base, "dir")
	os.Mkdir(dir, 0o777)
	os.Chmod(dir, 0o777)
	if err := EnsureRoot(dir, os.Getuid()+1); !errors.Is(err, ErrUntrusted) {
		t.Fatalf("foreign owner: %v", err)
	}
	if perm(t, dir) != 0o777 {
		t.Fatal("touched a dir we refused")
	}
}

func TestEnsureRootTransientErrorIsNotUntrusted(t *testing.T) {
	parent := t.TempDir()
	os.Chmod(parent, 0o500)
	t.Cleanup(func() { os.Chmod(parent, 0o700) })
	err := EnsureRoot(filepath.Join(parent, "aq-root"), os.Getuid())
	if err == nil || errors.Is(err, ErrUntrusted) {
		t.Fatalf("want a plain create error, got %v", err)
	}
}

func TestRemoveInReadOnlyDirs(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "task")
	sub := filepath.Join(dir, "ro", "deeper")
	os.MkdirAll(sub, 0o700)
	os.WriteFile(filepath.Join(sub, "big"), []byte("x"), 0o400)
	os.Chmod(sub, 0o500)
	os.Chmod(filepath.Join(dir, "ro"), 0o000)
	if err := RemoveIn(root, "task", os.Getuid()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatal("dir left behind", err)
	}
}

func TestRemoveInDoesNotFollowSymlinks(t *testing.T) {
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "keep"), nil, 0o600)
	os.Chmod(outside, 0o500)
	t.Cleanup(func() { os.Chmod(outside, 0o700) })
	root := t.TempDir()
	dir := filepath.Join(root, "task")
	os.Mkdir(dir, 0o700)
	os.Symlink(outside, filepath.Join(dir, "link"))
	ro := filepath.Join(dir, "ro")
	os.Mkdir(ro, 0o700)
	os.WriteFile(filepath.Join(ro, "f"), nil, 0o600)
	os.Chmod(ro, 0o500)
	if err := RemoveIn(root, "task", os.Getuid()); err != nil {
		t.Fatal(err)
	}
	if perm(t, outside) != 0o500 {
		t.Fatal("chmod followed a symlink out of the tree")
	}
	if _, err := os.Stat(filepath.Join(outside, "keep")); err != nil {
		t.Fatal("removed through a symlink", err)
	}
}

func TestRemoveInMissing(t *testing.T) {
	root := t.TempDir()
	if err := RemoveIn(root, "gone", os.Getuid()); err != nil {
		t.Fatal(err)
	}
	if err := RemoveIn(filepath.Join(root, "no-root"), "gone", os.Getuid()); err != nil {
		t.Fatal(err)
	}
}

func TestRemoveInRejectsPaths(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"", ".", "..", "a/b", "../x"} {
		if err := RemoveIn(root, name, os.Getuid()); err == nil {
			t.Fatalf("%q accepted", name)
		}
	}
}

// A root replaced by a symlink, absolute or relative, must not be cleaned: the
// link could point at the home dir or the job dirs. Nothing behind it goes.
func TestOpenRefusesReplacedRoot(t *testing.T) {
	parent := t.TempDir()
	victim := t.TempDir()
	sibling := filepath.Join(parent, "sibling")
	for _, d := range []string{victim, sibling} {
		os.MkdirAll(filepath.Join(d, "keep"), 0o700)
	}
	cases := map[string]string{"absolute": victim, "relative": "sibling"}
	for name, target := range cases {
		root := filepath.Join(parent, "aq-"+name)
		os.Symlink(target, root)
		if d, err := Open(root, os.Getuid()); err == nil || !errors.Is(err, ErrUntrusted) {
			if d != nil {
				d.Close()
			}
			t.Fatalf("%s: want ErrUntrusted, got %v", name, err)
		}
		if err := RemoveIn(root, "keep", os.Getuid()); err == nil {
			t.Fatalf("%s: removal through a replaced root reported success", name)
		}
	}
	for _, d := range []string{victim, sibling} {
		if _, err := os.Stat(filepath.Join(d, "keep")); err != nil {
			t.Fatal("removed through a replaced root", d, err)
		}
	}
}

// Group- or world-writable roots can be swapped from under us: refuse them.
func TestOpenRefusesWritableByOthers(t *testing.T) {
	root := filepath.Join(t.TempDir(), "aq")
	os.Mkdir(root, 0o700)
	os.Chmod(root, 0o777)
	if _, err := Open(root, os.Getuid()); !errors.Is(err, ErrUntrusted) {
		t.Fatalf("want ErrUntrusted, got %v", err)
	}
}

func TestDirEntriesAndRemove(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "a", "b"), 0o700)
	os.WriteFile(filepath.Join(root, "f"), nil, 0o600)
	d, err := Open(root, os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ents, err := d.Entries()
	if err != nil || len(ents) != 2 {
		t.Fatal(ents, err)
	}
	if err := d.Remove("a"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(root, "a")); !os.IsNotExist(err) {
		t.Fatal("entry left behind", err)
	}
}

func perm(t *testing.T, p string) os.FileMode {
	t.Helper()
	st, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return st.Mode().Perm()
}

func TestCheckRoot(t *testing.T) {
	base := t.TempDir()
	if err := CheckRoot(filepath.Join(base, "missing"), os.Getuid()); err != nil {
		t.Fatal("a missing root is made at launch, not refused:", err)
	}
	link := filepath.Join(base, "link")
	os.Symlink(t.TempDir(), link)
	if err := CheckRoot(link, os.Getuid()); !errors.Is(err, ErrUntrusted) {
		t.Fatal(err)
	}
	if err := CheckRoot(base, os.Getuid()+1); !errors.Is(err, ErrUntrusted) {
		t.Fatal(err)
	}
	if err := CheckRoot(base, os.Getuid()); err != nil {
		t.Fatal(err)
	}
}
