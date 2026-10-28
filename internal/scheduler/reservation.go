package scheduler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"time"
	"unicode/utf8"

	"agent-queue/internal/model"
	"agent-queue/internal/store"
	"agent-queue/internal/units"
)

// maxTeardownScript is the largest teardown script a reservation may carry.
const maxTeardownScript = 64 << 10

var dockerProjectRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// ReserveRequest asks for a stand (Memory, CPUs) plus a budget for nested tasks
// (TaskMemory, TaskCPUs) for TTL from the grant. Teardown is the script's content.
type ReserveRequest struct {
	NoNotify      bool              `json:"no_notify,omitempty"`
	Name          string            `json:"name"`
	Cwd           string            `json:"cwd"`
	Env           map[string]string `json:"env"`
	Memory        uint64            `json:"memory"`
	CPUs          int               `json:"cpus"`
	TaskMemory    uint64            `json:"task_memory"`
	TaskCPUs      int               `json:"task_cpus"`
	TTL           time.Duration     `json:"ttl"`
	Teardown      []byte            `json:"-"` // hashed separately
	DockerProject string            `json:"docker_project"`
	Priority      model.Priority    `json:"priority"`
	SessionID     string            `json:"session_id"`
	ClientType    string            `json:"client_type"`
	Project       string            `json:"project"`
	IdemKey       string            `json:"-"`
}

// requestHash covers the whole request and the sha256 of the teardown script.
func (r ReserveRequest) requestHash() (string, error) {
	script := sha256.Sum256(r.Teardown)
	canon, err := json.Marshal(struct {
		Kind model.Kind `json:"kind"`
		ReserveRequest
		TeardownSHA256 string `json:"teardown_sha256"`
	}{model.KindReservation, r, hex.EncodeToString(script[:])})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:]), nil
}

func (s *Scheduler) validateReserve(r ReserveRequest) error {
	cfg := s.d.Cfg
	switch {
	case !filepath.IsAbs(r.Cwd):
		return fmt.Errorf("cwd %q is not absolute", r.Cwd)
	case r.Memory == 0:
		return errors.New("stand memory must be at least 1 byte")
	case r.CPUs < 0 || r.TaskCPUs < 0:
		return errors.New("cpus must not be negative")
	case (r.TaskMemory == 0) != (r.TaskCPUs == 0):
		return errors.New("task memory and task cpus are set only together")
	case r.TaskMemory != 0 && r.TaskMemory < minMemory:
		return fmt.Errorf("task memory %d is below the 64 MiB minimum", r.TaskMemory)
	case r.TTL <= 0:
		return errors.New("ttl must be positive")
	case r.TTL > cfg.Reservations.MaxTTL:
		return fmt.Errorf("ttl %s exceeds max_ttl %s", r.TTL, cfg.Reservations.MaxTTL)
	case r.Memory > cfg.PoolMemory || r.TaskMemory > cfg.PoolMemory-r.Memory:
		return fmt.Errorf("stand memory %d plus task memory %d exceeds pool memory %d", r.Memory, r.TaskMemory, cfg.PoolMemory)
	case r.CPUs > cfg.PoolCPUs || r.TaskCPUs > cfg.PoolCPUs-r.CPUs:
		return fmt.Errorf("stand cpus %d plus task cpus %d exceed pool cpus %d", r.CPUs, r.TaskCPUs, cfg.PoolCPUs)
	case len(r.Teardown) == 0:
		return errors.New("teardown script is empty")
	case len(r.Teardown) > maxTeardownScript:
		return fmt.Errorf("teardown script is %d bytes, over the 64 KiB maximum", len(r.Teardown))
	case !utf8.Valid(r.Teardown):
		return errors.New("teardown script must be valid UTF-8")
	case r.DockerProject != "" && !dockerProjectRe.MatchString(r.DockerProject):
		return fmt.Errorf("docker project %q does not match %s", r.DockerProject, dockerProjectRe)
	}
	return nil
}

