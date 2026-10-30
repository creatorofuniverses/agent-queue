package profile

import (
	"github.com/creatorofuniverses/agent-queue/internal/model"
	"strings"
	"testing"
	"time"
)

func TestPercentileNearestRank(t *testing.T) {
	v := []int{10, 9, 8, 7, 6, 5, 4, 3, 2, 1}
	if Percentile(v, 90) != 9 || Percentile(v, 50) != 5 || Percentile(v, 100) != 10 || v[0] != 10 || Percentile([]int{7}, 90) != 7 || Percentile([]int{}, 90) != 0 {
		t.Fatal(v)
	}
}
func TestRunUsable(t *testing.T) {
	for _, tc := range []struct {
		s  model.State
		r  model.Reason
		ok bool
	}{{model.StateSucceeded, "", true}, {model.StateFailed, model.ReasonCommandFailed, true}, {model.StateFailed, model.ReasonOOM, false}, {model.StateFailed, model.ReasonOOMGroup, false}, {model.StateFailed, model.ReasonOOMHostGuard, false}, {model.StateFailed, model.ReasonTimeout, false}, {model.StateCancelled, model.ReasonCancelled, false}, {model.StateFailed, model.ReasonLaunchFailed, false}, {model.StateFailed, model.ReasonLost, false}} {
		r := Run{State: tc.s, Reason: tc.r, MemoryPeak: 1, Duration: time.Second}
		if r.Usable() != tc.ok {
			t.Fatal(r)
		}
	}
	if (Run{State: model.StateSucceeded, Duration: time.Second}).Usable() || (Run{State: model.StateSucceeded, MemoryPeak: 1}).Usable() {
		t.Fatal("zero usable")
	}
}
func params() Params { return Params{10, 90, 90, 1.2, 64 << 20, 16 << 30} }
func run(peak uint64, d time.Duration) Run {
	var h Histogram
	for i := 0; i < 10; i++ {
		h.Add(100 << 20)
	}
	return Run{State: model.StateSucceeded, MemoryPeak: peak, Duration: d, Hist: h}
}
func TestComputeEstimate(t *testing.T) {
	e, ok := Compute([]Run{run(150<<20, 60*time.Second), run(200<<20, 90*time.Second), run(180<<20, 120*time.Second)}, params())
	if !ok || e.Limit != 240<<20 || e.Expected != 109051903 || e.Duration != 120*time.Second || e.Runs != 3 || e.MaxPeak != 200<<20 {
		t.Fatal(e, ok)
	}
}
func TestComputeWindowAndClamp(t *testing.T) {
	var rs []Run
	for i := 0; i < 12; i++ {
		rs = append(rs, run(100<<20, time.Second))
	}
	rs[10].MemoryPeak = 1 << 40
	e, _ := Compute(rs, params())
	if e.Limit != 120<<20 || e.Runs != 10 {
		t.Fatal(e)
	}
	e, _ = Compute([]Run{run(30<<20, time.Second)}, params())
	if e.Limit != 64<<20 || e.Expected > e.Limit {
		t.Fatal(e)
	}
	e, _ = Compute([]Run{run(20<<30, time.Second)}, params())
	if e.Limit != 16<<30 || e.Expected > e.Limit {
		t.Fatal(e)
	}
}
func TestComputeNoUsable(t *testing.T) {
	if _, ok := Compute([]Run{{State: model.StateFailed, Reason: model.ReasonOOM}, {State: model.StateCancelled}}, params()); ok {
		t.Fatal("usable")
	}
}
func TestValidName(t *testing.T) {
	for _, s := range []string{"", strings.Repeat("x", 257), "a\nb", string([]byte{255})} {
		if ValidName(s) == nil {
			t.Fatal(s)
		}
	}
	if e := ValidName("/home/x/repo:build"); e != nil {
		t.Fatal(e)
	}
}
func TestMarginRoundsUpWithinBounds(t *testing.T) {
	p := params()
	if got := p.Margin(100 << 20); got != 120<<20 {
		t.Fatal(got)
	}
	if got := p.Margin(1); got != 64<<20 {
		t.Fatal("below the minimum", got)
	}
	if got := p.Margin(15 << 30); got != 16<<30 {
		t.Fatal("above the maximum", got)
	}
}
func TestForReservation(t *testing.T) {
	const mib = 1 << 20
	u := func(v uint64) *uint64 { return &v }
	res := func(docker string, stand uint64, budget *uint64) *model.Item {
		return &model.Item{Kind: model.KindReservation, State: model.StateReleased, DockerProject: docker, MemoryPeak: stand, TaskMemoryPeak: budget}
	}
	hist := []*model.Item{ // newest first
		res("p", 300*mib, u(0)), // no nested task ran: the budget did not learn anything
		res("", 0, u(900*mib)),  // no docker project: the stand was not measured
		res("p", 0, u(500*mib)), // a measured stand at 0: unlabeled containers, not a free stand
		res("p", 500*mib, u(700*mib)),
	}
	p := params()
	p.Window = 2
	got := ForReservation(hist, p)
	want := Reservation{Stand: 600 * mib, StandRuns: 2, Budget: 1080 * mib, BudgetRuns: 2}
	if got != want {
		t.Fatalf("%+v, want %+v", got, want)
	}
	if got := ForReservation(nil, p); got != (Reservation{}) {
		t.Fatal(got)
	}
}
