// Package config loads and validates the aq configuration.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/creatorofuniverses/agent-queue/internal/units"
)

type PressureConfig struct {
	CloseFullAvg10 float64
	CloseAfter     time.Duration
	OpenFullAvg10  float64
	OpenAfter      time.Duration
}

type LogsConfig struct {
	MaxStdout, MaxStderr uint64
}

type ReservationsConfig struct {
	MaxTTL, ExpiryWarning, TeardownTimeout time.Duration
	TeardownMemory                         uint64
	TeardownConcurrency                    int
}

const (
	DeliveryPush       = "push"
	DeliveryClientWait = "client-wait"
)

type AgentConfig struct {
	Name, IdentityEnv, Delivery string
	Command                     []string
}
type NotifyConfig struct{ RetryMin, RetryMax, DeliveryTimeout, GiveUpAfter, AdmissionClosedAlert, CleaningStuckAlert time.Duration }
type WebhookConfig struct {
	URL, TokenFile string
	Timeout        time.Duration
	// Format is the request body: "aq" (the event as JSON) or "gong"
	// (a Gong /notify request that Gong relays to Telegram).
	Format string
}

const (
	WebhookFormatAQ   = "aq"
	WebhookFormatGong = "gong"
)

type HookConfig struct{ Patterns []string }

func (c Config) Agent(name string) (AgentConfig, bool) {
	for _, a := range c.Agents {
		if a.Name == name {
			return a, true
		}
	}
	return AgentConfig{}, false
}

type ProfilesConfig struct {
	Window, MemoryPercentile, DurationPercentile int
	LimitMargin                                  float64
}
type SchedulerConfig struct{ Backfill bool }
type RetentionConfig struct {
	LogsMaxAge                                   time.Duration
	LogsMaxTotal                                 uint64
	HistoryMaxAge, SamplesMaxAge, SamplesFullRes time.Duration
}
type StatsConfig struct {
	SampleInterval time.Duration
	AgentProcesses []string
}

type Config struct {
	Profiles            ProfilesConfig
	Scheduler           SchedulerConfig
	Retention           RetentionConfig
	Stats               StatsConfig
	Agents              []AgentConfig
	Notify              NotifyConfig
	Webhook             WebhookConfig
	Hook                HookConfig
	PoolMemory          uint64
	PoolCPUs            int
	ReserveFloorPercent int
	MinDiskFree         uint64
	MemoryHighRatio     float64
	CancelGrace         time.Duration
	Pressure            PressureConfig
	Logs                LogsConfig
	Reservations        ReservationsConfig
	SliceName           string
	UnitPrefix          string
}

const gib = uint64(1) << 30

func Default() Config {
	return Config{
		Profiles:            ProfilesConfig{10, 90, 90, 1.2},
		Scheduler:           SchedulerConfig{true},
		Retention:           RetentionConfig{7 * 24 * time.Hour, 5 * gib, 90 * 24 * time.Hour, 30 * 24 * time.Hour, 2 * 24 * time.Hour},
		Stats:               StatsConfig{30 * time.Second, []string{"claude", "codex"}},
		Agents:              []AgentConfig{{"codex", "CODEX_THREAD_ID", DeliveryPush, []string{"codex", "queue", "--thread", "{session}", "--message", "{text}"}}, {"claude", "CLAUDE_CODE_SESSION_ID", DeliveryClientWait, nil}},
		Notify:              NotifyConfig{time.Second, 5 * time.Minute, 30 * time.Second, 24 * time.Hour, 10 * time.Minute, 5 * time.Minute},
		Webhook:             WebhookConfig{Timeout: 10 * time.Second, Format: WebhookFormatAQ},
		Hook:                HookConfig{Patterns: []string{`(^|[\s;&|(])(pytest|py\.test)\b.*\s(-n|--numprocesses)(\s|=)`, `(^|[\s;&|(])docker\s+compose\b.*\s(up|run)\b`, `(^|[\s;&|(])docker-compose\b.*\s(up|run)\b`, `(^|[\s;&|(])docker\s+run\b`}},
		PoolMemory:          16 * gib,
		PoolCPUs:            16,
		ReserveFloorPercent: 10,
		MinDiskFree:         2 * gib,
		MemoryHighRatio:     1.0,
		CancelGrace:         10 * time.Second,
		Pressure: PressureConfig{
			CloseFullAvg10: 5.0,
			CloseAfter:     10 * time.Second,
			OpenFullAvg10:  1.0,
			OpenAfter:      30 * time.Second,
		},
		Logs: LogsConfig{MaxStdout: 64 << 20, MaxStderr: 64 << 20},
		Reservations: ReservationsConfig{
			MaxTTL:              24 * time.Hour,
			ExpiryWarning:       15 * time.Minute,
			TeardownTimeout:     5 * time.Minute,
			TeardownMemory:      512 << 20,
			TeardownConcurrency: 2,
		},
		SliceName:  "aq.slice",
		UnitPrefix: "aq-",
	}
}

