package linux

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/creatorofuniverses/agent-queue/internal/platform"
)

// DiskMeterConfig configures the raw disk-write reader.
type DiskMeterConfig struct {
	ProcRoot, CgroupRoot string // defaults /proc and /sys/fs/cgroup
	DevRoot              string // default /sys/dev/block
	Slices               []string
	// Containers maps a container id to its source key ("docker:<project>").
	// Without it, or when it fails, a container is "docker:<id[:12]>".
	Containers func(ctx context.Context) (map[string]string, error)
}

// DiskMeter reads cumulative physical-disk write counters of the host and of
// the cgroups that form level-1 sources. It keeps no counters of its own.
type DiskMeter struct {
	cfg     DiskMeterConfig
	manager string // user@UID.service cgroup, relative to the cgroup root

	mu       sync.Mutex
	lastWarn time.Time
}

func NewDiskMeter(cfg DiskMeterConfig) *DiskMeter {
	if cfg.ProcRoot == "" {
		cfg.ProcRoot = "/proc"
	}
	if cfg.CgroupRoot == "" {
		cfg.CgroupRoot = "/sys/fs/cgroup"
	}
	if cfg.DevRoot == "" {
		cfg.DevRoot = "/sys/dev/block"
	}
	b, _ := os.ReadFile(filepath.Join(cfg.ProcRoot, "self/cgroup"))
	return &DiskMeter{cfg: cfg, manager: userManagerCgroup(string(b), os.Getuid())}
}

var _ platform.DiskMeter = (*DiskMeter)(nil)

// Snapshot reads the counters, by physical disk. A source without io.stat (no
// io controller) is skipped rather than counted as zero; only an unreadable
// diskstats is an error. Parents are raw: the accountant subtracts their
// children, so a child re-created under them (an aq slice, the user manager)
// cannot make a parent's remainder jump.
func (m *DiskMeter) Snapshot(ctx context.Context) (platform.DiskSnapshot, error) {
	host, devs, err := m.hostWrites()
	if err != nil {
		return platform.DiskSnapshot{}, err
	}
	containers := m.containers(ctx)
	snap := platform.DiskSnapshot{Host: host, Devices: devs, Parents: map[string]uint64{}, ParentDevices: map[string]map[string]uint64{}}
	root := m.cfg.CgroupRoot
	read := func(rel string) (uint64, map[string]uint64, bool) {
		return physicalWrites(filepath.Join(root, rel, "io.stat"), m.cfg.DevRoot)
	}
	add := func(source, rel, parent string) bool {
		b, d, ok := read(rel)
		if ok {
			snap.Cgroups = append(snap.Cgroups, platform.CgroupIO{Source: source, ID: rel, Parent: parent, Bytes: b, Devices: d})
		}
		return ok
	}
	parent := func(name, rel string) bool {
		b, d, ok := read(rel)
		if ok {
			snap.Parents[name], snap.ParentDevices[name] = b, d
		}
		return ok
	}

	// system.slice: every direct child, sub-slices, mounts and sockets too,
	// so only cgroups that ended show as system:ended (a sub-slice keeps the
	// writes of its short-lived template units); containers under the
	// cgroupfs driver.
	parent("system", "system.slice")
	for _, name := range dirNames(filepath.Join(root, "system.slice")) {
		add(unitSource(name, containers), filepath.Join("system.slice", name), "system")
	}
	for _, id := range dirNames(filepath.Join(root, "docker")) {
		add(dockerSource(id, containers), filepath.Join("docker", id), "")
	}

	userSlice := filepath.Dir(m.manager)
	// the user manager: aq's task and teardown slices, and the leaves below
	// the other slices.
	if parent("user", m.manager) {
		var aqRels []string
		for _, s := range m.cfg.Slices {
			rel := filepath.Join(m.manager, sliceCgroupPath(s))
			aqRels = append(aqRels, rel)
			add("aq", rel, "user")
		}
		m.walkManager(m.manager, aqRels, func(rel string) {
			add("user:"+filepath.Base(rel), rel, "user")
		})
		// the login slice holds the manager and the sessions beside it.
		if parent("session", userSlice) {
			add("", m.manager, "session")
		}
	}
	if userSlice != "." {
		for _, name := range unitDirs(filepath.Join(root, userSlice)) {
			if strings.HasPrefix(name, "session-") && strings.HasSuffix(name, ".scope") {
				add("session:"+name, filepath.Join(userSlice, name), "session")
			}
		}
	}
	return snap, nil
}

// walkManager calls leaf for each .service/.scope below dir (a leaf's own
// children are not visited), skipping the aq slices.
func (m *DiskMeter) walkManager(dir string, skip []string, leaf func(rel string)) {
	ents, err := os.ReadDir(filepath.Join(m.cfg.CgroupRoot, dir))
	if err != nil {
		return
	}
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		rel := filepath.Join(dir, e.Name())
		if isAqSlice(rel, skip) {
			continue
		}
		switch {
		case isUnit(e.Name()):
			leaf(rel)
		case strings.HasSuffix(e.Name(), ".slice"):
			m.walkManager(rel, skip, leaf)
		}
	}
}

