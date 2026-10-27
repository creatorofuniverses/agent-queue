package admission

import (
	"testing"
	"time"
)

func TestPressureGate(t *testing.T) {
	g := PressureGate{CloseAt: 5, CloseAfter: 10 * time.Second, OpenBelow: 1, OpenAfter: 30 * time.Second}
	t0 := time.Unix(1000, 0)
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }
	step := func(s int, v float64, wantClosed, wantChanged bool) {
		t.Helper()
		c, ch := g.Observe(at(s), v)
		if c != wantClosed || ch != wantChanged {
			t.Fatalf("t=%ds v=%v: got closed=%v changed=%v, want %v %v", s, v, c, ch, wantClosed, wantChanged)
		}
	}
	step(0, 5.0, false, false)
	step(9, 5.0, false, false)
	step(10, 5.0, true, true)
	step(11, 5.0, true, false)
	// open window: 0.5 from t=20
	step(20, 0.5, true, false)
	step(49, 0.5, true, false)
	step(50, 0.5, false, true)

	// spike resets the open countdown
	step(60, 5.0, false, false)
	step(70, 5.0, true, true)
	step(80, 0.5, true, false)
	step(100, 2.0, true, false) // spike mid-window
	step(110, 0.5, true, false)
	step(139, 0.5, true, false)
	step(140, 0.5, false, true)
}

func TestPressureGateDipResetsClose(t *testing.T) {
	g := PressureGate{CloseAt: 5, CloseAfter: 10 * time.Second, OpenBelow: 1, OpenAfter: 30 * time.Second}
	t0 := time.Unix(0, 0)
	g.Observe(t0, 6)
	g.Observe(t0.Add(5*time.Second), 1)
	if c, _ := g.Observe(t0.Add(12*time.Second), 6); c {
		t.Fatal("closed despite dip")
	}
}