// Raw TOML shape: sizes and durations are strings; absent keys are nil.
type rawAgent struct {
	Name        *string  `toml:"name"`
	IdentityEnv *string  `toml:"identity_env"`
	Delivery    *string  `toml:"delivery"`
	Command     []string `toml:"command"`
}
type rawConfig struct {
	Profiles struct {
		Window             *int     `toml:"window"`
		MemoryPercentile   *int     `toml:"memory_percentile"`
		DurationPercentile *int     `toml:"duration_percentile"`
		LimitMargin        *float64 `toml:"limit_margin"`
	} `toml:"profiles"`
	Scheduler struct {
		Backfill *bool `toml:"backfill"`
	} `toml:"scheduler"`
	Retention struct {
		LogsMaxAge     *string `toml:"logs_max_age"`
		LogsMaxTotal   *string `toml:"logs_max_total"`
		HistoryMaxAge  *string `toml:"history_max_age"`
		SamplesMaxAge  *string `toml:"samples_max_age"`
		SamplesFullRes *string `toml:"samples_full_res"`
	} `toml:"retention"`
	Stats struct {
		SampleInterval *string   `toml:"sample_interval"`
		AgentProcesses *[]string `toml:"agent_processes"`
	} `toml:"stats"`
	Agents []rawAgent `toml:"agents"`
	Notify struct {
		RetryMin             *string `toml:"retry_min"`
		RetryMax             *string `toml:"retry_max"`
		DeliveryTimeout      *string `toml:"delivery_timeout"`
		GiveUpAfter          *string `toml:"give_up_after"`
		AdmissionClosedAlert *string `toml:"admission_closed_alert"`
		CleaningStuckAlert   *string `toml:"cleaning_stuck_alert"`
	} `toml:"notify"`
	Webhook struct {
		URL       *string `toml:"url"`
		TokenFile *string `toml:"token_file"`
		Timeout   *string `toml:"timeout"`
		Format    *string `toml:"format"`
	} `toml:"webhook"`
	Hook struct {
		Patterns *[]string `toml:"patterns"`
	} `toml:"hook"`
	PoolMemory          *string  `toml:"pool_memory"`
	PoolCPUs            *int     `toml:"pool_cpus"`
	ReserveFloorPercent *int     `toml:"reserve_floor_percent"`
	MinDiskFree         *string  `toml:"min_disk_free"`
	MemoryHighRatio     *float64 `toml:"memory_high_ratio"`
	CancelGrace         *string  `toml:"cancel_grace"`
	SliceName           *string  `toml:"slice_name"`
	UnitPrefix          *string  `toml:"unit_prefix"`
	Pressure            struct {
		CloseFullAvg10 *float64 `toml:"close_full_avg10"`
		CloseAfter     *string  `toml:"close_after"`
		OpenFullAvg10  *float64 `toml:"open_full_avg10"`
		OpenAfter      *string  `toml:"open_after"`
	} `toml:"pressure"`
	Logs struct {
		MaxStdout *string `toml:"max_stdout"`
		MaxStderr *string `toml:"max_stderr"`
	} `toml:"logs"`
	Reservations struct {
		MaxTTL              *string `toml:"max_ttl"`
		ExpiryWarning       *string `toml:"expiry_warning"`
		TeardownTimeout     *string `toml:"teardown_timeout"`
		TeardownMemory      *string `toml:"teardown_memory"`
		TeardownConcurrency *int    `toml:"teardown_concurrency"`
	} `toml:"reservations"`
}

