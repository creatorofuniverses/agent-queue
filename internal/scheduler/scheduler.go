// Package scheduler owns admission: the "does it fit" check and taking the
// resources happen under one mutex, so concurrent submitters cannot overcommit.
package scheduler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"resource-dispatcher/internal/admission"
	"resource-dispatcher/internal/config"
	"resource-dispatcher/internal/model"
	"resource-dispatcher/internal/platform"
	"resource-dispatcher/internal/store"
	"resource-dispatcher/internal/units"
)

// ErrRejected: the request failed validation; the item is stored as REJECTED.
var ErrRejected = errors.New("request rejected")

// launchFailedCancel is the CancelReason of a unit stopped because its launch
// failed; such an item settles as LAUNCH_FAILED rather than CANCELLED.
const launchFailedCancel = "launch failed"

// minMemory keeps room for the unit's ExecStopPost recorder under MemoryMax (R9).
const minMemory = 64 << 20

type Deps struct {
	Store *store.Store
	Exec  platform.Executor
	Host  platform.HostMetrics
	Meter platform.StandMeter // nil: no stand is ever measured
	Cfg   config.Config
	Paths config.Paths
	Now   func() time.Time
}

type SubmitRequest struct {
	Name       string            `json:"name"`
	Argv       []string          `json:"argv"`
	Cwd        string            `json:"cwd"`
	Env        map[string]string `json:"env"`
	Limit      uint64            `json:"limit"`
	Expected   uint64            `json:"expected"`
	CPUs       int               `json:"cpus"`
	Timeout    time.Duration     `json:"timeout"`
	Priority   model.Priority    `json:"priority"`
	SessionID  string            `json:"session_id"`
	ClientType string            `json:"client_type"`
	Project    string            `json:"project"`
	IdemKey    string            `json:"-"`
	GitRev     string            `json:"git_rev"`
	GitDirty   bool              `json:"git_dirty"`
}

type Scheduler struct {
	d     Deps
	kick  chan struct{}
	guard atomic.Bool
	bg    sync.WaitGroup // background stops and temp dir removals

	// mu guards everything below and serializes every state transition.
	// It is never held across Launch, Stop or git.
	mu             sync.Mutex
	gate           admission.PressureGate
	pressureClosed bool
	pressureAvg    float64
	recovering     bool
	headID         model.ItemID
	headDecision   admission.Decision
	resultSeen     map[model.ItemID]time.Time     // result read while processes remain
	goneSince      map[model.ItemID]time.Time     // no processes and no result since
	stopping       map[model.ItemID]bool          // a background Stop is in flight
	lastStop       map[model.ItemID]time.Time     // when the last background Stop was issued
	waiters        map[model.ItemID]chan struct{} // closed once waitDone
	passDone       chan struct{}                  // closed and replaced at the end of every pass
	standUsage     map[model.ItemID]uint64        // fresh stand measurements seen by the last pass
}

// New returns a scheduler that admits nothing (BlockRecovering) until Recover succeeds.
func New(d Deps) *Scheduler {
	if d.Now == nil {
		d.Now = time.Now
	}
	p := d.Cfg.Pressure
	return &Scheduler{
		d:    d,
		kick: make(chan struct{}, 1),
		gate: admission.PressureGate{
			CloseAt: p.CloseFullAvg10, CloseAfter: p.CloseAfter,
			OpenBelow: p.OpenFullAvg10, OpenAfter: p.OpenAfter,
		},
		headDecision: admission.Decision{Admit: true},
		resultSeen:   map[model.ItemID]time.Time{},
		goneSince:    map[model.ItemID]time.Time{},
		stopping:     map[model.ItemID]bool{},
		lastStop:     map[model.ItemID]time.Time{},
		recovering:   true, // admission stays closed until Recover succeeds
		waiters:      map[model.ItemID]chan struct{}{},
		passDone:     make(chan struct{}),
		standUsage:   map[model.ItemID]uint64{},
	}
}

