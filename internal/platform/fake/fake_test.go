package fake_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"resource-dispatcher/internal/model"
	"resource-dispatcher/internal/platform"
	"resource-dispatcher/internal/platform/fake"
)

var (
	_ platform.Executor    = (*fake.Executor)(nil)
	_ platform.HostMetrics = (*fake.Host)(nil)
	_ platform.StandMeter  = (*fake.Meter)(nil)
)

func TestFakeExecutorLifecycle(t *testing.T) {
	ctx := context.Background()
	e := fake.NewExecutor()
	id := model.ItemID("a")
	before := 0
	e.SetBeforeLaunch(func(platform.LaunchSpec) { before++ })
	if _, err := e.Launch(ctx, platform.LaunchSpec{ID: id, JobDir: "/j/a"}); err != nil {
		t.Fatal(err)
	}
	if before != 1 || len(e.Launched()) != 1 {
		t.Fatal("hook or launch not recorded")
	}
	if ok, _ := e.Alive(ctx, id); !ok {
		t.Fatal("want alive")
	}
	if r, _ := e.ReadResult("/j/a"); r != nil {
		t.Fatal("result before finish")
	}
	e.SetCurrent(id, 42)
	if v, _ := e.MemoryCurrent(id); v != 42 {
		t.Fatal("current")
	}
	if l, _ := e.ListManaged(ctx); len(l) != 1 {
		t.Fatal("managed")
	}
	e.Finish(id, platform.ExitInfo{ExitCode: "0"})
	if r, _ := e.ReadResult("/j/a"); r == nil || r.ExitCode != "0" {
		t.Fatal("result")
	}
	e.SetAlive(id, false)
	if ok, _ := e.Alive(ctx, id); ok {
		t.Fatal("want dead")
	}
	if l, _ := e.ListManaged(ctx); len(l) != 0 {
		t.Fatal("managed after death")
	}
	_ = e.Stop(ctx, id)
	if s := e.Stopped(); len(s) != 1 || s[0] != id {
		t.Fatal("stopped")
	}
	_ = e.SetSliceCeiling(10, 8)
	if c := e.Ceilings(); len(c) != 1 || c[0] != [2]uint64{10, 8} {
		t.Fatal("ceilings")
	}
	e.SetSlice(5, 2)
	if v, _ := e.SliceCurrent(); v != 5 {
		t.Fatal("slice")
	}
	if v, _ := e.SliceOOMKills(); v != 2 {
		t.Fatal("slice oom")
	}
	boom := errors.New("boom")
	e.FailLaunch(boom)
	if _, err := e.Launch(ctx, platform.LaunchSpec{ID: "b"}); !errors.Is(err, boom) {
		t.Fatal("fail launch")
	}
}

func TestFakeHost(t *testing.T) {
	h := fake.NewHost()
	h.Set(platform.MemInfo{Total: 8, Available: 4})
	h.SetPressure(1.5)
	h.SetDisk(7)
	h.SetOOMKills(3)
	h.SetNumCPU(4)
	m, _ := h.Memory()
	p, _ := h.PressureFullAvg10()
	d, _ := h.DiskFree("/")
	k, _ := h.KernelOOMKills()
	if m.Total != 8 || p != 1.5 || d != 7 || k != 3 || h.NumCPU() != 4 {
		t.Fatal("host values")
	}
}

func TestFakeMeter(t *testing.T) {
	m := fake.NewMeter()
	if _, ok := m.Usage("p"); ok {
		t.Fatal("usage before Set")
	}
	m.Set("p", 42)
	if v, ok := m.Usage("p"); !ok || v != 42 {
		t.Fatalf("usage %d %v", v, ok)
	}
	m.Unset("p")
	if _, ok := m.Usage("p"); ok {
		t.Fatal("usage after Unset")
	}
	in := []string{"a", "b"}
	m.Watch(in)
	in[0] = "x" // Watch keeps its own copy
	if w := m.Watched(); !slices.Equal(w, []string{"a", "b"}) {
		t.Fatalf("watched %v", w)
	}
	m.Watch(nil)
	if w := m.Watched(); len(w) != 0 {
		t.Fatalf("watch replaces the set: %v", w)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	select {
	case <-done:
		t.Fatal("Run returned before ctx ended")
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run ignored ctx")
	}
}

func TestFakeStopHookAndTeardownSlice(t *testing.T) {
	ctx := context.Background()
	e := fake.NewExecutor()
	var seen []model.ItemID
	e.SetBeforeStop(func(id model.ItemID) {
		seen = append(seen, id)
		if len(e.Stopped()) != 0 { // the hook runs before the stop is recorded, outside the lock
			t.Error("stop recorded before the hook")
		}
	})
	if err := e.Stop(ctx, "teardown-x"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(seen, []model.ItemID{"teardown-x"}) || !slices.Equal(e.Stopped(), []model.ItemID{"teardown-x"}) {
		t.Fatalf("hook %v stopped %v", seen, e.Stopped())
	}
	if pool, td := e.Ensured(); pool != 0 || td != 0 {
		t.Fatalf("ensured %d %d before any call", pool, td)
	}
	if err := e.EnsureSlice(ctx, 1<<30); err != nil {
		t.Fatal(err)
	}
	if err := e.EnsureTeardownSlice(ctx, 1<<29); err != nil {
		t.Fatal(err)
	}
	if pool, td := e.Ensured(); pool != 1<<30 || td != 1<<29 {
		t.Fatalf("ensured %d %d", pool, td)
	}
}
