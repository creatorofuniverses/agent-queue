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

	"resource-dispatcher/internal/model"
	"resource-dispatcher/internal/store"
	"resource-dispatcher/internal/units"
)

// maxTeardownScript is the largest teardown script a reservation may carry.
const maxTeardownScript = 64 << 10

var dockerProjectRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// ReserveRequest asks for a stand (Memory, CPUs) plus a budget for nested tasks
// (TaskMemory, TaskCPUs) for TTL from the grant. Teardown is the script's content.
type ReserveRequest struct {
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
	case r.CPUs+r.TaskCPUs > cfg.PoolCPUs:
		return fmt.Errorf("stand cpus %d plus task cpus %d exceed pool cpus %d", r.CPUs, r.TaskCPUs, cfg.PoolCPUs)
	case len(r.Teardown) == 0:
		return errors.New("teardown script is empty")
	case len(r.Teardown) > maxTeardownScript:
		return fmt.Errorf("teardown script is %d bytes, over the 64 KiB maximum", len(r.Teardown))
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
		RequestHash: hash, NodeID: "local", CreatedAt: now,
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
// parent is a GRANTED or EXPIRING reservation not held past its TTL, the limit fits the
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
	if cpus+it.CPUs > parent.TaskCPUs {
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
		evs = newEvents(it, model.EventReservationGranted,
			fmt.Sprintf("reservation %s granted; expires at %s", it.ID, it.ExpiresAt.UTC().Format(time.RFC3339)),
			model.AudienceSession)
	}
	return s.persistLocked(ctx, it, evs...)
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
func newEvents(it *model.Item, kind model.EventKind, detail string, aud ...model.Audience) []model.Event {
	var out []model.Event
	for _, a := range aud {
		if a == model.AudienceSession && it.SessionID == "" {
			continue
		}
		out = append(out, model.Event{
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