// Load reads the TOML file at path over Default(). A missing file yields defaults.
func Load(path string) (Config, error) {
	c := Default()
	var raw rawConfig
	md, err := toml.DecodeFile(path, &raw)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return c, nil
		}
		return Config{}, fmt.Errorf("config %s: %w", path, err)
	}
	if und := md.Undecoded(); len(und) > 0 {
		keys := make([]string, len(und))
		for i, k := range und {
			keys[i] = k.String()
		}
		return Config{}, fmt.Errorf("config %s: unknown keys: %s", path, strings.Join(keys, ", "))
	}
	var errs []error
	size := func(key string, src *string, dst *uint64) {
		if src == nil {
			return
		}
		v, err := units.ParseSize(*src)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", key, err))
			return
		}
		*dst = v
	}
	dur := func(key string, src *string, dst *time.Duration) {
		if src == nil {
			return
		}
		v, err := units.ParseDuration(*src)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", key, err))
			return
		}
		*dst = v
	}
	dur("retention.logs_max_age", raw.Retention.LogsMaxAge, &c.Retention.LogsMaxAge)
	size("retention.logs_max_total", raw.Retention.LogsMaxTotal, &c.Retention.LogsMaxTotal)
	dur("retention.history_max_age", raw.Retention.HistoryMaxAge, &c.Retention.HistoryMaxAge)
	dur("retention.samples_max_age", raw.Retention.SamplesMaxAge, &c.Retention.SamplesMaxAge)
	dur("retention.samples_full_res", raw.Retention.SamplesFullRes, &c.Retention.SamplesFullRes)
	dur("stats.sample_interval", raw.Stats.SampleInterval, &c.Stats.SampleInterval)
	if v := raw.Stats.AgentProcesses; v != nil {
		c.Stats.AgentProcesses = *v
	}
	if v := raw.Profiles.Window; v != nil {
		c.Profiles.Window = *v
	}
	if v := raw.Profiles.MemoryPercentile; v != nil {
		c.Profiles.MemoryPercentile = *v
	}
	if v := raw.Profiles.DurationPercentile; v != nil {
		c.Profiles.DurationPercentile = *v
	}
	if v := raw.Profiles.LimitMargin; v != nil {
		c.Profiles.LimitMargin = *v
	}
	if v := raw.Scheduler.Backfill; v != nil {
		c.Scheduler.Backfill = *v
	}
	size("pool_memory", raw.PoolMemory, &c.PoolMemory)
	size("min_disk_free", raw.MinDiskFree, &c.MinDiskFree)
	size("logs.max_stdout", raw.Logs.MaxStdout, &c.Logs.MaxStdout)
	size("logs.max_stderr", raw.Logs.MaxStderr, &c.Logs.MaxStderr)
	size("reservations.teardown_memory", raw.Reservations.TeardownMemory, &c.Reservations.TeardownMemory)
	dur("reservations.max_ttl", raw.Reservations.MaxTTL, &c.Reservations.MaxTTL)
	dur("reservations.expiry_warning", raw.Reservations.ExpiryWarning, &c.Reservations.ExpiryWarning)
	dur("reservations.teardown_timeout", raw.Reservations.TeardownTimeout, &c.Reservations.TeardownTimeout)
	dur("cancel_grace", raw.CancelGrace, &c.CancelGrace)
	dur("pressure.close_after", raw.Pressure.CloseAfter, &c.Pressure.CloseAfter)
	dur("pressure.open_after", raw.Pressure.OpenAfter, &c.Pressure.OpenAfter)
	dur("notify.retry_min", raw.Notify.RetryMin, &c.Notify.RetryMin)
	dur("notify.retry_max", raw.Notify.RetryMax, &c.Notify.RetryMax)
	dur("notify.delivery_timeout", raw.Notify.DeliveryTimeout, &c.Notify.DeliveryTimeout)
	dur("notify.give_up_after", raw.Notify.GiveUpAfter, &c.Notify.GiveUpAfter)
	dur("notify.admission_closed_alert", raw.Notify.AdmissionClosedAlert, &c.Notify.AdmissionClosedAlert)
	dur("notify.cleaning_stuck_alert", raw.Notify.CleaningStuckAlert, &c.Notify.CleaningStuckAlert)
	dur("webhook.timeout", raw.Webhook.Timeout, &c.Webhook.Timeout)
	str := func(src *string, dst *string) {
		if src != nil {
			*dst = *src
		}
	}
	str(raw.Webhook.URL, &c.Webhook.URL)
	str(raw.Webhook.TokenFile, &c.Webhook.TokenFile)
	str(raw.Webhook.Format, &c.Webhook.Format)
	if raw.Hook.Patterns != nil {
		c.Hook.Patterns = *raw.Hook.Patterns
	}
	if len(raw.Agents) > 0 {
		c.Agents = nil
		for _, r := range raw.Agents {
			a := AgentConfig{Command: r.Command}
			str(r.Name, &a.Name)
			str(r.IdentityEnv, &a.IdentityEnv)
			str(r.Delivery, &a.Delivery)
			c.Agents = append(c.Agents, a)
		}
	}
	if len(errs) > 0 {
		return Config{}, fmt.Errorf("config %s: %w", path, errors.Join(errs...))
	}
	if raw.PoolCPUs != nil {
		c.PoolCPUs = *raw.PoolCPUs
	}
	if raw.ReserveFloorPercent != nil {
		c.ReserveFloorPercent = *raw.ReserveFloorPercent
	}
	if raw.MemoryHighRatio != nil {
		c.MemoryHighRatio = *raw.MemoryHighRatio
	}
	if raw.SliceName != nil {
		c.SliceName = *raw.SliceName
	}
	if raw.UnitPrefix != nil {
		c.UnitPrefix = *raw.UnitPrefix
	}
	if raw.Reservations.TeardownConcurrency != nil {
		c.Reservations.TeardownConcurrency = *raw.Reservations.TeardownConcurrency
	}
	if raw.Pressure.CloseFullAvg10 != nil {
		c.Pressure.CloseFullAvg10 = *raw.Pressure.CloseFullAvg10
	}
	if raw.Pressure.OpenFullAvg10 != nil {
		c.Pressure.OpenFullAvg10 = *raw.Pressure.OpenFullAvg10
	}
	return c, nil
}

