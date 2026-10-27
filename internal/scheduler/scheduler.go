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
)

// ErrRejected: the request failed validation; the item is stored as REJECTED.
var ErrRejected = errors.New("request rejected")

// minMemory keeps room for the unit's ExecStopPost recorder under MemoryMax (R9).
const minMemory = 64 << 20

type Deps struct {
	Store *store.Store
	Exec  platform.Executor
	Host  platform.HostMetrics
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
	waiters        map[model.ItemID]chan struct{} // closed on the terminal transition
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

// persistLocked writes it. On a terminal state, which is only ever reached with no
// processes left, it wakes waiters and removes the item's temp dir (never its JobDir).
func (s *Scheduler) persistLocked(ctx context.Context, it *model.Item) error {
	if err := s.d.Store.Update(ctx, it); err != nil {
		return err
	}
	if it.State.Terminal() {
		delete(s.resultSeen, it.ID)
		delete(s.goneSince, it.ID)
		delete(s.lastStop, it.ID)
		if ch, ok := s.waiters[it.ID]; ok {
			close(ch)
			delete(s.waiters, it.ID)
		}
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
// gate, admit queue heads while they fit, then launch the admitted items outside the lock.
func (s *Scheduler) Tick(ctx context.Context) error {
	s.mu.Lock()
	items, err := s.d.Store.ListActive(ctx)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	s.reapLocked(ctx, items)
	admitted, err := s.admitLocked(ctx, items)
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
	snap := admission.Snapshot{
		MemAvailable: mem.Available, DiskFree: disk,
		PressureClosed: s.pressureClosed, Recovering: s.recovering,
	}
	var queue []*model.Item
	for _, it := range items {
		switch it.State {
		case model.StateQueued:
			queue = append(queue, it)
			continue
		case model.StateStarting, model.StateRunning:
			cur, _ := s.d.Exec.MemoryCurrent(it.ID)
			if it.Expected > cur {
				snap.Unconsumed += it.Expected - cur
			}
		case model.StateCleaning, model.StateRecovering:
		default:
			continue // finalized by reapLocked in this pass
		}
		snap.Committed.MemoryBytes += it.Expected
		snap.Committed.CPUs += it.CPUs
	}

	pol := s.policy(mem.Total)
	s.headID, s.headDecision = "", admission.Decision{Admit: true}
	var admitted []*model.Item
	for _, it := range queue {
		dec := admission.Decide(pol, snap, it.Committed())
		if !dec.Admit {
			s.headID, s.headDecision = it.ID, dec
			break
		}
		it.State, it.AdmittedAt = model.StateStarting, now
		it.JobDir = s.d.Paths.JobDir(it.ID)
		if err := s.persistLocked(ctx, it); err != nil {
			return admitted, err
		}
		// A just-admitted item has consumed nothing yet.
		snap.Committed.MemoryBytes += it.Expected
		snap.Committed.CPUs += it.CPUs
		snap.Unconsumed += it.Expected
		admitted = append(admitted, it)
	}
	return admitted, nil
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

	s.mu.Lock()
	cur, err = s.d.Store.Get(pctx, it.ID)
	if err != nil {
		s.mu.Unlock()
		// Stays STARTING; Recover settles it from the unit after a restart.
		slog.Error("scheduler: load launched item", "id", it.ID, "launched", lerr == nil, "err", err)
		return
	}
	if lerr != nil {
		s.endLocked(cur, model.StateFailed, model.ReasonLaunchFailed, lerr.Error())
	} else {
		cur.State, cur.StartedAt, cur.UnitName = model.StateRunning, s.d.Now(), unit
		if gitOK {
			cur.GitRev, cur.GitDirty = rev, dirty
		}
	}
	if err := s.persistLocked(pctx, cur); err != nil {
		// The item stays STARTING in the store; Recover settles it from the unit.
		slog.Error("scheduler: record launch outcome", "id", cur.ID, "state", cur.State, "err", err)
	}
	if lerr == nil && cur.CancelRequested { // cancelled while Launch was in flight
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

// gitState reads the revision and dirty flag of cwd; ok is false outside a repository.
func gitState(ctx context.Context, cwd string) (rev string, dirty, ok bool) {
	out, err := exec.CommandContext(ctx, "git", "-C", cwd, "rev-parse", "HEAD").Output()
	if err != nil {
		return "", false, false
	}
	st, err := exec.CommandContext(ctx, "git", "-C", cwd, "status", "--porcelain").Output()
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

// Wait blocks until id is terminal or ctx ends.
func (s *Scheduler) Wait(ctx context.Context, id model.ItemID) (*model.Item, error) {
	s.mu.Lock()
	it, err := s.d.Store.Get(ctx, id)
	if err != nil || it.State.Terminal() {
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
