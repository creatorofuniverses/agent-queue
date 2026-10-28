package scheduler

import (
	"github.com/creatorofuniverses/agent-queue/internal/admission"
	"github.com/creatorofuniverses/agent-queue/internal/model"
	"time"
)

type Commitments struct {
	TasksExpected      uint64 `json:"tasks_expected"`
	TasksUnconsumed    uint64 `json:"tasks_unconsumed"`
	Stands             uint64 `json:"stands"`
	StandsUnconsumed   uint64 `json:"stands_unconsumed"`
	Budgets            uint64 `json:"budgets"`
	BudgetsUnconsumed  uint64 `json:"budgets_unconsumed"`
	TeardownUnconsumed uint64 `json:"teardown_unconsumed"`
	Memory             uint64 `json:"memory"`
	CPUs               int    `json:"cpus"`
	PoolMemory         uint64 `json:"pool_memory"`
	PoolCPUs           int    `json:"pool_cpus"`
}
type PassSummary struct {
	At                    time.Time
	Commitments           Commitments
	Queued, Running, Held int
	HeadID                model.ItemID
	Head                  admission.Decision
}
type passState struct {
	policy  admission.Policy
	after   admission.Snapshot
	holders []admission.Holder
	items   []*model.Item
	waiting []admission.Waiting
	at      time.Time
	summary PassSummary
	starts  map[model.ItemID]time.Time
	// aborted: the latest pass failed before saving, so this state is stale.
	aborted bool
}

func (s *Scheduler) LastPass() PassSummary { s.mu.Lock(); defer s.mu.Unlock(); return s.last.summary }
func (s *Scheduler) ExpectedStart(id model.ItemID) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	at, ok := s.last.starts[id]
	return at, ok
}
func (s *Scheduler) waitingOf(it *model.Item) admission.Waiting {
	d := it.EstDuration
	history := d > 0
	if it.IsReservation() {
		d = it.Timeout + s.d.Cfg.Reservations.TeardownTimeout
		history = true
	} else if d <= 0 {
		// A task that hits its timeout is stopped and given the kill grace.
		d = it.Timeout + s.d.Cfg.CancelGrace
	}
	return admission.Waiting{ID: it.ID, Res: it.Committed(), Duration: d, FromHistory: history, ExtraOnly: it.IsReservation()}
}
func (s *Scheduler) holdersLocked(items []*model.Item, now time.Time) []admission.Holder {
	// A reservation is held past its TTL until its nested tasks end.
	nestedEnd := map[model.ItemID]time.Time{}
	for _, it := range items {
		if it.ParentID != "" && (it.State == model.StateStarting || it.State == model.StateRunning) {
			start := it.StartedAt
			if start.IsZero() {
				start = now
			}
			nestedEnd[it.ParentID] = maxTime(nestedEnd[it.ParentID], start.Add(it.Timeout+s.d.Cfg.CancelGrace))
		}
	}
	var out []admission.Holder
	for _, it := range items {
		if it.ParentID != "" || it.State == model.StateQueued || it.State.Terminal() {
			continue
		}
		h := admission.Holder{ID: it.ID, Res: it.Committed(), Live: s.holderLive[it.ID], Teardown: it.IsReservation()}
		w := s.waitingOf(it)
		h.FromHistory = w.FromHistory
		h.End = now
		if it.IsReservation() {
			switch it.State {
			case model.StateGranted, model.StateExpiring:
				h.End = maxTime(maxTime(now, it.ExpiresAt), nestedEnd[it.ID]).Add(s.d.Cfg.Reservations.TeardownTimeout)
			case model.StateTearingDown:
				if it.TeardownStartedAt.IsZero() {
					h.End = now.Add(s.d.Cfg.Reservations.TeardownTimeout)
				} else {
					h.End = maxTime(now, it.TeardownStartedAt.Add(s.d.Cfg.Reservations.TeardownTimeout))
				}
			case model.StateTeardownFailed, model.StateTeardownUnknown:
				h.Never = true
			default:
				continue
			}
		} else {
			switch it.State {
			case model.StateStarting:
				h.End = now.Add(w.Duration)
			case model.StateRunning:
				h.End = maxTime(now, it.StartedAt.Add(w.Duration))
			case model.StateCleaning, model.StateRecovering:
			default:
				continue
			}
		}
		out = append(out, h)
	}
	return out
}
func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
func (s *Scheduler) savePassLocked(pol admission.Policy, pass admission.Pass, items, queue []*model.Item, waiting []admission.Waiting, now time.Time) {
	last := passState{policy: pol, after: pass.After, holders: pass.Holders, at: now, starts: map[model.ItemID]time.Time{}}
	for i, it := range queue {
		if it.State == model.StateQueued {
			v := *it
			last.items = append(last.items, &v)
			last.waiting = append(last.waiting, waiting[i])
		}
	}
	c := s.commitments
	c.Memory = pass.After.Committed.MemoryBytes
	c.CPUs = pass.After.Committed.CPUs
	for _, i := range pass.Admitted {
		it := queue[i]
		if it.IsReservation() {
			c.Stands += it.Expected
			c.StandsUnconsumed += it.Expected
			c.Budgets += it.TaskMemory
			c.BudgetsUnconsumed += it.TaskMemory
		} else {
			c.TasksExpected += it.Expected
			c.TasksUnconsumed += it.Expected
		}
	}
	last.summary = PassSummary{At: now, Commitments: c, HeadID: s.headID, Head: s.headDecision}
	for _, it := range items {
		if it.State == model.StateQueued && it.ParentID == "" {
			last.summary.Queued++
		}
		if it.IsReservation() {
			if it.State.Held() {
				last.summary.Held++
			}
		} else {
			switch it.State {
			case model.StateStarting, model.StateRunning, model.StateCleaning, model.StateRecovering:
				last.summary.Running++
			}
		}
	}
	if !s.headDecision.Blocker.Gate() {
		fs := admission.Schedule(now, admission.FreeCapacity(pol, pass.After), pass.Holders, last.waiting)
		for i, f := range fs {
			if f.OK {
				last.starts[last.waiting[i].ID] = f.Start
			}
		}
	}
	s.last = last
}
