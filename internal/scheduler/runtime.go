package scheduler

import (
	"context"
	"encoding/json"

	"github.com/creatorofuniverses/agent-queue/internal/model"
)

// runtimeState is the scheduler state that otherwise lives only in memory: the
// samples of running tasks (their profile runs) and the guard history (OOM
// attribution). The daemon saves it on a clean shutdown and restores it on start.
type runtimeState struct {
	Samples map[model.ItemID]*runSamples `json:"samples"`
	Guard   []guardInterval              `json:"guard"`
}

// SnapshotRuntime returns the in-memory state for RestoreRuntime. An open guard
// interval is closed now: the next daemon opens its own if the guard is still on.
func (s *Scheduler) SnapshotRuntime() ([]byte, error) {
	s.mu.Lock()
	v := runtimeState{Samples: make(map[model.ItemID]*runSamples, len(s.samples))}
	for id, r := range s.samples {
		c := *r
		v.Samples[id] = &c
	}
	s.mu.Unlock()
	s.guardMu.Lock()
	now := s.d.Now()
	for _, g := range s.guardIntervals {
		if g.Until.IsZero() {
			g.Until = now
		}
		v.Guard = append(v.Guard, g)
	}
	s.guardMu.Unlock()
	return json.Marshal(v)
}

// RestoreRuntime loads a snapshot after Recover. Samples are kept only for tasks
// that are still running; it returns how many were restored.
func (s *Scheduler) RestoreRuntime(ctx context.Context, b []byte) (int, error) {
	var v runtimeState
	if err := json.Unmarshal(b, &v); err != nil {
		return 0, err
	}
	n := 0
	s.mu.Lock()
	for id, r := range v.Samples {
		it, err := s.d.Store.Get(ctx, id)
		if err != nil || it.State != model.StateRunning || r == nil {
			continue
		}
		if cur := s.samples[id]; cur != nil {
			r.Hist.Merge(cur.Hist)
			r.Samples += cur.Samples
		}
		s.samples[id] = r
		n++
	}
	s.mu.Unlock()
	s.guardMu.Lock()
	s.guardIntervals = append(v.Guard, s.guardIntervals...)
	s.pruneGuardLocked(s.d.Now())
	s.guardMu.Unlock()
	return n, nil
}
