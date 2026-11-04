package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/creatorofuniverses/agent-queue/internal/diskacct"
	"github.com/creatorofuniverses/agent-queue/internal/model"
	"github.com/creatorofuniverses/agent-queue/internal/platform"
	"github.com/creatorofuniverses/agent-queue/internal/scheduler"
	"github.com/creatorofuniverses/agent-queue/internal/stats"
	"github.com/creatorofuniverses/agent-queue/internal/store"
	"github.com/creatorofuniverses/agent-queue/internal/units"
)

type SamplerDeps struct {
	Host     platform.HostMetrics
	Usage    platform.UsageMeter
	Store    *store.Store
	Pass     func() scheduler.PassSummary
	Pressure func() (bool, float64)
	Agents   []string
	Now      func() time.Time

	// Disk, Acct and Alarm account physical-disk writes by source; nil Disk
	// or Acct: not sampled. One Accountant and Alarm per daemon start: the
	// first sample is only a baseline.
	Disk  platform.DiskMeter
	Acct  *diskacct.Accountant
	Alarm *diskacct.Alarm
	// Emit records an operator event (the alarm's raise or all-clear).
	Emit func(context.Context, model.Event)
	// LastDisk is the rate of the last disk delta, for aq top.
	LastDisk *atomic.Pointer[DiskRate]
	// Home is the daemon's $HOME, shown as ~ in agent projects.
	Home string

	diskAt     time.Time // the last disk sample the accountant took
	diskWarned time.Time // the last logged disk meter error
}

// DiskRate is the host's disk write rate over the last sample and the source
// that wrote the most in it, with the agent estimated to write most inside it.
type DiskRate struct {
	At             time.Time `json:"at"`
	BytesPerSec    float64   `json:"bytes_per_sec"`
	Top            string    `json:"top,omitempty"`
	TopBytesPerSec float64   `json:"top_bytes_per_sec,omitempty"`
	TopAgent       string    `json:"top_agent,omitempty"` // "agent:<name>:<project>"
}

