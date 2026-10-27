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
	"unicode/utf8"

	"resource-dispatcher/internal/model"
	"resource-dispatcher/internal/platform"
	"resource-dispatcher/internal/runner"
)

type ExecutorConfig struct {
	SliceName, UnitPrefix, AqBinary string
	TeardownSlice                   string   // slice of teardown units (model.IsTeardownUnitID)
	CgroupRoot                      string   // "/sys/fs/cgroup" when empty
	RuntimeDir                      string   // $XDG_RUNTIME_DIR when empty
	ExtraProperties                 []string // tests only: appended as -p after the standard properties
}

// Executor runs tasks as transient systemd user units in a slice and reads their cgroups.
type Executor struct {
	cfg     ExecutorConfig
	manager string // user@UID.service cgroup, relative to CgroupRoot
	runCmd  func(ctx context.Context, name string, args ...string) error
}

// launchTimeout bounds systemd-run and the cleanup after a failed launch.
const launchTimeout = 30 * time.Second

var _ platform.Executor = (*Executor)(nil)

func NewExecutor(cfg ExecutorConfig) (*Executor, error) {
	if cfg.SliceName == "" || cfg.UnitPrefix == "" || cfg.AqBinary == "" {
		return nil, errors.New("linux executor: SliceName, UnitPrefix and AqBinary are required")
	}
	if cfg.TeardownSlice == "" || cfg.TeardownSlice == cfg.SliceName {
		return nil, errors.New("linux executor: TeardownSlice is required and must differ from SliceName")
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
	if cfg.RuntimeDir == "" {
		cfg.RuntimeDir = os.Getenv("XDG_RUNTIME_DIR")
	}
	return &Executor{cfg: cfg, manager: manager, runCmd: run}
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
	high := spec.MemoryHigh
	if high == 0 || high > spec.Limit {
		high = spec.Limit // an unset or oversized MemoryHigh must not throttle below the limit
	}
	args := []string{"--user", "--quiet", "--collect", "--expand-environment=no",
		"--unit", cfg.UnitPrefix + string(spec.ID), "--slice", cfg.unitSlice(spec.ID)}
	oomAdjust := "900" // the kernel's first choice in a pinch, before anything else on the host
	if model.IsTeardownUnitID(spec.ID) {
		oomAdjust = "0" // the script that frees a stand must not be the one killed
	}
	props := []string{
		"MemoryMax=" + strconv.FormatUint(spec.Limit, 10),
		"MemoryHigh=" + strconv.FormatUint(high, 10),
		"MemorySwapMax=0",
		"CPUQuota=" + strconv.Itoa(spec.CPUs*100) + "%",
		"KillMode=control-group",
		"OOMPolicy=kill",
		"OOMScoreAdjust=" + oomAdjust,
		"RuntimeMaxSec=" + ceilSeconds(spec.Timeout),
		"TimeoutStopSec=" + ceilSeconds(spec.KillGrace),
		// SIGABRT can be ignored and dumps core; on Fedora the drop-in from
		// EnsureSlice is what makes this stick (see writeKillDropIn).
		"TimeoutStopFailureMode=kill",
		"ExecStopPost=" + execQuote(cfg.AqBinary) + " _record-result " + execQuote(spec.JobDir),
	}
	for _, p := range append(props, cfg.ExtraProperties...) {
		args = append(args, "-p", p)
	}
	keys := make([]string, 0, len(spec.Env))
	for k := range spec.Env {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		args = append(args, "--setenv", k+"="+spec.Env[k])
	}
	// The cwd goes to _exec, not to the unit: WorkingDirectory= also applies to
	// ExecStopPost, which would fail (200/CHDIR) if the job deleted its cwd.
	args = append(args, "--", cfg.AqBinary, "_exec",
		"--job-dir", spec.JobDir, "--tmp-dir", spec.TmpDir, "--cwd", spec.Cwd,
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

// validateEnv rejects what systemd-run would refuse; errors name the key only.
func validateEnv(env map[string]string) error {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		ok := k != ""
		for i, c := range k {
			if !(c == '_' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || i > 0 && c >= '0' && c <= '9') {
				ok = false
			}
		}
		if !ok {
			return fmt.Errorf("invalid environment variable name %q", k)
		}
		if v := env[k]; !utf8.ValidString(v) || strings.ContainsRune(v, 0) {
			return fmt.Errorf("environment variable %s: value is not valid UTF-8 or contains NUL", k)
		}
	}
	return nil
}

// redactEnv removes environment values from systemd-run's stderr: the value part of
// "environment variable K=V: ..." lines, and any occurrence of a value of 4+ bytes.
func redactEnv(s string, env map[string]string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if !strings.Contains(l, "environment variable") {
			continue
		}
		if eq := strings.Index(l, "="); eq >= 0 {
			rest := ""
			if j := strings.LastIndex(l, ": "); j > eq {
				rest = l[j:]
			}
			lines[i] = l[:eq+1] + "<redacted>" + rest
		}
	}
	s = strings.Join(lines, "\n")
	for _, v := range env {
		if len(v) >= 4 {
			s = strings.ReplaceAll(s, v, "<redacted>")
		}
	}
	return s
}

// detached outlives the caller's cancellation: once systemd-run is under way the
// manager may start the unit, so the call and any cleanup must finish.
func detached(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), launchTimeout)
}