func (s *Scheduler) policy(memTotal uint64) admission.Policy {
	return admission.Policy{
		Pool:         model.Resources{MemoryBytes: s.d.Cfg.PoolMemory, CPUs: s.d.Cfg.PoolCPUs},
		ReserveFloor: s.d.Cfg.ReserveFloor(memTotal),
		MinDiskFree:  s.d.Cfg.MinDiskFree,
	}
}

func validate(r SubmitRequest, p admission.Policy) error {
	switch {
	case len(r.Argv) == 0:
		return errors.New("argv is empty")
	case !filepath.IsAbs(r.Cwd):
		return fmt.Errorf("cwd %q is not absolute", r.Cwd)
	case r.Timeout <= 0:
		return errors.New("timeout must be positive")
	case r.Expected > r.Limit:
		return fmt.Errorf("expected memory %d exceeds limit %d", r.Expected, r.Limit)
	}
	if st, err := os.Stat(r.Cwd); err != nil || !st.IsDir() {
		return fmt.Errorf("cwd %q is not an existing directory", r.Cwd)
	}
	if err := admission.Validate(p, model.Resources{MemoryBytes: r.Limit, CPUs: r.CPUs}); err != nil {
		return err
	}
	if r.Limit < minMemory {
		return fmt.Errorf("memory limit %d is below the 64 MiB minimum", r.Limit)
	}
	return nil
}

// Submit validates and stores r: QUEUED on success, REJECTED (returned with
// ErrRejected) otherwise. A repeated IdemKey with the same request returns the stored item.
func (s *Scheduler) Submit(ctx context.Context, r SubmitRequest) (*model.Item, error) {
	if r.Expected == 0 {
		r.Expected = r.Limit
	}
	canon, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(canon)
	now := s.d.Now()
	it := &model.Item{
		ID: model.NewID(), Kind: model.KindTask, Name: r.Name, State: model.StateQueued, Priority: r.Priority,
		Argv: r.Argv, Cwd: r.Cwd, Env: r.Env, Limit: r.Limit, Expected: r.Expected, CPUs: r.CPUs, Timeout: r.Timeout,
		SessionID: r.SessionID, ClientType: r.ClientType, Project: r.Project, IdemKey: r.IdemKey,
		RequestHash: hex.EncodeToString(sum[:]), NodeID: "local", CreatedAt: now,
		GitRev: r.GitRev, GitDirty: r.GitDirty,
	}
	vErr := validate(r, s.policy(0))
	if vErr != nil {
		it.State, it.Diagnostic, it.EndedAt = model.StateRejected, vErr.Error(), now
		it.Env = nil // values may be secrets; a terminal item never needs them
	}
	stored, created, err := s.d.Store.InsertItem(ctx, it)
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

// Cancel is idempotent: QUEUED becomes CANCELLED at once; STARTING/RUNNING get
// CancelRequested and a background Stop (the outcome is recorded at finalization
// by Tick); others are returned unchanged. It never waits for the Stop.
func (s *Scheduler) Cancel(ctx context.Context, id model.ItemID, reason string) (*model.Item, error) {
	s.mu.Lock()
	it, err := s.d.Store.Get(ctx, id)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	switch it.State {
	case model.StateQueued:
		it.CancelRequested, it.CancelReason = true, reason
		s.endLocked(it, model.StateCancelled, model.ReasonCancelled, "")
		err = s.persistLocked(ctx, it)
		s.mu.Unlock()
		return it, err
	case model.StateStarting, model.StateRunning:
		if !it.CancelRequested {
			it.CancelRequested, it.CancelReason = true, reason
			if err := s.persistLocked(ctx, it); err != nil {
				s.mu.Unlock()
				return nil, err
			}
		}
		// A STARTING item is caught before or right after its Launch.
		s.stopAsyncLocked(id)
		s.mu.Unlock()
		return it, nil
	}
	s.mu.Unlock()
	return it, nil
}

// endLocked sets a terminal state stamped with the current time.
func (s *Scheduler) endLocked(it *model.Item, st model.State, r model.Reason, diag string) {
	it.State, it.Reason, it.EndedAt = st, r, s.d.Now()
	if diag != "" {
		it.Diagnostic = diag
	}
}

// persistLocked writes it together with evs (one transaction; events without CreatedAt
// are stamped now) and wakes its waiters once waitDone. On a terminal state, which is
// only ever reached with no processes left, it drops the env and removes the item's
// temp dir (never its JobDir).
func (s *Scheduler) persistLocked(ctx context.Context, it *model.Item, evs ...model.Event) error {
	if it.State.Terminal() {
		it.Env = nil // values may be secrets; a terminal item never needs them
	}
	now := s.d.Now()
	for i := range evs {
		if evs[i].CreatedAt.IsZero() {
			evs[i].CreatedAt = now
		}
	}
	if err := s.d.Store.UpdateWithEvents(ctx, it, evs); err != nil {
		return err
	}
	if waitDone(it) {
		if ch, ok := s.waiters[it.ID]; ok {
			close(ch)
			delete(s.waiters, it.ID)
		}
	}
	if it.State.Terminal() {
		delete(s.resultSeen, it.ID)
		delete(s.goneSince, it.ID)
		delete(s.lastStop, it.ID)
		tmp := s.d.Paths.TmpDir(it.ID)
		s.background(func() {
			if err := os.RemoveAll(tmp); err != nil {
				slog.Warn("scheduler: remove temp dir", "dir", tmp, "err", err)
			}
		})
	}
	return nil
}

func (s *Scheduler) background(f func()) {
	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		f()
	}()
}

