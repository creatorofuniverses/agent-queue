package linux

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"testing"

	"github.com/creatorofuniverses/agent-queue/internal/platform"
)

const diskManager = "user.slice/user-1000.slice/user@1000.service"

// diskTree builds a fake /proc, /sys/dev/block and cgroup root; io maps a cgroup
// path to its wbytes on the physical disk 259:0 (and 900-style noise on dm 252:0).
func diskTree(t *testing.T, io map[string]int) DiskMeterConfig {
	t.Helper()
	root := t.TempDir()
	put := func(rel, body string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	put("proc/self/cgroup", "0::/"+diskManager+"/app.slice/aq.service\n")
	put("proc/diskstats", "   259       0 nvme0n1 1 0 2 3 4 0 1000 5 0 6 7\n   252       0 dm-0 1 0 2 3 4 0 1000 5 0 6 7\n")
	put("dev/259:0/device", "")
	if err := os.MkdirAll(filepath.Join(root, "dev/252:0"), 0o755); err != nil {
		t.Fatal(err)
	}
	for rel, n := range io {
		put(filepath.Join("cg", rel, "io.stat"), "259:0 rbytes=1 wbytes="+strconv.Itoa(n)+" rios=1\n252:0 rbytes=1 wbytes="+strconv.Itoa(n)+"\n")
	}
	return DiskMeterConfig{
		ProcRoot: filepath.Join(root, "proc"), CgroupRoot: filepath.Join(root, "cg"),
		DevRoot: filepath.Join(root, "dev"), Slices: []string{"aqtest.slice"},
		Containers: func(context.Context) (map[string]string, error) {
			return map[string]string{"abc": "docker:runi-x"}, nil
		},
	}
}

func diskIOTree() map[string]int {
	m := diskManager
	return map[string]int{
		"system.slice":                               900,
		"system.slice/systemd-journald.service":      100,
		"system.slice/docker-abc.scope":              300,
		m:                                            700,
		m + "/aqtest.slice":                          200,
		m + "/app.slice/app-tmux.scope":              50,
		m + "/app.slice/ai-memory.service":           40,
		"user.slice/user-1000.slice":                 1000,
		"user.slice/user-1000.slice/session-3.scope": 250,
	}
}

func TestDiskMeterSnapshot(t *testing.T) {
	m := NewDiskMeter(diskTree(t, diskIOTree()))
	s, err := m.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.Host != 1000*512 || !maps.Equal(s.Devices, map[string]uint64{"259:0": 1000 * 512}) {
		t.Fatalf("host = %d %v", s.Host, s.Devices)
	}
	dev := func(n uint64) map[string]uint64 { return map[string]uint64{"259:0": n} }
	want := []platform.CgroupIO{
		{Source: "aq", ID: diskManager + "/aqtest.slice", Parent: "user", Bytes: 200, Devices: dev(200)},
		{Source: "docker:runi-x", ID: "system.slice/docker-abc.scope", Parent: "system", Bytes: 300, Devices: dev(300)},
		{Source: "system:systemd-journald.service", ID: "system.slice/systemd-journald.service", Parent: "system", Bytes: 100, Devices: dev(100)},
		{Source: "user:app-tmux.scope", ID: diskManager + "/app.slice/app-tmux.scope", Parent: "user", Bytes: 50, Devices: dev(50)},
		{Source: "user:ai-memory.service", ID: diskManager + "/app.slice/ai-memory.service", Parent: "user", Bytes: 40, Devices: dev(40)},
		{Source: "session:session-3.scope", ID: "user.slice/user-1000.slice/session-3.scope", Parent: "session", Bytes: 250, Devices: dev(250)},
		// the user manager: no source, but a child of the login slice
		{Source: "", ID: diskManager, Parent: "session", Bytes: 700, Devices: dev(700)},
	}
	if len(s.Cgroups) != len(want) {
		t.Fatalf("cgroups = %+v", s.Cgroups)
	}
	for _, w := range want {
		if !slices.ContainsFunc(s.Cgroups, func(c platform.CgroupIO) bool { return reflect.DeepEqual(c, w) }) {
			t.Errorf("missing %+v in %+v", w, s.Cgroups)
		}
	}
	// Parents are the raw counters; their children are subtracted when stepping.
	for k, v := range map[string]uint64{"system": 900, "user": 700, "session": 1000} {
		if s.Parents[k] != v || !maps.Equal(s.ParentDevices[k], dev(v)) {
			t.Errorf("parent %s = %d %v, want %d", k, s.Parents[k], s.ParentDevices[k], v)
		}
	}
}

// Every live child of system.slice is a source of its own, not only services
// and scopes: a sub-slice (its short-lived template units included), a mount,
// a socket. Otherwise their writes showed as system:ended.
func TestDiskMeterSystemChildren(t *testing.T) {
	io := map[string]int{
		"system.slice":                                       900,
		"system.slice/system-getty.slice":                    30,
		"system.slice/system-getty.slice/getty@tty1.service": 30,
		`system.slice/system-systemd\x2dcoredump.slice`:      400,
		"system.slice/home.mount":                            5,
		"system.slice/docker-abc.scope":                      300,
	}
	cfg := diskTree(t, io)
	s, err := NewDiskMeter(cfg).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]uint64{}
	for _, c := range s.Cgroups {
		if c.Parent != "system" {
			t.Errorf("%+v: parent %q", c, c.Parent)
		}
		got[c.Source] += c.Bytes
	}
	want := map[string]uint64{"system:system-getty.slice": 30, `system:system-systemd\x2dcoredump.slice`: 400, "system:home.mount": 5, "docker:runi-x": 300}
	if !maps.Equal(got, want) {
		t.Fatalf("sources = %v, want %v", got, want)
	}
	containers, _ := cfg.Containers(context.Background())
	for _, c := range s.Cgroups {
		if k := sourceKey(c.ID+"/x", diskManager, nil, containers); k != c.Source {
			t.Errorf("sourceKey below %q = %q, snapshot says %q", c.ID, k, c.Source)
		}
	}
}

