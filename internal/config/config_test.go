package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestLoadMissingFileGivesDefaults(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "nope.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c, Default()) {
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
	// The slice lock ignores AQ_RUNTIME_DIR: every daemon on a slice must see the same file.
	t.Setenv("AQ_RUNTIME_DIR", "/elsewhere")
	if p, err = DefaultPaths(); err != nil || p.SliceLock("aq.slice") != "/run/user/1000/agent-queue.aq.slice.lock" {
		t.Fatalf("slice lock %q %v", p.SliceLock("aq.slice"), err)
	}
	t.Setenv("XDG_RUNTIME_DIR", "")
	if p, err = DefaultPaths(); err != nil || p.SliceLockDir != fmt.Sprintf("/run/user/%d", os.Getuid()) {
		t.Fatalf("slice lock dir without XDG_RUNTIME_DIR: %q %v", p.SliceLockDir, err)
	}
	t.Setenv("AQ_RUNTIME_DIR", "")
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

func TestReservationsConfigDefaults(t *testing.T) {
	r := Default().Reservations
	want := ReservationsConfig{MaxTTL: 24 * time.Hour, ExpiryWarning: 15 * time.Minute,
		TeardownTimeout: 5 * time.Minute, TeardownMemory: 512 << 20, TeardownConcurrency: 2}
	if r != want {
		t.Fatalf("got %+v want %+v", r, want)
	}
}

func TestTeardownSliceName(t *testing.T) {
	for in, want := range map[string]string{"aq.slice": "aqtd.slice", "aqe2e.slice": "aqe2etd.slice", "aqtest.slice": "aqtesttd.slice"} {
		c := Default()
		c.SliceName = in
		if got := c.TeardownSliceName(); got != want {
			t.Errorf("%s: got %s want %s", in, got, want)
		}
	}
}

func TestLoadReservations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.toml")
	body := "[reservations]\nmax_ttl = \"2h\"\nexpiry_warning = \"1m\"\nteardown_timeout = \"30s\"\nteardown_memory = \"1G\"\nteardown_concurrency = 3\n"
	os.WriteFile(path, []byte(body), 0o600)
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	want := ReservationsConfig{MaxTTL: 2 * time.Hour, ExpiryWarning: time.Minute,
		TeardownTimeout: 30 * time.Second, TeardownMemory: 1 << 30, TeardownConcurrency: 3}
	if c.Reservations != want {
		t.Fatalf("got %+v", c.Reservations)
	}
}

func TestValidateReservations(t *testing.T) {
	for name, tc := range map[string]struct {
		mut  func(*Config)
		want string
	}{
		"memory too small":  {func(c *Config) { c.Reservations.TeardownMemory = 32 << 20 }, "teardown_memory"},
		"memory above pool": {func(c *Config) { c.Reservations.TeardownMemory = c.PoolMemory + 1 }, "teardown_memory"},
		"concurrency 0":     {func(c *Config) { c.Reservations.TeardownConcurrency = 0 }, "teardown_concurrency"},
		"warning >= ttl":    {func(c *Config) { c.Reservations.ExpiryWarning = 25 * time.Hour }, "expiry_warning"},
		"ttl 0":             {func(c *Config) { c.Reservations.MaxTTL = 0 }, "max_ttl"},
		"timeout 0":         {func(c *Config) { c.Reservations.TeardownTimeout = 0 }, "teardown_timeout"},
		"warning 0":         {func(c *Config) { c.Reservations.ExpiryWarning = 0 }, "expiry_warning"},
	} {
		c := Default()
		tc.mut(&c)
		if err := c.Validate(64*gib, 20); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v", name, err)
		}
	}
	path := filepath.Join(t.TempDir(), "c.toml")
	os.WriteFile(path, []byte("[reservations]\nbogus = 1\n"), 0o600)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "reservations.bogus") {
		t.Fatalf("unknown key: got %v", err)
	}
}

func TestTeardownPaths(t *testing.T) {
	p := Paths{StateDir: "/s"}
	if got := p.TeardownScript("x"); got != "/s/jobs/x/teardown.sh" {
		t.Error(got)
	}
	if got := p.TeardownDir("x", 2); got != "/s/jobs/x/teardown-2" {
		t.Error(got)
	}
	if got := p.TeardownTmpDir("x", 2); got != "/s/tmp/teardown-x-2" {
		t.Error(got)
	}
}

