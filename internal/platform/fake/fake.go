// Package fake provides goroutine-safe, test-controlled platform implementations.
package fake

import (
	"context"
	"math"
	"sync"

	"resource-dispatcher/internal/model"
	"resource-dispatcher/internal/platform"
)

type Executor struct {
	mu           sync.Mutex
	caps         platform.Capabilities
	launched     []platform.LaunchSpec
	stopped      []model.ItemID
	alive        map[model.ItemID]bool
	current      map[model.ItemID]uint64
	results      map[string]*platform.ExitInfo // by JobDir
	jobDir       map[model.ItemID]string
	failErr      error
	beforeLaunch func(platform.LaunchSpec)
	beforeStop   func(model.ItemID)
	sliceCur     uint64
	sliceOOM     uint64
	ceilings     [][2]uint64
	actualMax    uint64
	ceilingErr   error
	poolMemory   uint64
	teardownMax  uint64
}

func NewExecutor() *Executor {
	return &Executor{
		caps:    platform.Capabilities{HardMemoryLimit: true, TreeKill: true, OOMAttribution: true, SurvivesDaemonRestart: true},
		alive:   map[model.ItemID]bool{},
		current: map[model.ItemID]uint64{},
		results: map[string]*platform.ExitInfo{},
		jobDir:  map[model.ItemID]string{},
	}
}

func (e *Executor) Capabilities() platform.Capabilities {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.caps
}

// SetBeforeLaunch installs a hook called at the start of Launch, before the launch is recorded.
func (e *Executor) SetBeforeLaunch(f func(platform.LaunchSpec)) {
	e.mu.Lock()
	e.beforeLaunch = f
	e.mu.Unlock()
}

// SetBeforeStop installs a hook called at the start of Stop, before the stop is recorded.
func (e *Executor) SetBeforeStop(f func(model.ItemID)) {
	e.mu.Lock()
	e.beforeStop = f
	e.mu.Unlock()
}

// FailLaunch makes subsequent Launch calls return err (nil clears it).
func (e *Executor) FailLaunch(err error) {
	e.mu.Lock()
	e.failErr = err
	e.mu.Unlock()
}

func (e *Executor) Launch(_ context.Context, spec platform.LaunchSpec) (string, error) {
	e.mu.Lock()
	hook := e.beforeLaunch
	e.mu.Unlock()
	if hook != nil {
		hook(spec) // outside the lock so the hook may call back into the fake
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.jobDir[spec.ID] = spec.JobDir // a failed launch may still leave a unit that finishes
	if e.failErr != nil {
		return "", e.failErr
	}
	e.launched = append(e.launched, spec)
	e.alive[spec.ID] = true
	return "aq-" + string(spec.ID) + ".service", nil
}

func (e *Executor) Finish(id model.ItemID, info platform.ExitInfo) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.results[e.jobDir[id]] = &info
}

func (e *Executor) ReadResult(jobDir string) (*platform.ExitInfo, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if r, ok := e.results[jobDir]; ok {
		c := *r
		return &c, nil
	}
	return nil, nil
}

func (e *Executor) SetAlive(id model.ItemID, v bool) {
	e.mu.Lock()
	e.alive[id] = v
	e.mu.Unlock()
}

func (e *Executor) Alive(_ context.Context, id model.ItemID) (bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.alive[id], nil
}

func (e *Executor) SetCurrent(id model.ItemID, v uint64) {
	e.mu.Lock()
	e.current[id] = v
	e.mu.Unlock()
}

func (e *Executor) MemoryCurrent(id model.ItemID) (uint64, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.current[id], nil
}

func (e *Executor) Stop(_ context.Context, id model.ItemID) error {
	e.mu.Lock()
	hook := e.beforeStop
	e.mu.Unlock()
	if hook != nil {
		hook(id) // outside the lock so the hook may call back into the fake
	}
	e.mu.Lock()
	e.stopped = append(e.stopped, id)
	e.mu.Unlock()
	return nil
}

func (e *Executor) Stopped() []model.ItemID {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]model.ItemID(nil), e.stopped...)
}

func (e *Executor) Launched() []platform.LaunchSpec {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]platform.LaunchSpec(nil), e.launched...)
}

func (e *Executor) ListManaged(context.Context) ([]model.ItemID, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []model.ItemID
	for _, s := range e.launched {
		if e.alive[s.ID] {
			out = append(out, s.ID)
		}
	}
	return out, nil
}

func (e *Executor) SetSlice(current, oomKills uint64) {
	e.mu.Lock()
	e.sliceCur, e.sliceOOM = current, oomKills
	e.mu.Unlock()
}

func (e *Executor) SliceCurrent() (uint64, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.sliceCur, nil
}

