package linux

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"resource-dispatcher/internal/model"
	"resource-dispatcher/internal/platform"
	"resource-dispatcher/internal/runner"
)

type ExecutorConfig struct {
	SliceName, UnitPrefix, AqBinary string
	CgroupRoot                      string   // "/sys/fs/cgroup" when empty
	ExtraProperties                 []string // tests only: appended as -p after the standard properties
}

// Executor runs tasks as transient systemd user units in a slice and reads their cgroups.
type Executor struct {
	cfg     ExecutorConfig
	manager string // user@UID.service cgroup, relative to CgroupRoot
}

var _ platform.Executor = (*Executor)(nil)

func NewExecutor(cfg ExecutorConfig) (*Executor, error) {
	if cfg.SliceName == "" || cfg.UnitPrefix == "" || cfg.AqBinary == "" {
		return nil, errors.New("linux executor: SliceName, UnitPrefix and AqBinary are required")
	}
	self, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return nil, err
	}
	e := newExecutor(cfg, userManagerCgroup(string(self), os.Getuid()))
	dir := filepath.Join(e.cfg.CgroupRoot, e.manager)
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("linux executor: user manager cgroup %s not found", dir)
	}
	return e, nil
}

func newExecutor(cfg ExecutorConfig, manager string) *Executor {
	if cfg.CgroupRoot == "" {
		cfg.CgroupRoot = "/sys/fs/cgroup"
	}
	return &Executor{cfg: cfg, manager: manager}
}

func (e *Executor) Capabilities() platform.Capabilities {
	return platform.Capabilities{HardMemoryLimit: true, TreeKill: true, OOMAttribution: true, SurvivesDaemonRestart: true}
}

// ceilSeconds rounds up to whole seconds, at least 1 (0 means "no limit" to systemd).
func ceilSeconds(d time.Duration) string {
	s := int64((d + time.Second - 1) / time.Second)
	return strconv.FormatInt(max(s, 1), 10)
}

// execQuote quotes one word of a systemd Exec*= line when it needs it; "$" is doubled
// because systemd expands environment variables there.
func execQuote(s string) string {
	if !strings.ContainsAny(s, " \t\n\"'\\$;") {
		return s
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `$$`)
	return `"` + r.Replace(s) + `"`
}

// SystemdRunArgs builds the systemd-run argv (without "systemd-run") for spec.
func SystemdRunArgs(cfg ExecutorConfig, spec platform.LaunchSpec) []string {
	args := []string{"--user", "--quiet", "--collect", "--expand-environment=no",
		"--unit", cfg.UnitPrefix + string(spec.ID), "--slice", cfg.SliceName}
	props := []string{
		"MemoryMax=" + strconv.FormatUint(spec.Limit, 10),
		"MemoryHigh=" + strconv.FormatUint(spec.MemoryHigh, 10),
		"MemorySwapMax=0",
		"CPUQuota=" + strconv.Itoa(spec.CPUs*100) + "%",
		"KillMode=control-group",
		"OOMPolicy=kill",
		"OOMScoreAdjust=900",
		"RuntimeMaxSec=" + ceilSeconds(spec.Timeout),
		"TimeoutStopSec=" + ceilSeconds(spec.KillGrace),
		"ExecStopPost=" + execQuote(cfg.AqBinary) + " _record-result " + execQuote(spec.JobDir),
	}
	for _, p := range append(props, cfg.ExtraProperties...) {
		args = append(args, "-p", p)
	}
	if spec.Cwd != "" {
		args = append(args, "--working-directory", spec.Cwd)
	}
	keys := make([]string, 0, len(spec.Env))
	for k := range spec.Env {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		args = append(args, "--setenv", k+"="+spec.Env[k])
	}
	args = append(args, "--", cfg.AqBinary, "_exec",
		"--job-dir", spec.JobDir, "--tmp-dir", spec.TmpDir,
		"--max-stdout", strconv.FormatUint(spec.MaxStdout, 10),
		"--max-stderr", strconv.FormatUint(spec.MaxStderr, 10), "--")
	return append(args, spec.Argv...)
}

// run executes a command; the error carries its stderr, never its arguments (they may hold env values).
func run(ctx context.Context, name string, args ...string) error {
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// Launch starts the unit synchronously. systemd-run rejects invalid properties before
// anything starts; afterwards the unit's memory.max is checked against spec.Limit and
// the unit is stopped if the limit is not in place (the command may have begun by then).
// A unit whose cgroup is already gone finished before the check and is accepted.
func (e *Executor) Launch(ctx context.Context, spec platform.LaunchSpec) (string, error) {
	if err := os.MkdirAll(spec.JobDir, 0o700); err != nil {
		return "", err
	}
	unit := e.unitName(spec.ID)
	if err := run(ctx, "systemd-run", SystemdRunArgs(e.cfg, spec)...); err != nil {
		return "", err
	}
	if err := e.verifyLimit(spec.ID, spec.Limit); err != nil {
		stopErr := e.Stop(ctx, spec.ID)
		return "", errors.Join(fmt.Errorf("%s started without its memory limit, stopped: %w", unit, err), stopErr)
	}
	return unit, nil
}

func (e *Executor) verifyLimit(id model.ItemID, limit uint64) error {
	dir := e.unitDir(id)
	b, err := os.ReadFile(filepath.Join(dir, "memory.max"))
	if err != nil {
		if _, serr := os.Stat(dir); errors.Is(serr, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	page := uint64(os.Getpagesize())
	if got, want := strings.TrimSpace(string(b)), strconv.FormatUint(limit/page*page, 10); got != want {
		return fmt.Errorf("memory.max is %s, want %s", got, want)
	}
	return nil
}

func (e *Executor) ReadResult(jobDir string) (*platform.ExitInfo, error) {
	return runner.ReadResultFile(jobDir)
}

// Stop blocks until systemd has stopped the unit (TERM to the cgroup, KILL after TimeoutStopSec).
// Stopping a unit whose cgroup no longer exists is not an error.
func (e *Executor) Stop(ctx context.Context, id model.ItemID) error {
	err := run(ctx, "systemctl", "--user", "stop", e.unitName(id))
	if err != nil {
		if _, serr := os.Stat(e.unitDir(id)); errors.Is(serr, fs.ErrNotExist) {
			return nil
		}
	}
	return err
}

func (e *Executor) EnsureSlice(ctx context.Context, poolMemory uint64) error {
	if err := run(ctx, "systemctl", "--user", "start", e.cfg.SliceName); err != nil {
		return err
	}
	return run(ctx, "systemctl", "--user", "set-property", "--runtime", e.cfg.SliceName,
		"MemoryMax="+strconv.FormatUint(poolMemory, 10), "MemorySwapMax=0")
}