// Reserve validates and stores r: QUEUED on success, REJECTED (returned with
// ErrRejected) otherwise. The teardown script is copied to Paths.TeardownScript
// before the item is stored; a repeated IdemKey with the same request returns the
// stored item and leaves no second copy. A docker project is exclusive among
// non-terminal reservations.
func (s *Scheduler) Reserve(ctx context.Context, r ReserveRequest) (*model.Item, error) {
	hash, err := r.requestHash()
	if err != nil {
		return nil, err
	}
	now := s.d.Now()
	it := &model.Item{
		ID: model.NewID(), Kind: model.KindReservation, Name: r.Name, State: model.StateQueued, Priority: r.Priority,
		Cwd: r.Cwd, Env: r.Env, Limit: r.Memory, Expected: r.Memory, CPUs: r.CPUs, Timeout: r.TTL,
		TaskMemory: r.TaskMemory, TaskCPUs: r.TaskCPUs, DockerProject: r.DockerProject,
		SessionID: r.SessionID, ClientType: r.ClientType, Project: r.Project, IdemKey: r.IdemKey,
		RequestHash: hash, NodeID: "local", CreatedAt: now, NoNotify: r.NoNotify,
	}
	if vErr := s.validateReserve(r); vErr != nil {
		s.rejectItem(it, vErr.Error())
		return s.insertRejected(ctx, it)
	}

	jobDir := s.d.Paths.JobDir(it.ID)
	if err := writeTeardownScript(s.d.Paths.TeardownScript(it.ID), r.Teardown); err != nil {
		removeDir(jobDir)
		return nil, err
	}
	it.JobDir = jobDir

	s.mu.Lock()
	stored, created, err := s.insertReservationLocked(ctx, it, r)
	s.mu.Unlock()
	if err != nil || !created || stored.State == model.StateRejected {
		removeDir(jobDir) // a repeat, a conflict or a rejection needs no copy
	}
	if err != nil {
		return nil, err
	}
	if stored.State == model.StateRejected {
		return stored, fmt.Errorf("%w: %s", ErrRejected, stored.Diagnostic)
	}
	if created {
		s.Kick()
	}
	return stored, nil
}

// insertReservationLocked checks docker project exclusivity and inserts it, so two
// reserves of one project cannot both pass. A repeat of the holder's own request
// (same idempotency key) is not a conflict: InsertItem returns the holder.
func (s *Scheduler) insertReservationLocked(ctx context.Context, it *model.Item, r ReserveRequest) (*model.Item, bool, error) {
	if r.DockerProject != "" {
		active, err := s.d.Store.ListActive(ctx)
		if err != nil {
			return nil, false, err
		}
		for _, x := range active {
			if !x.IsReservation() || x.DockerProject != r.DockerProject {
				continue
			}
			if r.IdemKey != "" && x.IdemKey == r.IdemKey && x.ClientType == r.ClientType && x.Project == r.Project {
				continue
			}
			s.rejectItem(it, fmt.Sprintf("docker project %s is already used by reservation %s", r.DockerProject, x.ID))
			break
		}
	}
	return s.d.Store.InsertItem(ctx, it)
}

// insertNestedLocked checks a nested task against its reservation and the
// reservation's task budget and inserts it (REJECTED if a check fails), so concurrent
// submits cannot overrun the budget. A repeat of an accepted request (same idempotency
// key) is not counted against itself: InsertItem returns the stored item.
func (s *Scheduler) insertNestedLocked(ctx context.Context, it *model.Item) (*model.Item, bool, error) {
	diag, err := s.nestedRejectionLocked(ctx, it)
	if err != nil {
		return nil, false, err
	}
	if diag != "" {
		s.rejectItem(it, diag)
	}
	return s.d.Store.InsertItem(ctx, it)
}