func (e *Executor) SliceOOMKills() (uint64, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.sliceOOM, nil
}

func (e *Executor) SetSliceCeiling(max, high uint64) error {
	e.mu.Lock()
	if e.ceilingErr != nil {
		defer e.mu.Unlock()
		return e.ceilingErr
	}
	e.actualMax = max
	e.ceilings = append(e.ceilings, [2]uint64{max, high})
	e.mu.Unlock()
	return nil
}

// SliceCeiling is the last applied max, or the externally set one; unlimited before any.
func (e *Executor) SliceCeiling() (uint64, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.ceilings) == 0 && e.actualMax == 0 {
		return math.MaxUint64, nil
	}
	return e.actualMax, nil
}

// ResetSliceCeiling simulates systemd resetting memory.max behind the guard's back.
func (e *Executor) ResetSliceCeiling(max uint64) {
	e.mu.Lock()
	e.actualMax = max
	e.mu.Unlock()
}

// FailSetCeiling makes SetSliceCeiling return err (nil clears it).
func (e *Executor) FailSetCeiling(err error) {
	e.mu.Lock()
	e.ceilingErr = err
	e.mu.Unlock()
}

func (e *Executor) Ceilings() [][2]uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([][2]uint64(nil), e.ceilings...)
}

func (e *Executor) EnsureSlice(_ context.Context, poolMemory uint64) error {
	e.mu.Lock()
	e.poolMemory = poolMemory
	e.mu.Unlock()
	return nil
}

func (e *Executor) EnsureTeardownSlice(_ context.Context, max uint64) error {
	e.mu.Lock()
	e.teardownMax = max
	e.mu.Unlock()
	return nil
}

// Ensured is the pool memory of the last EnsureSlice and the max of the last
// EnsureTeardownSlice (0 for one never called).
func (e *Executor) Ensured() (poolMemory, teardownMax uint64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.poolMemory, e.teardownMax
}

type Host struct {
	mu       sync.Mutex
	mem      platform.MemInfo
	pressure float64
	disk     uint64
	oom      uint64
	cpus     int
}

func NewHost() *Host { return &Host{cpus: 1} }

func (h *Host) Set(m platform.MemInfo) { h.mu.Lock(); h.mem = m; h.mu.Unlock() }
func (h *Host) SetPressure(v float64)  { h.mu.Lock(); h.pressure = v; h.mu.Unlock() }
func (h *Host) SetDisk(v uint64)       { h.mu.Lock(); h.disk = v; h.mu.Unlock() }
func (h *Host) SetOOMKills(v uint64)   { h.mu.Lock(); h.oom = v; h.mu.Unlock() }
func (h *Host) SetNumCPU(n int)        { h.mu.Lock(); h.cpus = n; h.mu.Unlock() }

func (h *Host) Memory() (platform.MemInfo, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.mem, nil
}

func (h *Host) PressureFullAvg10() (float64, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.pressure, nil
}

func (h *Host) DiskFree(string) (uint64, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.disk, nil
}

func (h *Host) KernelOOMKills() (uint64, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.oom, nil
}

func (h *Host) NumCPU() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cpus
}

// Meter is a StandMeter whose measurements the test sets directly.
type Meter struct {
	mu      sync.Mutex
	usage   map[string]uint64
	watched []string
}

func NewMeter() *Meter { return &Meter{usage: map[string]uint64{}} }

// Set makes project report a fresh measurement of bytes.
func (m *Meter) Set(project string, bytes uint64) {
	m.mu.Lock()
	m.usage[project] = bytes
	m.mu.Unlock()
}

// Unset makes project report no fresh measurement.
func (m *Meter) Unset(project string) {
	m.mu.Lock()
	delete(m.usage, project)
	m.mu.Unlock()
}

// Watched is the set from the last Watch.
func (m *Meter) Watched() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.watched...)
}

func (m *Meter) Watch(projects []string) {
	m.mu.Lock()
	m.watched = append([]string(nil), projects...)
	m.mu.Unlock()
}

func (m *Meter) Usage(project string) (uint64, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.usage[project]
	return v, ok
}

// Run measures nothing; it blocks until ctx ends.
func (m *Meter) Run(ctx context.Context) { <-ctx.Done() }

type UsageMeter struct {
	mu    sync.Mutex
	usage platform.Usage
	err   error
}

func (m *UsageMeter) Set(v platform.Usage) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.usage = v
	m.err = nil
}
func (m *UsageMeter) Fail(e error) { m.mu.Lock(); defer m.mu.Unlock(); m.err = e }
func (m *UsageMeter) Usage(ctx context.Context, agents []string) (platform.Usage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v := m.usage
	v.Agents = append([]platform.AgentUsage(nil), v.Agents...)
	return v, m.err
}
