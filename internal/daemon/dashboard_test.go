package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/creatorofuniverses/agent-queue/internal/agentsess"
	"github.com/creatorofuniverses/agent-queue/internal/model"
	"github.com/creatorofuniverses/agent-queue/internal/platform"
	"github.com/creatorofuniverses/agent-queue/internal/scheduler"
	"github.com/creatorofuniverses/agent-queue/internal/store"
	"github.com/creatorofuniverses/agent-queue/internal/tmpscan"
)

func (e *env) end(t *testing.T, id model.ItemID, info platform.ExitInfo, want model.State) {
	t.Helper()
	e.ex.Finish(id, info)
	e.ex.SetAlive(id, false)
	e.waitState(t, id, want)
}

// The dashboard is the queue, what runs and what used how much, and the last day's outcomes.
func TestDashboardView(t *testing.T) {
	e := newEnv(t)
	e.cfg.PoolMemory = 4 * gib
	e.start(t)
	ok := e.mustSubmit(t, e.submitArgs(gib))
	oom := e.mustSubmit(t, e.submitArgs(gib))
	e.waitState(t, ok.ID, model.StateRunning)
	e.waitState(t, oom.ID, model.StateRunning)
	e.end(t, ok.ID, platform.ExitInfo{ServiceResult: "success", ExitCode: "exited", ExitStatus: "0"}, model.StateSucceeded)
	e.end(t, oom.ID, platform.ExitInfo{ServiceResult: "oom-kill"}, model.StateFailed)

	run := e.mustSubmit(t, e.submitArgs(3*gib))
	e.waitState(t, run.ID, model.StateRunning)
	queued := e.mustSubmit(t, e.submitArgs(2*gib))
	e.ex.SetCurrent(run.ID, 700*mib)

	var v DashboardView
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := e.call(t, "dashboard", nil, &v); err != nil {
			t.Fatal(err)
		}
		if len(v.Items) == 2 && v.Items[0].MemoryCurrent != nil && *v.Items[0].MemoryCurrent == 700*mib && v.Items[1].Position == 1 && v.Host.GuardCeiling != 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("items %+v", v.Items)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if v.Version != 1 || v.At.IsZero() || v.Pool.PoolMemory != 4*gib || v.Queue.Running != 1 || v.Queue.Queued != 1 || v.Host.MemTotal != 64*gib {
		t.Fatalf("%+v", v)
	}
	if v.Host.GuardCeiling != 4*gib || v.Host.GuardActive || v.Host.GuardCutting {
		t.Fatalf("guard: %+v", v.Host)
	}
	if v.Items[0].ID != run.ID || v.Items[1].ID != queued.ID || v.Items[1].MemoryCurrent != nil || v.Items[1].BlockedBy == "" {
		t.Fatalf("items %+v", v.Items)
	}
	if len(v.Recent) != 2 || v.Recent[0].ID != oom.ID || v.Recent[1].ID != ok.ID {
		t.Fatalf("recent %+v", v.Recent)
	}
	d := v.Day
	if d.Finished != 2 || d.Succeeded != 1 || d.Failed != 1 || d.OOM != 1 || d.Timeout != 0 || d.Cancelled != 0 || d.Since.IsZero() {
		t.Fatalf("day %+v", d)
	}

	// The same field reaches aq list and aq status.
	var list []ItemView
	if err := e.call(t, "list", listArgs{}, &list); err != nil {
		t.Fatal(err)
	}
	for _, it := range list {
		if it.ID == run.ID && (it.MemoryCurrent == nil || *it.MemoryCurrent != 700*mib) {
			t.Fatalf("list: %+v", it)
		}
	}
}

// The daemon keeps dashboard.json in its runtime dir and removes it when it stops.
func TestDashboardFile(t *testing.T) {
	e := newEnv(t)
	e.start(t)
	path := e.paths.Dashboard()
	deadline := time.Now().Add(5 * time.Second)
	for {
		b, err := os.ReadFile(path)
		var v DashboardView
		if err == nil && json.Unmarshal(b, &v) == nil && v.Version == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no dashboard file: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatal(st, err)
	}
	e.stop()
	<-e.done
	e.done <- nil // for the cleanup's wait
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dashboard left after stop: %v", err)
	}
}