// nestedRejectionLocked is why it may not run within its parent, or "" if it may: the
// parent is a GRANTED or EXPIRING reservation neither held past its TTL nor due (a due
// one may be waiting for a teardown slot; new tasks would hold it), the limit fits the
// task memory, and its expected memory and cpus plus those of the reservation's other
// non-terminal nested tasks fit the task budget.
func (s *Scheduler) nestedRejectionLocked(ctx context.Context, it *model.Item) (string, error) {
	pid := it.ParentID
	parent, err := s.d.Store.Get(ctx, pid)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return fmt.Sprintf("reservation %s not found", pid), nil
	case err != nil:
		return "", err
	case !parent.IsReservation():
		return fmt.Sprintf("%s is not a reservation", pid), nil
	case parent.State != model.StateGranted && parent.State != model.StateExpiring:
		return fmt.Sprintf("reservation %s is %s, not granted", pid, parent.State), nil
	case parent.TTLHeld:
		return fmt.Sprintf("reservation %s is held past its TTL; extend it first", pid), nil
	case !s.d.Now().Before(parent.ExpiresAt):
		return fmt.Sprintf("reservation %s is past its TTL; extend it first", pid), nil
	case it.Limit > parent.TaskMemory:
		return fmt.Sprintf("task budget: memory limit %s exceeds task memory %s of reservation %s",
			units.FormatSize(it.Limit), units.FormatSize(parent.TaskMemory), pid), nil
	}
	active, err := s.d.Store.ListActive(ctx)
	if err != nil {
		return "", err
	}
	var mem uint64
	var cpus int
	for _, x := range active {
		if x.ParentID != pid {
			continue
		}
		if it.IdemKey != "" && x.IdemKey == it.IdemKey && x.ClientType == it.ClientType && x.Project == it.Project {
			continue
		}
		mem += x.Expected
		cpus += x.CPUs
	}
	if mem > parent.TaskMemory || it.Expected > parent.TaskMemory-mem {
		return fmt.Sprintf("task budget: memory %s expected plus %s of nested tasks exceeds task memory %s of reservation %s",
			units.FormatSize(it.Expected), units.FormatSize(mem), units.FormatSize(parent.TaskMemory), pid), nil
	}
	if cpus > parent.TaskCPUs || it.CPUs > parent.TaskCPUs-cpus {
		return fmt.Sprintf("task budget: cpus %d plus %d of nested tasks exceed task cpus %d of reservation %s",
			it.CPUs, cpus, parent.TaskCPUs, pid), nil
	}
	return "", nil
}

// admitNestedLocked starts every queued nested task: they are paid for by their
// reservation's budget, so neither the queue, the pool, pressure, disk nor live memory
// holds them back; only recovery does. One whose parent is no longer GRANTED or
// EXPIRING (which Submit and Release rule out) is cancelled instead.
func (s *Scheduler) admitNestedLocked(ctx context.Context, items, nested []*model.Item, now time.Time) ([]*model.Item, error) {
	if s.recovering || len(nested) == 0 {
		return nil, nil
	}
	byID := make(map[model.ItemID]*model.Item, len(items))
	for _, it := range items {
		byID[it.ID] = it
	}
	var admitted []*model.Item
	for _, it := range nested {
		var parentState model.State
		if p, ok := byID[it.ParentID]; ok {
			parentState = p.State
		} else if p, err := s.d.Store.Get(ctx, it.ParentID); err == nil {
			parentState = p.State // terminal: not in the active list
		} else if !errors.Is(err, store.ErrNotFound) {
			return admitted, err
		}
		if parentState != model.StateGranted && parentState != model.StateExpiring {
			diag := fmt.Sprintf("reservation %s is %s", it.ParentID, parentState)
			if parentState == "" {
				diag = fmt.Sprintf("reservation %s not found", it.ParentID)
			}
			s.endLocked(it, model.StateCancelled, model.ReasonCancelled, diag)
			if err := s.persistLocked(ctx, it); err != nil {
				return admitted, err
			}
			continue
		}
		if err := s.admitTaskLocked(ctx, it, now); err != nil {
			return admitted, err
		}
		admitted = append(admitted, it)
	}
	return admitted, nil
}

