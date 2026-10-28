package scheduler

import (
	"context"
	"fmt"
	"resource-dispatcher/internal/model"
	"resource-dispatcher/internal/profile"
	"time"
)

type runSamples struct {
	Last    time.Time
	Hist    profile.Histogram
	Samples int
}

func (s *Scheduler) ProfileEstimate(ctx context.Context, name string) (profile.Estimate, bool, error) {
	p := s.d.Cfg.Profiles
	rs, e := s.d.Store.UsableRuns(ctx, name, p.Window)
	if e != nil {
		return profile.Estimate{}, false, e
	}
	v, ok := profile.Compute(rs, profile.Params{Window: p.Window, MemoryPercentile: p.MemoryPercentile, DurationPercentile: p.DurationPercentile, LimitMargin: p.LimitMargin, MinMemory: minMemory, MaxMemory: s.d.Cfg.PoolMemory})
	return v, ok, nil
}
func (s *Scheduler) applyProfile(ctx context.Context, r *SubmitRequest) (from bool, duration time.Duration, err error) {
	if r.Profile != "" {
		if err = profile.ValidName(r.Profile); err != nil {
			return
		}
		v, ok, e := s.ProfileEstimate(ctx, r.Profile)
		if e != nil {
			return false, 0, e
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
			return false, 0, fmt.Errorf("memory not given and profile %s has no usable runs; pass --memory", r.Profile)
		}
	}
	if r.Limit == 0 {
		return false, 0, fmt.Errorf("memory limit is required")
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
	r := &profile.Run{ItemID: it.ID, Profile: it.Profile, EndedAt: it.EndedAt, State: it.State, Reason: it.Reason, CPUs: it.CPUs, Limit: it.Limit, Expected: it.Expected, MemoryFromProfile: it.MemoryFromProfile, EstDuration: it.EstDuration, Duration: it.EndedAt.Sub(start), MemoryPeak: it.MemoryPeak}
	if v := s.samples[it.ID]; v != nil {
		r.Hist = v.Hist
		r.Samples = v.Samples
	}
	if r.Hist.Count() == 0 {
		r.Hist.Add(it.MemoryPeak)
	}
	return r
}
