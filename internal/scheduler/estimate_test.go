package scheduler

import (
	"context"
	"errors"
	"resource-dispatcher/internal/admission"
	"resource-dispatcher/internal/model"
	"resource-dispatcher/internal/store"
	"testing"
	"time"
)

func estimate(t *testing.T, e *env, r EstimateRequest) Estimate {
	t.Helper()
	ctx, cancel := context.WithTimeout(e.ctx, 3*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); e.s.Run(ctx) }()
	v, err := e.s.Estimate(ctx, r)
	cancel()
	<-done
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func TestEstimateStartsNow(t *testing.T) {
	e := newEnv(t, nil)
	v := estimate(t, e, EstimateRequest{Limit: 100 * mib, CPUs: 1, Timeout: time.Minute})
	its, _ := e.st.List(e.ctx, store.ListFilter{IncludeTerminal: true})
	if !v.StartsNow || v.Position != 0 || v.Confidence != "now" || len(its) != 0 {
		t.Fatal(v, its)
	}
}
func TestEstimateBackfill(t *testing.T) {
	e, _, _ := backfillEnv(t)
	v := estimate(t, e, EstimateRequest{Limit: gib, CPUs: 1, Timeout: 10 * time.Minute})
	if !v.StartsNow || !v.Backfill {
		t.Fatal(v)
	}
}
func TestEstimateQueued(t *testing.T) {
	e, a, head := backfillEnv(t)
	v := estimate(t, e, EstimateRequest{Limit: 10 * gib, CPUs: 1, Timeout: time.Hour})
	want := e.get(t, a.ID).StartedAt.Add(2 * time.Hour)
	if v.Position != 2 || v.QueueAhead != 1 || v.QueueAheadMemory != 10*gib || v.Blocked.Blocker != admission.BlockPoolMemory || !v.ExpectedStart.Equal(want) || v.Confidence != "timeout" {
		t.Fatal(v, head)
	}
	v = estimate(t, e, EstimateRequest{Limit: 7 * gib, CPUs: 1, Timeout: 2 * time.Hour})
	if v.StartsNow || v.Blocked.Blocker != admission.BlockQueue {
		t.Fatal(v)
	}
}
func TestEstimatePriority(t *testing.T) {
	e, _, _ := backfillEnv(t)
	v := estimate(t, e, EstimateRequest{Limit: 10 * gib, CPUs: 1, Timeout: time.Hour, Priority: model.PriorityHigh})
	if v.Position != 1 || v.QueueAhead != 0 {
		t.Fatal(v)
	}
}
func TestEstimateUnknownOnPressure(t *testing.T) {
	e := newEnv(t, nil)
	e.host.SetPressure(10)
	e.tick(t)
	e.clk.Advance(11 * time.Second)
	v := estimate(t, e, EstimateRequest{Limit: 100 * mib, CPUs: 1})
	if v.HasStart || v.Blocked.Blocker != admission.BlockPressure || v.Confidence != "unknown" {
		t.Fatal(v)
	}
}
func TestEstimateFromProfile(t *testing.T) {
	e := newEnv(t, nil)
	train(t, e)
	v := estimate(t, e, EstimateRequest{Profile: "p", CPUs: 1})
	if !v.MemoryFromProfile || v.ProfileRuns != 3 || v.DurationSource != "profile" {
		t.Fatal(v)
	}
	ctx, cancel := context.WithTimeout(e.ctx, time.Second)
	defer cancel()
	if _, err := e.s.Estimate(ctx, EstimateRequest{Profile: "missing", CPUs: 1}); !errors.Is(err, ErrRejected) {
		t.Fatal(err)
	}
}
func TestEstimateConfidencePartial(t *testing.T) {
	e, a, _ := backfillEnv(t)
	a = e.get(t, a.ID)
	a.EstDuration = 30 * time.Minute
	e.st.Update(e.ctx, a)
	b := e.submit(t, e.req(gib))
	e.tick(t)
	v := estimate(t, e, EstimateRequest{Limit: 10 * gib, CPUs: 1, Timeout: time.Hour})
	if v.Confidence != "partial" || v.HoldersWithHistory != 1 || v.HoldersWithoutHistory < 1 {
		t.Fatal(v, b)
	}
}
