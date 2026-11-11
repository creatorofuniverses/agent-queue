package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/creatorofuniverses/agent-queue/internal/model"
	"github.com/creatorofuniverses/agent-queue/internal/scheduler"
)

// DashboardVersion changes only when a field of DashboardView changes meaning or goes away.
const DashboardVersion = 1

const (
	dashboardWindow = 24 * time.Hour
	dashboardRecent = 20
	// numbersEvery bounds rewrites caused by memory figures alone; heartbeatEvery keeps "at" fresh.
	numbersEvery   = 10 * time.Second
	heartbeatEvery = 30 * time.Second
)

// DashboardView is what a panel draws: the queue, what each item uses now, and the last day.
// It is aq dashboard's output and the content of the runtime dir's dashboard.json.
type DashboardView struct {
	Version int                   `json:"version"`
	At      time.Time             `json:"at"`
	Daemon  DashboardDaemon       `json:"daemon"`
	Host    DashboardHost         `json:"host"`
	Pool    scheduler.Commitments `json:"pool"`
	Queue   QueueView             `json:"queue"`
	Items   []ItemView            `json:"items"`  // non-terminal, in queue order
	Recent  []ItemView            `json:"recent"` // ended within the day, latest first
	Day     DayView               `json:"day"`
}
type DashboardDaemon struct {
	Version string `json:"version"`
}
type DashboardHost struct {
	MemTotal       uint64  `json:"mem_total"`
	MemAvailable   uint64  `json:"mem_available"`
	PSIFullAvg10   float64 `json:"psi_full_avg10"`
	PressureClosed bool    `json:"pressure_closed"`
	GuardActive    bool    `json:"guard_active"`
}

// DayView counts the tasks (not reservations) that ended since Since. Failed includes OOM and Timeout.
type DayView struct {
	Since             time.Time `json:"since"`
	Finished          int       `json:"finished"`
	Succeeded         int       `json:"succeeded"`
	Failed            int       `json:"failed"`
	Cancelled         int       `json:"cancelled"`
	OOM               int       `json:"oom"`
	Timeout           int       `json:"timeout"`
	WaitMedianSeconds float64   `json:"wait_median_seconds"`
	WaitMaxSeconds    float64   `json:"wait_max_seconds"`
	RunSeconds        float64   `json:"run_seconds"`
	Reservations      int       `json:"reservations"` // ended in the same window
}

func (d *server) dashboard(ctx context.Context, now time.Time) (DashboardView, error) {
	v := DashboardView{Version: DashboardVersion, At: now.UTC(), Daemon: DashboardDaemon{Version}, Items: []ItemView{}, Recent: []ItemView{}}
	if mem, err := d.host.Memory(); err == nil {
		v.Host.MemTotal, v.Host.MemAvailable = mem.Total, mem.Available
	}
	v.Host.PressureClosed, v.Host.PSIFullAvg10 = d.sched.Pressure()
	v.Host.GuardActive = d.sched.GuardActive()
	p := d.sched.LastPass()
	v.Pool = p.Commitments
	v.Queue = QueueView{p.Queued, p.Running, p.Held, p.HeadID, p.Head.Blocker, p.Head.Detail}

	active, err := d.st.ListActive(ctx)
	if err != nil {
		return v, err
	}
	pos := map[model.ItemID]int{}
	for _, it := range active {
		if it.State == model.StateQueued && it.ParentID == "" {
			pos[it.ID] = len(pos) + 1
		}
	}
	for _, it := range active {
		iv := d.view(it, now)
		if it.State == model.StateQueued {
			d.markQueued(&iv, it, pos)
		}
		v.Items = append(v.Items, iv)
	}

	v.Day.Since = now.Add(-dashboardWindow).UTC()
	ended, err := d.st.EndedSince(ctx, v.Day.Since, 0)
	if err != nil {
		return v, err
	}
	var waits []float64
	for _, it := range ended {
		if len(v.Recent) < dashboardRecent {
			v.Recent = append(v.Recent, d.view(it, now))
		}
		if it.IsReservation() {
			v.Day.Reservations++
			continue
		}
		switch it.State {
		case model.StateSucceeded:
			v.Day.Succeeded++
		case model.StateFailed:
			v.Day.Failed++
			switch it.Reason {
			case model.ReasonOOM, model.ReasonOOMGroup, model.ReasonOOMHostGuard:
				v.Day.OOM++
			case model.ReasonTimeout:
				v.Day.Timeout++
			}
		case model.StateCancelled:
			v.Day.Cancelled++
		default:
			continue // rejected: never ran
		}
		v.Day.Finished++
		if !it.StartedAt.IsZero() {
			waits = append(waits, nonNeg(it.StartedAt.Sub(it.CreatedAt)))
			v.Day.RunSeconds += nonNeg(it.EndedAt.Sub(it.StartedAt))
		}
	}
	if len(waits) > 0 {
		slices.Sort(waits)
		v.Day.WaitMedianSeconds, v.Day.WaitMaxSeconds = waits[len(waits)/2], waits[len(waits)-1]
	}
	return v, nil
}

