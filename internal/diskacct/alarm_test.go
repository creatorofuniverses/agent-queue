package diskacct

import (
	"testing"
	"time"
)

const (
	step    = 30 * time.Second
	window  = 10 * time.Minute
	thresh  = uint64(50) << 30
	alarmT0 = 1_000_000
)

// feed runs n samples; f gives the delta for sample i (1-based, at i*step).
func feed(a *Alarm, from, n int, f func(i int) Delta) (evs []AlarmEvent, at []int) {
	for i := from; i < from+n; i++ {
		t := time.Unix(alarmT0, 0).Add(time.Duration(i) * step)
		if ev, ok := a.Observe(t, step, f(i)); ok {
			evs = append(evs, ev)
			at = append(at, i)
		}
	}
	return
}

func host(b int64) Delta { return Delta{Host: b} }

func TestAlarmSustainedBursty(t *testing.T) {
	a := NewAlarm(thresh, window)
	evs, at := feed(a, 1, 60, func(i int) Delta {
		if i%4 == 0 {
			return host(4 * gib)
		}
		return host(0)
	})
	if len(evs) != 1 || !evs[0].High {
		t.Fatalf("events = %+v", evs)
	}
	if at[0] != 20 {
		t.Errorf("raised at sample %d, want 20 (window just filled)", at[0])
	}
	if evs[0].Since != window {
		t.Errorf("Since = %v", evs[0].Since)
	}
}

func TestAlarmSplitBurst(t *testing.T) {
	a := NewAlarm(thresh, window)
	evs, _ := feed(a, 1, 40, func(i int) Delta {
		if i == 25 || i == 26 {
			return host(9 * gib / 2)
		}
		return host(0)
	})
	if len(evs) != 0 {
		t.Fatalf("events = %+v", evs)
	}
}

func TestAlarmAllClear(t *testing.T) {
	a := NewAlarm(thresh, window)
	// 100 GiB/h = 0.4167 GiB per 30 s; then 10 GiB/h.
	hi := host(perStep(100))
	lo := host(perStep(10))
	evs, _ := feed(a, 1, 30, func(i int) Delta {
		d := hi
		d.Sources = map[string]int64{"aq": d.Host}
		return d
	})
	if len(evs) != 1 || !evs[0].High {
		t.Fatalf("raise: %+v", evs)
	}
	evs, at := feed(a, 31, 60, func(i int) Delta {
		d := lo
		d.Sources = map[string]int64{"aq": d.Host}
		return d
	})
	if len(evs) != 1 || evs[0].High {
		t.Fatalf("clear: %+v", evs)
	}
	// Average drops below 25 GiB/h only once most of the window is quiet:
	// 10 min window, x min at 100, rest at 10: 10+90*x/10 < 25 => x < 1.67 min.
	if at[0] < 31+14 {
		t.Errorf("cleared too early at %d", at[0])
	}
	if evs[0].BytesPerHour >= float64(thresh)/2 {
		t.Errorf("rate %v", evs[0].BytesPerHour)
	}
	if evs[0].Since <= window {
		t.Errorf("Since = %v, want the whole episode", evs[0].Since)
	}
	if len(evs[0].Top) != 1 || evs[0].Top[0].Source != "aq" {
		t.Errorf("Top = %+v", evs[0].Top)
	}
	if evs[0].Written < uint64(30*hi.Host) {
		t.Errorf("Written = %d, want at least the raising samples", evs[0].Written)
	}
	// Episode ended: it can fire again.
	evs, _ = feed(a, 91, 30, func(int) Delta { return hi })
	if len(evs) != 1 || !evs[0].High {
		t.Fatalf("second raise: %+v", evs)
	}
}

func TestAlarmNeedsFullWindow(t *testing.T) {
	a := NewAlarm(thresh, window)
	hi := host(perStep(100))
	evs, at := feed(a, 1, 19, func(int) Delta { return hi })
	if len(evs) != 0 {
		t.Fatalf("events before the window filled: %+v at %v", evs, at)
	}
	evs, _ = feed(a, 20, 1, func(int) Delta { return hi })
	if len(evs) != 1 {
		t.Fatalf("no raise on the full window")
	}
}

func TestAlarmTopSources(t *testing.T) {
	a := NewAlarm(thresh, window)
	d := Delta{
		Host: perStep(100),
		Sources: map[string]int64{
			"session:session-3.scope": 600 << 20,
			"docker:x":                200 << 20,
			"system:j.service":        100 << 20,
			"system:ended":            50 << 20,
			"user:k":                  -5,
		},
		Agents: []AgentDelta{
			{Key: "agent:codex:p", Within: "session:session-3.scope", Bytes: 100 << 20},
			{Key: "agent:claude:p", Within: "session:session-3.scope", Bytes: 400 << 20},
			{Key: "agent:other:q", Within: "docker:x", Bytes: -3},
		},
	}
	evs, _ := feed(a, 1, 20, func(int) Delta { return d })
	if len(evs) != 1 {
		t.Fatalf("events = %+v", evs)
	}
	top := evs[0].Top
	if len(top) != 3 || top[0].Source != "session:session-3.scope" || top[1].Source != "docker:x" || top[2].Source != "system:j.service" {
		t.Fatalf("Top = %+v", top)
	}
	if top[0].Agent != "agent:claude:p" || top[1].Agent != "" {
		t.Errorf("agents = %q, %q", top[0].Agent, top[1].Agent)
	}
	if want := float64(600<<20) * 20 * 6; top[0].BytesPerHour < want*0.99 || top[0].BytesPerHour > want*1.01 {
		t.Errorf("rate = %v, want %v", top[0].BytesPerHour, want)
	}
}

