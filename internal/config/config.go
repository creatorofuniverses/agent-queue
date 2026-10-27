// Package config loads and validates the aq configuration.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/BurntSushi/toml"

	"resource-dispatcher/internal/units"
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

type Config struct {
	PoolMemory          uint64
	PoolCPUs            int
	ReserveFloorPercent int
	MinDiskFree         uint64
	MemoryHighRatio     float64
	CancelGrace         time.Duration
	Pressure            PressureConfig
	Logs                LogsConfig
	SliceName           string
	UnitPrefix          string
}

const gib = uint64(1) << 30

func Default() Config {
	return Config{
		PoolMemory:          16 * gib,
		PoolCPUs:            16,
		ReserveFloorPercent: 10,
		MinDiskFree:         2 * gib,
		MemoryHighRatio:     0.9,
		CancelGrace:         10 * time.Second,
		Pressure: PressureConfig{
			CloseFullAvg10: 5.0,
			CloseAfter:     10 * time.Second,
			OpenFullAvg10:  1.0,
			OpenAfter:      30 * time.Second,
		},
		Logs:       LogsConfig{MaxStdout: 64 << 20, MaxStderr: 64 << 20},
		SliceName:  "aq.slice",
		UnitPrefix: "aq-",
	}
}

// Raw TOML shape: sizes and durations are strings; absent keys are nil.
type rawConfig struct {
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
}

// Load reads the TOML file at path over Default(). A missing file yields defaults.
func Load(path string) (Config, error) {
	c := Default()
	var raw rawConfig
	if _, err := toml.DecodeFile(path, &raw); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return c, nil
		}
		return Config{}, fmt.Errorf("config %s: %w", path, err)
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
		v, err := time.ParseDuration(*src)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", key, err))
			return
		}
		*dst = v
	}
	size("pool_memory", raw.PoolMemory, &c.PoolMemory)
	size("min_disk_free", raw.MinDiskFree, &c.MinDiskFree)
	size("logs.max_stdout", raw.Logs.MaxStdout, &c.Logs.MaxStdout)
	size("logs.max_stderr", raw.Logs.MaxStderr, &c.Logs.MaxStderr)
	dur("cancel_grace", raw.CancelGrace, &c.CancelGrace)
	dur("pressure.close_after", raw.Pressure.CloseAfter, &c.Pressure.CloseAfter)
	dur("pressure.open_after", raw.Pressure.OpenAfter, &c.Pressure.OpenAfter)
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
	return nil
}