// stopAsyncLocked stops id's unit in the background, at most one Stop per item at a time.
// Stop blocks up to cancel_grace, so neither Cancel nor Tick waits for it.
func (s *Scheduler) stopAsyncLocked(id model.ItemID) {
	if s.stopping[id] {
		return
	}
	s.stopping[id] = true
	s.lastStop[id] = s.d.Now()
	s.background(func() {
		ctx, cancel := context.WithTimeout(context.Background(), s.d.Cfg.CancelGrace+time.Minute)
		defer cancel()
		if err := s.d.Exec.Stop(ctx, id); err != nil {
			slog.Warn("scheduler: stop unit", "id", id, "err", err)
		}
		s.mu.Lock()
		delete(s.stopping, id)
		s.mu.Unlock()
	})
}

// Tick runs one scheduling pass: reap finished or vanished units, update the pressure
// gate, admit queue heads while they fit (granting reservations at once), then launch
// the admitted tasks outside the lock.
func (s *Scheduler) Tick(ctx context.Context) error {
	s.mu.Lock()
	items, err := s.d.Store.ListActive(ctx)
	if err != nil {
		s.endPassLocked()
		s.mu.Unlock()
		return err
	}
	s.reapLocked(ctx, items)
	admitted, err := s.admitLocked(ctx, items)
	s.endPassLocked()
	s.mu.Unlock()

	for _, it := range admitted {
		s.start(ctx, it)
	}
	return err
}

func (s *Scheduler) admitLocked(ctx context.Context, items []*model.Item) ([]*model.Item, error) {
	now := s.d.Now()
	if avg, err := s.d.Host.PressureFullAvg10(); err != nil {
		slog.Warn("scheduler: read memory pressure; gate keeps its state", "err", err)
	} else {
		s.pressureClosed, _ = s.gate.Observe(now, avg)
		s.pressureAvg = avg
	}
	mem, err := s.d.Host.Memory()
	if err != nil {
		return nil, err
	}
	disk, err := s.d.Host.DiskFree(s.d.Paths.StateDir)
	if err != nil {
		return nil, err
	}
	snap, queue, _ := s.snapshotLocked(ctx, items, mem, disk)

	pol := s.policy(mem.Total)
	s.headID, s.headDecision = "", admission.Decision{Admit: true}
	var admitted []*model.Item
	for _, it := range queue {
		req := it.Committed()
		dec := admission.Decide(pol, snap, req)
		if !dec.Admit {
			s.headID, s.headDecision = it.ID, dec
			break
		}
		if it.IsReservation() {
			if err := s.grantLocked(ctx, it, now); err != nil {
				return admitted, err
			}
		} else {
			it.State, it.AdmittedAt = model.StateStarting, now
			it.JobDir = s.d.Paths.JobDir(it.ID)
			if err := s.persistLocked(ctx, it); err != nil {
				return admitted, err
			}
			admitted = append(admitted, it)
		}
		// A just-admitted item has consumed nothing yet.
		snap.Committed.MemoryBytes += req.MemoryBytes
		snap.Committed.CPUs += req.CPUs
		snap.Unconsumed += req.MemoryBytes
	}
	// A reservation that stays queued through a pass is told when it is granted.
	for _, it := range queue {
		if it.IsReservation() && it.State == model.StateQueued && !it.NotifyGrant {
			it.NotifyGrant = true
			if err := s.persistLocked(ctx, it); err != nil {
				slog.Error("scheduler: record grant notification", "id", it.ID, "err", err)
			}
		}
	}
	return admitted, nil
}

