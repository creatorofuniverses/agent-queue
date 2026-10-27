package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"resource-dispatcher/internal/config"
	"resource-dispatcher/internal/platform/linux"
	"resource-dispatcher/internal/protocol"
	"resource-dispatcher/internal/units"
)

// Doctor failure codes.
const (
	codeCgroupV2Missing   = "CGROUP_V2_MISSING"
	codeControllerMissing = "CONTROLLER_MISSING"
	codeSliceMissing      = "SLICE_MISSING"
	codeProbeFailed       = "PROBE_FAILED"
	codeConfigInvalid     = "CONFIG_INVALID"
	codeDiskLow           = "DISK_LOW"
	codeDaemonUnavailable = "DAEMON_UNAVAILABLE"
	codeSystemdTooOld     = "SYSTEMD_TOO_OLD"
	codeKillDropinMissing = "KILL_DROPIN_MISSING"
)

// minSystemd is needed for `systemd-run --expand-environment=no`.
const minSystemd = 254

const (
	cgroupRoot   = "/sys/fs/cgroup"
	probeLimit   = 64 << 20
	probeTimeout = 30 * time.Second
)

// Check is one doctor result.
type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

// doctorSystem is everything doctor needs from the real machine, so tests can fake it.
type doctorSystem struct {
	UID       int
	ReadFile  func(path string) ([]byte, error)
	Exists    func(path string) bool
	Run       func(ctx context.Context, name string, args ...string) (string, error)
	DiskFree  func(path string) (uint64, error)
	MemTotal  func() (uint64, error)
	NumCPU    func() int
	RandomHex func() string
	Ping      func(ctx context.Context, socket string) error
}

func realDoctorSystem() doctorSystem {
	host := linux.NewHost("/proc")
	return doctorSystem{
		UID:      os.Getuid(),
		ReadFile: os.ReadFile,
		Exists:   func(p string) bool { _, err := os.Stat(p); return err == nil },
		Run: func(ctx context.Context, name string, args ...string) (string, error) {
			out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
			return string(out), err
		},
		DiskFree: host.DiskFree,
		MemTotal: func() (uint64, error) { m, err := host.Memory(); return m.Total, err },
		NumCPU:   host.NumCPU,
		RandomHex: func() string {
			var b [4]byte
			_, _ = rand.Read(b[:])
			return hex.EncodeToString(b[:])
		},
		Ping: func(ctx context.Context, socket string) error {
			return protocol.Call(ctx, socket, "ping", nil, nil)
		},
	}
}

// newDoctorSystem is replaced in tests.
var newDoctorSystem = realDoctorSystem

func cmdDoctor(c *cmdCtx, args []string) error {
	fs := c.newFlagSet("doctor")
	asJSON := fs.Bool("json", false, "print JSON")
	if pos, err := parseFlags(fs, args); err != nil {
		return err
	} else if len(pos) > 0 {
		return usagef("doctor: unexpected argument %q", pos[0])
	}
	paths, err := c.paths()
	if err != nil {
		return err
	}
	checks := runDoctor(c.ctx, newDoctorSystem(), paths, c.env("XDG_RUNTIME_DIR"))
	failed := false
	for _, ch := range checks {
		failed = failed || !ch.OK
	}
	if *asJSON {
		if err := printJSON(c, map[string]any{"checks": checks}); err != nil {
			return err
		}
	} else {
		for _, ch := range checks {
			if ch.OK {
				fmt.Fprintf(c.stdout, "ok    %-14s %s\n", ch.Name, ch.Detail)
			} else {
				fmt.Fprintf(c.stdout, "FAIL  %-14s %s: %s\n", ch.Name, ch.Code, ch.Detail)
			}
		}
	}
	if failed {
		return &exitError{code: exitFailed}
	}
	return nil
}

func runDoctor(ctx context.Context, sys doctorSystem, paths config.Paths, xdgRuntime string) []Check {
	var out []Check
	add := func(name, code string, err error, okDetail string) {
		if err != nil {
			out = append(out, Check{Name: name, Code: code, Detail: err.Error()})
			return
		}
		out = append(out, Check{Name: name, OK: true, Detail: okDetail})
	}

	cfg, cfgErr := config.Load(paths.ConfigFile)
	if cfgErr == nil {
		cfgErr = validateConfig(sys, cfg)
	} else {
		cfg = config.Default()
	}
	manager := managerCgroup(sys)

	_, err := sys.ReadFile(filepath.Join(cgroupRoot, "cgroup.controllers"))
	if err != nil {
		err = fmt.Errorf("%s/cgroup.controllers: %v", cgroupRoot, err)
	}
	add("cgroup_v2", codeCgroupV2Missing, err, "unified hierarchy")

	add("controllers", codeControllerMissing, checkControllers(sys, manager), "memory cpu pids delegated")

	slicePath := filepath.Join(cgroupRoot, manager, sliceRel(cfg.SliceName))
	var sliceErr error
	if !sys.Exists(slicePath) {
		sliceErr = fmt.Errorf("%s does not exist (start the daemon once)", slicePath)
	}
	add("slice", codeSliceMissing, sliceErr, cfg.SliceName)

	add("probe", codeProbeFailed, runProbe(ctx, sys, cfg), "memory.max enforced")
	add("config", codeConfigInvalid, cfgErr, paths.ConfigFile)

	var diskErr error
	if free, err := sys.DiskFree(existingParent(sys, paths.StateDir)); err != nil {
		diskErr = err
	} else if free < cfg.MinDiskFree {
		diskErr = fmt.Errorf("%s free in %s, need %s", units.FormatSize(free), paths.StateDir, units.FormatSize(cfg.MinDiskFree))
	}
	add("disk", codeDiskLow, diskErr, "enough free space")

	pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	add("daemon", codeDaemonUnavailable, sys.Ping(pctx, paths.Socket()), paths.Socket())

	ver, verErr := systemdVersion(ctx, sys)
	add("systemd", codeSystemdTooOld, verErr, fmt.Sprintf("version %d", ver))

	dropin := filepath.Join(xdgRuntime, "systemd", "user", cfg.UnitPrefix+".service.d", "99-aq-kill.conf")
	var dropErr error
	if xdgRuntime == "" || !sys.Exists(dropin) {
		dropErr = fmt.Errorf("%s missing (written by the daemon)", dropin)
	}
	add("kill_dropin", codeKillDropinMissing, dropErr, dropin)
	return out
}

