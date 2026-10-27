// Package platform defines the OS-facing interfaces the scheduler depends on.
package platform

import (
	"context"
	"time"

	"resource-dispatcher/internal/model"
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
	ServiceResult, ExitCode, ExitStatus   string
	MemoryPeak, OwnMaxEvents, OwnOOMKills uint64
	EndedAt                               time.Time
	LaunchError                           string
	LogsTruncated                         bool
}

type MemInfo struct {
	Total, Available, SwapTotal, SwapFree uint64
}

type Executor interface {
	Capabilities() Capabilities
	Launch(ctx context.Context, spec LaunchSpec) (unitName string, err error)
	ReadResult(jobDir string) (*ExitInfo, error)              // (nil, nil) while not finished
	Alive(ctx context.Context, id model.ItemID) (bool, error) // task cgroup has processes
	MemoryCurrent(id model.ItemID) (uint64, error)            // 0, nil if no cgroup
	Stop(ctx context.Context, id model.ItemID) error          // TERM all, KILL after KillGrace
	ListManaged(ctx context.Context) ([]model.ItemID, error)  // ids by prefixed cgroup/unit
	SliceCurrent() (uint64, error)
	SliceOOMKills() (uint64, error)
	SetSliceCeiling(max, high uint64) error
	EnsureSlice(ctx context.Context, poolMemory uint64) error // slice exists, MemorySwapMax=0, MemoryMax=pool
}

type HostMetrics interface {
	Memory() (MemInfo, error)
	PressureFullAvg10() (float64, error)
	DiskFree(path string) (uint64, error)
	KernelOOMKills() (uint64, error)
	NumCPU() int
}
