package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"slices"
	"time"

	"resource-dispatcher/internal/model"
	"resource-dispatcher/internal/platform"
)

// claimTeardownsLocked gives the free teardown slots (teardown_concurrency minus the
// reservations TEARING_DOWN) to the due reservations by ascending ExpiresAt. A claimed
// one is persisted TEARING_DOWN with attempt+1 and a clean slate before its Launch,
// and marked launching; Tick launches it after the pass. One without a slot, or whose
// previous attempt's Stop is still in flight, stays EXPIRING (and accounted) for a
// later pass. Nothing is claimed while recovering.
func (s *Scheduler) claimTeardownsLocked(ctx context.Context, items, due []*model.Item) []*model.Item {
	if s.recovering || len(due) == 0 {
		return nil
	}
	free := s.d.Cfg.Reservations.TeardownConcurrency
	for _, it := range items {
		if it.IsReservation() && it.State == model.StateTearingDown {
			free--
		}
	}
	slices.SortStableFunc(due, func(a, b *model.Item) int { return a.ExpiresAt.Compare(b.ExpiresAt) })
	var claimed []*model.Item
	for _, it := range due {
		if free <= 0 || s.stopping[model.TeardownUnitID(it.ID)] {
			s.awaitSlotLocked(ctx, it)
			continue
		}
		prev := *it
		it.State, it.TeardownAttempt, it.TeardownLaunchError = model.StateTearingDown, it.TeardownAttempt+1, ""
		it.Reason, it.Diagnostic, it.ExitCode, it.Signal, it.LogsTruncated = model.ReasonNone, "", nil, "", false
		it.UnitName, it.TTLHeld = "", false
		if err := s.persistLocked(ctx, it); err != nil {
			slog.Error("scheduler: record teardown start", "id", it.ID, "err", err)
			*it = prev // not persisted: due again next pass
			continue
		}
		free--
		s.teardownLaunching[it.ID] = true
		claimed = append(claimed, it)
	}
	return claimed
}

// awaitSlotLocked marks a due reservation that has to wait for a teardown slot EXPIRING
// (one with a short TTL may still be GRANTED: it never got a warning).
func (s *Scheduler) awaitSlotLocked(ctx context.Context, it *model.Item) {
	if it.State != model.StateGranted {
		return
	}
	it.State = model.StateExpiring
	if err := s.persistLocked(ctx, it); err != nil {
		slog.Error("scheduler: record reservation waiting for teardown", "id", it.ID, "err", err)
		it.State = model.StateGranted
	}
}

// teardownSpec is the unit of it's current teardown attempt: bash on the daemon's copy
// of the script, in the original cwd if it still exists and an empty <JobDir>/cwd
// otherwise, with the captured env plus AQ_ORIGINAL_CWD and AQ_RESERVATION_ID.
func (s *Scheduler) teardownSpec(it *model.Item) platform.LaunchSpec {
	id, attempt := it.ID, it.TeardownAttempt
	jobDir := s.d.Paths.TeardownDir(id, attempt)
	cwd := it.Cwd
	if st, err := os.Stat(cwd); err != nil || !st.IsDir() {
		cwd = filepath.Join(jobDir, "cwd")
	}
	env := make(map[string]string, len(it.Env)+2)
	for k, v := range it.Env {
		env[k] = v
	}
	env["AQ_ORIGINAL_CWD"] = it.Cwd
	env["AQ_RESERVATION_ID"] = string(id)
	r := s.d.Cfg.Reservations
	return platform.LaunchSpec{
		ID: model.TeardownUnitID(id), Argv: []string{"bash", s.d.Paths.TeardownScript(id)}, Cwd: cwd, Env: env,
		Limit:      r.TeardownMemory,
		MemoryHigh: uint64(math.Floor(float64(r.TeardownMemory) * s.d.Cfg.MemoryHighRatio)),
		CPUs:       1, Timeout: r.TeardownTimeout, KillGrace: s.d.Cfg.CancelGrace,
		JobDir: jobDir, TmpDir: s.d.Paths.TeardownTmpDir(id, attempt),
		MaxStdout: s.d.Cfg.Logs.MaxStdout, MaxStderr: s.d.Cfg.Logs.MaxStderr,
	}
}