func TestDashboardSessionsTmpAlerts(t *testing.T) {
	e := newEnv(t)
	st, err := store.Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	e.cfg.Sessions.AlertIdleCount = 3
	e.cfg.Alerts.Window = time.Minute
	d := &server{
		host:         e.host,
		st:           st,
		sched:        scheduler.New(scheduler.Deps{Store: st, Exec: e.ex, Host: e.host, Cfg: e.cfg, Paths: e.paths}),
		lastSessions: &atomic.Pointer[[]agentsess.Session]{},
		lastTmp:      &atomic.Pointer[tmpscan.Report]{},
		alerts:       newAlertSet(e.cfg),
	}
	now := time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC)
	cache := &dayCache{}
	snapshot := func() map[string]json.RawMessage {
		t.Helper()
		v, err := d.dashboardWith(t.Context(), now, cache)
		if err != nil {
			t.Fatal(err)
		}
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(b, &fields); err != nil {
			t.Fatal(err)
		}
		return fields
	}
	first := snapshot()
	if first["sessions"] != nil || first["tmp"] != nil || string(first["alerts"]) != "[]" {
		t.Errorf("before sampling: sessions=%s tmp=%s alerts=%s, want absent sessions and tmp and empty alerts", first["sessions"], first["tmp"], first["alerts"])
	}

	sessions := []agentsess.Session{
		{SessionUsage: platform.SessionUsage{AgentBytes: 8, RunBytes: 3}, Name: "first"},
		{SessionUsage: platform.SessionUsage{AgentBytes: 4, RunBytes: 2}, Name: "second"},
	}
	d.lastSessions.Store(&sessions)
	root := t.TempDir()
	r := tmpscan.Report{Root: root, At: now.Add(-time.Second), OrphanBytes: 73, CandidateBytes: 19}
	for i := range 60 {
		r.Items = append(r.Items, tmpscan.Item{Bytes: uint64(60 - i), State: "recent"})
	}
	d.lastTmp.Store(&r)
	fs := platform.FSUsage{Size: 100, Free: 40, Inodes: 32, InodesFree: 20, Tmpfs: true}
	e.host.SetFS(fs, nil)
	d.alerts.observeLevel(model.EventSessionsIdle, now.Add(-time.Minute), 4, true)
	d.alerts.observeLevel(model.EventSessionsIdle, now, 4, true)

	fields := snapshot()
	if string(fields["version"]) != "1" {
		t.Errorf("dashboard version = %s, want 1", fields["version"])
	}
	var gotSessions struct {
		AgentBytes uint64              `json:"agent_bytes"`
		RunBytes   uint64              `json:"run_bytes"`
		Items      []agentsess.Session `json:"items"`
	}
	if err := json.Unmarshal(fields["sessions"], &gotSessions); err != nil {
		t.Errorf("sessions field missing or invalid: %v", err)
	} else if gotSessions.AgentBytes != 12 || gotSessions.RunBytes != 5 ||
		len(gotSessions.Items) != 2 || gotSessions.Items[0].Name != "first" || gotSessions.Items[1].Name != "second" {
		t.Errorf("sessions = %+v, want both sessions and totals of 12 agent bytes and 5 run bytes", gotSessions)
	}
	var gotTmp struct {
		FS             platform.FSUsage `json:"fs"`
		ScannedAt      time.Time        `json:"scanned_at"`
		OrphanBytes    uint64           `json:"orphan_bytes"`
		CandidateBytes uint64           `json:"candidate_bytes"`
		Items          []tmpscan.Item   `json:"items"`
	}
	if err := json.Unmarshal(fields["tmp"], &gotTmp); err != nil {
		t.Errorf("tmp field missing or invalid: %v", err)
	} else {
		if gotTmp.FS != fs || !gotTmp.ScannedAt.Equal(r.At) || gotTmp.OrphanBytes != 73 || gotTmp.CandidateBytes != 19 {
			t.Errorf("tmp metadata = %s", fields["tmp"])
		}
		if len(gotTmp.Items) != 50 {
			t.Errorf("tmp.items length = %d, want 50", len(gotTmp.Items))
		}
		for i, item := range gotTmp.Items {
			if item.Bytes != uint64(60-i) || item.State != "recent" {
				t.Fatalf("tmp.items[%d] = %+v, want largest items first with their scan state", i, item)
			}
		}
	}
	var alerts []AlertView
	if err := json.Unmarshal(fields["alerts"], &alerts); err != nil {
		t.Errorf("alerts field missing or invalid: %v", err)
	} else if len(alerts) != 1 || alerts[0].Name != "sessions_idle" || !alerts[0].Since.Equal(now) ||
		alerts[0].Value != 4 || alerts[0].Threshold != 3 {
		t.Errorf("alerts = %+v, want the active sessions_idle episode", alerts)
	}

	e.host.SetFS(fs, errors.New("statfs failed"))
	if err := json.Unmarshal(snapshot()["tmp"], &gotTmp); err != nil || gotTmp.FS != (platform.FSUsage{}) {
		t.Errorf("tmp.fs = %+v after statfs failure, want zero values (decode error %v)", gotTmp.FS, err)
	}
}

