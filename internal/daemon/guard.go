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
	next, _ := admission.GuardCeiling(admission.GuardInput{
		Pool: g.cfg.PoolMemory, SliceCurrent: cur, MemAvailable: mem.Available,
		Floor: g.cfg.ReserveFloor(mem.Total), Min: guardMin,
	})
	if !g.have || admission.GuardApply(g.applied, next) {
		// memory.high == memory.max: a soft limit below max only throttles without swap.
		if err := g.exec.SetSliceCeiling(next, next); err != nil {
			slog.Warn("host guard: set slice ceiling", "err", err)
			return
		}
		g.applied, g.have = next, true
	}
	g.setActive(g.applied < g.cfg.PoolMemory)
}
