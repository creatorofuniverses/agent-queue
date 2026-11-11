package daemon

import (
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/creatorofuniverses/agent-queue/internal/model"
	"github.com/creatorofuniverses/agent-queue/internal/platform"
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
		if len(v.Items) == 2 && v.Items[0].MemoryCurrent != nil && *v.Items[0].MemoryCurrent == 700*mib && v.Items[1].Position == 1 {
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
