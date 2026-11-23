package levelalarm

import (
	"testing"
	"time"
)

const (
	testThreshold = 70
	testWindow    = 10 * time.Minute
	testInterval  = 30 * time.Second
)

var testStart = time.Unix(0, 0).UTC()

func sampleTime(i int) time.Time {
	return testStart.Add(time.Duration(i) * testInterval)
}

func expectLevel(t *testing.T, l *Level, i int, v float64, valid bool, want Event, wantEvent bool) {
	t.Helper()
	got, emitted := l.Observe(sampleTime(i), v, valid)
	if got != want || emitted != wantEvent {
		t.Fatalf("sample %d: Observe = (%+v, %t), want (%+v, %t)", i, got, emitted, want, wantEvent)
	}
}

func openLevel(t *testing.T) (*Level, Event) {
	t.Helper()
	l := NewLevel(testThreshold, testWindow, testInterval)
	for i := range 20 {
		expectLevel(t, l, i, 80, true, Event{}, false)
	}
	raise := Event{High: true, Value: 80, Threshold: testThreshold, Since: sampleTime(20)}
	expectLevel(t, l, 20, 80, true, raise, true)
	return l, raise
}

func TestLevelRaisesOnWindowAverage(t *testing.T) {
	l, raise := openLevel(t)
	for i := 21; i <= 40; i++ {
		expectLevel(t, l, i, 80, true, Event{}, false)
	}
	if got, active := l.Active(); !active || got != raise {
		t.Fatalf("Active = (%+v, %t), want (%+v, true)", got, active, raise)
	}

	// The first sample is outside the full window, whose average is exactly 70.
	l = NewLevel(testThreshold, testWindow, testInterval)
	expectLevel(t, l, 0, 0, true, Event{}, false)
	for i := 1; i < 20; i++ {
		expectLevel(t, l, i, 70, true, Event{}, false)
	}
	raise.Value = 70
	expectLevel(t, l, 20, 70, true, raise, true)
}

func TestLevelClearsBelowHalf(t *testing.T) {
	l, raise := openLevel(t)
	for i := 21; i <= 40; i++ {
		expectLevel(t, l, i, 40, true, Event{}, false)
	}
	wantActive := raise
	wantActive.Value = 40
	if got, active := l.Active(); !active || got != wantActive {
		t.Fatalf("Active = (%+v, %t), want (%+v, true)", got, active, wantActive)
	}

	expectLevel(t, l, 41, 0, true, Event{}, false)  // average 38
	expectLevel(t, l, 42, 0, true, Event{}, false)  // average 36
	expectLevel(t, l, 43, 20, true, Event{}, false) // average 35
	clear := Event{Value: 33, Threshold: testThreshold, Since: raise.Since}
	expectLevel(t, l, 44, 0, true, clear, true)
	expectLevel(t, l, 45, 0, true, Event{}, false)
	if got, active := l.Active(); active || got != (Event{}) {
		t.Fatalf("Active after clear = (%+v, %t)", got, active)
	}
}

func TestLevelSpikeDoesNotClear(t *testing.T) {
	l, raise := openLevel(t)
	for i := 21; i <= 38; i++ {
		// Thirteen low samples bring the average to 34.5.
		if i == 33 {
			clear := Event{Value: 34.5, Threshold: testThreshold, Since: raise.Since}
			expectLevel(t, l, i, 10, true, clear, true)
			continue
		}
		expectLevel(t, l, i, 10, true, Event{}, false)
	}
	for i := 39; i <= 40; i++ {
		expectLevel(t, l, i, 60, true, Event{}, false)
	}
	if got, active := l.Active(); active {
		t.Fatalf("spike reopened the alarm: %+v", got)
	}
}

func TestLevelSkipsMissing(t *testing.T) {
	l := NewLevel(testThreshold, testWindow, testInterval)
	for i := range 20 {
		expectLevel(t, l, i, 80*float64(i%2), i%2 == 1, Event{}, false)
	}
	raise := Event{High: true, Value: 80, Threshold: testThreshold, Since: sampleTime(20)}
	expectLevel(t, l, 20, 0, false, raise, true) // exactly ten valid samples
	for i := 21; i <= 40; i++ {
		expectLevel(t, l, i, 0, false, Event{}, false)
	}
	expectLevel(t, l, 41, 0, true, Event{}, false) // one valid low sample
	if got, active := l.Active(); !active || got != raise {
		t.Fatalf("Active with insufficient samples = (%+v, %t), want (%+v, true)", got, active, raise)
	}
}

func TestLevelMissedSamplesDoNotReduceExpectedCount(t *testing.T) {
	l := NewLevel(testThreshold, testWindow, testInterval)
	expectLevel(t, l, 0, 0, true, Event{}, false)
	expectLevel(t, l, 20, 82, true, Event{}, false)
	if got, active := l.Active(); active {
		t.Fatalf("one sample opened an alert: %+v", got)
	}
}

func TestLevelZeroThresholdNeverFires(t *testing.T) {
	l := NewLevel(0, testWindow, testInterval)
	for i := range 40 {
		expectLevel(t, l, i, 80, true, Event{}, false)
	}
}

func TestLevelFreshStartNoClear(t *testing.T) {
	l := NewLevel(testThreshold, testWindow, testInterval)
	for i := range 40 {
		expectLevel(t, l, i, 10, true, Event{}, false)
	}
}

func TestPassTwoInARow(t *testing.T) {
	const gib = 1 << 30
	const threshold = 2 * gib
	p := NewPass(threshold)
	raise := Event{High: true, Value: 3 * gib, Threshold: threshold, Since: sampleTime(3)}
	clear := Event{Value: 0, Threshold: threshold, Since: raise.Since}
	steps := []struct {
		value    float64
		complete bool
		event    Event
	}{
		{value: 3 * gib, complete: true},
		{value: gib, complete: true}, // half resets the high streak
		{value: 3 * gib, complete: true},
		{value: 3 * gib, complete: true, event: raise},
		{value: 0, complete: false}, // cannot start the low streak
		{value: 0, complete: true},
		{value: gib, complete: true}, // half resets the low streak
		{value: 0, complete: true},
		{value: 3 * gib, complete: false}, // preserves the low streak
		{value: 0, complete: true, event: clear},
		{value: threshold, complete: true},
		{value: 0, complete: false}, // preserves the high streak
		{value: threshold, complete: true, event: Event{
			High: true, Value: threshold, Threshold: threshold, Since: sampleTime(12),
		}},
	}
	var lastComplete float64
	for i, step := range steps {
		if step.complete {
			lastComplete = step.value
		}
		wantEvent := step.event != (Event{})
		got, emitted := p.Observe(sampleTime(i), step.value, step.complete)
		if got != step.event || emitted != wantEvent {
			t.Fatalf("pass %d: Observe = (%+v, %t), want (%+v, %t)", i, got, emitted, step.event, wantEvent)
		}
		var wantActive Event
		switch {
		case i >= 3 && i < 9:
			wantActive = raise
			wantActive.Value = lastComplete
		case i == 12:
			wantActive = step.event
		}
		if got, active := p.Active(); got != wantActive || active != wantActive.High {
			t.Fatalf("pass %d: Active = (%+v, %t), want (%+v, %t)", i, got, active, wantActive, wantActive.High)
		}
	}
}

func TestPassZeroThresholdNeverFires(t *testing.T) {
	p := NewPass(0)
	for i := range 3 {
		if got, emitted := p.Observe(sampleTime(i), 1, true); emitted {
			t.Fatalf("disabled alarm emitted %+v", got)
		}
	}
}