// ReserveFloor is the memory kept free for the host, rounded down to a byte.
func (c Config) ReserveFloor(memTotal uint64) uint64 {
	return memTotal * uint64(c.ReserveFloorPercent) / 100
}

// Validate checks the config against host resources.
func (c Config) Validate(memTotal uint64, numCPU int) error {
	if c.ReserveFloorPercent < 0 || c.ReserveFloorPercent > 100 {
		return fmt.Errorf("reserve_floor_percent %d out of range 0..100", c.ReserveFloorPercent)
	}
	if max := memTotal - c.ReserveFloor(memTotal); c.PoolMemory > max {
		return fmt.Errorf("pool_memory %s exceeds MemTotal minus %d%% reserve (%s)",
			units.FormatSize(c.PoolMemory), c.ReserveFloorPercent, units.FormatSize(max))
	}
	if c.PoolCPUs < 1 || c.PoolCPUs > numCPU {
		return fmt.Errorf("pool_cpus %d must be within 1..%d", c.PoolCPUs, numCPU)
	}
	if !(c.MemoryHighRatio > 0 && c.MemoryHighRatio <= 1) {
		return fmt.Errorf("memory_high_ratio %v must be in (0, 1]", c.MemoryHighRatio)
	}
	if err := validateSliceName(c.SliceName); err != nil {
		return err
	}
	if !strings.HasSuffix(c.UnitPrefix, "-") || !unitWord(strings.TrimSuffix(c.UnitPrefix, "-")) {
		return fmt.Errorf("unit_prefix %q must be [A-Za-z0-9_-] ending in \"-\", not starting with \"-\"", c.UnitPrefix)
	}
	if err := c.validateReservations(); err != nil {
		return err
	}
	if err := c.validatePlan4(); err != nil {
		return err
	}
	return c.validateNotify()
}

