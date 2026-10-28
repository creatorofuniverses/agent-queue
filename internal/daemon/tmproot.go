package daemon

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/creatorofuniverses/agent-queue/internal/config"
	"github.com/creatorofuniverses/agent-queue/internal/platform"
)

// TmpPlan is where task temp dirs live. An empty Root means the state dir
// (config.Paths.TmpBase), with Reason saying why.
type TmpPlan struct {
	Root   string `json:"root"`
	Base   string `json:"base"`
	Reason string `json:"reason,omitempty"`
}

// PlanTmpRoot puts task temp dirs in <tmp_dir or /tmp>/aq-<uid>-<slice> when
// that is tmpfs, or whenever tmp_dir is set: temp files on the state dir's disk
// wear the SSD for data nobody keeps. The slice in the name keeps daemons apart
// (one per slice, by the slice lock): each sweeps its root of dirs it does not
// know. With create it makes the private root, refusing a symlink or a dir owned
// by someone else (a shared /tmp lets any user plant one).
func PlanTmpRoot(cfg config.Config, uid int, fsUsage func(string) (platform.FSUsage, error), create bool) TmpPlan {
	p := TmpPlan{Base: cfg.TmpDir}
	if p.Base == "" {
		p.Base = "/tmp"
	}
	u, err := fsUsage(p.Base)
	if err != nil {
		p.Reason = fmt.Sprintf("%s: %v", p.Base, err)
		return p
	}
	if !u.Tmpfs && cfg.TmpDir == "" {
		p.Reason = p.Base + " is not tmpfs; set tmp_dir to use it anyway"
		return p
	}
	root := filepath.Join(p.Base, fmt.Sprintf("aq-%d-%s", uid, strings.TrimSuffix(cfg.SliceName, ".slice")))
	if create {
		if err := os.Mkdir(root, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			p.Reason = err.Error()
			return p
		}
	}
	st, err := os.Lstat(root)
	switch {
	case errors.Is(err, fs.ErrNotExist) && !create:
		p.Root = root // the daemon creates it at start
		return p
	case err != nil:
		p.Reason = err.Error()
		return p
	case !st.IsDir():
		p.Reason = root + " is not a directory"
		return p
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); !ok || int(sys.Uid) != uid {
		p.Reason = fmt.Sprintf("%s is owned by another user", root)
		return p
	}
	if create && st.Mode().Perm() != 0o700 {
		if err := os.Chmod(root, 0o700); err != nil {
			p.Reason = err.Error()
			return p
		}
	}
	p.Root = root
	return p
}
