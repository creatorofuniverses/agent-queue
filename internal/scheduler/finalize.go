package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

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
		case ei.OwnOOMEvents > 0:
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

// reapLocked settles RUNNING/CLEANING items. A result moves the item to CLEANING;
// it is finalized once its cgroup is empty, and while processes linger 2×cancel_grace
// after the result the unit is stopped again in the background. No processes and no
// result, continuously for cancel_grace, means the unit is gone without recording a
// result: FAILED/LOST. Alive is read before the result: the unit writes result.json
// before its cgroup empties, so "not alive, then no result" is never a result in flight.
// A TEARING_DOWN reservation goes to reapTeardownLocked once its Launch has returned.
func (s *Scheduler) reapLocked(ctx context.Context, items []*model.Item) {
	now := s.d.Now()
	for _, it := range items {
		if it.IsReservation() {
			if it.State == model.StateTearingDown && !s.teardownLaunching[it.ID] {
				s.reapTeardownLocked(ctx, it, now)
			}
			continue
		}
		if it.State != model.StateRunning && it.State != model.StateCleaning {
			continue
		}
		alive, err := s.d.Exec.Alive(ctx, it.ID)
		if err != nil {
			slog.Warn("scheduler: read unit liveness", "id", it.ID, "err", err)
			continue
		}
		// A cancelled unit that is still alive is stopped again every cancel_grace:
		// the first Stop may have run before the unit existed, failed, or predate a restart.
		if alive && it.CancelRequested && !s.stopping[it.ID] {
			if last, ok := s.lastStop[it.ID]; !ok || now.Sub(last) >= s.d.Cfg.CancelGrace {
				s.stopAsyncLocked(it.ID)
			}
		}
		ei, rerr := s.d.Exec.ReadResult(it.JobDir)
		if ei == nil {
			s.reapMissingLocked(ctx, it, alive, rerr, now)
			continue
		}
		delete(s.goneSince, it.ID)
		if it.State == model.StateRunning {
			it.State = model.StateCleaning
			if err := s.persistLocked(ctx, it); err != nil {
				slog.Error("scheduler: record cleaning", "id", it.ID, "err", err)
				continue
			}
		}
		seen, ok := s.resultSeen[it.ID]
		if !ok {
			seen = now
			s.resultSeen[it.ID] = now
		}
		if !alive {
			if err := s.finalizeLocked(ctx, it, ei); err != nil {
				slog.Error("scheduler: record outcome", "id", it.ID, "err", err)
				it.State = model.StateCleaning // not persisted: keep it in the accounting
			}
			continue
		}
		s.alertCleaningLocked(ctx, it, now)
		if now.Sub(seen) >= 2*s.d.Cfg.CancelGrace {
			s.resultSeen[it.ID] = now
			if it.Diagnostic != lingerMsg {
				it.Diagnostic = lingerMsg
				if err := s.persistLocked(ctx, it); err != nil {
					slog.Error("scheduler: record diagnostic", "id", it.ID, "err", err)
				}
			}
			s.stopAsyncLocked(it.ID)
		}
	}
}

// reapMissingLocked handles an item without a (readable) result.
func (s *Scheduler) reapMissingLocked(ctx context.Context, it *model.Item, alive bool, rerr error, now time.Time) {
	if alive {
		delete(s.goneSince, it.ID)
		return
	}
	since, ok := s.goneSince[it.ID]
	if !ok {
		s.goneSince[it.ID] = now
		return
	}
	if now.Sub(since) < s.d.Cfg.CancelGrace {
		return
	}
	diag := "unit gone without result"
	if rerr != nil {
		diag += fmt.Sprintf(" (unreadable result: %v)", rerr)
	}
	prev := it.State
	s.endLocked(it, model.StateFailed, model.ReasonLost, diag)
	if err := s.persistLocked(ctx, it); err != nil {
		slog.Error("scheduler: record lost unit", "id", it.ID, "err", err)
		it.State = prev // not persisted: keep it in the accounting
	}
}

// finalizeLocked records the outcome from ei and frees the item's resources.
func (s *Scheduler) finalizeLocked(ctx context.Context, it *model.Item, ei *platform.ExitInfo) error {
	st, reason := MapOutcome(*ei, it.CancelRequested, s.guardAt(ei.EndedAt))
	if reason == model.ReasonCancelled && it.CancelReason == launchFailedCancel {
		st, reason = model.StateFailed, model.ReasonLaunchFailed
	}
	s.endLocked(it, st, reason, ei.LaunchError)
	if !ei.EndedAt.IsZero() {
		it.EndedAt = ei.EndedAt
	}
	it.ExitCode, it.Signal = exitDetails(*ei)
	it.MemoryPeak, it.LogsTruncated = ei.MemoryPeak, ei.LogsTruncated
	return s.persistLocked(ctx, it)
}