func TestDashboardShapeChangesOnAlert(t *testing.T) {
	decode := func(data string) DashboardView {
		t.Helper()
		var v DashboardView
		if err := json.Unmarshal([]byte(data), &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	quiet := decode(`{"host":{"mem_available":100},"alerts":[]}`)
	open := decode(`{"host":{"mem_available":100},"alerts":[{"name":"tmp_full","value":80},{"name":"swap_full","value":90}]}`)
	updated := decode(`{"host":{"mem_available":90},"alerts":[{"name":"swap_full","value":91},{"name":"tmp_full","value":81}]}`)
	closed := decode(`{"host":{"mem_available":90},"alerts":[{"name":"swap_full","value":91}]}`)
	if bytes.Equal(dashShape(quiet), dashShape(open)) {
		t.Error("opening alerts did not change the dashboard shape")
	}
	if !bytes.Equal(dashShape(open), dashShape(updated)) {
		t.Error("changing memory, alert values, and alert order changed the dashboard shape")
	}
	if bytes.Equal(dashShape(updated), dashShape(closed)) {
		t.Error("closing an alert did not change the dashboard shape")
	}

	w := &dashWriter{path: filepath.Join(t.TempDir(), "dashboard.json")}
	now := time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC)
	for i, step := range []struct {
		view  DashboardView
		wrote bool
	}{{quiet, true}, {open, true}, {updated, false}, {closed, true}} {
		if wrote, err := w.offer(step.view, now.Add(time.Duration(i)*time.Second)); err != nil || wrote != step.wrote {
			t.Errorf("step %d: wrote %v, err %v, want wrote %v", i, wrote, err, step.wrote)
		}
	}
}

func dash(state model.State, mem uint64, avail uint64) DashboardView {
	cur := mem
	return DashboardView{Version: 1, Host: DashboardHost{MemAvailable: avail},
		Items: []ItemView{{ID: "a", State: state, MemoryCurrent: &cur}}}
}

// A change of what is in the queue is written at once; numbers alone at most every
// numbersEvery; an unchanged view still every heartbeat, so a stale "at" means the daemon is gone.
func TestDashboardWriterPolicy(t *testing.T) {
	dir := t.TempDir()
	w := &dashWriter{path: dir + "/dashboard.json"}
	t0 := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	steps := []struct {
		at    time.Duration
		v     DashboardView
		wrote bool
	}{
		{0, dash(model.StateRunning, gib, 10*gib), true},                   // first
		{time.Second, dash(model.StateRunning, gib, 10*gib), false},        // nothing changed
		{2 * time.Second, dash(model.StateRunning, 2*gib, 9*gib), false},   // numbers only, too soon
		{5 * time.Second, dash(model.StateSucceeded, 2*gib, 9*gib), true},  // state changed
		{8 * time.Second, dash(model.StateSucceeded, 3*gib, 9*gib), false}, // numbers, 3 s after a write
		{15 * time.Second, dash(model.StateSucceeded, 3*gib, 9*gib), true}, // numbers, 10 s after
		{40 * time.Second, dash(model.StateSucceeded, 3*gib, 9*gib), false},
		{45 * time.Second, dash(model.StateSucceeded, 3*gib, 9*gib), true}, // heartbeat
	}
	for i, s := range steps {
		now := t0.Add(s.at)
		s.v.At = now
		wrote, err := w.offer(s.v, now)
		if err != nil || wrote != s.wrote {
			t.Fatalf("step %d: wrote %v err %v, want %v", i, wrote, err, s.wrote)
		}
	}
	b, err := os.ReadFile(w.path)
	var v DashboardView
	if err != nil || json.Unmarshal(b, &v) != nil || !v.At.Equal(t0.Add(45*time.Second)) {
		t.Fatalf("%s %v", b, err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("temp files left: %v", entries)
	}
}

// The loop reuses the day's part while no item left the active set, for at most a heartbeat.
func TestDayCacheFresh(t *testing.T) {
	t0 := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	var none *dayCache
	c := &dayCache{}
	if none.fresh("a", t0) || c.fresh("", t0) {
		t.Fatal("an empty cache is never fresh")
	}
	c.key, c.at = "a,b", t0
	for _, tc := range []struct {
		key  string
		at   time.Duration
		want bool
	}{{"a,b", 5 * time.Second, true}, {"a", 5 * time.Second, false}, {"a,b,c", time.Second, false}, {"a,b", heartbeatEvery, false}} {
		if got := c.fresh(tc.key, t0.Add(tc.at)); got != tc.want {
			t.Fatalf("%+v: %v", tc, got)
		}
	}
}

// The guard is cutting aq only when its ceiling is below what aq promised to the work in its
// slice: tasks' expected memory and reservations' task budgets. Stands run outside the slice.
func TestGuardCutting(t *testing.T) {
	c := scheduler.Commitments{TasksExpected: 3 * gib, Budgets: 5 * gib, Stands: 10 * gib, PoolMemory: 27 * gib}
	for _, tc := range []struct {
		ceiling uint64
		want    bool
	}{
		{0, false},        // no ceiling written yet
		{27 * gib, false}, // the pool
		{20 * gib, false}, // below the pool, above the promises: armed, not cutting
		{8 * gib, false},  // exactly the promises
		{7 * gib, true},   // below them
		{256 * mib, true}, // the guard's floor
	} {
		if got := guardCutting(tc.ceiling, c); got != tc.want {
			t.Errorf("ceiling %d: cutting %v, want %v", tc.ceiling>>20, got, tc.want)
		}
	}
}
