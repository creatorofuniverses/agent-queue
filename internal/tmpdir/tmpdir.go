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

// RemoveAll removes dir like os.RemoveAll, then, if a task left directories
// without write or search permission, restores u+rwx on the ones we own and
// retries. It never follows a symlink out of the tree.
func RemoveAll(dir string) error {
	err := os.RemoveAll(dir)
	if err == nil {
		return nil
	}
	uid := os.Getuid()
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, werr error) error {
		if d == nil || !d.IsDir() {
			// WalkDir reports a symlink as a non-dir and does not descend into it.
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		if sys, ok := info.Sys().(*syscall.Stat_t); ok && int(sys.Uid) == uid && info.Mode().Perm()&0o700 != 0o700 {
			_ = os.Chmod(path, info.Mode().Perm()|0o700)
			if werr != nil {
				// The dir was unreadable when WalkDir tried; walk it again now.
				_ = RemoveAll(path)
				return fs.SkipDir
			}
		}
		return nil
	})
	return os.RemoveAll(dir)
}