func (s *Scheduler) rejectItem(it *model.Item, diag string) {
	it.State, it.Diagnostic, it.EndedAt = model.StateRejected, diag, s.d.Now()
	it.Env, it.JobDir = nil, "" // values may be secrets; a terminal item never needs them
}

func (s *Scheduler) insertRejected(ctx context.Context, it *model.Item) (*model.Item, error) {
	stored, _, err := s.d.Store.InsertItem(ctx, it)
	if err != nil {
		return nil, err
	}
	if stored.State == model.StateRejected {
		return stored, fmt.Errorf("%w: %s", ErrRejected, stored.Diagnostic)
	}
	return stored, nil // an earlier, accepted request with the same key
}

// writeTeardownScript writes the daemon's private copy (0700, in a 0700 directory).
func writeTeardownScript(path string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("teardown script dir: %w", err)
	}
	if err := os.WriteFile(path, content, 0o700); err != nil {
		return fmt.Errorf("teardown script: %w", err)
	}
	return nil
}

func removeDir(dir string) {
	if err := os.RemoveAll(dir); err != nil {
		slog.Warn("scheduler: remove job dir", "dir", dir, "err", err)
	}
}

// grantLocked moves an admitted reservation from QUEUED to GRANTED; its TTL starts
// now. One that waited in the queue gets reservation_granted.
func (s *Scheduler) grantLocked(ctx context.Context, it *model.Item, now time.Time) error {
	it.State, it.AdmittedAt, it.StartedAt = model.StateGranted, now, now
	it.ExpiresAt = now.Add(it.Timeout)
	it.WarnAt = s.warnAt(it.ExpiresAt, now)
	var evs []model.Event
	if it.NotifyGrant {
		evs = s.newEvents(it, model.EventReservationGranted,
			fmt.Sprintf("reservation %s granted; expires at %s", it.ID, it.ExpiresAt.UTC().Format(time.RFC3339)),
			model.AudienceSession)
	}
	return s.persistLocked(ctx, it, evs...)
}

// Release ends a reservation without running its teardown script: GRANTED or EXPIRING
// with no unfinished nested task becomes RELEASED, QUEUED becomes CANCELLED and a
// terminal one is returned unchanged; with force, TEARDOWN_FAILED and TEARDOWN_UNKNOWN
// also become RELEASED ("released by operator (--force)") unless, as for RetryTeardown,
// a Stop of the teardown unit is in flight or the unit has processes. Anything else
// (TEARING_DOWN included, force or not) is ErrBadState, item unchanged. The nested check and the
// change share one critical section, so a concurrent --within submit cannot slip in.
func (s *Scheduler) Release(ctx context.Context, id model.ItemID, force bool) (*model.Item, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	it, err := s.d.Store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if !it.IsReservation() {
		return it, fmt.Errorf("%w: %s is a task, not a reservation; use aq cancel", ErrBadState, id)
	}
	var evs []model.Event
	switch {
	case it.State.Terminal():
		return it, nil
	case it.State == model.StateQueued:
		s.endLocked(it, model.StateCancelled, model.ReasonCancelled, "")
	case it.State == model.StateGranted || it.State == model.StateExpiring,
		force && (it.State == model.StateTeardownFailed || it.State == model.StateTeardownUnknown):
		active, err := s.d.Store.ListActive(ctx)
		if err != nil {
			return nil, err
		}
		if n := countNested(active, id); n > 0 {
			return it, fmt.Errorf("%w: reservation %s has %d unfinished nested task(s); wait for them or cancel them first",
				ErrBadState, id, n)
		}
		diag := ""
		if it.State == model.StateTeardownFailed || it.State == model.StateTeardownUnknown {
			if err := s.teardownUnitGoneLocked(ctx, id); err != nil {
				return it, err
			}
			evs = s.newEvents(it, model.EventReservationForceReleased, fmt.Sprintf("stand %s released by operator after %s", standLabel(it), it.State), model.AudienceOperator)
			diag = "released by operator (--force)"
		}
		s.endLocked(it, model.StateReleased, model.ReasonNone, diag)
	case it.State == model.StateTeardownFailed || it.State == model.StateTeardownUnknown:
		return it, fmt.Errorf("%w: reservation %s is %s; use aq teardown --retry or aq release --force",
			ErrBadState, id, it.State)
	default:
		return it, fmt.Errorf("%w: reservation %s is %s", ErrBadState, id, it.State)
	}
	if err := s.persistLocked(ctx, it, evs...); err != nil {
		return nil, err
	}
	s.Kick() // the pool has room again
	return it, nil
}