// snapshotLocked builds the admission snapshot from the active items and returns the
// non-nested queue (in order) and the queued nested tasks. Counted against the pool:
// non-nested tasks while STARTING..RECOVERING, reservations while Held, each by
// Committed(). Unconsumed: a task's max(0, expected - memory.current) while
// STARTING/RUNNING; a held reservation's budget max(0, task_memory - Σ memory.current
// of its STARTING/RUNNING nested tasks) plus its stand, which is max(0, memory -
// measured) with a fresh measurement of its docker project and memory otherwise.
// Nested tasks count only towards their parent's sum. It also hands the meter the
// held projects and records stand_over_declared once per reservation.
func (s *Scheduler) snapshotLocked(ctx context.Context, items []*model.Item, mem platform.MemInfo, disk uint64) (admission.Snapshot, []*model.Item, []*model.Item) {
	snap := admission.Snapshot{
		MemAvailable: mem.Available, DiskFree: disk,
		PressureClosed: s.pressureClosed, Recovering: s.recovering,
	}
	nestedCur := map[model.ItemID]uint64{}
	var queue, nestedQueued []*model.Item
	for _, it := range items {
		if it.ParentID == "" {
			continue
		}
		switch it.State {
		case model.StateQueued:
			nestedQueued = append(nestedQueued, it)
		case model.StateStarting, model.StateRunning:
			cur, _ := s.d.Exec.MemoryCurrent(it.ID)
			nestedCur[it.ParentID] += cur
		}
	}

	var projects []string
	s.standUsage = map[model.ItemID]uint64{}
	for _, it := range items {
		if it.ParentID != "" {
			continue
		}
		if it.State == model.StateQueued {
			queue = append(queue, it)
			continue
		}
		if it.IsReservation() {
			if !it.State.Held() {
				continue // terminal in this pass
			}
			snap.Unconsumed += satSub(it.TaskMemory, nestedCur[it.ID]) + s.standUnconsumedLocked(ctx, it)
			if it.DockerProject != "" {
				projects = append(projects, it.DockerProject)
			}
		} else {
			switch it.State {
			case model.StateStarting, model.StateRunning:
				cur, _ := s.d.Exec.MemoryCurrent(it.ID)
				snap.Unconsumed += satSub(it.Expected, cur)
			case model.StateCleaning, model.StateRecovering:
			default:
				continue // finalized by reapLocked in this pass
			}
		}
		c := it.Committed()
		snap.Committed.MemoryBytes += c.MemoryBytes
		snap.Committed.CPUs += c.CPUs
	}
	if s.d.Meter != nil {
		slices.Sort(projects)
		s.d.Meter.Watch(slices.Compact(projects))
	}
	return snap, queue, nestedQueued
}

