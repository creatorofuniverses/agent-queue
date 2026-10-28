package daemon

import (
	"context"
	"os"
	"path/filepath"
	"resource-dispatcher/internal/config"
	"resource-dispatcher/internal/model"
	"resource-dispatcher/internal/store"
	"testing"
	"time"
)

func retentionEnv(t *testing.T) (*store.Store, config.Paths, config.Config, time.Time) {
	t.Helper()
	root := t.TempDir()
	p := config.Paths{StateDir: root}
	s, e := store.Open(p.DB())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	return s, p, config.Default(), time.Now().UTC()
}
func retentionItem(t *testing.T, s *store.Store, p config.Paths, ended time.Time, size int64) *model.Item {
	t.Helper()
	state := model.StateSucceeded
	if ended.IsZero() {
		state = model.StateRunning
	}
	it := &model.Item{ID: model.NewID(), Kind: model.KindTask, State: state, CreatedAt: ended.Add(-time.Minute), EndedAt: ended}
	if _, _, e := s.InsertItem(context.Background(), it); e != nil {
		t.Fatal(e)
	}
	dir := p.JobDir(it.ID)
	os.MkdirAll(dir, 0700)
	f, e := os.Create(filepath.Join(dir, "stdout.log"))
	if e != nil {
		t.Fatal(e)
	}
	if e = f.Truncate(size); e != nil {
		t.Fatal(e)
	}
	f.Close()
	return it
}
func requirePath(t *testing.T, p string, want bool) {
	t.Helper()
	_, e := os.Stat(p)
	if want && e != nil || !want && !os.IsNotExist(e) {
		t.Fatal(p, e)
	}
}
func TestRetentionPrunesOldLogs(t *testing.T) {
	s, p, cfg, now := retentionEnv(t)
	old := retentionItem(t, s, p, now.Add(-8*24*time.Hour), 10)
	fresh := retentionItem(t, s, p, now.Add(-24*time.Hour), 10)
	if e := retentionStep(t.Context(), s, p, cfg, now); e != nil {
		t.Fatal(e)
	}
	requirePath(t, p.JobDir(old.ID), false)
	requirePath(t, p.JobDir(fresh.ID), true)
	it, _ := s.Get(t.Context(), old.ID)
	if !it.LogsPruned {
		t.Fatal(it)
	}
}
func TestRetentionTotalLimitOldestFirst(t *testing.T) {
	s, p, cfg, now := retentionEnv(t)
	cfg.Retention.LogsMaxTotal = 256 * mib
	var its []*model.Item
	for _, days := range []int{3, 2, 1} {
		its = append(its, retentionItem(t, s, p, now.Add(-time.Duration(days)*24*time.Hour), 100<<20))
	}
	active := retentionItem(t, s, p, time.Time{}, 100<<20)
	if e := retentionStep(t.Context(), s, p, cfg, now); e != nil {
		t.Fatal(e)
	}
	for i, it := range its {
		requirePath(t, p.JobDir(it.ID), i == 2)
	}
	requirePath(t, p.JobDir(active.ID), true)
}
func TestRetentionKeepsFreshOrphanAndActive(t *testing.T) {
	s, p, cfg, now := retentionEnv(t)
	orphan := model.NewID()
	os.MkdirAll(p.JobDir(orphan), 0700)
	os.WriteFile(p.TeardownScript(orphan), []byte("true"), 0700)
	terminal := retentionItem(t, s, p, now.Add(-24*time.Hour), 1)
	active := retentionItem(t, s, p, time.Time{}, 1)
	active.Kind = model.KindReservation
	active.State = model.StateTeardownFailed
	active.CreatedAt = now.Add(-8 * 24 * time.Hour)
	s.Update(t.Context(), active)
	os.MkdirAll(p.TmpDir(active.ID), 0700)
	tmp := p.TeardownTmpDir(terminal.ID, 1)
	os.MkdirAll(tmp, 0700)
	for _, dir := range []string{p.JobDir(terminal.ID), p.JobDir(active.ID), p.TmpDir(active.ID), tmp} {
		os.Chtimes(dir, now.Add(-2*time.Hour), now.Add(-2*time.Hour))
	}
	if e := retentionStep(t.Context(), s, p, cfg, now); e != nil {
		t.Fatal(e)
	}
	requirePath(t, p.JobDir(orphan), true)
	requirePath(t, p.JobDir(terminal.ID), true)
	requirePath(t, p.JobDir(active.ID), true)
	requirePath(t, p.TmpDir(active.ID), true)
	requirePath(t, tmp, false)
	os.Chtimes(p.JobDir(orphan), now.Add(-2*time.Hour), now.Add(-2*time.Hour))
	if e := retentionStep(t.Context(), s, p, cfg, now); e != nil {
		t.Fatal(e)
	}
	requirePath(t, p.JobDir(orphan), false)
}
func TestRetentionRemovesOldHookFiles(t *testing.T) {
	s, p, cfg, now := retentionEnv(t)
	for _, tc := range []struct {
		name string
		age  time.Duration
		keep bool
	}{{"hook-hits-old.jsonl", 91 * 24 * time.Hour, false}, {"hook-hits-new.jsonl", 24 * time.Hour, true}, {"hook-hits.jsonl", 91 * 24 * time.Hour, true}} {
		path := filepath.Join(p.StateDir, tc.name)
		os.WriteFile(path, []byte("x"), 0600)
		os.Chtimes(path, now.Add(-tc.age), now.Add(-tc.age))
	}
	if e := retentionStep(t.Context(), s, p, cfg, now); e != nil {
		t.Fatal(e)
	}
	requirePath(t, filepath.Join(p.StateDir, "hook-hits-old.jsonl"), false)
	requirePath(t, filepath.Join(p.StateDir, "hook-hits-new.jsonl"), true)
	requirePath(t, filepath.Join(p.StateDir, "hook-hits.jsonl"), true)
}
func TestLogsOfPrunedItem(t *testing.T) {
	e := newEnv(t)
	os.MkdirAll(e.paths.StateDir, 0700)
	s, err := store.Open(e.paths.DB())
	if err != nil {
		t.Fatal(err)
	}
	it := retentionItem(t, s, e.paths, time.Now(), 1)
	it.LogsPruned = true
	s.Update(t.Context(), it)
	s.Close()
	e.start(t)
	for _, follow := range []bool{false, true} {
		var v LogLines
		if err = e.call(t, "logs", LogsArgs{ID: it.ID, Follow: follow}, &v); err != nil || !v.Pruned || len(v.Lines) != 0 {
			t.Fatal(v, err)
		}
	}
	var v ResultView
	if err = e.call(t, "result", map[string]any{"id": it.ID}, &v); err != nil || !v.LogsPruned || v.StdoutPath != "" || v.StderrPath != "" {
		t.Fatal(v, err)
	}
}
func TestRetentionSurvivesUnmeasurableJobs(t *testing.T) {
	s, p, cfg, now := retentionEnv(t)
	old := retentionItem(t, s, p, now.Add(-8*24*time.Hour), 10)
	live := retentionItem(t, s, p, time.Time{}, 10)
	locked := filepath.Join(p.JobDir(live.ID), "locked")
	os.MkdirAll(locked, 0700)
	os.Chmod(locked, 0)
	t.Cleanup(func() { os.Chmod(locked, 0700) })
	if e := retentionStep(t.Context(), s, p, cfg, now); e != nil {
		t.Fatal(e)
	}
	requirePath(t, p.JobDir(old.ID), false)
}
func TestRetentionHistoryWaitsForLogs(t *testing.T) {
	s, p, cfg, now := retentionEnv(t)
	cfg.Retention.HistoryMaxAge = 24 * time.Hour // shorter than logs_max_age (7d)
	it := retentionItem(t, s, p, now.Add(-2*24*time.Hour), 1)
	os.Chtimes(p.JobDir(it.ID), now.Add(-2*time.Hour), now.Add(-2*time.Hour))
	for i := 0; i < 2; i++ {
		if e := retentionStep(t.Context(), s, p, cfg, now); e != nil {
			t.Fatal(e)
		}
	}
	requirePath(t, p.JobDir(it.ID), true)
	if got, e := s.Get(t.Context(), it.ID); e != nil || got == nil {
		t.Fatal("row deleted before its logs expired", e)
	}
}
