package daemon

import (
	"context"
	"os"
	"resource-dispatcher/internal/model"
	"resource-dispatcher/internal/profile"
	"resource-dispatcher/internal/store"
	"testing"
	"time"
)

func seedProfile(t *testing.T, e *env, name string) {
	t.Helper()
	if err := os.MkdirAll(e.paths.StateDir, 0700); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(e.paths.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for i := 0; i < 2; i++ {
		now := time.Now().UTC().Add(time.Duration(i) * time.Second)
		it := &model.Item{ID: model.NewID(), Kind: model.KindTask, State: model.StateSucceeded, Profile: name, CreatedAt: now.Add(-time.Minute), EndedAt: now}
		s.InsertItem(context.Background(), it)
		var h profile.Histogram
		h.Add(100 * mib)
		r := profile.Run{ItemID: it.ID, Profile: name, EndedAt: now, State: it.State, MemoryPeak: 150 * mib, Duration: time.Minute, Hist: h}
		if err = s.UpdateFinal(context.Background(), it, nil, &r); err != nil {
			t.Fatal(err)
		}
	}
}
func TestDaemonSubmitProfileView(t *testing.T) {
	e := newEnv(t)
	seedProfile(t, e, "p")
	e.start(t)
	r := e.submitArgs(0)
	r.Profile = "p"
	var v ItemView
	if err := e.call(t, "submit", r, &v); err != nil || v.Profile != "p" || !v.MemoryFromProfile || v.EstimatedSeconds <= 0 {
		t.Fatal(v, err)
	}
}
func TestProfileShowAndList(t *testing.T) {
	e := newEnv(t)
	seedProfile(t, e, "p")
	e.start(t)
	var v ProfileView
	if err := e.call(t, "profile_show", map[string]string{"profile": "p"}, &v); err != nil || v.Estimate == nil || v.Estimate.Runs != 2 || len(v.Runs) != 2 || !v.Runs[0].Usable || v.Runs[0].EndedAt.Before(v.Runs[1].EndedAt) {
		t.Fatal(v, err)
	}
	var list []ProfileListEntry
	if err := e.call(t, "profile_list", nil, &list); err != nil || len(list) != 1 {
		t.Fatal(list, err)
	}
	if err := e.call(t, "profile_show", map[string]string{"profile": "missing"}, &v); err == nil {
		t.Fatal("missing")
	}
}