// launchTeardown launches a claimed (TEARING_DOWN, launching) attempt without the mutex,
// then applies the outcome only if the reservation is still TEARING_DOWN with the same
// attempt. A failed Launch whose unit is gone is TEARDOWN_FAILED; one whose unit may
// still run keeps TEARING_DOWN with TeardownLaunchError, and the reaper stops the unit
// until it is gone. A Launch error is kept in launchFailed until it is stored, and a
// unit that may run is stopped even if the reservation cannot be loaded.
func (s *Scheduler) launchTeardown(ctx context.Context, it *model.Item) {
	pctx := context.WithoutCancel(ctx)
	uid := model.TeardownUnitID(it.ID)
	spec := s.teardownSpec(it)
	var (
		unit string
		lerr error
	)
	if spec.Cwd != it.Cwd {
		if err := os.MkdirAll(spec.Cwd, 0o700); err != nil {
			lerr = fmt.Errorf("teardown cwd: %w", err)
		}
	}
	if lerr == nil {
		unit, lerr = s.d.Exec.Launch(ctx, spec)
	}
	unitAlive := false
	if lerr != nil {
		// Launch stops the unit on failure, but that Stop may itself have failed.
		alive, aerr := s.d.Exec.Alive(pctx, uid)
		if aerr != nil {
			slog.Warn("scheduler: read teardown unit liveness after failed launch", "id", it.ID, "err", aerr)
		}
		unitAlive = alive || aerr != nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.teardownLaunching, it.ID)
	if lerr != nil {
		s.launchFailed[it.ID] = launchFailure{attempt: it.TeardownAttempt, err: lerr.Error()}
		if unitAlive {
			s.stopAsyncLocked(uid)
		}
	}
	cur, err := s.d.Store.Get(pctx, it.ID)
	if err != nil {
		// Stays TEARING_DOWN; the reaper records a launch error and settles it from the unit.
		slog.Error("scheduler: load launched teardown", "id", it.ID, "launched", lerr == nil, "err", err)
		return
	}
	if cur.State != model.StateTearingDown || cur.TeardownAttempt != it.TeardownAttempt {
		slog.Warn("scheduler: teardown changed during its launch; outcome dropped", "id", it.ID,
			"state", cur.State, "attempt", cur.TeardownAttempt, "launched_attempt", it.TeardownAttempt)
		delete(s.launchFailed, it.ID)
		return
	}
	switch {
	case lerr == nil:
		cur.UnitName = unit
		if err := s.persistLocked(pctx, cur); err != nil {
			slog.Error("scheduler: record teardown unit", "id", cur.ID, "err", err)
		}
	case unitAlive:
		s.recordLaunchFailureLocked(pctx, cur)
	default:
		_ = s.failTeardownLocked(pctx, cur, *cur, model.ReasonLaunchFailed, "launch failed: "+lerr.Error(), false)
	}
}

// reapTeardownLocked settles a TEARING_DOWN reservation whose Launch has returned.
// After a launch error (stored, or pending in launchFailed, which it stores first) the
// unit is stopped every cancel_grace while it lives, and once it is gone the attempt
// is TEARDOWN_FAILED whatever it recorded. Otherwise Alive is
// read before the result, as for tasks: a result with no processes left maps exit 0 to
// TORN_DOWN and anything else to TEARDOWN_FAILED; no result and no processes,
// continuously for cancel_grace, is TEARDOWN_UNKNOWN.
func (s *Scheduler) reapTeardownLocked(ctx context.Context, it *model.Item, now time.Time) {
	uid := model.TeardownUnitID(it.ID)
	s.recordLaunchFailureLocked(ctx, it)
	alive, err := s.d.Exec.Alive(ctx, uid)
	if err != nil {
		slog.Warn("scheduler: read teardown unit liveness", "id", it.ID, "err", err)
		return
	}
	if it.TeardownLaunchError != "" {
		if alive {
			if last, ok := s.lastStop[uid]; !s.stopping[uid] && (!ok || now.Sub(last) >= s.d.Cfg.CancelGrace) {
				s.stopAsyncLocked(uid)
			}
			return
		}
		_ = s.settleGoneTeardownLocked(ctx, it, nil, nil) // logged; retried next pass
		return
	}
	ei, rerr := s.d.Exec.ReadResult(s.d.Paths.TeardownDir(it.ID, it.TeardownAttempt))
	if ei == nil {
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
		_ = s.settleGoneTeardownLocked(ctx, it, nil, rerr)
		return
	}
	delete(s.goneSince, it.ID)
	if alive {
		// Processes outlive the recorded result: stop them again after 2×cancel_grace.
		seen, ok := s.resultSeen[uid]
		if !ok {
			s.resultSeen[uid] = now
		} else if now.Sub(seen) >= 2*s.d.Cfg.CancelGrace {
			s.resultSeen[uid] = now
			s.stopAsyncLocked(uid)
		}
		return
	}
	_ = s.settleGoneTeardownLocked(ctx, it, ei, nil)
}

