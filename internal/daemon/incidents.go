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
	host     platform.HostMetrics
	st       *store.Store
	pressure func() (closed bool, avg10 float64)

	oomSeen   bool
	oomPrev   uint64
	psiSeen   bool
	psiClosed bool
}

// RunIncidents records kernel OOM kills and pressure-gate transitions until ctx ends.
// The first sample of each is a baseline, not an incident.
func RunIncidents(ctx context.Context, host platform.HostMetrics, st *store.Store, pressure func() (closed bool, avg10 float64), every time.Duration) {
	s := &incidentSampler{host: host, st: st, pressure: pressure}
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
	if n, err := s.host.KernelOOMKills(); err != nil {
		slog.Warn("incidents: read kernel oom kills", "err", err)
	} else {
		if s.oomSeen && n > s.oomPrev {
			s.add(ctx, "kernel_oom", float64(n-s.oomPrev))
		}
		s.oomSeen, s.oomPrev = true, n
	}
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
	if err := s.st.AddIncident(ctx, model.Incident{At: time.Now().UTC(), Kind: kind, Value: v}); err != nil {
		slog.Warn("incidents: record", "kind", kind, "err", err)
	}
}
