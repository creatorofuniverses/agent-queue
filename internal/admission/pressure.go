package admission

import "time"

// PressureGate is a hysteresis gate over PSI memory "full avg10".
// Not goroutine-safe: the scheduler owns the single instance.
type PressureGate struct {
	CloseAt    float64
	CloseAfter time.Duration
	OpenBelow  float64
	OpenAfter  time.Duration

	closed   bool
	counting bool      // the transition condition currently holds
	since    time.Time // when it started holding
}

// Observe feeds one sample and reports the gate state and whether it changed.
func (g *PressureGate) Observe(now time.Time, fullAvg10 float64) (closed bool, changed bool) {
	var holds bool
	var after time.Duration
	if g.closed {
		holds, after = fullAvg10 < g.OpenBelow, g.OpenAfter
	} else {
		holds, after = fullAvg10 >= g.CloseAt, g.CloseAfter
	}
	if !holds {
		g.counting = false
		return g.closed, false
	}
	if !g.counting {
		g.counting, g.since = true, now
	}
	if now.Sub(g.since) >= after {
		g.closed = !g.closed
		g.counting = false
		return g.closed, true
	}
	return g.closed, false
}
