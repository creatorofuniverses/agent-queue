package daemon

import (
	"context"
	"log/slog"
	"time"

	"resource-dispatcher/internal/admission"
	"resource-dispatcher/internal/config"
	"resource-dispatcher/internal/platform"
)

// guardMin is the lowest ceiling the guard sets.
const guardMin = 256 << 20

type guard struct {
	exec      platform.Executor
	host      platform.HostMetrics
	cfg       config.Config
	setActive func(bool)
	applied   uint64
	have      bool
}

// RunGuard keeps the slice ceiling below what the host can spare, so that
// under memory shortage managed tasks are killed first. It blocks until ctx ends.
func RunGuard(ctx context.Context, exec platform.Executor, host platform.HostMetrics, cfg config.Config, setActive func(bool), every time.Duration) {
	g := &guard{exec: exec, host: host, cfg: cfg, setActive: setActive}
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
