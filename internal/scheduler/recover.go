package scheduler

import (
	"context"
	"fmt"
	"time"

	"github.com/creatorofuniverses/agent-queue/internal/model"
	"github.com/creatorofuniverses/agent-queue/internal/platform"
)

// Recover reconciles stored items with units left by a previous daemon. Admission,
// closed since New (BlockRecovering), opens only when it succeeds; orphan units (task
// units and teardown-<id> units no active item owns) are stopped.
func (s *Scheduler) Recover(ctx context.Context) error {
	s.mu.Lock()
	s.recovering = true
	known, err := s.reconcileLocked(ctx)
	s.mu.Unlock()
	if err != nil {
		return err
	}

	managed, err := s.d.Exec.ListManaged(ctx)
	if err != nil {
		return fmt.Errorf("recover: list units: %w", err)
	}
	for _, id := range managed {
		if !known[id] {
			if err := s.d.Exec.Stop(ctx, id); err != nil {
				return fmt.Errorf("recover: stop orphan %s: %w", id, err)
			}
		}
	}

	s.mu.Lock()
	s.recovering = false
	s.mu.Unlock()
	s.Kick()
	return nil
}

// reconcileLocked settles each non-terminal item and returns the unit ids still owned:
// those of active tasks and of adopted teardowns. Reservations go their own way;
// nested tasks are tasks.
func (s *Scheduler) reconcileLocked(ctx context.Context) (map[model.ItemID]bool, error) {
	items, err := s.d.Store.ListActive(ctx)
	if err != nil {
		return nil, fmt.Errorf("recover: %w", err)
	}
	known := map[model.ItemID]bool{}
	for _, it := range items {
		if it.IsReservation() {
			if err := s.recoverReservationLocked(ctx, it, known); err != nil {
				return nil, fmt.Errorf("recover %s: %w", it.ID, err)
			}
			continue
		}
		if it.State == model.StateQueued {
			known[it.ID] = true
			continue
		}
		if it.JobDir == "" {
			it.JobDir = s.d.Paths.JobDir(it.ID)
		}
		// Alive before the result, as in reapLocked: a unit writes result.json before
		// its cgroup empties, so a unit exiting between the reads still shows its result.
		alive, err := s.d.Exec.Alive(ctx, it.ID)
		if err != nil {
			return nil, fmt.Errorf("recover %s: %w", it.ID, err)
		}
		ei, rerr := s.d.Exec.ReadResult(it.JobDir)
		switch {
		case ei != nil && !alive:
			err = s.finalizeLocked(ctx, it, ei)
		case ei != nil:
			it.State = model.StateCleaning
			s.resultSeen[it.ID] = s.d.Now()
			err = s.persistLocked(ctx, it)
		case alive:
			// An unreadable result while processes remain is settled by Tick.
			it.State = model.StateRunning
			if it.StartedAt.IsZero() {
				it.StartedAt = it.AdmittedAt
				if it.StartedAt.IsZero() {
					it.StartedAt = s.d.Now()
				}
			}
			err = s.persistLocked(ctx, it)
		case rerr != nil:
			// result.json exists, so the unit did run: never requeue it.
			s.endLocked(it, model.StateFailed, model.ReasonLost, fmt.Sprintf("unreadable result: %v", rerr))
			err = s.persistLocked(ctx, it)
		case it.State == model.StateStarting && it.CancelRequested:
			s.endLocked(it, model.StateCancelled, model.ReasonCancelled, "")
			err = s.persistLocked(ctx, it)
		case it.State == model.StateStarting:
			// Results are always written by the unit, so no unit ever ran.
			it.State, it.AdmittedAt = model.StateQueued, time.Time{}
			err = s.persistLocked(ctx, it)
		default:
			s.endLocked(it, model.StateFailed, model.ReasonLost, "unit and result are gone after daemon restart")
			err = s.persistLocked(ctx, it)
		}
		if err != nil {
			return nil, fmt.Errorf("recover %s: %w", it.ID, err)
		}
		if !it.State.Terminal() {
			known[it.ID] = true
		}
	}
	return known, nil
}

// recoverReservationLocked settles a reservation left by a previous daemon. Only a
// TEARING_DOWN one has a unit: a live unit is adopted (and, after a launch error, stopped
// again, which also starts the reaper's cancel_grace); a gone one is settled as the
// reaper would, from its result, and TEARDOWN_UNKNOWN at once without one: the daemon
// was down, so there is no grace to wait for. No teardown is launched here. QUEUED,
// GRANTED, EXPIRING, TEARDOWN_FAILED and TEARDOWN_UNKNOWN have no unit and stay as they
// are; expiry that passed during the downtime is handled by the next Tick.
func (s *Scheduler) recoverReservationLocked(ctx context.Context, it *model.Item, known map[model.ItemID]bool) error {
	if it.State != model.StateTearingDown {
		return nil
	}
	uid := model.TeardownUnitID(it.ID)
	// Alive before the result, as in reapTeardownLocked.
	alive, err := s.d.Exec.Alive(ctx, uid)
	if err != nil {
		return err
	}
	if alive {
		known[uid] = true
		if it.TeardownLaunchError != "" {
			s.stopAsyncLocked(uid)
		}
		return nil
	}
	var (
		ei   *platform.ExitInfo
		rerr error
	)
	if it.TeardownLaunchError == "" { // a launch error fails the attempt whatever it recorded
		ei, rerr = s.d.Exec.ReadResult(s.d.Paths.TeardownDir(it.ID, it.TeardownAttempt))
	}
	return s.settleGoneTeardownLocked(ctx, it, ei, rerr)
}
