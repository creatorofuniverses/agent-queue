package daemon

import (
	"agent-queue/internal/admission"
	"agent-queue/internal/model"
	"agent-queue/internal/scheduler"
	"context"
	"encoding/json"
	"time"
)

type EstimateArgs struct {
	Limit          uint64 `json:"limit"`
	Expected       uint64 `json:"expected"`
	CPUs           int    `json:"cpus"`
	TimeoutSeconds int64  `json:"timeout_seconds"`
	Priority       string `json:"priority"`
	Profile        string `json:"profile"`
}
type EstimateView struct {
	StartsNow             bool              `json:"starts_now"`
	Backfill              bool              `json:"backfill"`
	Position              int               `json:"position"`
	ExpectedStart         *time.Time        `json:"expected_start,omitempty"`
	ExpectedWaitSeconds   float64           `json:"expected_wait_seconds"`
	BlockedBy             admission.Blocker `json:"blocked_by"`
	BlockedDetail         string            `json:"blocked_detail"`
	QueueAhead            int               `json:"queue_ahead"`
	QueueAheadMemory      uint64            `json:"queue_ahead_memory"`
	Limit                 uint64            `json:"limit"`
	Expected              uint64            `json:"expected"`
	CPUs                  int               `json:"cpus"`
	MemoryFromProfile     bool              `json:"memory_from_profile"`
	Profile               string            `json:"profile"`
	ProfileRuns           int               `json:"profile_runs"`
	DurationSeconds       float64           `json:"duration_seconds"`
	DurationSource        string            `json:"duration_source"`
	Confidence            string            `json:"confidence"`
	HoldersWithHistory    int               `json:"holders_with_history"`
	HoldersWithoutHistory int               `json:"holders_without_history"`
}

func (d *server) estimate(ctx context.Context, raw json.RawMessage) (any, error) {
	var a EstimateArgs
	if e := decode(raw, &a); e != nil {
		return nil, e
	}
	p, e := model.ParsePriority(a.Priority)
	if e != nil {
		return nil, badRequest("%v", e)
	}
	v, e := d.sched.Estimate(ctx, scheduler.EstimateRequest{Limit: a.Limit, Expected: a.Expected, CPUs: a.CPUs, Timeout: time.Duration(a.TimeoutSeconds) * time.Second, Priority: p, Profile: a.Profile})
	if e != nil {
		return nil, e
	}
	out := EstimateView{StartsNow: v.StartsNow, Backfill: v.Backfill, Position: v.Position, BlockedBy: v.Blocked.Blocker, BlockedDetail: v.Blocked.Detail, QueueAhead: v.QueueAhead, QueueAheadMemory: v.QueueAheadMemory, Limit: v.Limit, Expected: v.Expected, CPUs: v.CPUs, MemoryFromProfile: v.MemoryFromProfile, Profile: v.Profile, ProfileRuns: v.ProfileRuns, DurationSeconds: v.Duration.Seconds(), DurationSource: v.DurationSource, Confidence: v.Confidence, HoldersWithHistory: v.HoldersWithHistory, HoldersWithoutHistory: v.HoldersWithoutHistory}
	if v.HasStart {
		out.ExpectedStart = optTime(v.ExpectedStart)
		out.ExpectedWaitSeconds = nonNeg(time.Until(v.ExpectedStart))
	}
	return out, nil
}
