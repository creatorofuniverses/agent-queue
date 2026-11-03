// Package platform defines the OS-facing interfaces the scheduler depends on.
package platform

import (
	"context"
	"time"

	"github.com/creatorofuniverses/agent-queue/internal/model"
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
	// IOWriteBytes/IOReadBytes: physical-disk bytes from the unit's io.stat;
	// nil when the io controller is not available (unknown, not zero).
	IOWriteBytes  *uint64 `json:",omitempty"`
	IOReadBytes   *uint64 `json:",omitempty"`
	EndedAt       time.Time
	LaunchError   string
	LogsTruncated bool
}

type MemInfo struct {
	Total, Available, SwapTotal, SwapFree uint64
	Free                                  uint64 // MemFree: not even used as cache
}

// EndedCgroups is the OtherOOMKills key for kills in the user's cgroups that no
// longer exist, such as a test slice or a session that has ended: their own
// counters are gone, the user slice above them keeps the sum.
const EndedCgroups = "ended cgroups"

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
	// SliceOOMs counts the OOMs at the slice's own ceiling (memory.events.local),
	// not at the limit of a task inside it.
	SliceOOMs() (uint64, error)
	TeardownSliceOOMKills() (uint64, error)
	// OtherOOMKills: oom_kill of every other slice of the user manager, of the
	// user's login sessions and of system.slice, by cgroup name, so kills outside
	// aq can be told apart; EndedCgroups holds the user's cgroups removed since.
	OtherOOMKills() (map[string]uint64, error)
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
	// FSUsage reports the filesystem holding path; task temp dirs live there.
	FSUsage(path string) (FSUsage, error)
	KernelOOMKills() (uint64, error)
	NumCPU() int
}

// FSUsage is a statfs result: bytes and inodes available to an unprivileged user.
type FSUsage struct {
	Size       uint64 `json:"size"`
	Free       uint64 `json:"free"`
	Inodes     uint64 `json:"inodes"`
	InodesFree uint64 `json:"inodes_free"`
	Tmpfs      bool   `json:"tmpfs"`
}

// StandMeter measures external stands; Usage never blocks.
type StandMeter interface {
	Watch(projects []string)                      // replaces the set to measure
	Usage(project string) (bytes uint64, ok bool) // ok=false: no fresh measurement
	// Containers is the same measurement per container, largest first.
	Containers(project string) ([]ContainerUsage, bool)
	Run(ctx context.Context) // refresh loop until ctx ends
}

// MemorySnapshot is who held the host's memory at one moment, recorded when
// the host guard's ceiling makes the kernel OOM in the aq slice so the cause can be named later.
type MemorySnapshot struct {
	// Uncharged is memory in use but charged to no cgroup: the kernel, drivers
	// and compressed swap (zram).
	Available, Uncharged uint64
	Cgroups              []CgroupMemory  // leaf cgroups by memory.current, largest first
	Processes            []ProcessMemory // by RSS, largest first
}
type CgroupMemory struct {
	Path  string `json:"path"`
	Bytes uint64 `json:"bytes"`
}
type ProcessMemory struct {
	PID    int    `json:"pid"`
	Name   string `json:"name"`
	Cgroup string `json:"cgroup"`
	RSS    uint64 `json:"rss"`
	Agent  string `json:"agent,omitempty"` // the agent it descends from ("claude 1234"): it ran this outside aq
}

// ContainerUsage is one container of a stand: its compose service, or its name.
type ContainerUsage struct {
	Name  string `json:"name"`
	Bytes uint64 `json:"bytes"`
}

type AgentUsage struct {
	Name  string `json:"name"`
	Bytes uint64 `json:"bytes"`
	Procs int    `json:"procs"`
	// Spawned is what the agent's processes run outside aq and docker: their
	// descendants that are not agents themselves (a test run, a build).
	Spawned      uint64 `json:"spawned"`
	SpawnedProcs int    `json:"spawned_procs"`
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
