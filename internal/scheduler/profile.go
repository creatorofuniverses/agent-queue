package scheduler

import (
	"context"
	"fmt"
	"github.com/creatorofuniverses/agent-queue/internal/model"
	"github.com/creatorofuniverses/agent-queue/internal/profile"
	"time"
)

type runSamples struct {
	Last    time.Time
	Hist    profile.Histogram
	Samples int
}

func (s *Scheduler) ProfileEstimate(ctx context.Context, name string) (profile.Estimate, bool, error) {
	rs, e := s.d.Store.UsableRuns(ctx, name, s.d.Cfg.Profiles.Window)
	if e != nil {
		return profile.Estimate{}, false, e
	}
	v, ok := profile.Compute(rs, s.profileParams())
	return v, ok, nil
}

func (s *Scheduler) profileParams() profile.Params {
	p := s.d.Cfg.Profiles
	return profile.Params{Window: p.Window, MemoryPercentile: p.MemoryPercentile, DurationPercentile: p.DurationPercentile, LimitMargin: p.LimitMargin, MinMemory: minMemory, MaxMemory: s.d.Cfg.PoolMemory}
}

// chargeByHistory charges a new reservation what the history of its profile
// says it needs, never more than declared: the stand as Expected (Limit stays
// the declaration) and the task budget as TaskExpected (TaskMemory stays the
// ceiling its nested tasks are admitted within).
func (s *Scheduler) chargeByHistory(ctx context.Context, it *model.Item) error {
	if it.Profile == "" {
		return nil
	}
	p := s.profileParams()
	hist, err := s.d.Store.ReservationHistory(ctx, it.Profile, 5*p.Window) // stand and budget runs are counted apart
	if err != nil {
		return fmt.Errorf("profile %s: %w", it.Profile, err)
	}
	v := profile.ForReservation(hist, p)
	if v.StandRuns > 0 && it.DockerProject != "" && v.Stand < it.Limit {
		it.Expected, it.MemoryFromProfile = v.Stand, true
	}
	if v.BudgetRuns > 0 && v.Budget < it.TaskMemory {
		it.TaskExpected, it.MemoryFromProfile = v.Budget, true
	}
	return nil
}

// applyProfile returns reject for a request that can never be admitted as given,
// and err for a store failure the caller may retry.
func (s *Scheduler) applyProfile(ctx context.Context, r *SubmitRequest) (from bool, duration time.Duration, reject, err error) {
	if r.Profile != "" {
		if reject = profile.ValidName(r.Profile); reject != nil {
			return
		}
		v, ok, e := s.ProfileEstimate(ctx, r.Profile)
		if e != nil {
			return false, 0, nil, e
		}
		if ok {
			duration = min(v.Duration, r.Timeout)
			if r.Limit == 0 {
				r.Limit = v.Limit
				if r.Expected == 0 {
					r.Expected = v.Expected
				}
				from = true
			}
		} else if r.Limit == 0 {
			return false, 0, fmt.Errorf("memory not given and profile %s has no usable runs; pass --memory", r.Profile), nil
		}
	}
	if r.Limit == 0 {
		return false, 0, fmt.Errorf("memory limit is required"), nil
	}
	return
}
func (s *Scheduler) sampleLocked(it *model.Item, cur uint64) {
	if it.Profile == "" || it.State != model.StateRunning || cur == 0 {
		return
	}
	now := s.d.Now()
	v := s.samples[it.ID]
	if v == nil {
		v = &runSamples{}
		s.samples[it.ID] = v
	}
	if !v.Last.IsZero() && now.Sub(v.Last) < time.Second {
		return
	}
	v.Last = now
	v.Hist.Add(cur)
	v.Samples++
}
func (s *Scheduler) profileRunLocked(it *model.Item) *profile.Run {
	start := it.StartedAt
	if start.IsZero() {
		start = it.AdmittedAt
	}
	if it.Profile == "" || start.IsZero() {
		return nil
	}
	duration := it.EndedAt.Sub(start)
	if duration <= 0 && !it.EndedAt.IsZero() {
		// StartedAt is stamped after systemd-run returns, so a run of a few
		// milliseconds can end before it; such a run still counts.
		duration = max(it.EndedAt.Sub(it.AdmittedAt), time.Millisecond)
	}
	r := &profile.Run{ItemID: it.ID, Profile: it.Profile, EndedAt: it.EndedAt, State: it.State, Reason: it.Reason, CPUs: it.CPUs, Limit: it.Limit, Expected: it.Expected, MemoryFromProfile: it.MemoryFromProfile, EstDuration: it.EstDuration, Duration: duration, MemoryPeak: it.MemoryPeak, IOWriteBytes: it.IOWriteBytes}
	if v := s.samples[it.ID]; v != nil {
		r.Hist = v.Hist
		r.Samples = v.Samples
	}
	if r.Hist.Count() == 0 {
		r.Hist.Add(it.MemoryPeak)
	}
	return r
}
