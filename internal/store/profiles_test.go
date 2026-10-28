package store

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/creatorofuniverses/agent-queue/internal/model"
	"github.com/creatorofuniverses/agent-queue/internal/profile"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestItemPlan4FieldsRoundTrip(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	it := newItem(0, time.Now().UTC())
	it.Profile = "p"
	it.EstDuration = time.Minute
	it.MemoryFromProfile = true
	it.ExtendCount = 2
	it.LogsPruned = true
	it.TeardownStartedAt = time.Now().UTC()
	it.ExpiredAt = it.TeardownStartedAt
	got, _, e := s.InsertItem(ctx, it)
	if e != nil || !reflect.DeepEqual(got, it) {
		t.Fatal(got, e)
	}
	got.ExtendCount++
	if e = s.Update(ctx, got); e != nil {
		t.Fatal(e)
	}
	again, e := s.Get(ctx, it.ID)
	if e != nil || !reflect.DeepEqual(again, got) {
		t.Fatal(again, e)
	}
}
func TestMigration0004OnPlan3DB(t *testing.T) {
	p := filepath.Join(t.TempDir(), "old.db")
	db, e := sql.Open("sqlite", p)
	if e != nil {
		t.Fatal(e)
	}
	names := []string{"0001_init.sql", "0002_reservations.sql", "0003_notifications.sql"}
	for i, n := range names {
		b, e := migrationsFS.ReadFile("migrations/" + n)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = db.Exec(string(b)); e != nil {
			t.Fatal(e)
		}
		_, e = db.Exec(fmt.Sprintf("PRAGMA user_version=%d", i+1))
		if e != nil {
			t.Fatal(e)
		}
	}
	_, e = db.Exec("INSERT INTO items(id,kind,state) VALUES('old','task','QUEUED')")
	if e != nil {
		t.Fatal(e)
	}
	db.Close()
	s, e := Open(p)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	it, e := s.Get(context.Background(), "old")
	if e != nil || it.Profile != "" || it.EstDuration != 0 || it.LogsPruned {
		t.Fatal(it, e)
	}
	for _, n := range []string{"profile_runs", "host_samples"} {
		if _, e = s.db.Exec("SELECT * FROM " + n); e != nil {
			t.Fatal(e)
		}
	}
}
func saveRun(t *testing.T, s *Store, p string, st model.State, reason model.Reason, peak uint64, at time.Time) profile.Run {
	t.Helper()
	ctx := context.Background()
	it := newItem(0, at.Add(-time.Minute))
	it.State = st
	it.Reason = reason
	it.EndedAt = at
	it.Profile = p
	var h profile.Histogram
	h.Add(peak)
	r := profile.Run{ItemID: it.ID, Profile: p, EndedAt: at, State: st, Reason: reason, MemoryPeak: peak, Duration: time.Minute, Hist: h, Samples: 1, CPUs: 1, Limit: 256 << 20, Expected: 128 << 20}
	if _, _, e := s.InsertItem(ctx, it); e != nil {
		t.Fatal(e)
	}
	if e := s.UpdateFinal(ctx, it, nil, &r); e != nil {
		t.Fatal(e)
	}
	return r
}
func TestUpdateFinalWritesRunOnce(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	r := saveRun(t, s, "p", model.StateSucceeded, "", 100, time.Now())
	it, _ := s.Get(ctx, r.ItemID)
	if e := s.UpdateFinal(ctx, it, nil, &r); e != nil {
		t.Fatal(e)
	}
	rs, e := s.ProfileRuns(ctx, "p", 20)
	if e != nil || len(rs) != 1 {
		t.Fatal(rs, e)
	}
	r.ItemID = "missing"
	it.ID = r.ItemID
	if e = s.UpdateFinal(ctx, it, nil, &r); e != ErrNotFound {
		t.Fatal(e)
	}
	rs, _ = s.ProfileRuns(ctx, "p", 20)
	if len(rs) != 1 {
		t.Fatal(rs)
	}
}
func TestUsableRunsFilter(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	now := time.Now().UTC()
	for i, tc := range []struct {
		s model.State
		r model.Reason
		p uint64
	}{{model.StateSucceeded, "", 100}, {model.StateFailed, model.ReasonCommandFailed, 200}, {model.StateFailed, model.ReasonOOM, 300}, {model.StateCancelled, "", 400}, {model.StateSucceeded, "", 0}} {
		saveRun(t, s, "p", tc.s, tc.r, tc.p, now.Add(time.Duration(i)*time.Second))
	}
	rs, e := s.UsableRuns(ctx, "p", 10)
	if e != nil || len(rs) != 2 || rs[0].MemoryPeak != 200 || rs[0].Hist.Count() != 1 {
		t.Fatal(rs, e)
	}
	rs, e = s.UsableRuns(ctx, "p", 1)
	if e != nil || len(rs) != 1 {
		t.Fatal(rs, e)
	}
}
func TestProfilesList(t *testing.T) {
	s := openTemp(t)
	now := time.Now().UTC()
	saveRun(t, s, "b", model.StateSucceeded, "", 100, now)
	saveRun(t, s, "a", model.StateFailed, model.ReasonOOM, 100, now)
	saveRun(t, s, "a", model.StateSucceeded, "", 100, now.Add(time.Second))
	ps, e := s.Profiles(context.Background())
	if e != nil || len(ps) != 2 || ps[0].Name != "a" || ps[0].Runs != 2 || ps[0].Usable != 1 || !ps[0].LastEnded.Equal(now.Add(time.Second)) {
		t.Fatal(ps, e)
	}
}

