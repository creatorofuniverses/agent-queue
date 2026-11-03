package daemon

import (
	"context"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/creatorofuniverses/agent-queue/internal/model"
	"github.com/creatorofuniverses/agent-queue/internal/platform"
	"github.com/creatorofuniverses/agent-queue/internal/store"
)

type incidentSampler struct {
	sliceOOMKills func() (uint64, error)
	slicePrev     uint64
	sliceSeen     bool
	groupOOMKills func() (map[string]uint64, error)
	groupPrev     map[string]uint64 // nil until a baseline is read
	oomd          platform.OOMKillLog
	steps         int
	oomdErrorAt   time.Time
	host          platform.HostMetrics
	st            *store.Store
	pressure      func() (closed bool, avg10 float64)

	oomSeen   bool
	oomPrev   uint64
	psiSeen   bool
	psiClosed bool
}

// RunIncidents records kernel OOM kills and pressure-gate transitions until ctx ends.
// The first sample of each is a baseline, not an incident.
func RunIncidents(ctx context.Context, host platform.HostMetrics, st *store.Store, pressure func() (closed bool, avg10 float64), sliceOOMKills func() (uint64, error), groupOOMKills func() (map[string]uint64, error), oomd platform.OOMKillLog, every time.Duration) {
	s := &incidentSampler{host: host, st: st, pressure: pressure, sliceOOMKills: sliceOOMKills, groupOOMKills: groupOOMKills, oomd: oomd}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		s.step(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *incidentSampler) step(ctx context.Context) {
	slice, valid := uint64(0), false
	if s.sliceOOMKills != nil {
		var err error
		slice, err = s.sliceOOMKills()
		valid = err == nil
	}
	var groups map[string]uint64
	if s.groupOOMKills != nil {
		if g, err := s.groupOOMKills(); err == nil {
			groups = g
		}
	}
	if n, err := s.host.KernelOOMKills(); err != nil {
		slog.Warn("incidents: read kernel oom kills", "err", err)
	} else {
		if s.oomSeen && n > s.oomPrev {
			delta := n - s.oomPrev
			detail := "unattributed"
			if valid && s.sliceSeen {
				delta = sub(delta, sub(slice, s.slicePrev))
				detail = ""
				delta = s.recordGroups(ctx, groups, delta)
			}
			if delta > 0 {
				s.record(ctx, time.Now().UTC(), "kernel_oom", float64(delta), detail)
			}
		}
		s.oomSeen, s.oomPrev = true, n
		// Advance with the kernel baseline so all deltas span one interval.
		s.sliceSeen, s.slicePrev = valid, slice
		s.groupPrev = groups
	}
	if s.oomd != nil && s.steps%6 == 0 {
		kills, err := s.oomd.Kills(ctx)
		if err != nil {
			if s.oomdErrorAt.IsZero() || time.Since(s.oomdErrorAt) >= time.Hour {
				slog.Warn("incidents: read oomd journal", "err", err)
				s.oomdErrorAt = time.Now()
			}
		} else {
			for _, k := range kills {
				s.record(ctx, k.At, "oomd_kill", 1, k.Cgroup)
			}
		}
	}
	s.steps++
	closed, avg := s.pressure()
	if s.psiSeen && closed != s.psiClosed {
		kind := "psi_open"
		if closed {
			kind = "psi_close"
		}
		s.add(ctx, kind, avg)
	}
	s.psiSeen, s.psiClosed = true, closed
}

// recordGroups records the part of delta kernel OOM kills that landed in a
// known cgroup outside aq, one incident per cgroup, and returns the rest. A
// cgroup new since the last sample counts from zero. Ended cgroups come last:
// a child that disappears moves its old kills into their count.
func (s *incidentSampler) recordGroups(ctx context.Context, groups map[string]uint64, delta uint64) uint64 {
	if groups == nil || s.groupPrev == nil {
		return delta
	}
	now := time.Now().UTC()
	names := slices.DeleteFunc(slices.Sorted(maps.Keys(groups)), func(n string) bool { return n == platform.EndedCgroups })
	if _, ok := s.groupPrev[platform.EndedCgroups]; ok {
		names = append(names, platform.EndedCgroups)
	}
	for _, name := range names {
		d := min(sub(groups[name], s.groupPrev[name]), delta)
		if d > 0 {
			s.record(ctx, now, "kernel_oom", float64(d), name)
			delta -= d
		}
	}
	return delta
}

func (s *incidentSampler) add(ctx context.Context, kind string, v float64) {
	s.record(ctx, time.Now().UTC(), kind, v, "")
}
func (s *incidentSampler) record(ctx context.Context, at time.Time, kind string, v float64, detail string) {
	if err := s.st.AddIncident(ctx, model.Incident{At: at, Kind: kind, Value: v, Detail: detail}); err != nil {
		slog.Warn("incidents: record", "kind", kind, "err", err)
	}
}