func (c Config) validateReservations() error {
	r := c.Reservations
	switch {
	case r.MaxTTL <= 0:
		return fmt.Errorf("max_ttl %v must be > 0", r.MaxTTL)
	case r.ExpiryWarning <= 0 || r.ExpiryWarning >= r.MaxTTL:
		return fmt.Errorf("expiry_warning %v must be > 0 and less than max_ttl %v", r.ExpiryWarning, r.MaxTTL)
	case r.TeardownTimeout <= 0:
		return fmt.Errorf("teardown_timeout %v must be > 0", r.TeardownTimeout)
	case r.TeardownMemory < 64<<20 || r.TeardownMemory > c.PoolMemory:
		return fmt.Errorf("teardown_memory %s must be within 64M..pool_memory (%s)",
			units.FormatSize(r.TeardownMemory), units.FormatSize(c.PoolMemory))
	case r.TeardownConcurrency < 1:
		return fmt.Errorf("teardown_concurrency %d must be >= 1", r.TeardownConcurrency)
	}
	return nil
}

// TeardownSliceName is the slice for teardown units, separate from the task
// slice so the pool and host guard do not apply to it: "aq.slice" -> "aqtd.slice".
func (c Config) TeardownSliceName() string {
	return strings.TrimSuffix(c.SliceName, ".slice") + "td.slice"
}

// reservedSlices are the user manager's own slices (and init.scope): the daemon
// stops every unit in its slice it does not know, and sets the slice's memory.max.
var reservedSlices = []string{"app", "background", "session", "user", "system", "machine", "init"}

func validateSliceName(s string) error {
	name, ok := strings.CutSuffix(s, ".slice")
	switch {
	case !ok:
		return fmt.Errorf("slice_name %q must end in .slice", s)
	case !unitWord(name) || strings.HasSuffix(name, "-") || strings.Contains(name, "--"):
		// "-.slice" is the root slice; a dash nests ("a-b.slice" is inside "a.slice")
		return fmt.Errorf("slice_name %q must be [A-Za-z0-9_-] with no leading, trailing or double \"-\"", s)
	}
	for _, r := range reservedSlices {
		if name == r {
			return fmt.Errorf("slice_name %q is a system slice", s)
		}
	}
	return nil
}

