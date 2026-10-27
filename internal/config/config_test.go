package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadMissingFileGivesDefaults(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "nope.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if c != Default() {
		t.Fatalf("got %+v, want defaults", c)
	}
	d := Default()
	if d.PoolMemory != 16*gib || d.PoolCPUs != 16 || d.ReserveFloorPercent != 10 ||
		d.MinDiskFree != 2*gib || d.MemoryHighRatio != 1.0 || d.CancelGrace != 10*time.Second ||
		d.SliceName != "aq.slice" || d.UnitPrefix != "aq-" {
		t.Fatalf("bad defaults: %+v", d)
	}
	p := d.Pressure
	if p.CloseFullAvg10 != 5.0 || p.CloseAfter != 10*time.Second || p.OpenFullAvg10 != 1.0 || p.OpenAfter != 30*time.Second {
		t.Fatalf("bad pressure defaults: %+v", p)
	}
	if d.Logs.MaxStdout != 64<<20 || d.Logs.MaxStderr != 64<<20 {
		t.Fatalf("bad logs defaults: %+v", d.Logs)
	}
}

func TestLoadOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.toml")
	body := `pool_memory = "1G"
pool_cpus = 2
cancel_grace = "3s"
slice_name = "aq-test.slice"
unit_prefix = "aqtest-"
[pressure]
close_after = "5s"
[logs]
max_stdout = "1M"
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.PoolMemory != gib || c.PoolCPUs != 2 || c.CancelGrace != 3*time.Second ||
		c.SliceName != "aq-test.slice" || c.UnitPrefix != "aqtest-" ||
		c.Pressure.CloseAfter != 5*time.Second || c.Logs.MaxStdout != 1<<20 {
		t.Fatalf("overrides not applied: %+v", c)
	}
	// untouched keys keep defaults
	if c.ReserveFloorPercent != 10 || c.Pressure.OpenAfter != 30*time.Second || c.Logs.MaxStderr != 64<<20 {
		t.Fatalf("defaults lost: %+v", c)
	}
}

func TestLoadRejectsBadValues(t *testing.T) {
	for _, body := range []string{`pool_memory = "abc"`, `cancel_grace = "xx"`, `pool_cpus = "a"`} {
		path := filepath.Join(t.TempDir(), "c.toml")
		os.WriteFile(path, []byte(body), 0o600)
		if _, err := Load(path); err == nil {
			t.Errorf("expected error for %q", body)
		}
	}
}

func TestValidateRejectsPoolAboveRAM(t *testing.T) {
	c := Default()
	c.PoolMemory = 30 * gib
	err := c.Validate(31*gib, 20)
	if err == nil || !strings.Contains(err.Error(), "pool_memory") {
		t.Fatalf("got %v", err)
	}
}

func TestValidateRejectsCPUAboveCount(t *testing.T) {
	c := Default()
	c.PoolCPUs = 32
	err := c.Validate(64*gib, 20)
	if err == nil || !strings.Contains(err.Error(), "pool_cpus") {
		t.Fatalf("got %v", err)
	}
	c.PoolCPUs = 0
	if c.Validate(64*gib, 20) == nil {
		t.Fatal("pool_cpus 0 must fail")
	}
}

func TestValidateRatio(t *testing.T) {
	c := Default()
	for _, r := range []float64{0, -1, 1.5} {
		c.MemoryHighRatio = r
		if err := c.Validate(64*gib, 20); err == nil || !strings.Contains(err.Error(), "memory_high_ratio") {
			t.Errorf("ratio %v: got %v", r, err)
		}
	}
	c.MemoryHighRatio = 1
	if err := c.Validate(64*gib, 20); err != nil {
		t.Fatal(err)
	}
}

func TestValidateOK(t *testing.T) {
	if err := Default().Validate(31*gib, 20); err != nil {
		t.Fatal(err)
	}
}

func TestReserveFloor(t *testing.T) {
	c := Default()
	mem := 31 * gib
	want := mem * 10 / 100
	if got := c.ReserveFloor(mem); got != want {
		t.Fatalf("got %d want %d", got, want)
	}
	if got := c.ReserveFloor(1005); got != 100 {
		t.Fatalf("floor rounding: got %d", got)
	}
}

func TestPathsEnvOverrides(t *testing.T) {
	t.Setenv("AQ_STATE_DIR", "/s")
	t.Setenv("AQ_RUNTIME_DIR", "/r")
	t.Setenv("AQ_CONFIG", "/c/x.toml")
	p, err := DefaultPaths()
	if err != nil {
		t.Fatal(err)
	}
	if p.StateDir != "/s" || p.RuntimeDir != "/r" || p.ConfigFile != "/c/x.toml" {
		t.Fatalf("%+v", p)
	}
	if p.DB() != "/s/aq.db" || p.Socket() != "/r/aq.sock" || p.Lock() != "/r/daemon.lock" ||
		p.JobDir("j1") != "/s/jobs/j1" || p.TmpDir("j1") != "/s/tmp/j1" {
		t.Fatalf("bad derived paths: %+v", p)
	}
}

func TestPathsDefaults(t *testing.T) {
	t.Setenv("AQ_STATE_DIR", "")
	t.Setenv("AQ_RUNTIME_DIR", "")
	t.Setenv("AQ_CONFIG", "")
	t.Setenv("HOME", "/home/u")
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	p, err := DefaultPaths()
	if err != nil {
		t.Fatal(err)
	}
	if p.StateDir != "/home/u/.local/state/agent-queue" || p.RuntimeDir != "/run/user/1000/agent-queue" ||
		p.ConfigFile != "/home/u/.config/agent-queue/config.toml" {
		t.Fatalf("%+v", p)
	}
	t.Setenv("XDG_RUNTIME_DIR", "")
	if _, err := DefaultPaths(); err == nil {
		t.Fatal("missing XDG_RUNTIME_DIR must error (nothing in /tmp)")
	}
}

func TestLoadRejectsUnknownKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.toml")
	body := "pool_memroy = \"1G\"\npool_cpus = 2\n[pressure]\nclose_aftr = \"1s\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "pool_memroy") || !strings.Contains(err.Error(), "pressure.close_aftr") {
		t.Fatalf("got %v", err)
	}
}

func TestValidateSliceAndPrefix(t *testing.T) {
	for _, s := range []string{"aq.slice", "aqtest.slice", "aq-test.slice", "my_pool.slice"} {
		c := Default()
		c.SliceName = s
		if err := c.Validate(64*gib, 20); err != nil {
			t.Errorf("slice %q: %v", s, err)
		}
	}
	for _, s := range []string{"", "aq", "aq.service", ".slice", "-.slice", "-aq.slice", "aq-.slice", "a--b.slice",
		"a/b.slice", "a b.slice", "app.slice", "background.slice", "session.slice", "user.slice", "system.slice",
		"init.scope", "init.slice"} {
		c := Default()
		c.SliceName = s
		if err := c.Validate(64*gib, 20); err == nil || !strings.Contains(err.Error(), "slice_name") {
			t.Errorf("slice %q: got %v", s, err)
		}
	}
	for _, p := range []string{"aq-", "aqtest-", "a_b-", "A1-"} {
		c := Default()
		c.UnitPrefix = p
		if err := c.Validate(64*gib, 20); err != nil {
			t.Errorf("prefix %q: %v", p, err)
		}
	}
	for _, p := range []string{"", "-", "aq", "a q-", "aq/-", "aq.-", "aq@-"} {
		c := Default()
		c.UnitPrefix = p
		if err := c.Validate(64*gib, 20); err == nil || !strings.Contains(err.Error(), "unit_prefix") {
			t.Errorf("prefix %q: got %v", p, err)
		}
	}
}
