package daemon

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/creatorofuniverses/agent-queue/internal/config"
	"github.com/creatorofuniverses/agent-queue/internal/platform"
	"github.com/creatorofuniverses/agent-queue/internal/tmpdir"
)

// TmpPlan is where task temp dirs live. An empty Root means the state dir
// (config.Paths.TmpBase), with Reason saying why. A Root with a Reason is
// planned but could not be made yet (a full /tmp): tasks still go there, the
// temp gate holds the queue, and the runner makes the root at the next launch.
type TmpPlan struct {
	Root   string `json:"root"`
	Base   string `json:"base"`
	Reason string `json:"reason,omitempty"`
}

// PlanTmpRoot puts task temp dirs in <tmp_dir or /tmp>/aq-<uid>-<slice> when
// that is tmpfs, or whenever tmp_dir is set: temp files on the state dir's disk
// wear the SSD for data nobody keeps. The slice in the name keeps daemons apart
// (one per slice, by the slice lock): each sweeps its root of dirs it does not
// know. With create it makes the private root. Only a planted root (a symlink
// or someone else's dir: a shared /tmp lets any user plant one) sends temp
// dirs to the state dir; a root that cannot be made right now stays planned,
// since falling back would keep tasks on the SSD for the daemon's lifetime.
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
		err = tmpdir.EnsureRoot(root, uid)
	} else {
		err = tmpdir.CheckRoot(root, uid)
	}
	if errors.Is(err, tmpdir.ErrUntrusted) {
		p.Reason = err.Error()
		return p
	}
	p.Root = root
	if err != nil {
		p.Reason = "not created yet: " + err.Error()
	}
	return p
}