// recordLaunchFailureLocked stores the pending Launch error of it's current attempt as
// TeardownLaunchError. If the write fails it stays pending and is still set on it, so
// the caller treats the attempt as failed; a pending error of another attempt, or of
// one already stored, is dropped.
func (s *Scheduler) recordLaunchFailureLocked(ctx context.Context, it *model.Item) {
	f, ok := s.launchFailed[it.ID]
	if !ok {
		return
	}
	if f.attempt != it.TeardownAttempt || it.TeardownLaunchError != "" {
		delete(s.launchFailed, it.ID)
		return
	}
	it.TeardownLaunchError = f.err
	it.Diagnostic = "teardown launch failed, stopping its unit: " + f.err
	if err := s.persistLocked(ctx, it); err != nil {
		slog.Error("scheduler: record teardown launch error", "id", it.ID, "err", err)
		return
	}
	delete(s.launchFailed, it.ID)
}

// settleGoneTeardownLocked records the outcome of a TEARING_DOWN attempt with no
// processes left: TEARDOWN_FAILED after a launch error, whatever the attempt recorded;
// otherwise from its result ei (exit 0 TORN_DOWN, anything else TEARDOWN_FAILED), and
// TEARDOWN_UNKNOWN without one (rerr says why it was unreadable). On a failed write it
// stays TEARING_DOWN and the error is returned.
func (s *Scheduler) settleGoneTeardownLocked(ctx context.Context, it *model.Item, ei *platform.ExitInfo, rerr error) error {
	prev := *it
	switch {
	case it.TeardownLaunchError != "":
		return s.failTeardownLocked(ctx, it, prev, model.ReasonLaunchFailed, "launch failed: "+it.TeardownLaunchError, false)
	case ei == nil:
		cause := "teardown unit gone without a result"
		if rerr != nil {
			cause += fmt.Sprintf(" (unreadable result: %v)", rerr)
		}
		it.State, it.Reason, it.Diagnostic = model.StateTeardownUnknown, model.ReasonLost, cause
		evs := newEvents(it, model.EventTeardownUnknown,
			fmt.Sprintf("teardown of stand %s: %s; the stand may still be up", standLabel(it), cause),
			model.AudienceOperator)
		return s.settleTeardownLocked(ctx, it, prev, evs)
	}
	it.ExitCode, it.Signal = exitDetails(*ei)
	it.LogsTruncated = ei.LogsTruncated
	st, reason := MapOutcome(*ei, false, false)
	if st != model.StateSucceeded {
		return s.failTeardownLocked(ctx, it, prev, reason, s.outcomeCause(*ei, reason), true)
	}
	s.endLocked(it, model.StateTornDown, model.ReasonNone, "")
	if !ei.EndedAt.IsZero() {
		it.EndedAt = ei.EndedAt
	}
	evs := newEvents(it, model.EventTeardownSucceeded, "torn down forgotten stand "+standLabel(it), model.AudienceOperator)
	return s.settleTeardownLocked(ctx, it, prev, evs)
}

// failTeardownLocked records the current attempt as TEARDOWN_FAILED with
// teardown_failed; withLogs points at the attempt's stderr.log.
func (s *Scheduler) failTeardownLocked(ctx context.Context, it *model.Item, prev model.Item, reason model.Reason, cause string, withLogs bool) error {
	if withLogs {
		cause += "; see " + filepath.Join(s.d.Paths.TeardownDir(it.ID, it.TeardownAttempt), "stderr.log")
	}
	it.State, it.Reason, it.Diagnostic = model.StateTeardownFailed, reason, "teardown failed: "+cause
	evs := newEvents(it, model.EventTeardownFailed,
		fmt.Sprintf("teardown of stand %s failed: %s", standLabel(it), cause), model.AudienceOperator)
	return s.settleTeardownLocked(ctx, it, prev, evs)
}

