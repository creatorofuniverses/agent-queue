package diskacct

import (
	"cmp"
	"slices"
	"time"
)

const alarmParts = 5

// SourceRate is one source's write rate over the window or episode. Agent is
// the best estimate inside the source, when there is one.
type SourceRate struct {
	Source, Agent string
	BytesPerHour  float64
}

// AlarmEvent is a raise (High) or an all-clear.
type AlarmEvent struct {
	High         bool
	BytesPerHour float64
	Written      uint64        // over the window (raise) or the episode (all-clear)
	Since        time.Duration // length of that window or episode
	Top          []SourceRate  // up to 3, descending
}

type alarmSample struct {
	at, from time.Time // the sample covers (from, at]
	d        Delta
}

// Alarm decides from per-sample deltas when the host has written heavily for
// long enough, and when that is over. Pure: time only comes from Observe.
type Alarm struct {
	rate   uint64 // bytes per hour; 0 disables
	window time.Duration

	samples []alarmSample
	first   time.Time // start of the first sample's interval

	active bool
	since  time.Time // episode start: the window start at the raise
	ep     tally
}

// NewAlarm returns an alarm for a threshold in bytes per hour (0 never fires)
// and a window length, which the caller guarantees is positive.
func NewAlarm(rate uint64, window time.Duration) *Alarm {
	return &Alarm{rate: rate, window: window}
}

// Observe feeds the delta of one sample taken at at over the last span. It
// returns an event when the alarm rises or clears.
func (a *Alarm) Observe(at time.Time, span time.Duration, d Delta) (AlarmEvent, bool) {
	if a.first.IsZero() {
		a.first = at.Add(-span)
	}
	a.samples = append(a.samples, alarmSample{at: at, from: at.Add(-span), d: d})
	start := at.Add(-a.window)
	i := 0
	for i < len(a.samples) && !a.samples[i].at.After(start) {
		i++
	}
	a.samples = a.samples[i:]
	if a.active {
		a.ep.add(d)
	}
	if a.rate == 0 || a.first.After(start) {
		return AlarmEvent{}, false
	}

	var win tally
	var parts [alarmParts]float64
	part := a.window / alarmParts
	for _, s := range a.samples {
		win.add(s.d)
		spread(&parts, start, part, a.window, s)
	}
	perHour := func(b float64, over time.Duration) float64 {
		return b * float64(time.Hour) / float64(over)
	}
	avg := perHour(float64(win.host), a.window)

	if !a.active {
		hot := 0
		for _, b := range parts {
			if perHour(b, part) >= float64(a.rate) {
				hot++
			}
		}
		if hot < 4 {
			return AlarmEvent{}, false
		}
		a.active, a.since = true, start
		a.ep = win
		return AlarmEvent{High: true, BytesPerHour: avg, Written: uint64(win.host),
			Since: a.window, Top: win.top(a.window)}, true
	}
	if avg < float64(a.rate)/2 {
		over := at.Sub(a.since)
		ev := AlarmEvent{BytesPerHour: avg, Written: uint64(a.ep.host), Since: over, Top: a.ep.top(over)}
		a.active = false
		a.ep = tally{}
		return ev, true
	}
	return AlarmEvent{}, false
}

// spread adds a sample's host writes to the parts of the window (start,
// start+end] that its interval (from, at] overlaps, in proportion to the overlap:
// the writes happened somewhere in that interval, so a steady rate fills
// every part evenly whatever the sampling interval, and a sample no longer
// than a part touches at most two. What fell before start is outside the
// window.
func spread(parts *[alarmParts]float64, start time.Time, part, end time.Duration, s alarmSample) {
	b := float64(max(0, s.d.Host))
	span := s.at.Sub(s.from)
	if span <= 0 {
		idx := min(max(int((s.at.Sub(start)-1)/part), 0), alarmParts-1)
		parts[idx] += b
		return
	}
	from, at := s.from.Sub(start), s.at.Sub(start) // offsets into the window
	for i := range alarmParts {
		lo, hi := time.Duration(i)*part, time.Duration(i+1)*part
		if i == alarmParts-1 {
			hi = end // the window may not split into five whole nanoseconds
		}
		if ov := min(hi, at) - max(lo, from); ov > 0 {
			parts[i] += b * float64(ov) / float64(span)
		}
	}
}

// tally sums deltas: the host's clamped per sample (a counter reset must not
// cancel writes), the sources' and agents' signed, so that an ended tail
// given back later cancels out, as in the report; top clamps.
type tally struct {
	host    int64
	sources map[string]int64
	agents  map[string]AgentDelta // by key; Bytes accumulated
}

func (t *tally) add(d Delta) {
	t.host += max(0, d.Host)
	for k, v := range d.Sources {
		if t.sources == nil {
			t.sources = map[string]int64{}
		}
		t.sources[k] += v
	}
	for _, ag := range d.Agents {
		if t.agents == nil {
			t.agents = map[string]AgentDelta{}
		}
		cur := t.agents[ag.Key]
		cur.Key, cur.Within = ag.Key, ag.Within
		cur.Bytes += ag.Bytes
		t.agents[ag.Key] = cur
	}
}

// top is the three sources that wrote the most, "other" (the host minus all
// sources: what no source measured) among them.
func (t *tally) top(over time.Duration) []SourceRate {
	var out []SourceRate
	rate := func(b int64) float64 { return float64(b) * float64(time.Hour) / float64(over) }
	other := t.host
	for k, b := range t.sources {
		other -= b
		if b <= 0 {
			continue
		}
		best := AgentDelta{}
		for _, ag := range t.agents {
			if ag.Within != k {
				continue
			}
			if ag.Bytes > best.Bytes || (ag.Bytes == best.Bytes && ag.Key < best.Key) {
				best = ag
			}
		}
		out = append(out, SourceRate{Source: k, Agent: best.Key, BytesPerHour: rate(b)})
	}
	if other > 0 {
		out = append(out, SourceRate{Source: "other", BytesPerHour: rate(other)})
	}
	slices.SortFunc(out, func(x, y SourceRate) int {
		return cmp.Or(cmp.Compare(y.BytesPerHour, x.BytesPerHour), cmp.Compare(x.Source, y.Source))
	})
	if len(out) > 3 {
		out = out[:3]
	}
	return out
}
