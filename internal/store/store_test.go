package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"resource-dispatcher/internal/model"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "aq.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func newItem(prio model.Priority, created time.Time) *model.Item {
	return &model.Item{
		ID: model.NewID(), Kind: model.KindTask, State: model.StateQueued,
		Priority: prio, Argv: []string{"echo", "hi"}, NodeID: "local",
		CreatedAt: created,
	}
}

func TestInsertGetRoundTrip(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	base := time.Date(2026, 1, 2, 3, 4, 5, 123456789, time.UTC)
	code := 3
	full := &model.Item{
		ID: model.NewID(), Kind: model.KindTask, Name: "build", State: model.StateFailed,
		Reason: model.ReasonCommandFailed, Priority: model.PriorityHigh,
		Argv: []string{"make", "-j", "4"}, Cwd: "/src", Env: map[string]string{"A": "1", "B": "two"},
		Limit: 4 << 30, Expected: 2 << 30, CPUs: 4, Timeout: 90 * time.Second,
		SessionID: "sess", ClientType: "claude", Project: "proj", IdemKey: "k1", RequestHash: "h1", NodeID: "local",
		CreatedAt: base, AdmittedAt: base.Add(time.Second), StartedAt: base.Add(2 * time.Second), EndedAt: base.Add(3 * time.Second),
		ExitCode: &code, Signal: "SIGKILL", MemoryPeak: 12345,
		UnitName: "aq-x.service", JobDir: "/jobs/x", GitRev: "abc", GitDirty: true,
		CancelRequested: true, CancelReason: "user", LogsTruncated: true, Diagnostic: "diag",
	}
	got, created, err := s.InsertItem(ctx, full)
	if err != nil || !created {
		t.Fatalf("insert: created=%v err=%v", created, err)
	}
	if !reflect.DeepEqual(got, full) {
		t.Fatalf("insert returned %+v want %+v", got, full)
	}
	got, err = s.Get(ctx, full.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, full) {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", got, full)
	}

	// Minimal item: nil ExitCode, nil Env, zero times.
	min := newItem(model.PriorityNormal, base)
	if _, _, err := s.InsertItem(ctx, min); err != nil {
		t.Fatal(err)
	}
	got, err = s.Get(ctx, min.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ExitCode != nil || len(got.Env) != 0 || !got.StartedAt.IsZero() || !got.EndedAt.IsZero() {
		t.Fatalf("minimal item mismatch: %+v", got)
	}

	if _, err := s.Get(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestUpdate(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	it := newItem(model.PriorityNormal, time.Now().UTC())
	if _, _, err := s.InsertItem(ctx, it); err != nil {
		t.Fatal(err)
	}
	code := 0
	it.State = model.StateSucceeded
	it.ExitCode = &code
	if err := s.Update(ctx, it); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(ctx, it.ID)
	if got.State != model.StateSucceeded || got.ExitCode == nil || *got.ExitCode != 0 {
		t.Fatalf("update not applied: %+v", got)
	}
	if err := s.Update(ctx, &model.Item{ID: "missing"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func idemItem(hash string) *model.Item {
	it := newItem(model.PriorityNormal, time.Now().UTC())
	it.ClientType, it.Project, it.IdemKey, it.RequestHash = "claude", "p", "key", hash
	return it
}

func TestIdempotentSameHash(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	a, created, err := s.InsertItem(ctx, idemItem("h"))
	if err != nil || !created {
		t.Fatalf("first: %v %v", created, err)
	}
	b, created, err := s.InsertItem(ctx, idemItem("h"))
	if err != nil || created {
		t.Fatalf("second: created=%v err=%v", created, err)
	}
	if a.ID != b.ID {
		t.Fatalf("ids differ: %s %s", a.ID, b.ID)
	}
}

func TestIdempotentConflict(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	if _, _, err := s.InsertItem(ctx, idemItem("h1")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.InsertItem(ctx, idemItem("h2")); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("want ErrIdempotencyConflict, got %v", err)
	}
}

func TestIdempotent100Concurrent(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	const n = 100
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		created int
		ids     = map[model.ItemID]bool{}
		errs    []error
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, c, err := s.InsertItem(ctx, idemItem("h"))
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			if c {
				created++
			}
			ids[got.ID] = true
		}()
	}
	wg.Wait()
	if len(errs) > 0 {
		t.Fatalf("errors: %v", errs[0])
	}
	if created != 1 || len(ids) != 1 {
		t.Fatalf("created=%d distinct ids=%d, want 1/1", created, len(ids))
	}
}

func TestListActiveOrder(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	n1 := newItem(model.PriorityNormal, t0)
	n2 := newItem(model.PriorityNormal, t0.Add(time.Second))
	h := newItem(model.PriorityHigh, t0.Add(2*time.Second))
	done := newItem(model.PriorityHigh, t0.Add(3*time.Second))
	done.State = model.StateSucceeded
	for _, it := range []*model.Item{n2, h, done, n1} {
		if _, _, err := s.InsertItem(ctx, it); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.ListActive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []model.ItemID{h.ID, n1.ID, n2.ID}
	if len(got) != len(want) {
		t.Fatalf("got %d items want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].ID != want[i] {
			t.Fatalf("pos %d: got %s want %s", i, got[i].ID, want[i])
		}
	}
}

func TestList(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	a := newItem(0, t0)
	a.SessionID = "s1"
	b := newItem(0, t0.Add(time.Second))
	b.SessionID = "s1"
	b.State = model.StateFailed
	c := newItem(0, t0.Add(2*time.Second))
	c.SessionID = "s2"
	for _, it := range []*model.Item{a, b, c} {
		if _, _, err := s.InsertItem(ctx, it); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := s.List(ctx, ListFilter{SessionID: "s1"})
	if len(got) != 1 || got[0].ID != a.ID {
		t.Fatalf("active s1: %+v", got)
	}
	got, _ = s.List(ctx, ListFilter{SessionID: "s1", IncludeTerminal: true})
	if len(got) != 2 || got[0].ID != b.ID {
		t.Fatalf("all s1 (created desc): %+v", got)
	}
	got, _ = s.List(ctx, ListFilter{IncludeTerminal: true, Limit: 2})
	if len(got) != 2 || got[0].ID != c.ID {
		t.Fatalf("limit: %+v", got)
	}
}

func TestMigrationsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aq.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	it := newItem(0, time.Now().UTC())
	if _, _, err := s.InsertItem(context.Background(), it); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	defer s.Close()
	if _, err := s.Get(context.Background(), it.ID); err != nil {
		t.Fatalf("data lost: %v", err)
	}
}

func TestIncidents(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	old := model.Incident{At: t0, Kind: "kernel_oom", Value: 1, Detail: "old"}
	nw := model.Incident{At: t0.Add(time.Hour), Kind: "psi_close", Value: 5.5, Detail: "new"}
	for _, inc := range []model.Incident{nw, old} {
		if err := s.AddIncident(ctx, inc); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Incidents(ctx, t0.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !reflect.DeepEqual(got[0], nw) {
		t.Fatalf("got %+v", got)
	}
	got, _ = s.Incidents(ctx, t0)
	if len(got) != 2 || got[0].Detail != "old" {
		t.Fatalf("all: %+v", got)
	}
}

func TestOpenKeepsDBPrivate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "aq.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.InsertItem(context.Background(), newItem(0, time.Now())); err != nil {
		t.Fatal(err)
	}
	s.Close()
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if st, err := os.Stat(p); err == nil && st.Mode().Perm() != 0o600 {
			t.Errorf("%s mode %v, want 0600", filepath.Base(p), st.Mode().Perm())
		}
	}

	// an existing database left readable by others is tightened
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Fatalf("existing db mode %v, want 0600", st.Mode().Perm())
	}
}

func TestReservationRoundTrip(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	it := &model.Item{
		ID: model.NewID(), Kind: model.KindReservation, State: model.StateGranted, NodeID: "local",
		Limit: 1 << 30, Expected: 1 << 30, CPUs: 1, Timeout: time.Hour, Cwd: "/src", CreatedAt: base,
		ParentID: "parent", TaskMemory: 3 << 30, TaskCPUs: 4,
		ExpiresAt: base.Add(time.Hour), WarnAt: base.Add(45 * time.Minute),
		DockerProject: "stand", TeardownAttempt: 2, TeardownLaunchError: "boom",
		NotifyGrant: true, TTLHeld: true, OverDeclaredNotified: true,
	}
	got, _, err := s.InsertItem(ctx, it)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, it) {
		t.Fatalf("insert returned %+v want %+v", got, it)
	}
	it.ParentID, it.TaskMemory, it.TaskCPUs = "", 5<<30, 6
	it.ExpiresAt, it.WarnAt = base.Add(2*time.Hour), time.Time{}
	it.DockerProject, it.TeardownAttempt, it.TeardownLaunchError = "other", 3, ""
	it.NotifyGrant, it.TTLHeld, it.OverDeclaredNotified = false, false, false
	if err := s.Update(ctx, it); err != nil {
		t.Fatal(err)
	}
	got, err = s.Get(ctx, it.ID)
	if err != nil || !reflect.DeepEqual(got, it) {
		t.Fatalf("after update %+v err=%v want %+v", got, err, it)
	}
}

func TestUpdateWithEventsAtomic(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	it := newItem(0, base)
	it.Kind, it.State = model.KindReservation, model.StateGranted
	if _, _, err := s.InsertItem(ctx, it); err != nil {
		t.Fatal(err)
	}
	ev := func(id string, kind model.EventKind, aud model.Audience, at time.Duration) model.Event {
		return model.Event{ID: id, ItemID: it.ID, Kind: kind, Audience: aud,
			SessionID: "sess", ClientType: "claude", Detail: "d-" + id, CreatedAt: base.Add(at)}
	}
	it.State = model.StateExpiring
	bad := []model.Event{ev("e1", model.EventReservationExpiring, model.AudienceSession, 0),
		ev("e1", model.EventReservationExpiring, model.AudienceOperator, 0)}
	if err := s.UpdateWithEvents(ctx, it, bad); err == nil {
		t.Fatal("duplicate event id must fail")
	}
	got, _ := s.Get(ctx, it.ID)
	if got.State != model.StateGranted {
		t.Fatalf("item updated despite failed event insert: %s", got.State)
	}
	if evs, _ := s.Events(ctx, it.ID); len(evs) != 0 {
		t.Fatalf("events leaked: %+v", evs)
	}
	good := []model.Event{
		ev("e3", model.EventTeardownFailed, model.AudienceOperator, 2*time.Second),
		ev("e2", model.EventReservationExpiring, model.AudienceSession, time.Second),
	}
	if err := s.UpdateWithEvents(ctx, it, good); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Get(ctx, it.ID)
	if got.State != model.StateExpiring {
		t.Fatalf("state %s", got.State)
	}
	evs, err := s.Events(ctx, it.ID)
	if err != nil || len(evs) != 2 || !reflect.DeepEqual(evs[0], good[1]) || !reflect.DeepEqual(evs[1], good[0]) {
		t.Fatalf("events %+v err=%v", evs, err)
	}
	// Empty evs behaves like Update.
	it.State = model.StateReleased
	if err := s.UpdateWithEvents(ctx, it, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.Get(ctx, it.ID); got.State != model.StateReleased {
		t.Fatalf("state %s", got.State)
	}
	if err := s.UpdateWithEvents(ctx, &model.Item{ID: "missing"}, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestMigration0002OnExistingDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aq.db")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	m1, err := migrationsFS.ReadFile("migrations/0001_init.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(string(m1)); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`PRAGMA user_version = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO items (id, kind, state, created_at) VALUES ('old','task','QUEUED','2026-01-02T03:04:05.000000000Z')`); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.Get(context.Background(), "old")
	if err != nil {
		t.Fatal(err)
	}
	if got.ParentID != "" || got.TaskMemory != 0 || got.TaskCPUs != 0 || !got.ExpiresAt.IsZero() || !got.WarnAt.IsZero() ||
		got.DockerProject != "" || got.TeardownAttempt != 0 || got.TeardownLaunchError != "" ||
		got.NotifyGrant || got.TTLHeld || got.OverDeclaredNotified {
		t.Fatalf("new fields not zero: %+v", got)
	}
}

func TestListActiveExcludesNewTerminal(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	base := time.Now().UTC()
	ids := map[model.State]model.ItemID{}
	for i, st := range []model.State{model.StateTornDown, model.StateReleased, model.StateTeardownFailed, model.StateGranted} {
		it := newItem(0, base.Add(time.Duration(i)*time.Second))
		it.State = st
		ids[st] = it.ID
		if _, _, err := s.InsertItem(ctx, it); err != nil {
			t.Fatal(err)
		}
	}
	act, err := s.ListActive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	in := map[model.ItemID]bool{}
	for _, it := range act {
		in[it.ID] = true
	}
	if in[ids[model.StateTornDown]] || in[ids[model.StateReleased]] || !in[ids[model.StateTeardownFailed]] || !in[ids[model.StateGranted]] {
		t.Fatalf("active set wrong: %v", in)
	}
}