// dashWriter rewrites the dashboard file: at once when what is queued, running or ended changes,
// at most every numbersEvery when only figures move, and every heartbeatEvery regardless.
type dashWriter struct {
	path        string
	last        time.Time
	shape, body []byte
}

func (w *dashWriter) offer(v DashboardView, now time.Time) (bool, error) {
	shape, body := dashShape(v), dashBody(v)
	since := now.Sub(w.last)
	switch {
	case w.last.IsZero(), !bytes.Equal(shape, w.shape):
	case since >= heartbeatEvery:
	case since >= numbersEvery && !bytes.Equal(body, w.body):
	default:
		return false, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return false, err
	}
	if err := writeAtomic(w.path, b); err != nil {
		return false, err
	}
	w.last, w.shape, w.body = now, shape, body
	return true, nil
}

// dashShape is what a panel must show without delay: items and their states, places and
// reasons to wait, the day's counts and the host's gates. Memory figures, forecasts and blocker
// details (they quote live memory) are left out.
func dashShape(v DashboardView) []byte {
	type item struct {
		ID       model.ItemID
		State    model.State
		Reason   model.Reason
		Position int
		Blocked  string
	}
	s := struct {
		Items, Recent []item
		Queue         QueueView
		Day           DayView
		Closed, Guard bool
	}{Queue: v.Queue, Day: v.Day, Closed: v.Host.PressureClosed, Guard: v.Host.GuardActive}
	s.Queue.BlockedDetail = ""
	s.Day.Since, s.Day.WaitMedianSeconds, s.Day.WaitMaxSeconds, s.Day.RunSeconds = time.Time{}, 0, 0, 0
	for _, list := range []struct {
		in  []ItemView
		out *[]item
	}{{v.Items, &s.Items}, {v.Recent, &s.Recent}} {
		for _, it := range list.in {
			*list.out = append(*list.out, item{it.ID, it.State, it.Reason, it.Position, string(it.BlockedBy)})
		}
	}
	b, _ := json.Marshal(s)
	return b
}

// dashBody is the whole view without its timestamp and the run and wait times that grow every second.
func dashBody(v DashboardView) []byte {
	v.At = time.Time{}
	strip := func(in []ItemView) []ItemView {
		out := slices.Clone(in)
		for i := range out {
			out[i].WaitSeconds, out[i].RunSeconds = 0, 0
		}
		return out
	}
	v.Items, v.Recent = strip(v.Items), strip(v.Recent)
	v.Day.Since = time.Time{}
	b, _ := json.Marshal(v)
	return b
}

// writeAtomic replaces path with b through a temp file in the same dir, so a reader never sees half.
func writeAtomic(path string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".dashboard-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(b)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp, 0o600)
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

// runDashboard offers a fresh view to the writer every second until ctx ends, then removes the file:
// a missing file means no daemon, a stale "at" one that died.
func runDashboard(ctx context.Context, d *server, path string) {
	w := &dashWriter{path: path}
	defer os.Remove(path)
	t := time.NewTicker(time.Second)
	defer t.Stop()
	failing := false
	for {
		now := time.Now()
		v, err := d.dashboard(ctx, now)
		if err == nil {
			_, err = w.offer(v, now)
		}
		if err != nil && ctx.Err() == nil && !failing {
			slog.Warn("dashboard: write", "path", path, "err", err)
		}
		failing = err != nil
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