func RunStatsSampler(ctx context.Context, d SamplerDeps, every time.Duration) {
	if d.Now == nil {
		d.Now = time.Now
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		sampleStep(ctx, &d, every)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// sampleStep writes the memory row, then accounts disk writes from the same
// usage scan: the process counters come from that one pass over /proc.
func sampleStep(ctx context.Context, d *SamplerDeps, every time.Duration) {
	mem, e := d.Host.Memory()
	if e != nil {
		slog.Warn("stats: host memory", "err", e)
		return
	}
	closed, avg := d.Pressure()
	p := d.Pass()
	c := p.Commitments
	usage, uerr := scanUsage(ctx, d.Usage, d.Agents)
	u := physicalView(usage, uerr, mem)
	var agents uint64
	for _, a := range u.Agents {
		agents += a.Bytes
	}
	at := d.Now()
	v := store.HostSample{At: at, Span: every, MemTotal: mem.Total, MemAvailable: mem.Available, SwapTotal: mem.SwapTotal, SwapFree: mem.SwapFree, PSIFullAvg10: avg, PressureClosed: closed, Agents: agents, Spawned: u.Spawned, Docker: u.Docker, Tasks: u.Tasks, Other: u.Other, TasksExpected: c.TasksExpected, TasksUnconsumed: c.TasksUnconsumed, Stands: c.Stands, StandsUnconsumed: c.StandsUnconsumed, Budgets: c.Budgets, BudgetsUnconsumed: c.BudgetsUnconsumed, CommittedMemory: c.Memory, CommittedCPUs: c.CPUs, Queued: p.Queued, Running: p.Running, HeadBlocker: string(p.Head.Blocker)}
	if e = d.Store.AddHostSample(ctx, v); e != nil {
		slog.Warn("stats: write host sample", "err", e)
	}
	// A failed scan (a /proc read stuck behind D-state processes, typically
	// during heavy writing) has no process counters: the sources, the host
	// and the alarm are still stepped, the agent estimate waits for the next
	// scan, which covers the gap.
	d.sampleDisk(ctx, usage.IO, uerr != nil, at)
}

func (d *SamplerDeps) sampleDisk(ctx context.Context, procs []platform.ProcIO, procsUnknown bool, at time.Time) {
	if d.Disk == nil || d.Acct == nil {
		return
	}
	sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	snap, e := d.Disk.Snapshot(sctx)
	cancel()
	if e != nil {
		if d.diskWarned.IsZero() || at.Sub(d.diskWarned) >= time.Hour {
			d.diskWarned = at
			slog.Warn("stats: disk writes", "err", e)
		}
		return
	}
	snap.Procs, snap.ProcsUnknown = procs, procsUnknown
	delta, ok := d.Acct.Step(snap)
	prev := d.diskAt
	d.diskAt = at
	if !ok {
		return
	}
	if e := d.Store.AddDiskWrites(ctx, diskRows(at, delta)); e != nil {
		slog.Warn("stats: write disk writes", "err", e)
	}
	span := at.Sub(prev)
	if span <= 0 {
		return
	}
	if d.LastDisk != nil {
		d.LastDisk.Store(diskRate(at, span, delta))
	}
	if d.Alarm == nil {
		return
	}
	if ev, fired := d.Alarm.Observe(at, span, delta); fired && d.Emit != nil {
		d.Emit(ctx, model.Event{ID: string(model.NewID()), Kind: alarmKind(ev), Audience: model.AudienceOperator,
			Detail: alarmText(ev, d.Home), CreatedAt: at.UTC()})
	}
}

// diskRows are a delta's hourly rows: the host counter, each source and each
// agent estimate (within the source it lives in). Zero values add nothing.
func diskRows(at time.Time, d diskacct.Delta) []store.DiskWrite {
	hour := at.UTC().Truncate(time.Hour)
	var rows []store.DiskWrite
	if d.Host != 0 {
		rows = append(rows, store.DiskWrite{Hour: hour, Source: "host", Bytes: d.Host})
	}
	for _, k := range slices.Sorted(maps.Keys(d.Sources)) {
		if b := d.Sources[k]; b != 0 {
			rows = append(rows, store.DiskWrite{Hour: hour, Source: k, Bytes: b})
		}
	}
	for _, a := range d.Agents {
		if a.Bytes != 0 {
			rows = append(rows, store.DiskWrite{Hour: hour, Source: a.Key, Within: a.Within, Bytes: a.Bytes})
		}
	}
	return rows
}

// diskRate is the host rate over span and the source that wrote the most; an
// "*:ended" tail only wins over a named source that wrote more.
func diskRate(at time.Time, span time.Duration, d diskacct.Delta) *DiskRate {
	sec := span.Seconds()
	r := &DiskRate{At: at.UTC(), BytesPerSec: float64(max(d.Host, 0)) / sec}
	var top int64
	ended := func(s string) bool { return strings.HasSuffix(s, ":ended") }
	for k, b := range d.Sources {
		switch {
		case b <= 0 || b < top:
			continue
		case b == top && ended(k) != ended(r.Top):
			if ended(k) {
				continue
			}
		case b == top && k > r.Top:
			continue
		}
		r.Top, top = k, b
	}
	if r.Top == "" {
		return r
	}
	r.TopBytesPerSec = float64(top) / sec
	var best int64
	for _, a := range d.Agents {
		if a.Within == r.Top && a.Bytes > best {
			r.TopAgent, best = a.Key, a.Bytes
		}
	}
	return r
}

func alarmKind(ev diskacct.AlarmEvent) model.EventKind {
	if ev.High {
		return model.EventDiskWritesHigh
	}
	return model.EventDiskWritesNormal
}

// alarmText is the operator's line: the rate, how long and how much, and who.
func alarmText(ev diskacct.AlarmEvent, home string) string {
	var top []string
	for _, s := range ev.Top {
		t := s.Source + " " + perHour(s.BytesPerHour)
		if s.Agent != "" {
			t += " (≈ " + stats.AgentLabel(s.Agent, home) + ")"
		}
		top = append(top, t)
	}
	if ev.High {
		who := strings.Join(top, ", ")
		if who == "" {
			who = "no source measured"
		}
		return fmt.Sprintf("disk writes at %s/h for %s (%s): %s", units.FormatSize(uint64(max(ev.BytesPerHour, 0))), shortDuration(ev.Since), units.FormatSize(ev.Written), who)
	}
	most := "no source measured"
	if len(top) > 0 {
		most = top[0]
	}
	return fmt.Sprintf("disk writes back to %s/h; the episode wrote %s in %s, most by %s", units.FormatSize(uint64(max(ev.BytesPerHour, 0))), units.FormatSize(ev.Written), shortDuration(ev.Since), most)
}

func perHour(b float64) string { return units.FormatSize(uint64(max(b, 0))) + "/h" }

// shortDuration is d to the second without zero tails: "10m", "1h2m", "45s".
func shortDuration(d time.Duration) string {
	s := d.Round(time.Second).String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}
