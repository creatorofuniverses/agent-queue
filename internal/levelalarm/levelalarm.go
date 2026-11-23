// Package levelalarm tracks alarm episodes from sampled levels and scan passes.
package levelalarm

import "time"

// Event is a raise (High) or an all-clear.
type Event struct {
	High      bool
	Value     float64 // window average (Level) or latest complete pass value (Pass)
	Threshold float64
	Since     time.Time // episode start
}

type sample struct {
	at    time.Time
	value float64
}

// Level raises when the window average reaches the threshold and clears when
// it falls below half.
type Level struct {
	threshold float64
	window    time.Duration
	interval  time.Duration
	samples   []sample

	started bool
	first   time.Time
	last    time.Time
	active  Event
}

// NewLevel returns an alarm for a positive window length and sample interval.
// A threshold of zero disables the alarm.
func NewLevel(threshold float64, window, interval time.Duration) *Level {
	return &Level{threshold: threshold, window: window, interval: interval}
}

// Observe records a sample at an increasing timestamp. Missing samples count
// toward the cadence but contribute no value to the average.
func (l *Level) Observe(at time.Time, v float64, ok bool) (Event, bool) {
	if l.threshold == 0 {
		return Event{}, false
	}
	if !l.started {
		l.started, l.first = true, at
	}
	advancing := at.After(l.last)
	l.last = at

	if ok {
		l.samples = append(l.samples, sample{at: at, value: v})
	}
	start := at.Add(-l.window)
	i := 0
	for i < len(l.samples) && !l.samples[i].at.After(start) {
		i++
	}
	l.samples = l.samples[i:]

	count := len(l.samples)
	if l.first.After(start) || !advancing || count == 0 {
		return Event{}, false
	}
	expected := float64(l.window) / float64(l.interval)
	if float64(count) < expected/2 {
		return Event{}, false
	}
	var sum float64
	for _, s := range l.samples {
		sum += s.value
	}
	avg := sum / float64(count)

	if !l.active.High {
		if avg >= l.threshold {
			l.active = Event{High: true, Value: avg, Threshold: l.threshold, Since: at}
			return l.active, true
		}
		return Event{}, false
	}
	l.active.Value = avg
	if avg < l.threshold/2 {
		event := l.active
		event.High = false
		l.active = Event{}
		return event, true
	}
	return Event{}, false
}

// Active returns the open episode with its latest eligible window average.
func (l *Level) Active() (Event, bool) {
	return l.active, l.active.High
}

// Pass raises after two complete passes at or above the threshold and clears
// after two complete passes below half.
type Pass struct {
	threshold float64
	high, low int
	active    Event
}

// NewPass returns a pass alarm. A threshold of zero disables it.
func NewPass(threshold float64) *Pass {
	return &Pass{threshold: threshold}
}

// Observe records a pass. Incomplete passes leave the streaks and the active episode unchanged.
func (p *Pass) Observe(at time.Time, v float64, complete bool) (Event, bool) {
	if p.threshold == 0 || !complete {
		return Event{}, false
	}
	if p.active.High {
		p.active.Value = v
	}

	switch {
	case v >= p.threshold:
		p.high, p.low = p.high+1, 0
	case v < p.threshold/2:
		p.high, p.low = 0, p.low+1
	default:
		p.high, p.low = 0, 0
	}

	if !p.active.High && p.high == 2 {
		p.active = Event{High: true, Value: v, Threshold: p.threshold, Since: at}
		return p.active, true
	}
	if p.active.High && p.low == 2 {
		event := p.active
		event.High = false
		p.active = Event{}
		return event, true
	}
	return Event{}, false
}

// Active returns the open episode with its latest complete pass value.
func (p *Pass) Active() (Event, bool) {
	return p.active, p.active.High
}
