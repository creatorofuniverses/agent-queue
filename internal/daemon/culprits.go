package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/creatorofuniverses/agent-queue/internal/model"
	"github.com/creatorofuniverses/agent-queue/internal/platform"
	"github.com/creatorofuniverses/agent-queue/internal/store"
	"github.com/creatorofuniverses/agent-queue/internal/units"
)

// culpritEvery bounds how often a guard engagement is snapshotted: an episode
// that flaps should not walk /proc every second.
const culpritEvery = 5 * time.Minute

// culpritRecorder records who held the host's memory when the host guard began
// cutting the aq slice, as a guard_engaged incident: the shortage that kills aq
// tasks usually comes from outside aq and is gone a minute later.
type culpritRecorder struct {
	st       *store.Store
	snapshot func() (platform.MemorySnapshot, error)
	mu       sync.Mutex
	last     time.Time
}

// engaged snapshots in the background, so the guard's protective write is not delayed.
func (r *culpritRecorder) engaged(ctx context.Context) {
	if now := time.Now().UTC(); r.due(now) {
		go r.record(ctx, now)
	}
}

func (r *culpritRecorder) due(now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.last.IsZero() && now.Sub(r.last) < culpritEvery {
		return false
	}
	r.last = now
	return true
}

func (r *culpritRecorder) record(ctx context.Context, at time.Time) {
	v, err := r.snapshot()
	if err != nil {
		slog.Warn("host guard: snapshot memory", "err", err)
		return
	}
	if err := r.st.AddIncident(ctx, model.Incident{At: at, Kind: "guard_engaged", Value: float64(v.Available), Detail: culpritText(v)}); err != nil {
		slog.Warn("host guard: record snapshot", "err", err)
	}
}

// culpritText names cgroups and processes by their last path element: the unit
// or scope says who started them, the full path only that it was a user's.
func culpritText(v platform.MemorySnapshot) string {
	parts := []string{"available " + units.FormatSize(v.Available), "in no cgroup (kernel, drivers, zram) " + units.FormatSize(v.Uncharged)}
	var cgs []string
	for _, c := range v.Cgroups {
		cgs = append(cgs, path.Base(c.Path)+" "+units.FormatSize(c.Bytes))
	}
	if len(cgs) > 0 {
		parts = append(parts, "cgroups: "+strings.Join(cgs, ", "))
	}
	var ps []string
	for _, p := range v.Processes {
		ps = append(ps, fmt.Sprintf("%s %s (pid %d, %s)", p.Name, units.FormatSize(p.RSS), p.PID, path.Base(p.Cgroup)))
	}
	if len(ps) > 0 {
		parts = append(parts, "processes: "+strings.Join(ps, ", "))
	}
	return strings.Join(parts, "; ")
}