func TestDefaultAgents(t *testing.T) {
	c := Default()
	want := []AgentConfig{{"codex", "CODEX_THREAD_ID", DeliveryPush, []string{"codex", "queue", "--thread", "{session}", "--message", "{text}"}}, {"claude", "CLAUDE_CODE_SESSION_ID", DeliveryClientWait, nil}}
	if !reflect.DeepEqual(c.Agents, want) {
		t.Fatalf("%+v", c.Agents)
	}
	if c.Notify != (NotifyConfig{time.Second, 5 * time.Minute, 30 * time.Second, 24 * time.Hour, 10 * time.Minute, 5 * time.Minute}) || c.Webhook.Timeout != 10*time.Second || c.Webhook.URL != "" {
		t.Fatal(c)
	}
}
func TestLoadAgentsReplaceDefaults(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.toml")
	os.WriteFile(p, []byte(`[[agents]]
name="pi"
identity_env="PI_SESSION"
delivery="push"
command=["pi","send","{session}","{text}"]
[notify]
retry_min="2s"
[webhook]
url="https://example.org/h"
[hook]
patterns=["x"]
`), 0600)
	c, e := Load(p)
	if e != nil {
		t.Fatal(e)
	}
	if len(c.Agents) != 1 || c.Agents[0].Name != "pi" || c.Notify.RetryMin != 2*time.Second || c.Webhook.URL != "https://example.org/h" || !reflect.DeepEqual(c.Hook.Patterns, []string{"x"}) {
		t.Fatal(c)
	}
}
func TestValidateNotify(t *testing.T) {
	cases := []struct {
		key string
		mut func(*Config)
	}{
		{"retry_min", func(c *Config) { c.Notify.RetryMin = c.Notify.RetryMax + 1 }},
		{"delivery_timeout", func(c *Config) { c.Notify.DeliveryTimeout = 0 }},
		{"name", func(c *Config) { c.Agents[0].Name = "Codex" }},
		{"name", func(c *Config) { c.Agents[0].Name = "cli" }},
		{"name", func(c *Config) { c.Agents = append(c.Agents, c.Agents[0]) }},
		{"identity_env", func(c *Config) { c.Agents[0].IdentityEnv = "1X" }},
		{"delivery", func(c *Config) { c.Agents[0].Delivery = "poll" }},
		{"command", func(c *Config) { c.Agents[0].Command = []string{"x", "{session}"} }},
		{"command", func(c *Config) { c.Agents[1].Command = []string{"x"} }},
		{"url", func(c *Config) { c.Webhook.URL = "ftp://x" }},
		{"url", func(c *Config) { c.Webhook.URL = "http://" }},
		{"token_file", func(c *Config) { c.Webhook.TokenFile = "rel/path" }},
		{"patterns", func(c *Config) { c.Hook.Patterns = []string{"(("} }},
	}
	for _, tc := range cases {
		c := Default()
		tc.mut(&c)
		if e := c.Validate(64*gib, 20); e == nil || !strings.Contains(e.Error(), tc.key) {
			t.Errorf("%s: %v", tc.key, e)
		}
	}
}
func TestExampleConfigMatchesDefault(t *testing.T) {
	c, e := Load("../../deploy/config.example.toml")
	if e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(c, Default()) {
		t.Fatalf("%+v != %+v", c, Default())
	}
}

func TestDefaultPlan4Sections(t *testing.T) {
	c := Default()
	if c.Profiles != (ProfilesConfig{10, 90, 90, 1.2}) || !c.Scheduler.Backfill || c.Retention != (RetentionConfig{168 * time.Hour, 5 * gib, 2160 * time.Hour, 720 * time.Hour, 48 * time.Hour}) || c.Stats.SampleInterval != 30*time.Second || !reflect.DeepEqual(c.Stats.AgentProcesses, []string{"claude", "codex"}) {
		t.Fatal(c)
	}
}
func TestLoadDaysInOldKeys(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.toml")
	os.WriteFile(p, []byte("[reservations]\nmax_ttl=\"1d\"\n"), 0600)
	c, e := Load(p)
	if e != nil || c.Reservations.MaxTTL != 24*time.Hour {
		t.Fatal(c, e)
	}
}
func TestValidatePlan4(t *testing.T) {
	for _, tc := range []struct{ body, key string }{
		{"[profiles]\nwindow=0", "window"}, {"[profiles]\nwindow=101", "window"}, {"[profiles]\nmemory_percentile=49", "memory_percentile"}, {"[profiles]\nduration_percentile=101", "duration_percentile"}, {"[profiles]\nlimit_margin=0.9", "limit_margin"}, {"[retention]\nsamples_full_res=\"30d\"", "samples_full_res"}, {"[retention]\nlogs_max_total=\"100M\"", "logs_max_total"}, {"[retention]\nlogs_max_age=\"0s\"", "logs_max_age"}, {"[stats]\nsample_interval=\"1s\"", "sample_interval"}, {"[stats]\nagent_processes=[\"a/b\"]", "agent_processes"}, {"[stats]\nagent_processes=[\"\"]", "agent_processes"},
	} {
		t.Run(tc.key+tc.body, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "c.toml")
			os.WriteFile(p, []byte(tc.body), 0600)
			c, e := Load(p)
			if e == nil {
				e = c.Validate(64*gib, 20)
			}
			if e == nil || !strings.Contains(e.Error(), tc.key) {
				t.Fatal(e)
			}
		})
	}
}
