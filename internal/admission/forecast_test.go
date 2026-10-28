package admission

import (
	"github.com/creatorofuniverses/agent-queue/internal/model"
	"reflect"
	"testing"
	"time"
)

const fg = uint64(1) << 30

func fp() Policy { return Policy{Pool: model.Resources{MemoryBytes: 16 * fg, CPUs: 16}} }
func fsnap() Snapshot {
	return Snapshot{Committed: model.Resources{MemoryBytes: 8 * fg, CPUs: 1}, MemAvailable: 64 * fg, DiskFree: 100 * fg}
}
func wait(mem uint64, d time.Duration) Waiting {
	return Waiting{Res: model.Resources{MemoryBytes: mem, CPUs: 1}, Duration: d}
}
func holder(mem uint64, end time.Time) Holder {
	return Holder{Res: model.Resources{MemoryBytes: mem, CPUs: 1}, Live: mem, End: end}
}
func TestFreeCapacityMatchesDecide(t *testing.T) {
	for _, s := range []Snapshot{fsnap(), {MemAvailable: fg, Unconsumed: 2 * fg}, {Committed: model.Resources{MemoryBytes: 17 * fg, CPUs: 17}, MemAvailable: 20 * fg}} {
		for _, r := range []model.Resources{{MemoryBytes: fg, CPUs: 1}, {MemoryBytes: 10 * fg, CPUs: 2}, {MemoryBytes: 20 * fg, CPUs: 20}} {
			if FreeCapacity(fp(), s).Fits(r) != Decide(fp(), s, r).Admit {
				t.Fatal(s, r)
			}
		}
	}
}
func TestShadow(t *testing.T) {
	now := time.Now()
	hs := []Holder{holder(4*fg, now.Add(10*time.Minute)), holder(4*fg, now.Add(5*time.Minute))}
	c := Capacity{8 * int64(fg), 64 * int64(fg), 14}
	at, extra, ok := Shadow(now, c, hs, model.Resources{MemoryBytes: 14 * fg, CPUs: 1})
	if !ok || !at.Equal(now.Add(10*time.Minute)) || extra.Pool != 2*int64(fg) {
		t.Fatal(at, extra, ok)
	}
	at, _, ok = Shadow(now, c, hs, model.Resources{MemoryBytes: 6 * fg, CPUs: 1})
	if !ok || !at.Equal(now) {
		t.Fatal(at)
	}
	hs[0].Never = true
	if _, _, ok = Shadow(now, c, hs, model.Resources{MemoryBytes: 14 * fg, CPUs: 1}); ok {
		t.Fatal("never released")
	}
	if _, _, ok = Shadow(now, c, hs, model.Resources{MemoryBytes: 20 * fg, CPUs: 1}); ok {
		t.Fatal("oversize")
	}
}
func TestShadowLiveMemory(t *testing.T) {
	now := time.Now()
	h := holder(4*fg, now.Add(time.Hour))
	h.Live = fg
	at, _, ok := Shadow(now, Capacity{8 * int64(fg), int64(fg), 10}, []Holder{h}, model.Resources{MemoryBytes: 2 * fg, CPUs: 1})
	if !ok || !at.Equal(h.End) {
		t.Fatal(at, ok)
	}
}
func TestShadowCleaningReleasesLittle(t *testing.T) {
	now := time.Now()
	a := holder(8*fg, now)
	a.Live = 100 << 20
	b := holder(4*fg, now.Add(10*time.Minute))
	c := Capacity{4 * int64(fg), 5 * int64(fg), 14}
	at, extra, ok := Shadow(now, c, []Holder{a, b}, model.Resources{MemoryBytes: 9 * fg, CPUs: 1})
	if !ok || !at.Equal(b.End) || extra.Live != 100<<20 {
		t.Fatal(at, extra, ok)
	}
	s := fsnap()
	s.Committed.MemoryBytes = 12 * fg
	s.MemAvailable = 5 * fg
	p := PlanPass(fp(), s, []Holder{a, b}, []Waiting{wait(9*fg, time.Hour), wait(3*fg, 2*time.Hour)}, now, true)
	if len(p.Admitted) != 0 {
		t.Fatal(p)
	}
}
func TestScheduleFIFO(t *testing.T) {
	now := time.Now()
	q := []Waiting{wait(10*fg, 30*time.Minute), wait(fg, 10*time.Minute), wait(6*fg, 2*time.Hour)}
	got := Schedule(now, FreeCapacity(fp(), fsnap()), []Holder{holder(8*fg, now.Add(time.Hour))}, q)
	for i, d := range []time.Duration{time.Hour, time.Hour, 70 * time.Minute} {
		if !got[i].OK || !got[i].Start.Equal(now.Add(d)) {
			t.Fatal(got)
		}
	}
	q = append([]Waiting{wait(20*fg, time.Hour)}, q...)
	for _, v := range Schedule(now, FreeCapacity(fp(), fsnap()), nil, q) {
		if v.OK {
			t.Fatal(v)
		}
	}
}
func TestPlanPassBackfillByTime(t *testing.T) {
	now := time.Now()
	p := PlanPass(fp(), fsnap(), []Holder{holder(8*fg, now.Add(time.Hour))}, []Waiting{wait(10*fg, time.Hour), wait(fg, 10*time.Minute)}, now, true)
	if !reflect.DeepEqual(p.Admitted, []int{1}) || !p.Backfilled[1] || p.Head != 0 || p.HeadDecision.Blocker != BlockPoolMemory {
		t.Fatal(p)
	}
}
func TestPlanPassBackfillByExtra(t *testing.T) {
	now := time.Now()
	p := PlanPass(fp(), fsnap(), []Holder{holder(8*fg, now.Add(time.Hour))}, []Waiting{wait(10*fg, time.Hour), wait(fg, 2*time.Hour), wait(6*fg, 2*time.Hour)}, now, true)
	if !reflect.DeepEqual(p.Admitted, []int{1}) {
		t.Fatal(p)
	}
}
func TestPlanPassReservationExtraOnly(t *testing.T) {
	now := time.Now()
	for _, n := range []uint64{5, 7} {
		q := []Waiting{wait(10*fg, time.Hour), wait(n*fg, 10*time.Minute)}
		q[1].ExtraOnly = true
		p := PlanPass(fp(), fsnap(), []Holder{holder(8*fg, now.Add(time.Hour))}, q, now, true)
		if (len(p.Admitted) == 1) != (n == 5) {
			t.Fatal(p)
		}
	}
}
func TestPlanPassNoBackfillOnGate(t *testing.T) {
	now := time.Now()
	s := fsnap()
	q := []Waiting{wait(10*fg, time.Hour), wait(fg, time.Minute)}
	if p := PlanPass(fp(), s, []Holder{holder(8*fg, now.Add(time.Hour))}, q, now, false); len(p.Admitted) != 0 {
		t.Fatal(p)
	}
	s.PressureClosed = true
	if p := PlanPass(fp(), s, nil, q, now, true); len(p.Admitted) != 0 {
		t.Fatal(p)
	}
}
func TestPlanPassHeadUnreachable(t *testing.T) {
	now := time.Now()
	s := fsnap()
	s.Committed.MemoryBytes = 4 * fg
	s.MemAvailable = 2 * fg
	p := PlanPass(fp(), s, []Holder{holder(4*fg, now.Add(time.Hour))}, []Waiting{wait(10*fg, time.Hour), wait(fg, time.Minute)}, now, true)
	if len(p.Admitted) != 0 {
		t.Fatal(p)
	}
}
func TestPlanPassPrefixThenBackfill(t *testing.T) {
	now := time.Now()
	s := fsnap()
	s.Committed.MemoryBytes = 4 * fg
	p := PlanPass(fp(), s, []Holder{holder(4*fg, now.Add(time.Hour))}, []Waiting{wait(2*fg, 2*time.Hour), wait(12*fg, time.Hour), wait(fg, 10*time.Minute)}, now, true)
	if !reflect.DeepEqual(p.Admitted, []int{0, 2}) || !reflect.DeepEqual(p.Backfilled, []bool{false, false, true}) || p.Head != 1 {
		t.Fatal(p)
	}
}

// Codex branch review r2: with one teardown slot, a backfilled reservation that
// expires first would take the slot the head's holder needs.
func TestPlanPassReservationWaitsForTeardownSlot(t *testing.T) {
	now := time.Now()
	a := holder(8*fg, now.Add(15*time.Minute))
	a.Teardown = true
	q := []Waiting{wait(10*fg, time.Hour), wait(5*fg, 14*time.Minute)}
	q[1].ExtraOnly = true
	for _, tc := range []struct {
		slots int
		admit bool
	}{{0, false}, {1, false}, {2, true}} {
		pol := fp()
		pol.TeardownSlots = tc.slots
		p := PlanPass(pol, fsnap(), []Holder{a}, q, now, true)
		if (len(p.Admitted) == 1) != tc.admit {
			t.Fatal(tc, p)
		}
	}
	// The head waits on no teardown: a task holder frees its memory.
	pol := fp()
	pol.TeardownSlots = 1
	if p := PlanPass(pol, fsnap(), []Holder{holder(8*fg, now.Add(15*time.Minute))}, q, now, true); len(p.Admitted) != 1 {
		t.Fatal(p)
	}
}
