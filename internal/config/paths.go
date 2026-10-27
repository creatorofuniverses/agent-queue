package config

import (
	"errors"
	"os"
	"path/filepath"

	"resource-dispatcher/internal/model"
)

type Paths struct {
	StateDir, RuntimeDir, ConfigFile string
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

func (p Paths) JobDir(id model.ItemID) string { return filepath.Join(p.StateDir, "jobs", string(id)) }
func (p Paths) TmpDir(id model.ItemID) string { return filepath.Join(p.StateDir, "tmp", string(id)) }