func TestDiskMeterDegrades(t *testing.T) {
	io := diskIOTree()
	delete(io, diskManager+"/app.slice/ai-memory.service")
	cfg := diskTree(t, io)
	cfg.Containers = func(context.Context) (map[string]string, error) { return nil, errors.New("no docker") }
	s, err := NewDiskMeter(cfg).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var docker, mem bool
	for _, c := range s.Cgroups {
		docker = docker || c.Source == "docker:abc"
		mem = mem || c.Source == "user:ai-memory.service"
	}
	if !docker || mem || len(s.Cgroups) != 6 {
		t.Fatalf("cgroups = %+v", s.Cgroups)
	}
}

func TestDiskMeterNoIOController(t *testing.T) {
	s, err := NewDiskMeter(diskTree(t, nil)).Snapshot(context.Background())
	if err != nil || len(s.Cgroups) != 0 || len(s.Parents) != 0 || s.Host != 1000*512 {
		t.Fatalf("%+v %v", s, err)
	}
}

func TestSourceKey(t *testing.T) {
	aq := []string{diskManager + "/aqtest.slice"}
	c := map[string]string{"abc": "docker:runi-x"}
	for rel, want := range map[string]string{
		diskManager + "/aqtest.slice/aqtest-1.service/x":     "aq",
		diskManager + "/app.slice/app-tmux.scope":            "user:app-tmux.scope",
		diskManager + "/app.slice/foo.service/sub/leaf":      "user:foo.service",
		diskManager + "/app.slice":                           "",
		"system.slice/docker-abc.scope/x":                    "docker:runi-x",
		"system.slice/docker-def456789012345.scope":          "docker:def456789012",
		"system.slice/sshd.service":                          "system:sshd.service",
		"system.slice/system-getty.slice/getty@tty1.service": "system:system-getty.slice",
		"system.slice/home.mount":                            "system:home.mount",
		"docker/abc":                                         "docker:runi-x",
		"user.slice/user-1000.slice/session-3.scope/a":       "session:session-3.scope",
		"init.scope": "",
	} {
		if got := sourceKey(rel, diskManager, aq, c); got != want {
			t.Errorf("sourceKey(%q) = %q, want %q", rel, got, want)
		}
	}
}