// countNested is the number of items in active (non-terminal by now) running within id.
func countNested(active []*model.Item, id model.ItemID) int {
	n := 0
	for _, x := range active {
		if x.ParentID == id && !x.State.Terminal() {
			n++
		}
	}
	return n
}

// Extend makes a GRANTED or EXPIRING reservation last at least ttl from now:
// ExpiresAt = max(ExpiresAt, now+ttl), so a repeat does not add up. It becomes GRANTED,
// no longer held, with WarnAt recomputed. A ttl outside (0, max_ttl] is ErrRejected and
// any other state ErrBadState; the item is then unchanged.
func (s *Scheduler) Extend(ctx context.Context, id model.ItemID, ttl time.Duration) (*model.Item, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	it, err := s.d.Store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	maxTTL := s.d.Cfg.Reservations.MaxTTL
	switch {
	case !it.IsReservation():
		return it, fmt.Errorf("%w: %s is a task, not a reservation", ErrBadState, id)
	case ttl <= 0:
		return it, fmt.Errorf("%w: ttl must be positive", ErrRejected)
	case ttl > maxTTL:
		return it, fmt.Errorf("%w: ttl %s exceeds max_ttl %s", ErrRejected, ttl, maxTTL)
	case it.State != model.StateGranted && it.State != model.StateExpiring:
		return it, fmt.Errorf("%w: reservation %s is %s, not granted", ErrBadState, id, it.State)
	}
	now := s.d.Now()
	if exp := now.Add(ttl); exp.After(it.ExpiresAt) {
		it.ExpiresAt = exp
	}
	it.State, it.TTLHeld = model.StateGranted, false
	it.ExtendCount++
	it.WarnAt = s.warnAt(it.ExpiresAt, now)
	if err := s.persistLocked(ctx, it); err != nil {
		return nil, err
	}
	return it, nil
}

// expireLocked warns about and holds expiring reservations and returns those due for
// teardown. A GRANTED reservation whose WarnAt has come becomes EXPIRING with
// reservation_expiring. One whose ExpiresAt has come while nested tasks are unfinished
// is held (holdLocked) and reconsidered only once its new ExpiresAt comes. One with no
// unfinished nested task is due: it is returned and left as it is.
func (s *Scheduler) expireLocked(ctx context.Context, items []*model.Item, now time.Time) []*model.Item {
	// The latest deadline of each reservation's unfinished nested tasks: started_at (now
	// for one not started yet) + timeout + 2×cancel_grace.
	deadline := map[model.ItemID]time.Time{}
	nested := map[model.ItemID]int{}
	for _, it := range items {
		if it.ParentID == "" || it.State.Terminal() { // terminal: finalized by reapLocked in this pass
			continue
		}
		start := it.StartedAt
		if start.IsZero() {
			start = now
		}
		if d := start.Add(it.Timeout + 2*s.d.Cfg.CancelGrace); d.After(deadline[it.ParentID]) {
			deadline[it.ParentID] = d
		}
		nested[it.ParentID]++
	}

	var due []*model.Item
	for _, it := range items {
		if !it.IsReservation() || (it.State != model.StateGranted && it.State != model.StateExpiring) {
			continue
		}
		if it.State == model.StateGranted && !it.WarnAt.IsZero() && !now.Before(it.WarnAt) {
			it.State = model.StateExpiring
			evs := s.newEvents(it, model.EventReservationExpiring,
				fmt.Sprintf("reservation %s expires at %s; extend it with aq extend", it.ID, it.ExpiresAt.UTC().Format(time.RFC3339)),
				model.AudienceSession, model.AudienceOperator)
			if err := s.persistLocked(ctx, it, evs...); err != nil {
				slog.Error("scheduler: record reservation expiring", "id", it.ID, "err", err)
				it.State = model.StateGranted // not persisted: warn again next pass
			}
		}
		// A reservation held past its TTL is due once its last nested task ends,
		// not at the held deadline.
		if now.Before(it.ExpiresAt) && !(it.TTLHeld && nested[it.ID] == 0) {
			continue
		}
		if it.ExpiredAt.IsZero() && !now.Before(it.ExpiresAt) {
			it.ExpiredAt = now
			if err := s.persistLocked(ctx, it); err != nil {
				it.ExpiredAt = time.Time{}
				slog.Error("scheduler: record expiry", "id", it.ID, "err", err)
				continue
			}
		}
		if n := nested[it.ID]; n > 0 {
			s.holdLocked(ctx, it, deadline[it.ID], n)
			continue
		}
		due = append(due, it)
	}
	return due
}