// Launch starts the unit synchronously. systemd-run rejects invalid properties before
// anything starts; afterwards the unit's memory.max is checked against spec.Limit and
// the unit is stopped if the limit is not in place (the command may have begun by then).
// A unit whose cgroup is already gone finished before the check and is accepted.
// On any failure the unit is stopped (a no-op if it never started).
func (e *Executor) Launch(ctx context.Context, spec platform.LaunchSpec) (string, error) {
	if err := validateEnv(spec.Env); err != nil {
		return "", err
	}
	if err := os.MkdirAll(spec.JobDir, 0o700); err != nil {
		return "", err
	}
	unit := e.unitName(spec.ID)
	runCtx, cancel := detached(ctx)
	err := e.runCmd(runCtx, "systemd-run", SystemdRunArgs(e.cfg, spec)...)
	cancel()
	if err != nil {
		err = errors.New(redactEnv(err.Error(), spec.Env))
	} else if verr := e.verifyLimit(spec.ID, spec.Limit); verr != nil {
		err = fmt.Errorf("%s started without its memory limit, stopped: %w", unit, verr)
	}
	if err != nil {
		stopCtx, cancel := detached(ctx)
		defer cancel()
		return "", errors.Join(err, e.Stop(stopCtx, spec.ID))
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
	err := e.runCmd(ctx, "systemctl", "--user", "stop", e.unitName(id))
	if err != nil {
		if _, serr := os.Stat(e.unitDir(id)); errors.Is(serr, fs.ErrNotExist) {
			return nil
		}
	}
	return err
}

const killDropIn = "[Service]\nTimeoutStopFailureMode=kill\n"

// writeKillDropIn installs a runtime drop-in for all <prefix>*.service units that
// overrides Fedora's service.d/10-timeout-abort.conf (SIGABRT + core dump on stop timeout).
// Drop-ins beat transient properties, so the -p value alone is not enough there.
func (e *Executor) writeKillDropIn() error {
	if e.cfg.RuntimeDir == "" {
		return errors.New("linux executor: XDG_RUNTIME_DIR is not set")
	}
	dir := filepath.Join(e.cfg.RuntimeDir, "systemd", "user", e.cfg.UnitPrefix+".service.d")
	path := filepath.Join(dir, "99-aq-kill.conf")
	if b, err := os.ReadFile(path); err == nil && string(b) == killDropIn {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".99-aq-kill.conf.*")
	if err != nil {
		return err
	}
	_, err = f.WriteString(killDropIn)
	if err == nil {
		err = f.Chmod(0o644)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		os.Remove(f.Name())
	}
	return err
}

// EnsureSlice installs the stop-mode drop-in, starts the slice and sets its pool limit.
func (e *Executor) EnsureSlice(ctx context.Context, poolMemory uint64) error {
	if err := e.writeKillDropIn(); err != nil {
		return err
	}
	return e.ensureSlice(ctx, e.cfg.SliceName, poolMemory)
}

// EnsureTeardownSlice starts the teardown slice and sets its limit. The drop-in from
// EnsureSlice covers teardown units too: they share the unit prefix.
func (e *Executor) EnsureTeardownSlice(ctx context.Context, max uint64) error {
	return e.ensureSlice(ctx, e.cfg.TeardownSlice, max)
}

func (e *Executor) ensureSlice(ctx context.Context, slice string, max uint64) error {
	if err := e.runCmd(ctx, "systemctl", "--user", "start", slice); err != nil {
		return err
	}
	return e.runCmd(ctx, "systemctl", "--user", "set-property", "--runtime", slice,
		"MemoryMax="+strconv.FormatUint(max, 10), "MemorySwapMax=0")
}
