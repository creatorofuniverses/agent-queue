package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/creatorofuniverses/agent-queue/internal/model"
	"github.com/creatorofuniverses/agent-queue/internal/notify"
	"github.com/creatorofuniverses/agent-queue/internal/scheduler"
	"github.com/creatorofuniverses/agent-queue/internal/store"
)

const (
	runtimeScheduler = "scheduler"
	runtimeLifecycle = "lifecycle"
)

// lifecycle is the last daemon's version and whether it stopped cleanly.
type lifecycle struct {
	Version   string    `json:"version"`
	StartedAt time.Time `json:"started_at"`
	StoppedAt time.Time `json:"stopped_at,omitzero"`
}

// startRuntime runs after Recover: it restores the scheduler state a clean
// shutdown saved and tells the operator about the restart. The first start
// ever is not a restart.
func startRuntime(ctx context.Context, st *store.Store, sched *scheduler.Scheduler, router *notify.Router, now time.Time) {
	var prev lifecycle
	b, found, err := st.LoadRuntime(ctx, runtimeLifecycle)
	if err == nil && found {
		err = json.Unmarshal(b, &prev)
	}
	if err != nil {
		slog.Warn("runtime: read lifecycle", "err", err)
	}
	restored := -1
	if snap, ok, err := st.LoadRuntime(ctx, runtimeScheduler); err != nil {
		slog.Warn("runtime: read scheduler state", "err", err)
	} else if ok {
		if restored, err = sched.RestoreRuntime(ctx, snap); err != nil {
			slog.Warn("runtime: restore scheduler state", "err", err)
		}
		// One use only: a later crash must not bring back stale samples.
		if err := st.DeleteRuntime(ctx, runtimeScheduler); err != nil {
			slog.Warn("runtime: drop scheduler state", "err", err)
		}
	}
	cur, _ := json.Marshal(lifecycle{Version: Version, StartedAt: now})
	if err := st.SaveRuntime(ctx, runtimeLifecycle, cur, now); err != nil {
		slog.Warn("runtime: save lifecycle", "err", err)
	}
	if !found {
		return
	}
	if err := st.AddEvents(ctx, []model.Event{restartEvent(ctx, st, router, prev, restored, now)}); err != nil {
		slog.Warn("runtime: record restart", "err", err)
	}
}

func restartEvent(ctx context.Context, st *store.Store, router *notify.Router, prev lifecycle, restored int, now time.Time) model.Event {
	tasks, held, queued := 0, 0, 0
	if items, err := st.ListActive(ctx); err == nil {
		for _, it := range items {
			switch {
			case it.State == model.StateQueued:
				queued++
			case it.IsReservation() && it.State.Held():
				held++
			case !it.IsReservation():
				tasks++
			}
		}
	}
	how := "after a clean stop"
	if prev.StoppedAt.IsZero() {
		how = "after an unclean stop (crash or kill); in-memory samples were lost"
	}
	version := Version
	if prev.Version != "" && prev.Version != Version {
		version = prev.Version + " → " + Version
	}
	detail := fmt.Sprintf("aq %s restarted %s: picked up %d running task(s), %d reservation(s), %d queued", version, how, tasks, held, queued)
	if restored > 0 {
		detail += fmt.Sprintf("; memory samples restored for %d task(s)", restored)
	}
	ch := router.Route("", model.AudienceOperator)
	status := model.EventPending
	if ch == model.ChannelNone {
		status = model.EventSkipped
	}
	return model.Event{ID: string(model.NewID()), Kind: model.EventDaemonRestarted, Audience: model.AudienceOperator,
		Channel: ch, Status: status, Detail: detail, CreatedAt: now}
}

// stopRuntime saves what startRuntime restores; it runs after a clean shutdown.
func stopRuntime(st *store.Store, sched *scheduler.Scheduler, started, now time.Time) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if snap, err := sched.SnapshotRuntime(); err != nil {
		slog.Warn("runtime: snapshot scheduler state", "err", err)
	} else if err := st.SaveRuntime(ctx, runtimeScheduler, snap, now); err != nil {
		slog.Warn("runtime: save scheduler state", "err", err)
	}
	b, _ := json.Marshal(lifecycle{Version: Version, StartedAt: started, StoppedAt: now})
	if err := st.SaveRuntime(ctx, runtimeLifecycle, b, now); err != nil {
		slog.Warn("runtime: save lifecycle", "err", err)
	}
}