func validateConfig(sys doctorSystem, cfg config.Config) error {
	total, err := sys.MemTotal()
	if err != nil {
		return err
	}
	return cfg.Validate(total, sys.NumCPU())
}

// managerCgroup is the user@UID.service cgroup relative to the cgroup root.
func managerCgroup(sys doctorSystem) string {
	if b, err := sys.ReadFile("/proc/self/cgroup"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			p, ok := strings.CutPrefix(line, "0::")
			if !ok {
				continue
			}
			parts := strings.Split(strings.Trim(strings.TrimSpace(p), "/"), "/")
			for i, part := range parts {
				if strings.HasPrefix(part, "user@") && strings.HasSuffix(part, ".service") {
					return strings.Join(parts[:i+1], "/")
				}
			}
		}
	}
	return fmt.Sprintf("user.slice/user-%d.slice/user@%d.service", sys.UID, sys.UID)
}

// sliceRel maps "a-b.slice" to "a.slice/a-b.slice".
func sliceRel(slice string) string {
	parts := strings.Split(strings.TrimSuffix(slice, ".slice"), "-")
	dirs := make([]string, len(parts))
	for i := range parts {
		dirs[i] = strings.Join(parts[:i+1], "-") + ".slice"
	}
	return filepath.Join(dirs...)
}

func checkControllers(sys doctorSystem, manager string) error {
	path := filepath.Join(cgroupRoot, manager, "cgroup.controllers")
	b, err := sys.ReadFile(path)
	if err != nil {
		return fmt.Errorf("%s: %v", path, err)
	}
	have := map[string]bool{}
	for _, f := range strings.Fields(string(b)) {
		have[f] = true
	}
	var missing []string
	for _, want := range []string{"memory", "cpu", "pids"} {
		if !have[want] {
			missing = append(missing, want)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("not delegated to the user manager: %s", strings.Join(missing, ", "))
	}
	return nil
}

// runProbe starts a throwaway unit with MemoryMax=64M and checks the kernel
// really applied it. The unit is always stopped afterwards.
func runProbe(ctx context.Context, sys doctorSystem, cfg config.Config) (err error) {
	unit := cfg.UnitPrefix + "probe-" + sys.RandomHex()
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	defer func() {
		// Cleanup must outlive a cancelled ctx.
		cctx, ccancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer ccancel()
		_, _ = sys.Run(cctx, "systemctl", "--user", "stop", unit+".service")
		_, _ = sys.Run(cctx, "systemctl", "--user", "reset-failed", unit+".service")
	}()
	out, runErr := sys.Run(ctx, "systemd-run", "--user", "--quiet", "--wait", "--collect", "--pipe",
		"--unit="+unit, "-p", "MemoryMax=64M",
		"/bin/sh", "-c", `cat "/sys/fs/cgroup$(sed -n 's/^0:://p' /proc/self/cgroup)/memory.max"`)
	if runErr != nil {
		return fmt.Errorf("systemd-run: %v: %s", runErr, strings.TrimSpace(out))
	}
	lines := strings.Fields(out)
	got := ""
	if len(lines) > 0 {
		got = lines[len(lines)-1]
	}
	if n, perr := strconv.ParseUint(got, 10, 64); perr != nil || n != probeLimit {
		return fmt.Errorf("probe memory.max = %q, want %d", got, probeLimit)
	}
	return nil
}

func systemdVersion(ctx context.Context, sys doctorSystem) (int, error) {
	out, err := sys.Run(ctx, "systemctl", "--version")
	if err != nil {
		return 0, fmt.Errorf("systemctl --version: %v", err)
	}
	first, _, _ := strings.Cut(out, "\n")
	f := strings.Fields(first)
	if len(f) < 2 || f[0] != "systemd" {
		return 0, fmt.Errorf("unrecognized systemctl --version output %q", first)
	}
	v, err := strconv.Atoi(f[1])
	if err != nil {
		return 0, fmt.Errorf("unrecognized systemd version %q", f[1])
	}
	if v < minSystemd {
		return v, fmt.Errorf("systemd %d is older than %d", v, minSystemd)
	}
	return v, nil
}

// existingParent walks up to the nearest existing directory (the state dir may not exist yet).
func existingParent(sys doctorSystem, p string) string {
	for !sys.Exists(p) && filepath.Dir(p) != p {
		p = filepath.Dir(p)
	}
	return p
}
