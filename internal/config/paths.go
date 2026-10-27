package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"resource-dispatcher/internal/model"
)

type Paths struct {
	StateDir, RuntimeDir, ConfigFile string
	// SliceLockDir holds the per-slice daemon lock; it does not follow AQ_RUNTIME_DIR,
	// so daemons with different runtime dirs still exclude each other on one slice.
	SliceLockDir string
}

// SliceLockDirFor is xdgRuntimeDir, or /run/user/<uid> when it is unset.
func SliceLockDirFor(xdgRuntimeDir string) string {
	if xdgRuntimeDir != "" {
		return xdgRuntimeDir
	}
	return fmt.Sprintf("/run/user/%d", os.Getuid())
}

// DefaultPaths resolves directories, honoring AQ_STATE_DIR, AQ_RUNTIME_DIR, AQ_CONFIG.
func DefaultPaths() (Paths, error) {
	var p Paths
	p.StateDir = os.Getenv("AQ_STATE_DIR")
	p.RuntimeDir = os.Getenv("AQ_RUNTIME_DIR")
	p.ConfigFile = os.Getenv("AQ_CONFIG")

	if p.StateDir == "" || p.ConfigFile == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return Paths{}, err
		}
		if p.StateDir == "" {
			p.StateDir = filepath.Join(home, ".local", "state", "agent-queue")
		}
		if p.ConfigFile == "" {
			p.ConfigFile = filepath.Join(home, ".config", "agent-queue", "config.toml")
		}
	}
	p.SliceLockDir = SliceLockDirFor(os.Getenv("XDG_RUNTIME_DIR"))
	if p.RuntimeDir == "" {
		xdg := os.Getenv("XDG_RUNTIME_DIR")
		if xdg == "" {
			return Paths{}, errors.New("XDG_RUNTIME_DIR is not set; set AQ_RUNTIME_DIR")
		}
		p.RuntimeDir = filepath.Join(xdg, "agent-queue")
	}
	return p, nil
}

func (p Paths) DB() string     { return filepath.Join(p.StateDir, "aq.db") }
func (p Paths) Socket() string { return filepath.Join(p.RuntimeDir, "aq.sock") }
func (p Paths) Lock() string   { return filepath.Join(p.RuntimeDir, "daemon.lock") }

func (p Paths) SliceLock(slice string) string {
	return filepath.Join(p.SliceLockDir, "agent-queue."+slice+".lock")
}

func (p Paths) JobDir(id model.ItemID) string { return filepath.Join(p.StateDir, "jobs", string(id)) }
func (p Paths) TmpDir(id model.ItemID) string { return filepath.Join(p.StateDir, "tmp", string(id)) }