// standUnconsumedLocked is the part of a held reservation's stand not yet in use.
func (s *Scheduler) standUnconsumedLocked(ctx context.Context, it *model.Item) uint64 {
	if it.DockerProject == "" || s.d.Meter == nil {
		return it.Expected
	}
	used, ok := s.d.Meter.Usage(it.DockerProject)
	if !ok {
		return it.Expected // no fresh measurement: assume nothing is used yet
	}
	s.standUsage[it.ID] = used
	if used > it.Expected && !it.OverDeclaredNotified {
		it.OverDeclaredNotified = true
		detail := fmt.Sprintf("stand of reservation %s (docker project %s) uses %s, declared %s",
			it.ID, it.DockerProject, units.FormatSize(used), units.FormatSize(it.Expected))
		evs := newEvents(it, model.EventStandOverDeclared, detail, model.AudienceSession, model.AudienceOperator)
		if err := s.persistLocked(ctx, it, evs...); err != nil {
			slog.Error("scheduler: record stand over declared", "id", it.ID, "err", err)
		}
	}
	return satSub(it.Expected, used)
}

// satSub returns a-b, or 0 if b > a.
func satSub(a, b uint64) uint64 {
	if b > a {
		return 0
	}
	return a - b
}

// endPassLocked wakes everyone waiting in AfterNextPass.
func (s *Scheduler) endPassLocked() {
	close(s.passDone)
	s.passDone = make(chan struct{})
}

// start launches an admitted (STARTING) item. The cancel check, git and Launch run
// without the mutex; the outcome is persisted even if ctx ends meanwhile.
func (s *Scheduler) start(ctx context.Context, it *model.Item) {
	pctx := context.WithoutCancel(ctx)
	rev, dirty, gitOK := gitState(ctx, it.Cwd)

	s.mu.Lock()
	cur, err := s.d.Store.Get(pctx, it.ID)
	if err != nil {
		s.mu.Unlock()
		// Stays STARTING; Recover requeues it after a restart.
		slog.Error("scheduler: load admitted item", "id", it.ID, "err", err)
		return
	}
	if cur.State != model.StateStarting {
		s.mu.Unlock()
		return
	}
	if cur.CancelRequested {
		s.endLocked(cur, model.StateCancelled, model.ReasonCancelled, "")
		if err := s.persistLocked(pctx, cur); err != nil {
			slog.Error("scheduler: record cancel before launch", "id", cur.ID, "err", err)
		}
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()

	unit, lerr := s.d.Exec.Launch(ctx, s.launchSpec(cur))
	unitAlive := false
	if lerr != nil {
		// Launch stops the unit on failure, but that Stop may itself have failed.
		alive, aerr := s.d.Exec.Alive(pctx, it.ID)
		if aerr != nil {
			slog.Warn("scheduler: read unit liveness after failed launch", "id", it.ID, "err", aerr)
		}
		unitAlive = alive || aerr != nil
	}

	s.mu.Lock()
	cur, err = s.d.Store.Get(pctx, it.ID)
	if err != nil {
		s.mu.Unlock()
		// Stays STARTING; Recover settles it from the unit after a restart.
		slog.Error("scheduler: load launched item", "id", it.ID, "launched", lerr == nil, "err", err)
		return
	}
	switch {
	case lerr != nil && unitAlive:
		// Not terminal while processes may run: stay RUNNING (resources counted) with a
		// cancel that the reaper enforces; it settles the item once the unit is gone.
		cur.State, cur.StartedAt, cur.Diagnostic = model.StateRunning, s.d.Now(), lerr.Error()
		if !cur.CancelRequested {
			cur.CancelRequested, cur.CancelReason = true, launchFailedCancel
		}
	case lerr != nil && cur.CancelRequested:
		s.endLocked(cur, model.StateCancelled, model.ReasonCancelled, lerr.Error())
	case lerr != nil:
		s.endLocked(cur, model.StateFailed, model.ReasonLaunchFailed, lerr.Error())
	default:
		cur.State, cur.StartedAt, cur.UnitName = model.StateRunning, s.d.Now(), unit
		if gitOK {
			cur.GitRev, cur.GitDirty = rev, dirty
		}
	}
	if err := s.persistLocked(pctx, cur); err != nil {
		// The item stays STARTING in the store; Recover settles it from the unit.
		slog.Error("scheduler: record launch outcome", "id", cur.ID, "state", cur.State, "err", err)
	}
	if cur.State == model.StateRunning && cur.CancelRequested { // cancelled while Launch was in flight, or launch failed
		s.stopAsyncLocked(it.ID)
	}
	s.mu.Unlock()
}

func (s *Scheduler) launchSpec(it *model.Item) platform.LaunchSpec {
	env := make(map[string]string, len(it.Env)+2)
	for k, v := range it.Env {
		env[k] = v
	}
	env["AQ_JOB_ID"] = string(it.ID)
	env["AQ_CPUS"] = strconv.Itoa(it.CPUs)
	return platform.LaunchSpec{
		ID: it.ID, Argv: it.Argv, Cwd: it.Cwd, Env: env,
		Limit:      it.Limit,
		MemoryHigh: uint64(math.Floor(float64(it.Limit) * s.d.Cfg.MemoryHighRatio)),
		CPUs:       it.CPUs, Timeout: it.Timeout, KillGrace: s.d.Cfg.CancelGrace,
		JobDir: s.d.Paths.JobDir(it.ID), TmpDir: s.d.Paths.TmpDir(it.ID),
		MaxStdout: s.d.Cfg.Logs.MaxStdout, MaxStderr: s.d.Cfg.Logs.MaxStderr,
	}
}

// gitTimeout bounds all git calls for one launch.
var gitTimeout = 5 * time.Second

// gitState reads the revision and dirty flag of cwd; ok is false outside a repository
// or when git does not answer within gitTimeout. --no-optional-locks keeps status from
// taking index.lock in the user's repository.
func gitState(ctx context.Context, cwd string) (rev string, dirty, ok bool) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	git := func(args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, "git", append([]string{"--no-optional-locks", "-C", cwd}, args...)...)
		cmd.WaitDelay = time.Second // a killed git's children may hold stdout
		return cmd.Output()
	}
	out, err := git("rev-parse", "HEAD")
	if err != nil {
		return "", false, false
	}
	st, err := git("status", "--porcelain")
	if err != nil {
		return "", false, false
	}
	return strings.TrimSpace(string(out)), len(strings.TrimSpace(string(st))) > 0, true
}

