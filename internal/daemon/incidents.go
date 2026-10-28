package daemon

import (
	"context"
	"log/slog"
	"time"

	"resource-dispatcher/internal/model"
	"resource-dispatcher/internal/platform"
	"resource-dispatcher/internal/store"
)

type incidentSampler struct {
	sliceOOMKills func() (uint64, error)
	slicePrev     uint64
	sliceSeen     bool
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
func RunIncidents(ctx context.Context, host platform.HostMetrics, st *store.Store, pressure func() (closed bool, avg10 float64), sliceOOMKills func() (uint64, error), oomd platform.OOMKillLog, every time.Duration) {
	s := &incidentSampler{host: host, st: st, pressure: pressure, sliceOOMKills: sliceOOMKills, oomd: oomd}
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
	if n, err := s.host.KernelOOMKills(); err != nil {
		slog.Warn("incidents: read kernel oom kills", "err", err)
	} else {
		if s.oomSeen && n > s.oomPrev {
			delta := n - s.oomPrev
			detail := "unattributed"
			if valid && s.sliceSeen {
				delta = sub(delta, sub(slice, s.slicePrev))
				detail = ""
			}
			if delta > 0 {
				s.record(ctx, time.Now().UTC(), "kernel_oom", float64(delta), detail)
			}
		}
		s.oomSeen, s.oomPrev = true, n
		// Advance with the kernel baseline so both deltas span one interval.
		s.sliceSeen, s.slicePrev = valid, slice
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

func (s *incidentSampler) add(ctx context.Context, kind string, v float64) {
	s.record(ctx, time.Now().UTC(), kind, v, "")
}
func (s *incidentSampler) record(ctx context.Context, at time.Time, kind string, v float64, detail string) {
	if err := s.st.AddIncident(ctx, model.Incident{At: at, Kind: kind, Value: v, Detail: detail}); err != nil {
		slog.Warn("incidents: record", "kind", kind, "err", err)
	}
}
