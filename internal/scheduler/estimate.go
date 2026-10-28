package scheduler

import (
	"context"
	"errors"
	"fmt"
	"github.com/creatorofuniverses/agent-queue/internal/admission"
	"github.com/creatorofuniverses/agent-queue/internal/model"
	"github.com/creatorofuniverses/agent-queue/internal/profile"
	"slices"
	"time"
)

type EstimateRequest struct {
	Limit, Expected uint64
	CPUs            int
	Timeout         time.Duration
	Priority        model.Priority
	Profile         string
}
type Estimate struct {
	StartsNow, Backfill                       bool
	Position                                  int
	ExpectedStart                             time.Time
	HasStart                                  bool
	Blocked                                   admission.Decision
	QueueAhead                                int
	QueueAheadMemory                          uint64
	Limit, Expected                           uint64
	CPUs                                      int
	MemoryFromProfile                         bool
	Profile                                   string
	ProfileRuns                               int
	Duration                                  time.Duration
	DurationSource, Confidence                string
	HoldersWithHistory, HoldersWithoutHistory int
}

func (s *Scheduler) Estimate(ctx context.Context, r EstimateRequest) (Estimate, error) {
	v := Estimate{Limit: r.Limit, Expected: r.Expected, CPUs: r.CPUs, Profile: r.Profile, Duration: r.Timeout, DurationSource: "timeout", Confidence: "unknown"}
	reject := func(err error) (Estimate, error) { return v, fmt.Errorf("%w: %v", ErrRejected, err) }
	if r.Timeout < 0 {
		return reject(fmt.Errorf("timeout must not be negative"))
	}
	if r.Priority != model.PriorityLow && r.Priority != model.PriorityNormal && r.Priority != model.PriorityHigh {
		return reject(fmt.Errorf("invalid priority"))
	}
	if r.Profile != "" {
		if e := profile.ValidName(r.Profile); e != nil {
			return reject(e)
		}
		est, ok, e := s.ProfileEstimate(ctx, r.Profile)
		if e != nil {
			return v, e
		}
		if ok {
			v.ProfileRuns = est.Runs
			v.Duration = est.Duration
			if r.Timeout > 0 {
				v.Duration = min(v.Duration, r.Timeout)
			}
			v.DurationSource = "profile"
			if r.Limit == 0 {
				v.Limit = est.Limit
				v.MemoryFromProfile = true
				if v.Expected == 0 {
					v.Expected = est.Expected
				}
			}
		} else if r.Limit == 0 {
			return reject(fmt.Errorf("memory not given and profile %s has no usable runs; pass --memory", r.Profile))
		}
	}
	if v.Expected == 0 {
		v.Expected = v.Limit
	}
	if v.Limit < minMemory {
		return reject(fmt.Errorf("memory limit must be at least 64 MiB"))
	}
	if v.Expected > v.Limit {
		return reject(fmt.Errorf("expected memory exceeds limit"))
	}
	if e := admission.Validate(s.policy(0), model.Resources{MemoryBytes: v.Limit, CPUs: v.CPUs}); e != nil {
		return reject(e)
	}
	if v.Duration == 0 {
		v.Duration = admission.Unbounded
		v.DurationSource = "unknown"
	}
	if err := s.AfterNextPass(ctx); err != nil {
		return v, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	last := s.last
	if last.aborted {
		return v, errors.New("the last admission pass failed; retry the estimate")
	}
	index := len(last.items)
	for i, it := range last.items {
		if it.Priority < r.Priority {
			index = i
			break
		}
	}
	v.QueueAhead = index
	for _, it := range last.items[:index] {
		v.QueueAheadMemory += it.Committed().MemoryBytes
	}
	q := admission.Waiting{Res: model.Resources{MemoryBytes: v.Expected, CPUs: v.CPUs}, Duration: v.Duration, FromHistory: v.DurationSource == "profile"}
	if v.DurationSource == "timeout" {
		q.Duration += s.d.Cfg.CancelGrace // same as waitingOf for a submitted task
	}
	queue := slices.Clone(last.waiting)
	queue = slices.Insert(queue, index, q)
	pass := admission.PlanPass(last.policy, last.after, last.holders, queue, last.at, s.d.Cfg.Scheduler.Backfill)
	for _, i := range pass.Admitted {
		if i == index {
			v.StartsNow = true
			v.Backfill = pass.Backfilled[i]
			v.HasStart = true
			v.ExpectedStart = last.at
			v.Confidence = "now"
			return v, nil
		}
	}
	v.Position = index + 1
	v.Blocked = admission.Decide(last.policy, last.after, q.Res)
	if v.Blocked.Admit {
		v.Blocked = admission.Decision{Blocker: admission.BlockQueue, Detail: "earlier items in queue"}
	}
	if pass.HeadDecision.Blocker.Gate() {
		v.Blocked = pass.HeadDecision
		return v, nil
	}
	forecast := admission.Schedule(last.at, admission.FreeCapacity(last.policy, last.after), last.holders, queue)
	if forecast[index].OK {
		v.ExpectedStart = forecast[index].Start
		v.HasStart = true
	}
	for _, h := range last.holders {
		if h.Never {
			continue
		}
		if h.FromHistory {
			v.HoldersWithHistory++
		} else {
			v.HoldersWithoutHistory++
		}
	}
	for _, q := range queue[:index] {
		if q.FromHistory {
			v.HoldersWithHistory++
		} else {
			v.HoldersWithoutHistory++
		}
	}
	if v.HasStart {
		switch {
		case v.HoldersWithoutHistory == 0:
			v.Confidence = "history"
		case v.HoldersWithHistory == 0:
			v.Confidence = "timeout"
		default:
			v.Confidence = "partial"
		}
	}
	return v, nil
}