// holdLocked keeps an expired reservation while n nested tasks are unfinished: EXPIRING,
// TTLHeld, ExpiresAt = max(ExpiresAt, until). Becoming held, and every move of
// ExpiresAt to a later value, records reservation_held_by_tasks for the operator.
func (s *Scheduler) holdLocked(ctx context.Context, it *model.Item, until time.Time, n int) {
	if it.TTLHeld && it.State == model.StateExpiring && !until.After(it.ExpiresAt) {
		return // held already, and no nested deadline moved past it
	}
	prev := *it
	if until.After(it.ExpiresAt) {
		it.ExpiresAt = until
	}
	it.State, it.TTLHeld = model.StateExpiring, true
	evs := s.newEvents(it, model.EventReservationHeldByTask,
		fmt.Sprintf("reservation %s expired with %d unfinished nested task(s); held until %s",
			it.ID, n, it.ExpiresAt.UTC().Format(time.RFC3339)),
		model.AudienceOperator)
	if err := s.persistLocked(ctx, it, evs...); err != nil {
		slog.Error("scheduler: record reservation held by tasks", "id", it.ID, "err", err)
		*it = prev // not persisted: hold again next pass
	}
}

// warnAt is when to warn about expiry: expiry_warning before expires, or zero if
// that moment is not after now.
func (s *Scheduler) warnAt(expires, now time.Time) time.Time {
	w := s.d.Cfg.Reservations.ExpiryWarning
	if expires.Sub(now) > w {
		return expires.Add(-w)
	}
	return time.Time{}
}

// newEvents builds one event of kind per audience; the session audience is skipped
// without a session. persistLocked stamps CreatedAt.
func (s *Scheduler) newEvents(it *model.Item, kind model.EventKind, detail string, aud ...model.Audience) []model.Event {
	var out []model.Event
	for _, a := range aud {
		if a == model.AudienceSession && (it.SessionID == "" || it.NoNotify) {
			continue
		}
		ch := s.d.Route(it.ClientType, a)
		status := model.EventPending
		if ch == model.ChannelNone {
			status = model.EventSkipped
		}
		out = append(out, model.Event{Channel: ch, Status: status,
			ID: string(model.NewID()), ItemID: it.ID, Kind: kind, Audience: a,
			SessionID: it.SessionID, ClientType: it.ClientType, Detail: detail,
		})
	}
	return out
}

// AfterNextPass kicks the scheduler and returns once an admission pass that started
// after the call has finished. It fails only when ctx ends first.
func (s *Scheduler) AfterNextPass(ctx context.Context) error {
	s.mu.Lock()
	ch := s.passDone // passes run under mu, so none is half done now
	s.mu.Unlock()
	s.Kick()
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// StandUsage is the fresh stand measurement of reservation id seen by the last pass.
func (s *Scheduler) StandUsage(id model.ItemID) (uint64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.standUsage[id]
	return v, ok
}
