// Package platform defines the OS-facing interfaces the scheduler depends on.
package platform

import (
	"context"
	"time"

	"agent-queue/internal/model"
)

type Capabilities struct {
	HardMemoryLimit, TreeKill, OOMAttribution, SurvivesDaemonRestart bool
}

type LaunchSpec struct {
	ID                   model.ItemID
	Argv                 []string
	Cwd                  string
	Env                  map[string]string
	Limit, MemoryHigh    uint64
	CPUs                 int
	Timeout, KillGrace   time.Duration
	JobDir, TmpDir       string
	MaxStdout, MaxStderr uint64
}

type ExitInfo struct {
	ServiceResult, ExitCode, ExitStatus string
	MemoryPeak                          uint64
	// OwnOOMEvents is memory.events "oom": OOMs caused by the unit's own memory.max.
	// ("max" is not used: it also counts page cache reclaim at the limit, with no OOM.)
	OwnOOMEvents, OwnOOMKills uint64
	EndedAt                   time.Time
	LaunchError               string
	LogsTruncated             bool
}

type MemInfo struct {
	Total, Available, SwapTotal, SwapFree uint64
}

type Executor interface {
	Capabilities() Capabilities
	Launch(ctx context.Context, spec LaunchSpec) (unitName string, err error)
	ReadResult(jobDir string) (*ExitInfo, error)              // (nil, nil) while not finished
	Alive(ctx context.Context, id model.ItemID) (bool, error) // unit cgroup tree has processes
	MemoryCurrent(id model.ItemID) (uint64, error)            // 0, nil if no cgroup
	Stop(ctx context.Context, id model.ItemID) error          // TERM all, KILL after KillGrace
	ListManaged(ctx context.Context) ([]model.ItemID, error)  // ids by prefixed cgroup/unit
	SliceCurrent() (uint64, error)
	SliceOOMKills() (uint64, error)
	TeardownSliceOOMKills() (uint64, error)
	SliceCeiling() (uint64, error) // actual memory.max; math.MaxUint64 when unlimited
	SetSliceCeiling(max, high uint64) error
	EnsureSlice(ctx context.Context, poolMemory uint64) error // slice exists, MemorySwapMax=0, MemoryMax=pool
	// EnsureTeardownSlice: the teardown slice exists with MemorySwapMax=0, MemoryMax=max.
	// Teardown units (model.IsTeardownUnitID) run there; the guard never touches it.
	EnsureTeardownSlice(ctx context.Context, max uint64) error
}

type HostMetrics interface {
	Memory() (MemInfo, error)
	PressureFullAvg10() (float64, error)
	DiskFree(path string) (uint64, error)
	KernelOOMKills() (uint64, error)
	NumCPU() int
}

// StandMeter measures external stands; Usage never blocks.
type StandMeter interface {
	Watch(projects []string)                      // replaces the set to measure
	Usage(project string) (bytes uint64, ok bool) // ok=false: no fresh measurement
	Run(ctx context.Context)                      // refresh loop until ctx ends
}

type AgentUsage struct {
	Name  string `json:"name"`
	Bytes uint64 `json:"bytes"`
	Procs int    `json:"procs"`
}
type Usage struct {
	Agents        []AgentUsage
	Docker, Tasks uint64
}
type UsageMeter interface {
	Usage(ctx context.Context, agents []string) (Usage, error)
}

type OOMKill struct {
	At     time.Time
	Cgroup string
}
type OOMKillLog interface {
	Kills(ctx context.Context) ([]OOMKill, error)
}
