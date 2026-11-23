package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/creatorofuniverses/agent-queue/internal/agentsess"
	"github.com/creatorofuniverses/agent-queue/internal/model"
	"github.com/creatorofuniverses/agent-queue/internal/platform"
	"github.com/creatorofuniverses/agent-queue/internal/scheduler"
	"github.com/creatorofuniverses/agent-queue/internal/tmpscan"
)

// DashboardVersion changes only when a field of DashboardView changes meaning or goes away.
const DashboardVersion = 1

const (
	dashboardWindow   = 24 * time.Hour
	dashboardRecent   = 20
	dashboardTmpItems = 50
	// numbersEvery bounds rewrites caused by figures alone; heartbeatEvery keeps "at" fresh.
	numbersEvery   = 10 * time.Second
	heartbeatEvery = 30 * time.Second
)

// DashboardView is what a panel draws: the queue, what each item uses now, and the last day.
// It is aq dashboard's output and the content of the runtime dir's dashboard.json.
type DashboardView struct {
	Version  int                   `json:"version"`
	At       time.Time             `json:"at"`
	Daemon   DashboardDaemon       `json:"daemon"`
	Host     DashboardHost         `json:"host"`
	Pool     scheduler.Commitments `json:"pool"`
	Queue    QueueView             `json:"queue"`
	Items    []ItemView            `json:"items"`  // non-terminal, in queue order
	Recent   []ItemView            `json:"recent"` // ended within the day, latest first
	Day      DayView               `json:"day"`
	Sessions *DashboardSessions    `json:"sessions,omitempty"` // nil before the first sample
	Tmp      *DashboardTmp         `json:"tmp,omitempty"`      // nil before the first scan
	Alerts   []AlertView           `json:"alerts"`             // [] when none
}
type DashboardSessions struct {
	AgentBytes uint64              `json:"agent_bytes"`
	RunBytes   uint64              `json:"run_bytes"`
	Items      []agentsess.Session `json:"items"`
}
type DashboardTmp struct {
	FS             platform.FSUsage `json:"fs"`
	ScannedAt      time.Time        `json:"scanned_at"`
	OrphanBytes    uint64           `json:"orphan_bytes"`
	CandidateBytes uint64           `json:"candidate_bytes"`
	Complete       bool             `json:"complete"`
	PartialItems   int              `json:"partial_items"` // whole scan, including items excluded below
	Items          []tmpscan.Item   `json:"items"`         // at most 50, largest first
}
type DashboardDaemon struct {
	Version string `json:"version"`
}
type DashboardHost struct {
	MemTotal       uint64  `json:"mem_total"`
	MemAvailable   uint64  `json:"mem_available"`
	PSIFullAvg10   float64 `json:"psi_full_avg10"`
	PressureClosed bool    `json:"pressure_closed"`
	// GuardActive: the guard's ceiling is below the pool. With a pool close to the host's
	// memory that holds almost always, so it does not mean aq is being cut; GuardCutting does.
	GuardActive  bool   `json:"guard_active"`
	GuardCeiling uint64 `json:"guard_ceiling"` // the slice ceiling the guard last wrote, 0 before its first write
	GuardCutting bool   `json:"guard_cutting"`
}

// guardCutting reports that the guard's ceiling is below what aq promised to the work inside its
// slice (tasks' expected memory and reservations' task budgets; stands and teardowns run outside),
// so a task can be killed before it reaches what it was admitted with.
func guardCutting(ceiling uint64, c scheduler.Commitments) bool {
	return ceiling > 0 && ceiling < c.TasksExpected+c.Budgets
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
	return d.dashboardWith(ctx, now, nil)
}