// What no source measured (the kernel and filesystem journal, other users,
// io not delegated) is named "other" in Top, as the report does: the host
// over the window minus the sources, ended tails taken back included.
func TestAlarmTopOther(t *testing.T) {
	a := NewAlarm(thresh, window)
	evs, _ := feed(a, 1, 20, func(i int) Delta {
		d := Delta{Host: perStep(100), Sources: map[string]int64{"session:s.scope": perStep(20), "system:ended": perStep(20)}}
		if i%2 == 0 { // a missed cgroup: ended shows it first, and gives it back
			d.Sources["system:ended"] = -perStep(10)
		}
		return d
	})
	if len(evs) != 1 {
		t.Fatalf("events = %+v", evs)
	}
	top := evs[0].Top
	if len(top) < 2 || top[0].Source != "other" || top[0].Agent != "" || top[1].Source != "session:s.scope" {
		t.Fatalf("Top = %+v", top)
	}
	// 100 − 20 − (20 − 10)/2 = 75 GiB/h
	if want := float64(75 << 30); top[0].BytesPerHour < want*0.99 || top[0].BytesPerHour > want*1.01 {
		t.Errorf("other = %v, want %v", top[0].BytesPerHour, want)
	}
}

func TestAlarmDisabled(t *testing.T) {
	a := NewAlarm(0, window)
	evs, _ := feed(a, 1, 100, func(int) Delta { return host(100 * gib) })
	if len(evs) != 0 {
		t.Fatalf("events = %+v", evs)
	}
}

func TestAlarmOneEventPerEpisodeAndNegativesClamped(t *testing.T) {
	a := NewAlarm(thresh, window)
	hi := host(perStep(100))
	evs, _ := feed(a, 1, 100, func(i int) Delta {
		if i%2 == 0 {
			return host(-50 * gib) // counter reset noise must not cancel writes
		}
		return host(hi.Host * 2)
	})
	if len(evs) != 1 || !evs[0].High {
		t.Fatalf("events = %+v", evs)
	}
}

// perStep is the bytes written in one 30 s step at g GiB per hour.
func perStep(g int64) int64 { return g << 30 / 120 }

// feedEvery is feed with its own sampling interval.
func feedEvery(a *Alarm, every time.Duration, from, n int, f func(i int) Delta) (evs []AlarmEvent, at []int) {
	for i := from; i < from+n; i++ {
		t := time.Unix(alarmT0, 0).Add(time.Duration(i) * every)
		if ev, ok := a.Observe(t, every, f(i)); ok {
			evs = append(evs, ev)
			at = append(at, i)
		}
	}
	return
}

// A window that is not a multiple of five sampling intervals: each sample's
// writes spread over the parts its interval overlaps, so a steady rate above
// the threshold shows in every part instead of piling up in the part where
// two samples end.
func TestAlarmWindowNotDivisibleBySamples(t *testing.T) {
	for name, c := range map[string]struct {
		every, window time.Duration
		perSample     int64
		fire          bool
	}{
		// 900 MiB/min = 52.7 GiB/h over 50: five 72 s parts of one-minute samples.
		"1m samples, 6m window, 900 MiB/min": {time.Minute, 6 * time.Minute, 900 * mib, true},
		"1m samples, 6m window, 800 MiB/min": {time.Minute, 6 * time.Minute, 800 * mib, false},
		"30s samples, 7m window, 52 GiB/h":   {step, 7 * time.Minute, perStep(52), true},
		"45s samples, 10m window, 51 GiB/h":  {45 * time.Second, window, perStep(51) * 3 / 2, true},
	} {
		a := NewAlarm(thresh, c.window)
		evs, _ := feedEvery(a, c.every, 1, 60, func(int) Delta { return host(c.perSample) })
		if got := len(evs) == 1 && evs[0].High; got != c.fire {
			t.Errorf("%s: events = %+v, want fired=%v", name, evs, c.fire)
		}
	}
}

// A window that is not a whole number of samples takes only the part of its
// oldest sample that falls inside it, for the average as for the parts: with
// 1m samples and a 6m30s window, a steady 24 GiB/h after an alarm is below
// half of 50 GiB/h and must clear, not read as 24×7/6.5 = 25.8 GiB/h.
func TestAlarmWindowTakesOverlapOfOldestSample(t *testing.T) {
	a := NewAlarm(thresh, 6*time.Minute+30*time.Second)
	perMin := func(g int64) int64 { return g << 30 / 60 }
	evs, _ := feedEvery(a, time.Minute, 1, 20, func(int) Delta {
		return Delta{Host: perMin(100), Sources: map[string]int64{"aq": perMin(100)}}
	})
	if len(evs) != 1 || !evs[0].High {
		t.Fatalf("raise: %+v", evs)
	}
	// Seven sample ends in the window, six and a half minutes of writes.
	if want := float64(100 << 30); evs[0].BytesPerHour > want*1.001 {
		t.Errorf("raise rate = %v, want %v", evs[0].BytesPerHour, want)
	}
	if want := uint64(perMin(100)) * 13 / 2; evs[0].Written > want+want/1000 {
		t.Errorf("raise written = %d, want %d", evs[0].Written, want)
	}
	if r := evs[0].Top[0].BytesPerHour; r > float64(100<<30)*1.001 {
		t.Errorf("raise top rate = %v", r)
	}
	evs, _ = feedEvery(a, time.Minute, 21, 30, func(int) Delta {
		return Delta{Host: perMin(24), Sources: map[string]int64{"aq": perMin(24)}}
	})
	if len(evs) != 1 || evs[0].High {
		t.Fatalf("clear: %+v", evs)
	}
}
