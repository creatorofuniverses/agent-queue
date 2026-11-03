package daemon

import (
	"context"
	"log/slog"
	"time"

	"github.com/creatorofuniverses/agent-queue/internal/admission"
	"github.com/creatorofuniverses/agent-queue/internal/config"
	"github.com/creatorofuniverses/agent-queue/internal/platform"
)

// guardMin is the lowest ceiling the guard sets.
const guardMin = 256 << 20

type guard struct {
	exec      platform.Executor
	host      platform.HostMetrics
	cfg       config.Config
	setActive func(bool)
	// engaged is called when the guard's ceiling made the kernel OOM in the
	// slice, so the cause of the shortage can be recorded.
	engaged func()
	applied uint64
	have    bool
	ooms    uint64 // OOMs at the slice's own ceiling, as last read
	oomSeen bool
}

// RunGuard keeps the slice ceiling below what the host can spare, so that
// under memory shortage managed tasks are killed first. It blocks until ctx ends.
func RunGuard(ctx context.Context, exec platform.Executor, host platform.HostMetrics, cfg config.Config, setActive func(bool), engaged func(), every time.Duration) {
	g := &guard{exec: exec, host: host, cfg: cfg, setActive: setActive, engaged: engaged}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		g.step()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// step samples once. On any read error it keeps the applied ceiling.
func (g *guard) step() {
	g.checkCut()
	mem, err := g.host.Memory()
	if err != nil {
		slog.Warn("host guard: read host memory", "err", err)
		return
	}
	cur, err := g.exec.SliceCurrent()
	if err != nil {
		slog.Warn("host guard: read slice usage", "err", err)
		return
	}
	actual, err := g.exec.SliceCeiling()
	if err != nil {
		slog.Warn("host guard: read slice ceiling", "err", err)
		return
	}
	next, _ := admission.GuardCeiling(admission.GuardInput{
		Pool: g.cfg.PoolMemory, SliceCurrent: cur, MemAvailable: mem.Available,
		Floor: g.cfg.ReserveFloor(mem.Total), Min: guardMin,
	})
	target := g.applied
	if !g.have || admission.GuardApply(g.applied, next) {
		target = next
	}
	// systemd resets memory.max when units start or stop in the slice, and a
	// signal can make the kernel set the limit without reclaiming: rewrite then.
	if g.have && target == g.applied && actual == g.applied && cur <= g.applied {
		g.setActive(g.applied < g.cfg.PoolMemory)
		return
	}
	active := target < g.cfg.PoolMemory
	if active {
		g.setActive(true) // before the write: the OOM kill happens inside it
	}
	// memory.high == memory.max: a soft limit below max only throttles without swap.
	// The write blocks for reclaim/OOM; bounded, but may take seconds.
	if err := g.exec.SetSliceCeiling(target, target); err != nil {
		slog.Warn("host guard: set slice ceiling", "err", err)
		g.setActive(g.have && g.applied < g.cfg.PoolMemory)
		return
	}
	g.applied, g.have = target, true
	if !active {
		g.setActive(false)
	}
}

// checkCut reports an OOM at the slice's own ceiling while that ceiling was the
// guard's. A ceiling below the pool alone is no cut: when the pool is close to
// the host's memory, the guard holds one from the moment the daemon starts.
func (g *guard) checkCut() {
	n, err := g.exec.SliceOOMs()
	if err != nil {
		return // the next read catches up
	}
	if g.oomSeen && n > g.ooms && g.have && g.applied < g.cfg.PoolMemory && g.engaged != nil {
		g.engaged()
	}
	g.ooms, g.oomSeen = n, true
}