func isAqSlice(rel string, aq []string) bool {
	for _, a := range aq {
		if rel == a {
			return true
		}
	}
	return false
}

func isUnit(name string) bool {
	return strings.HasSuffix(name, ".service") || strings.HasSuffix(name, ".scope")
}

// unitDirs names the .service/.scope subdirectories of dir.
func unitDirs(dir string) []string {
	var out []string
	for _, n := range dirNames(dir) {
		if isUnit(n) {
			out = append(out, n)
		}
	}
	return out
}

func dirNames(dir string) []string {
	ents, _ := os.ReadDir(dir)
	var out []string
	for _, e := range ents {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out
}

// unitSource is the source key of a direct child of system.slice: a unit
// or a sub-slice, a container scope by its container.
func unitSource(name string, containers map[string]string) string {
	if id, ok := strings.CutPrefix(name, "docker-"); ok {
		if id, ok := strings.CutSuffix(id, ".scope"); ok {
			return dockerSource(id, containers)
		}
	}
	return "system:" + name
}

func dockerSource(id string, containers map[string]string) string {
	if k, ok := containers[id]; ok {
		return k
	}
	return "docker:" + id[:min(12, len(id))]
}

// sourceKey is the level-1 source key of the cgroup at rel (a path relative to
// the cgroup root), or "" when it belongs to no source. manager is the user
// manager's cgroup, aqRels the cgroups of aq's own slices (both relative to
// the root), containers maps container ids to keys (may be nil). Processes get
// their ProcIO.Within from it.
func sourceKey(rel, manager string, aqRels []string, containers map[string]string) string {
	rel = strings.Trim(rel, "/")
	if rest, ok := strings.CutPrefix(rel, manager+"/"); ok && manager != "" {
		for _, a := range aqRels {
			if rel == a || strings.HasPrefix(rel, a+"/") {
				return "aq"
			}
		}
		for _, part := range strings.Split(rest, "/") {
			if isUnit(part) {
				return "user:" + part
			}
		}
		return ""
	}
	parts := strings.Split(rel, "/")
	switch {
	case len(parts) >= 2 && parts[0] == "system.slice":
		return unitSource(parts[1], containers)
	case len(parts) >= 2 && parts[0] == "docker":
		return dockerSource(parts[1], containers)
	}
	if manager != "" {
		if dir := filepath.Dir(manager); dir != "." && strings.HasPrefix(rel, dir+"/") {
			p := strings.Split(strings.TrimPrefix(rel, dir+"/"), "/")[0]
			if strings.HasPrefix(p, "session-") && strings.HasSuffix(p, ".scope") {
				return "session:" + p
			}
		}
	}
	return ""
}

// containers asks for the container map; failure degrades to id-based names
// and is logged at most once an hour.
func (m *DiskMeter) containers(ctx context.Context) map[string]string {
	if m.cfg.Containers == nil {
		return nil
	}
	c, err := m.cfg.Containers(ctx)
	if err != nil {
		m.mu.Lock()
		warn := time.Since(m.lastWarn) >= time.Hour
		if warn {
			m.lastWarn = time.Now()
		}
		m.mu.Unlock()
		if warn {
			slog.Warn("disk meter: docker container names unavailable", "err", err)
		}
		return nil
	}
	return c
}

// hostWrites is the sum of "sectors written" (field 10) of /proc/diskstats over
// physical disks, times 512, and the same by disk ("M:m").
func (m *DiskMeter) hostWrites() (uint64, map[string]uint64, error) {
	b, err := os.ReadFile(filepath.Join(m.cfg.ProcRoot, "diskstats"))
	if err != nil {
		return 0, nil, err
	}
	var sum uint64
	devs := map[string]uint64{}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 10 {
			continue
		}
		dev := f[0] + ":" + f[1]
		if _, err := os.Stat(filepath.Join(m.cfg.DevRoot, dev, "device")); err != nil {
			continue
		}
		n, _ := strconv.ParseUint(f[9], 10, 64)
		devs[dev] += n * 512
		sum += n * 512
	}
	return sum, devs, nil
}

// physicalWrites sums wbytes of an io.stat over physical disks (devices with a
// device link in devRoot; device-mapper, md, loop and zram have none and are
// counted where their bytes land), in all and by disk. ok is false when
// there is no io.stat.
func physicalWrites(path, devRoot string) (uint64, map[string]uint64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, nil, false
	}
	var w uint64
	devs := map[string]uint64{}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if _, err := os.Stat(filepath.Join(devRoot, f[0], "device")); err != nil {
			continue
		}
		for _, kv := range f[1:] {
			if v, ok := strings.CutPrefix(kv, "wbytes="); ok {
				n, _ := strconv.ParseUint(v, 10, 64)
				devs[f[0]] += n
				w += n
			}
		}
	}
	return w, devs, true
}
