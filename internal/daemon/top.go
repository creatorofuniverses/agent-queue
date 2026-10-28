package daemon

import (
	"agent-queue/internal/admission"
	"agent-queue/internal/model"
	"agent-queue/internal/platform"
	"agent-queue/internal/scheduler"
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"
)

type HostView struct {
	MemTotal       uint64  `json:"mem_total"`
	MemAvailable   uint64  `json:"mem_available"`
	SwapTotal      uint64  `json:"swap_total"`
	SwapFree       uint64  `json:"swap_free"`
	PSIFullAvg10   float64 `json:"psi_full_avg10"`
	PressureClosed bool    `json:"pressure_closed"`
}
type PhysicalView struct {
	Used   uint64                `json:"used"`
	Agents []platform.AgentUsage `json:"agents"`
	Docker uint64                `json:"docker"`
	Tasks  uint64                `json:"tasks"`
	Other  uint64                `json:"other"`
	Error  string                `json:"error,omitempty"`
}
type QueueView struct {
	Queued        int               `json:"queued"`
	Running       int               `json:"running"`
	Held          int               `json:"held"`
	HeadID        model.ItemID      `json:"head_id"`
	BlockedBy     admission.Blocker `json:"blocked_by"`
	BlockedDetail string            `json:"blocked_detail"`
}
type TopView struct {
	At          time.Time             `json:"at"`
	Host        HostView              `json:"host"`
	Physical    PhysicalView          `json:"physical"`
	Commitments scheduler.Commitments `json:"commitments"`
	Queue       QueueView             `json:"queue"`
}

// boundedUsage lets callers give up on a scan blocked in a /proc read (a D-state
// process holding its mmap lock ignores ctx) and keeps at most one scan in flight.
type boundedUsage struct {
	inner platform.UsageMeter
	busy  atomic.Bool
}

func (b *boundedUsage) Usage(ctx context.Context, agents []string) (platform.Usage, error) {
	if !b.busy.CompareAndSwap(false, true) {
		return platform.Usage{}, errors.New("previous usage scan still running")
	}
	type result struct {
		u platform.Usage
		e error
	}
	done := make(chan result, 1)
	go func() {
		defer b.busy.Store(false)
		u, e := b.inner.Usage(ctx, agents)
		done <- result{u, e}
	}()
	select {
	case r := <-done:
		return r.u, r.e
	case <-ctx.Done():
		return platform.Usage{}, ctx.Err()
	}
}

func physical(ctx context.Context, usage platform.UsageMeter, agents []string, mem platform.MemInfo) PhysicalView {
	if usage == nil {
		return PhysicalView{Error: "usage meter unavailable"}
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	u, e := usage.Usage(ctx, agents)
	if e != nil {
		return PhysicalView{Error: e.Error()}
	}
	v := PhysicalView{Used: sub(mem.Total, mem.Available), Agents: u.Agents, Docker: u.Docker, Tasks: u.Tasks}
	other := sub(sub(v.Used, u.Docker), u.Tasks)
	for _, a := range u.Agents {
		other = sub(other, a.Bytes)
	}
	v.Other = other
	return v
}
func sub(a, b uint64) uint64 {
	if b > a {
		return 0
	}
	return a - b
}
func (d *server) top(ctx context.Context) (any, error) {
	mem, e := d.host.Memory()
	if e != nil {
		return nil, fmt.Errorf("host memory: %w", e)
	}
	closed, avg := d.sched.Pressure()
	p := d.sched.LastPass()
	return TopView{At: time.Now().UTC(), Host: HostView{mem.Total, mem.Available, mem.SwapTotal, mem.SwapFree, avg, closed}, Physical: physical(ctx, d.opts.Usage, d.cfg.Stats.AgentProcesses, mem), Commitments: p.Commitments, Queue: QueueView{p.Queued, p.Running, p.Held, p.HeadID, p.Head.Blocker, p.Head.Detail}}, nil
}