// Kick asks Run for a Tick without blocking.
func (s *Scheduler) Kick() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// Run ticks on every Kick and at least once a second until ctx ends, then returns ctx.Err().
func (s *Scheduler) Run(ctx context.Context) error {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		if err := s.Tick(ctx); err != nil && ctx.Err() == nil {
			slog.Error("scheduler: tick", "err", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.kick:
		case <-t.C:
		}
	}
}

// waitDone reports whether Wait on it returns: a task once terminal, a reservation
// once it has left the queue.
func waitDone(it *model.Item) bool {
	if it.IsReservation() {
		return it.State != model.StateQueued
	}
	return it.State.Terminal()
}

// Wait blocks until id is waitDone or ctx ends.
func (s *Scheduler) Wait(ctx context.Context, id model.ItemID) (*model.Item, error) {
	s.mu.Lock()
	it, err := s.d.Store.Get(ctx, id)
	if err != nil || waitDone(it) {
		s.mu.Unlock()
		return it, err
	}
	ch, ok := s.waiters[id]
	if !ok {
		ch = make(chan struct{})
		s.waiters[id] = ch
	}
	s.mu.Unlock()
	select {
	case <-ch:
		return s.d.Store.Get(ctx, id)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// HeadDecision is the last admission decision for the queue head (zero ID and Admit when the queue was empty).
func (s *Scheduler) HeadDecision() (model.ItemID, admission.Decision) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.headID, s.headDecision
}

// Pressure is the gate state and full avg10 seen by the last Tick.
func (s *Scheduler) Pressure() (closed bool, avg10 float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pressureClosed, s.pressureAvg
}

func (s *Scheduler) GuardActive() bool     { return s.guard.Load() }
func (s *Scheduler) SetGuardActive(v bool) { s.guard.Store(v) }