// dashboardWith builds the view, taking the day's part from c while it is fresh (c may be nil).
func (d *server) dashboardWith(ctx context.Context, now time.Time, c *dayCache) (DashboardView, error) {
	v := DashboardView{
		Version: DashboardVersion, At: now.UTC(), Daemon: DashboardDaemon{Version},
		Items: []ItemView{}, Recent: []ItemView{}, Alerts: []AlertView{},
	}
	if mem, err := d.host.Memory(); err == nil {
		v.Host.MemTotal, v.Host.MemAvailable = mem.Total, mem.Available
	}
	v.Host.PressureClosed, v.Host.PSIFullAvg10 = d.sched.Pressure()
	v.Host.GuardActive = d.sched.GuardActive()
	p := d.sched.LastPass()
	v.Pool = p.Commitments
	v.Host.GuardCeiling = d.sched.GuardCeiling()
	v.Host.GuardCutting = guardCutting(v.Host.GuardCeiling, v.Pool)
	v.Queue = QueueView{p.Queued, p.Running, p.Held, p.HeadID, p.Head.Blocker, p.Head.Detail}

	if d.lastSessions != nil {
		if sessions := d.lastSessions.Load(); sessions != nil {
			v.Sessions = &DashboardSessions{Items: *sessions}
			for _, session := range *sessions {
				v.Sessions.AgentBytes += session.AgentBytes
				v.Sessions.RunBytes += session.RunBytes
			}
		}
	}
	if d.lastTmp != nil {
		if r := d.lastTmp.Load(); r != nil {
			v.Tmp = &DashboardTmp{
				ScannedAt: r.At.UTC(), OrphanBytes: r.OrphanBytes, CandidateBytes: r.CandidateBytes,
				Complete: r.Complete, PartialItems: r.PartialItems,
				Items: r.Items[:min(len(r.Items), dashboardTmpItems)],
			}
			if fs, err := d.host.FSUsage(r.Root); err == nil {
				v.Tmp.FS = fs
			}
		}
	}
	if d.alerts != nil {
		v.Alerts = d.alerts.Active()
	}

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

	ids := make([]string, len(active))
	for i, it := range active {
		ids[i] = string(it.ID)
	}
	key := strings.Join(ids, ",")
	if c.fresh(key, now) {
		v.Recent, v.Day = c.recent, c.day
		return v, nil
	}
	if v.Recent, v.Day, err = d.dayPart(ctx, now); err != nil {
		return v, err
	}
	if c != nil {
		*c = dayCache{key: key, at: now, recent: v.Recent, day: v.Day}
	}
	return v, nil
}

// dayCache keeps the ended part of the dashboard between the loop's builds: it changes only when
// an item leaves the active set (or a submit is rejected) and as the window slides, while reading
// a busy day's items every second would cost the daemon a few percent of a core.
type dayCache struct {
	key    string
	at     time.Time
	recent []ItemView
	day    DayView
}

func (c *dayCache) fresh(key string, now time.Time) bool {
	return c != nil && !c.at.IsZero() && c.key == key && now.Sub(c.at) < heartbeatEvery
}

// dayPart is the items that ended within the window, the latest first, and the tasks' outcomes among them.
func (d *server) dayPart(ctx context.Context, now time.Time) ([]ItemView, DayView, error) {
	recent := []ItemView{}
	day := DayView{Since: now.Add(-dashboardWindow).UTC()}
	ended, err := d.st.EndedSince(ctx, day.Since, 0)
	if err != nil {
		return recent, day, err
	}
	var waits []float64
	for _, it := range ended {
		if len(recent) < dashboardRecent {
			recent = append(recent, d.view(it, now))
		}
		if it.IsReservation() {
			day.Reservations++
			continue
		}
		switch it.State {
		case model.StateSucceeded:
			day.Succeeded++
		case model.StateFailed:
			day.Failed++
			switch it.Reason {
			case model.ReasonOOM, model.ReasonOOMGroup, model.ReasonOOMHostGuard:
				day.OOM++
			case model.ReasonTimeout:
				day.Timeout++
			}
		case model.StateCancelled:
			day.Cancelled++
		default:
			continue // rejected: never ran
		}
		day.Finished++
		if !it.StartedAt.IsZero() {
			waits = append(waits, nonNeg(it.StartedAt.Sub(it.CreatedAt)))
			day.RunSeconds += nonNeg(it.EndedAt.Sub(it.StartedAt))
		}
	}
	if len(waits) > 0 {
		slices.Sort(waits)
		day.WaitMedianSeconds, day.WaitMaxSeconds = waits[len(waits)/2], waits[len(waits)-1]
	}
	return recent, day, nil
}

// dashWriter rewrites the dashboard file: at once when tasks change or alerts open or close,
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
// reasons to wait, the day's counts, the host's gates and the names of active alerts.
func dashShape(v DashboardView) []byte {
	type item struct {
		ID       model.ItemID
		State    model.State
		Reason   model.Reason
		Position int
		Blocked  string
	}
	s := struct {
		Items, Recent          []item
		Queue                  QueueView
		Day                    DayView
		Closed, Guard, Cutting bool
		Alerts                 []string
	}{Queue: v.Queue, Day: v.Day, Closed: v.Host.PressureClosed, Guard: v.Host.GuardActive, Cutting: v.Host.GuardCutting}
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
	for _, alert := range v.Alerts {
		s.Alerts = append(s.Alerts, alert.Name)
	}
	slices.Sort(s.Alerts)
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
	c := &dayCache{}
	defer os.Remove(path)
	t := time.NewTicker(time.Second)
	defer t.Stop()
	failing := false
	for {
		now := time.Now()
		v, err := d.dashboardWith(ctx, now, c)
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