// unitWord reports whether s is non-empty, [A-Za-z0-9_-] only, and does not start with "-".
func unitWord(s string) bool {
	if s == "" || s[0] == '-' {
		return false
	}
	for _, c := range s {
		if !(c == '_' || c == '-' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

func (c Config) validateNotify() error {
	for _, d := range []struct {
		k string
		v time.Duration
	}{
		{"notify.retry_min", c.Notify.RetryMin}, {"notify.retry_max", c.Notify.RetryMax},
		{"notify.delivery_timeout", c.Notify.DeliveryTimeout}, {"notify.give_up_after", c.Notify.GiveUpAfter},
		{"notify.admission_closed_alert", c.Notify.AdmissionClosedAlert}, {"notify.cleaning_stuck_alert", c.Notify.CleaningStuckAlert}, {"webhook.timeout", c.Webhook.Timeout},
	} {
		if d.v <= 0 {
			return fmt.Errorf("%s must be > 0", d.k)
		}
	}
	if c.Notify.RetryMin > c.Notify.RetryMax {
		return fmt.Errorf("notify.retry_min must be <= retry_max")
	}
	seen := map[string]bool{}
	for _, a := range c.Agents {
		if !regexp.MustCompile(`^[a-z][a-z0-9_-]*$`).MatchString(a.Name) || a.Name == "cli" || seen[a.Name] {
			return fmt.Errorf("agents.name invalid or duplicate: %q", a.Name)
		}
		seen[a.Name] = true
		if !regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`).MatchString(a.IdentityEnv) {
			return fmt.Errorf("agents.identity_env invalid for %s", a.Name)
		}
		switch a.Delivery {
		case DeliveryPush:
			cmd := strings.Join(a.Command, " ")
			if len(a.Command) == 0 || a.Command[0] == "" || !strings.Contains(cmd, "{session}") || !strings.Contains(cmd, "{text}") {
				return fmt.Errorf("agents.command for %s needs executable, {session} and {text}", a.Name)
			}
		case DeliveryClientWait:
			if len(a.Command) != 0 {
				return fmt.Errorf("agents.command must be empty for client-wait")
			}
		default:
			return fmt.Errorf("agents.delivery invalid for %s", a.Name)
		}
	}
	if c.Webhook.URL != "" {
		u, e := url.Parse(c.Webhook.URL)
		if e != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("webhook.url must be http(s) with a host")
		}
	}
	if c.Webhook.Format != WebhookFormatAQ && c.Webhook.Format != WebhookFormatGong {
		return fmt.Errorf("webhook.format must be aq or gong")
	}
	if c.Webhook.TokenFile != "" && !filepath.IsAbs(c.Webhook.TokenFile) {
		return fmt.Errorf("webhook.token_file must be absolute")
	}
	for _, p := range c.Hook.Patterns {
		if _, e := regexp.Compile(p); e != nil {
			return fmt.Errorf("hook.patterns: %w", e)
		}
	}
	return nil
}

func (c Config) validatePlan4() error {
	p := c.Profiles
	if p.Window < 1 || p.Window > 100 {
		return fmt.Errorf("profiles.window must be within 1..100")
	}
	for _, v := range []struct {
		k string
		n int
	}{{"memory_percentile", p.MemoryPercentile}, {"duration_percentile", p.DurationPercentile}} {
		if v.n < 50 || v.n > 100 {
			return fmt.Errorf("profiles.%s must be within 50..100", v.k)
		}
	}
	if !(p.LimitMargin >= 1 && p.LimitMargin <= 4) {
		return fmt.Errorf("profiles.limit_margin must be within 1..4")
	}
	r := c.Retention
	for _, v := range []struct {
		k string
		d time.Duration
	}{{"logs_max_age", r.LogsMaxAge}, {"history_max_age", r.HistoryMaxAge}, {"samples_max_age", r.SamplesMaxAge}, {"samples_full_res", r.SamplesFullRes}} {
		if v.d <= 0 {
			return fmt.Errorf("retention.%s must be positive", v.k)
		}
	}
	if r.SamplesFullRes >= r.SamplesMaxAge {
		return fmt.Errorf("retention.samples_full_res must be less than samples_max_age")
	}
	if r.LogsMaxTotal < 256<<20 {
		return fmt.Errorf("retention.logs_max_total must be at least 256M")
	}
	if c.Stats.SampleInterval < 5*time.Second || c.Stats.SampleInterval > 5*time.Minute {
		return fmt.Errorf("stats.sample_interval must be within 5s..5m")
	}
	for _, n := range c.Stats.AgentProcesses {
		if n == "" || strings.Contains(n, "/") {
			return fmt.Errorf("stats.agent_processes must contain nonempty names without /")
		}
	}
	return nil
}
