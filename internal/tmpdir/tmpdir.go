// Package tmpdir guards the private root that holds task temp dirs and removes
// those dirs. The root may sit in a shared /tmp, where any user can plant a
// symlink or a dir of their own, and systemd-tmpfiles may delete it while idle,
// so its integrity is checked every time it is used, not once at daemon start.
package tmpdir

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// ErrUntrusted marks a root that exists but is not ours to use: a symlink, a
// non-directory, or a dir owned by another user. Other errors (ENOSPC, EACCES on
// the parent) are about creating it right now and may clear later.
var ErrUntrusted = errors.New("untrusted temp root")

// CheckRoot reports whether root may be used without changing anything: nil
// for a dir owned by uid or for a missing root (it is made at launch),
// ErrUntrusted for a planted one.
func CheckRoot(root string, uid int) error {
	st, err := os.Lstat(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return trusted(root, st, uid)
}

func trusted(root string, st fs.FileInfo, uid int) error {
	if !st.IsDir() {
		return fmt.Errorf("%w: %s is not a directory", ErrUntrusted, root)
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); !ok || int(sys.Uid) != uid {
		return fmt.Errorf("%w: %s is owned by another user", ErrUntrusted, root)
	}
	return nil
}

// EnsureRoot makes root a directory owned by uid with mode 0700. It creates only
// root itself, never a missing parent, refuses a planted root, and tightens the
// mode of one we own.
func EnsureRoot(root string, uid int) error {
	if err := os.Mkdir(root, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	st, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if err := trusted(root, st, uid); err != nil {
		return err
	}
	if st.Mode().Perm() != 0o700 {
		return os.Chmod(root, 0o700)
	}
	return nil
}

// Dir is a validated temp root, opened as a file descriptor: every operation
// stays inside the directory that passed the check, even if its path is later
// replaced by a symlink.
type Dir struct {
	root *os.Root
	path string
}

// Open opens root for enumeration and removal. The parent is opened by path,
// root through it; root must not be a symlink, must be a directory owned by
// uid and writable by no one else, and the opened directory must be the one
// that was checked. A missing root returns an error wrapping fs.ErrNotExist;
// anything else that fails the check wraps ErrUntrusted.
func Open(root string, uid int) (*Dir, error) {
	parent, err := os.OpenRoot(filepath.Dir(root))
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	name := filepath.Base(root)
	before, err := parent.Lstat(name)
	if err != nil {
		return nil, err
	}
	if before.Mode()&fs.ModeSymlink != 0 {
		return nil, fmt.Errorf("%w: %s is a symlink", ErrUntrusted, root)
	}
	if err := trusted(root, before, uid); err != nil {
		return nil, err
	}
	if before.Mode().Perm()&0o022 != 0 {
		return nil, fmt.Errorf("%w: %s is writable by others", ErrUntrusted, root)
	}
	r, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	after, err := r.Stat(".")
	if err != nil || !os.SameFile(before, after) {
		r.Close()
		return nil, fmt.Errorf("%w: %s changed while it was opened", ErrUntrusted, root)
	}
	return &Dir{root: r, path: root}, nil
}

// Close releases the root.
func (d *Dir) Close() error { return d.root.Close() }

// Entries lists the root's entries without following symlinks.
func (d *Dir) Entries() ([]fs.DirEntry, error) { return fs.ReadDir(d.root.FS(), ".") }

// Remove removes the entry name, which must be a single path component, with
// everything under it. If a task left directories without write or search
// permission, it restores u+rwx on the ones we own and retries. Nothing outside
// the root is touched: symlinks are removed, never followed.
func (d *Dir) Remove(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsRune(name, '/') {
		return fmt.Errorf("tmpdir: %q is not an entry name", name)
	}
	err := d.root.RemoveAll(name)
	if err == nil {
		return nil
	}
	uid := os.Getuid()
	_ = fs.WalkDir(d.root.FS(), name, func(path string, de fs.DirEntry, werr error) error {
		if de == nil || !de.IsDir() {
			// WalkDir reports a symlink as a non-dir and does not descend into it.
			return nil
		}
		info, ierr := de.Info()
		if ierr != nil {
			return nil
		}
		if sys, ok := info.Sys().(*syscall.Stat_t); ok && int(sys.Uid) == uid && info.Mode().Perm()&0o700 != 0o700 {
			_ = d.root.Chmod(path, info.Mode().Perm()|0o700)
			if werr != nil {
				// The dir was unreadable when WalkDir tried; walk it again now.
				_ = d.chmodTree(path, uid)
				return fs.SkipDir
			}
		}
		return nil
	})
	return d.root.RemoveAll(name)
}

// chmodTree restores u+rwx on owned directories under path, inside the root.
func (d *Dir) chmodTree(path string, uid int) error {
	return fs.WalkDir(d.root.FS(), path, func(p string, de fs.DirEntry, werr error) error {
		if de == nil || !de.IsDir() {
			return nil
		}
		info, ierr := de.Info()
		if ierr != nil {
			return nil
		}
		if sys, ok := info.Sys().(*syscall.Stat_t); ok && int(sys.Uid) == uid && info.Mode().Perm()&0o700 != 0o700 {
			_ = d.root.Chmod(p, info.Mode().Perm()|0o700)
			if werr != nil && p != path {
				_ = d.chmodTree(p, uid)
				return fs.SkipDir
			}
		}
		return nil
	})
}

// RemoveIn removes the entry name under root, after validating root as Open
// does. A missing root or entry is not an error: there is nothing to clean.
func RemoveIn(root, name string, uid int) error {
	d, err := Open(root, uid)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Remove(name)
}
