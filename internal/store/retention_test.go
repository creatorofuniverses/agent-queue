package store

import (
	"context"
	"github.com/creatorofuniverses/agent-queue/internal/model"
	"testing"
	"time"
)

func TestDeleteHistory(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	now := time.Now().UTC()
	old := saveRun(t, s, "p", model.StateSucceeded, "", 100, now.Add(-91*24*time.Hour))
	fresh := saveRun(t, s, "p", model.StateSucceeded, "", 100, now.Add(-10*24*time.Hour))
	active := newItem(0, now.Add(-100*24*time.Hour))
	s.InsertItem(ctx, active)
	for _, r := range []model.ItemID{old.ItemID, fresh.ItemID} {
		s.AddEvents(ctx, []model.Event{{ID: string(model.NewID()), ItemID: r, CreatedAt: now.Add(-100 * 24 * time.Hour)}})
	}
	s.AddIncident(ctx, model.Incident{At: now.Add(-91 * 24 * time.Hour), Kind: "kernel_oom"})
	n, e := s.DeleteHistory(ctx, now.Add(-90*24*time.Hour))
	if e != nil || n != 1 {
		t.Fatal(n, e)
	}
	if _, e = s.Get(ctx, old.ItemID); e != ErrNotFound {
		t.Fatal(e)
	}
	if _, e = s.Get(ctx, fresh.ItemID); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Get(ctx, active.ID); e != nil {
		t.Fatal(e)
	}
	evs, _ := s.Events(ctx, old.ItemID)
	rs, _ := s.ProfileRuns(ctx, "p", 10)
	incs, _ := s.Incidents(ctx, time.Time{})
	if len(evs) != 0 || len(rs) != 1 || len(incs) != 0 {
		t.Fatal(evs, rs, incs)
	}
}
func TestThinHostSamples(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(10 * time.Minute)
	base := now.Add(-72 * time.Hour)
	for i := 0; i < 60; i++ {
		s.AddHostSample(ctx, HostSample{At: base.Add(time.Duration(i) * 30 * time.Second), Span: 30 * time.Second, MemAvailable: uint64(i)})
	}
	s.AddHostSample(ctx, HostSample{At: now.Add(-time.Hour), Span: 30 * time.Second})
	for i := 0; i < 2; i++ {
		if e := s.ThinHostSamples(ctx, now.Add(-48*time.Hour)); e != nil {
			t.Fatal(e)
		}
		rs, e := s.HostSamples(ctx, time.Time{})
		if e != nil || len(rs) != 4 {
			t.Fatal(rs, e)
		}
		for j, v := range rs {
			if j < 3 && (v.Span != 600*time.Second || v.MemAvailable != uint64(j*20)) {
				t.Fatal(v)
			}
		}
	}
	if e := s.DeleteHostSamples(ctx, base.Add(10*time.Minute)); e != nil {
		t.Fatal(e)
	}
	rs, _ := s.HostSamples(ctx, time.Time{})
	if len(rs) != 3 {
		t.Fatal(rs)
	}
}
