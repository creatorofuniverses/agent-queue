package scheduler

import (
	"context"
	"strconv"
	"strings"

	"resource-dispatcher/internal/model"
	"resource-dispatcher/internal/platform"
)

// MapOutcome turns a unit's recorded result into the item's final state and reason.
func MapOutcome(ei platform.ExitInfo, cancelRequested, guardActive bool) (model.State, model.Reason) {
	switch {
	case ei.LaunchError != "":
		return model.StateFailed, model.ReasonLaunchFailed
	case cancelRequested:
		return model.StateCancelled, model.ReasonCancelled
	case ei.ServiceResult == "oom-kill" || ei.OwnOOMKills > 0:
		switch {
		case ei.OwnMaxEvents > 0:
			return model.StateFailed, model.ReasonOOM
		case guardActive:
			return model.StateFailed, model.ReasonOOMHostGuard
		}
		return model.StateFailed, model.ReasonOOMGroup
	case ei.ServiceResult == "timeout":
		return model.StateFailed, model.ReasonTimeout
	case ei.ServiceResult == "success", ei.ServiceResult == "exit-code" && ei.ExitStatus == "0":
		return model.StateSucceeded, model.ReasonNone
	}
	return model.StateFailed, model.ReasonCommandFailed
}

// exitDetails reads EXIT_STATUS: a number is the exit code, anything else a signal name.
func exitDetails(ei platform.ExitInfo) (code *int, signal string) {
	if ei.ExitStatus == "" {
		return nil, ""
	}
	if n, err := strconv.Atoi(ei.ExitStatus); err == nil {
		return &n, ""
	}
	if strings.HasPrefix(ei.ExitStatus, "SIG") {
		return nil, ei.ExitStatus
	}
	return nil, "SIG" + ei.ExitStatus
}

// lingerMsg is the diagnostic for a unit whose processes outlive its recorded result.
const lingerMsg = "processes did not exit within 2x cancel_grace after the command finished; stop repeated"

// reapLocked moves RUNNING items with a result to CLEANING and finalizes those
// whose cgroup is empty. It returns ids whose processes linger 2×cancel_grace
// after the result; the caller stops them outside the lock. Items stay in
// CLEANING, and so in the accounting, until their cgroup is empty.
func (s *Scheduler) reapLocked(ctx context.Context, items []*model.Item) []model.ItemID {
	var toStop []model.ItemID
	now := s.d.Now()
	for _, it := range items {
		if it.State != model.StateRunning && it.State != model.StateCleaning {
			continue
		}
		ei, err := s.d.Exec.ReadResult(it.JobDir)
		if err != nil || ei == nil {
			continue
		}
		if it.State == model.StateRunning {
			it.State = model.StateCleaning
			if err := s.persistLocked(ctx, it); err != nil {
				continue
			}
		}
		seen, ok := s.resultSeen[it.ID]
		if !ok {
			seen = now
			s.resultSeen[it.ID] = now
		}
		alive, err := s.d.Exec.Alive(ctx, it.ID)
		if err != nil {
			continue
		}
		if !alive {
			if err := s.finalizeLocked(ctx, it, ei); err != nil {
				it.State = model.StateCleaning // not persisted: keep it in the accounting
			}
			continue
		}
		if now.Sub(seen) >= 2*s.d.Cfg.CancelGrace {
			s.resultSeen[it.ID] = now
			if it.Diagnostic != lingerMsg {
				it.Diagnostic = lingerMsg
				s.persistLocked(ctx, it)
			}
			toStop = append(toStop, it.ID)
		}
	}
	return toStop
}

// finalizeLocked records the outcome from ei and frees the item's resources.
func (s *Scheduler) finalizeLocked(ctx context.Context, it *model.Item, ei *platform.ExitInfo) error {
	st, reason := MapOutcome(*ei, it.CancelRequested, s.guard.Load())
	s.endLocked(it, st, reason, ei.LaunchError)
	if !ei.EndedAt.IsZero() {
		it.EndedAt = ei.EndedAt
	}
	it.ExitCode, it.Signal = exitDetails(*ei)
	it.MemoryPeak, it.LogsTruncated = ei.MemoryPeak, ei.LogsTruncated
	return s.persistLocked(ctx, it)
}
