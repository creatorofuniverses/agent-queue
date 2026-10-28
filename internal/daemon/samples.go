package daemon

import (
	"context"
	"log/slog"
	"resource-dispatcher/internal/platform"
	"resource-dispatcher/internal/scheduler"
	"resource-dispatcher/internal/store"
	"time"
)

type SamplerDeps struct {
	Host     platform.HostMetrics
	Usage    platform.UsageMeter
	Store    *store.Store
	Pass     func() scheduler.PassSummary
	Pressure func() (bool, float64)
	Agents   []string
	Now      func() time.Time
}

func RunStatsSampler(ctx context.Context, d SamplerDeps, every time.Duration) {
	if d.Now == nil {
		d.Now = time.Now
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		sampleStep(ctx, d, every)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
func sampleStep(ctx context.Context, d SamplerDeps, every time.Duration) {
	mem, e := d.Host.Memory()
	if e != nil {
		slog.Warn("stats: host memory", "err", e)
		return
	}
	closed, avg := d.Pressure()
	p := d.Pass()
	c := p.Commitments
	u := physical(ctx, d.Usage, d.Agents, mem)
	var agents uint64
	for _, a := range u.Agents {
		agents += a.Bytes
	}
	v := store.HostSample{At: d.Now(), Span: every, MemTotal: mem.Total, MemAvailable: mem.Available, SwapTotal: mem.SwapTotal, SwapFree: mem.SwapFree, PSIFullAvg10: avg, PressureClosed: closed, Agents: agents, Docker: u.Docker, Tasks: u.Tasks, Other: u.Other, TasksExpected: c.TasksExpected, TasksUnconsumed: c.TasksUnconsumed, Stands: c.Stands, StandsUnconsumed: c.StandsUnconsumed, Budgets: c.Budgets, BudgetsUnconsumed: c.BudgetsUnconsumed, CommittedMemory: c.Memory, CommittedCPUs: c.CPUs, Queued: p.Queued, Running: p.Running, HeadBlocker: string(p.Head.Blocker)}
	if e = d.Store.AddHostSample(ctx, v); e != nil {
		slog.Warn("stats: write host sample", "err", e)
	}
}
