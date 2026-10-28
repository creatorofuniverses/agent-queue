// Package admission holds the pure admission logic: no I/O, no clocks.
package admission

import (
	"errors"
	"fmt"

	"resource-dispatcher/internal/model"
	"resource-dispatcher/internal/units"
)

var (
	// ErrInvalid: the request has zero memory or fewer than one CPU.
	ErrInvalid = errors.New("invalid resource request")
	// ErrTooLarge: the request can never fit into the pool.
	ErrTooLarge = errors.New("request exceeds pool")
)

type Policy struct {
	Pool         model.Resources
	ReserveFloor uint64
	MinDiskFree  uint64
	// TeardownSlots is teardown_concurrency; reservation backfill must not make
	// a teardown the head waits on queue for a slot. Zero means one slot.
	TeardownSlots int
}

type Snapshot struct {
	Committed                          model.Resources
	MemAvailable, Unconsumed, DiskFree uint64
	PressureClosed, Recovering         bool
}

type Blocker string

const (
	BlockNone       Blocker = ""
	BlockRecovering Blocker = "recovering"
	BlockPressure   Blocker = "pressure"
	BlockDisk       Blocker = "disk"
	BlockPoolMemory Blocker = "pool_memory"
	BlockPoolCPU    Blocker = "pool_cpu"
	BlockLiveMemory Blocker = "live_memory"
)

type Decision struct {
	Admit   bool
	Blocker Blocker
	Detail  string
}

// satSub returns a-b, or 0 if b > a.
func satSub(a, b uint64) uint64 {
	if b > a {
		return 0
	}
	return a - b
}

// satAdd returns a+b, capped at the uint64 maximum.
func satAdd(a, b uint64) uint64 {
	if s := a + b; s >= a {
		return s
	}
	return ^uint64(0)
}

// Decide checks the blockers in strict order and admits if none applies.
func Decide(p Policy, s Snapshot, req model.Resources) Decision {
	block := func(b Blocker, format string, args ...any) Decision {
		return Decision{Blocker: b, Detail: fmt.Sprintf(format, args...)}
	}
	switch {
	case s.Recovering:
		return block(BlockRecovering, "daemon is recovering state after restart")
	case s.PressureClosed:
		return block(BlockPressure, "memory pressure gate is closed")
	case s.DiskFree < p.MinDiskFree:
		return block(BlockDisk, "disk free %s below minimum %s",
			units.FormatSize(s.DiskFree), units.FormatSize(p.MinDiskFree))
	}
	if need := satAdd(s.Committed.MemoryBytes, req.MemoryBytes); need > p.Pool.MemoryBytes {
		return block(BlockPoolMemory, "pool memory: committed %s + requested %s exceeds pool %s",
			units.FormatSize(s.Committed.MemoryBytes), units.FormatSize(req.MemoryBytes), units.FormatSize(p.Pool.MemoryBytes))
	}
	if s.Committed.CPUs+req.CPUs > p.Pool.CPUs {
		return block(BlockPoolCPU, "pool cpus: committed %d + requested %d exceeds pool %d",
			s.Committed.CPUs, req.CPUs, p.Pool.CPUs)
	}
	free := satSub(s.MemAvailable, s.Unconsumed)
	if need := satAdd(p.ReserveFloor, req.MemoryBytes); free < need {
		return block(BlockLiveMemory, "live memory: %s available minus %s unconsumed leaves %s, need %s (floor %s + requested %s)",
			units.FormatSize(s.MemAvailable), units.FormatSize(s.Unconsumed), units.FormatSize(free),
			units.FormatSize(need), units.FormatSize(p.ReserveFloor), units.FormatSize(req.MemoryBytes))
	}
	return Decision{Admit: true}
}

// Validate rejects requests that are malformed or can never fit the pool.
func Validate(p Policy, req model.Resources) error {
	if req.MemoryBytes == 0 || req.CPUs < 1 {
		return ErrInvalid
	}
	if req.MemoryBytes > p.Pool.MemoryBytes || req.CPUs > p.Pool.CPUs {
		return ErrTooLarge
	}
	return nil
}