func TestMigration0006OnPlan5DB(t *testing.T) {
	p := filepath.Join(t.TempDir(), "old.db")
	db, e := sql.Open("sqlite", p)
	if e != nil {
		t.Fatal(e)
	}
	names := []string{"0001_init.sql", "0002_reservations.sql", "0003_notifications.sql", "0004_profiles_stats.sql", "0005_runtime_state.sql"}
	for i, n := range names {
		b, e := migrationsFS.ReadFile("migrations/" + n)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = db.Exec(string(b)); e != nil {
			t.Fatal(e)
		}
		if _, e = db.Exec(fmt.Sprintf("PRAGMA user_version=%d", i+1)); e != nil {
			t.Fatal(e)
		}
	}
	if _, e = db.Exec("INSERT INTO items(id,kind,state) VALUES('old','task','SUCCEEDED')"); e != nil {
		t.Fatal(e)
	}
	if _, e = db.Exec("INSERT INTO profile_runs(item_id,profile,ended_at,state,cpus,mem_limit,mem_expected,memory_from_profile,est_duration_ns,duration_ns,memory_peak,samples) VALUES('old','p','2026-10-01T00:00:00Z','SUCCEEDED',1,1,1,0,0,1,1,0)"); e != nil {
		t.Fatal(e)
	}
	db.Close()
	s, e := Open(p)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	ctx := context.Background()
	it, e := s.Get(ctx, "old")
	if e != nil || it.IOWriteBytes != nil || it.IOReadBytes != nil {
		t.Fatal(it, e)
	}
	rs, e := s.ProfileRuns(ctx, "p", 5)
	if e != nil || len(rs) != 1 || rs[0].IOWriteBytes != nil {
		t.Fatal(rs, e)
	}
}
func TestRunKeepsDiskWrites(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	it := newItem(0, time.Now().Add(-time.Minute))
	it.State, it.EndedAt, it.Profile = model.StateSucceeded, time.Now(), "p"
	w := uint64(5 << 30)
	r := profile.Run{ItemID: it.ID, Profile: "p", EndedAt: it.EndedAt, State: it.State, MemoryPeak: 1, Duration: time.Minute, IOWriteBytes: &w}
	if _, _, e := s.InsertItem(ctx, it); e != nil {
		t.Fatal(e)
	}
	if e := s.UpdateFinal(ctx, it, nil, &r); e != nil {
		t.Fatal(e)
	}
	rs, e := s.ProfileRuns(ctx, "p", 5)
	if e != nil || len(rs) != 1 || rs[0].IOWriteBytes == nil || *rs[0].IOWriteBytes != w {
		t.Fatal(rs, e)
	}
}