// settleTeardownLocked persists the end of an attempt whose processes are gone and
// removes that attempt's temp dir in the background (its logs stay). If the write
// fails, it is restored to prev and stays TEARING_DOWN, in the accounting.
func (s *Scheduler) settleTeardownLocked(ctx context.Context, it *model.Item, prev model.Item, evs []model.Event) error {
	if err := s.persistLocked(ctx, it, evs...); err != nil {
		slog.Error("scheduler: record teardown outcome", "id", it.ID, "state", it.State, "err", err)
		*it = prev
		return err
	}
	uid := model.TeardownUnitID(it.ID)
	delete(s.goneSince, it.ID)
	delete(s.resultSeen, uid)
	delete(s.lastStop, uid)
	delete(s.launchFailed, it.ID)
	tmp := s.d.Paths.TeardownTmpDir(it.ID, it.TeardownAttempt)
	s.background(func() {
		if err := os.RemoveAll(tmp); err != nil {
			slog.Warn("scheduler: remove teardown temp dir", "dir", tmp, "err", err)
		}
	})
	return nil
}

// outcomeCause describes a failed teardown result.
func (s *Scheduler) outcomeCause(ei platform.ExitInfo, reason model.Reason) string {
	switch reason {
	case model.ReasonTimeout:
		return fmt.Sprintf("timed out after teardown_timeout %s", s.d.Cfg.Reservations.TeardownTimeout)
	case model.ReasonOOM, model.ReasonOOMGroup, model.ReasonOOMHostGuard:
		return fmt.Sprintf("killed by the OOM killer (%s)", reason)
	case model.ReasonLaunchFailed:
		return "launch failed: " + ei.LaunchError
	}
	switch code, sig := exitDetails(ei); {
	case code != nil:
		return fmt.Sprintf("exit status %d", *code)
	case sig != "":
		return "killed by " + sig
	}
	return "service result " + ei.ServiceResult
}

// standLabel names a reservation's stand for the operator: its name, or its id.
func standLabel(it *model.Item) string {
	if it.Name != "" {
		return it.Name
	}
	return string(it.ID)
}

// teardownUnconsumedLocked is the part of a running teardown's memory not yet in use:
// all of teardown_memory while its Launch is in flight, else max(0, teardown_memory -
// memory.current). Teardown memory is outside the pool, so it only counts here.
func (s *Scheduler) teardownUnconsumedLocked(it *model.Item) uint64 {
	if it.State != model.StateTearingDown {
		return 0
	}
	mem := s.d.Cfg.Reservations.TeardownMemory
	if s.teardownLaunching[it.ID] {
		return mem
	}
	cur, _ := s.d.Exec.MemoryCurrent(model.TeardownUnitID(it.ID))
	return satSub(mem, cur)
}

// RetryTeardown starts the teardown of a TEARDOWN_FAILED or TEARDOWN_UNKNOWN reservation
// again: it becomes EXPIRING, due now with no warning, and takes the usual way (a slot,
// attempt+1, launch). It is ErrBadState in any other state, while the previous attempt's
// Stop is in flight, and while its teardown unit has processes.
func (s *Scheduler) RetryTeardown(ctx context.Context, id model.ItemID) (*model.Item, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	it, err := s.d.Store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	switch {
	case !it.IsReservation():
		return it, fmt.Errorf("%w: %s is a task, not a reservation", ErrBadState, id)
	case it.State != model.StateTeardownFailed && it.State != model.StateTeardownUnknown:
		return it, fmt.Errorf("%w: reservation %s is %s; only TEARDOWN_FAILED or TEARDOWN_UNKNOWN can be retried",
			ErrBadState, id, it.State)
	}
	if err := s.teardownUnitGoneLocked(ctx, id); err != nil {
		return it, err
	}
	it.State, it.ExpiresAt, it.WarnAt, it.TTLHeld = model.StateExpiring, s.d.Now(), time.Time{}, false
	if err := s.persistLocked(ctx, it); err != nil {
		return nil, err
	}
	s.Kick()
	return it, nil
}

// teardownUnitGoneLocked is ErrBadState while a Stop of id's teardown unit is in flight
// or the unit has processes: a new attempt or a release must not race the old one.
func (s *Scheduler) teardownUnitGoneLocked(ctx context.Context, id model.ItemID) error {
	uid := model.TeardownUnitID(id)
	if s.stopping[uid] {
		return fmt.Errorf("%w: reservation %s: previous teardown attempt is still stopping; retry shortly", ErrBadState, id)
	}
	alive, err := s.d.Exec.Alive(ctx, uid)
	if err != nil {
		return fmt.Errorf("read teardown unit liveness of %s: %w", id, err)
	}
	if alive {
		return fmt.Errorf("%w: teardown unit of reservation %s is still running", ErrBadState, id)
	}
	return nil
}
