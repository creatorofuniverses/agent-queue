package admission

import (
	"agent-queue/internal/model"
	"math"
	"slices"
	"sort"
	"time"
)

const Unbounded = 100 * 365 * 24 * time.Hour
const BlockQueue Blocker = "queue"

func (b Blocker) Gate() bool { return b == BlockRecovering || b == BlockPressure || b == BlockDisk }

type Capacity struct {
	Pool, Live int64
	CPUs       int
}

func signed(v uint64) int64 {
	if v > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(v)
}
func add(a, b int64) int64 {
	if b > 0 && a > math.MaxInt64-b {
		return math.MaxInt64
	}
	if b < 0 && a < math.MinInt64-b {
		return math.MinInt64
	}
	return a + b
}
func FreeCapacity(p Policy, s Snapshot) Capacity {
	return Capacity{add(signed(p.Pool.MemoryBytes), -signed(s.Committed.MemoryBytes)), add(add(signed(s.MemAvailable), -signed(s.Unconsumed)), -signed(p.ReserveFloor)), p.Pool.CPUs - s.Committed.CPUs}
}
func (c Capacity) Fits(r model.Resources) bool {
	return c.Pool >= 0 && c.Live >= 0 && r.MemoryBytes <= uint64(c.Pool) && r.MemoryBytes <= uint64(c.Live) && r.CPUs <= c.CPUs
}
func (c Capacity) take(r model.Resources) Capacity {
	c.Pool = add(c.Pool, -signed(r.MemoryBytes))
	c.Live = add(c.Live, -signed(r.MemoryBytes))
	c.CPUs -= r.CPUs
	return c
}
func (c Capacity) release(h Holder) Capacity {
	c.Pool = add(c.Pool, signed(h.Res.MemoryBytes))
	c.Live = add(c.Live, signed(h.Live))
	c.CPUs += h.Res.CPUs
	return c
}

type Holder struct {
	ID                 model.ItemID
	Res                model.Resources
	Live               uint64
	End                time.Time
	Never, FromHistory bool
	// Teardown: the holder releases through a teardown slot (a reservation).
	Teardown bool
}
type Waiting struct {
	ID                     model.ItemID
	Res                    model.Resources
	Duration               time.Duration
	FromHistory, ExtraOnly bool
}

func ordered(holders []Holder) []Holder {
	hs := slices.Clone(holders)
	sort.SliceStable(hs, func(i, j int) bool {
		if hs[i].Never != hs[j].Never {
			return !hs[i].Never
		}
		return hs[i].End.Before(hs[j].End)
	})
	return hs
}

// Shadow returns the first release time at which need fits, and the remaining capacity.
func Shadow(now time.Time, free Capacity, holders []Holder, need model.Resources) (time.Time, Capacity, bool) {
	hs := ordered(holders)
	at := now
	i := 0
	for {
		for i < len(hs) && !hs[i].Never && !hs[i].End.After(at) {
			free = free.release(hs[i])
			i++
		}
		if free.Fits(need) {
			return at, free.take(need), true
		}
		if i >= len(hs) || hs[i].Never {
			return time.Time{}, Capacity{}, false
		}
		at = hs[i].End
	}
}

type Forecast struct {
	Start time.Time
	OK    bool
}

func Schedule(now time.Time, free Capacity, holders []Holder, queue []Waiting) []Forecast {
	out := make([]Forecast, len(queue))
	hs := ordered(holders)
	at := now
	for i, q := range queue {
		for {
			for len(hs) > 0 && !hs[0].Never && !hs[0].End.After(at) {
				free = free.release(hs[0])
				hs = hs[1:]
			}
			if free.Fits(q.Res) {
				break
			}
			if len(hs) == 0 || hs[0].Never {
				return out
			}
			at = hs[0].End
		}
		out[i] = Forecast{at, true}
		free = free.take(q.Res)
		hs = append(hs, Holder{ID: q.ID, Res: q.Res, Live: q.Res.MemoryBytes, End: at.Add(q.Duration), FromHistory: q.FromHistory})
		hs = ordered(hs)
	}
	return out
}

type Pass struct {
	Admitted     []int
	Backfilled   []bool
	Head         int
	HeadDecision Decision
	After        Snapshot
	Holders      []Holder
}

// teardownSlotFree reports whether one more reservation can be backfilled without
// its teardown delaying a teardown the head waits on: either the head waits on
// none before the shadow, or every pending teardown, the new one included, gets a
// slot at once (a user release can start any of them early).
func teardownSlotFree(p Policy, holders []Holder, shadow time.Time) bool {
	pending, needed := 0, false
	for _, h := range holders {
		if !h.Teardown || h.Never {
			continue
		}
		pending++
		if !h.End.After(shadow) {
			needed = true
		}
	}
	return !needed || pending+1 <= max(p.TeardownSlots, 1)
}

func PlanPass(p Policy, s Snapshot, holders []Holder, queue []Waiting, now time.Time, backfill bool) Pass {
	out := Pass{Backfilled: make([]bool, len(queue)), Head: -1, HeadDecision: Decision{Admit: true}, After: s, Holders: slices.Clone(holders)}
	admit := func(i int, bf bool) {
		q := queue[i]
		out.Admitted = append(out.Admitted, i)
		out.Backfilled[i] = bf
		out.After.Committed.MemoryBytes = satAdd(out.After.Committed.MemoryBytes, q.Res.MemoryBytes)
		out.After.Committed.CPUs += q.Res.CPUs
		out.After.Unconsumed = satAdd(out.After.Unconsumed, q.Res.MemoryBytes)
		out.Holders = append(out.Holders, Holder{ID: q.ID, Res: q.Res, Live: q.Res.MemoryBytes, End: now.Add(q.Duration), FromHistory: q.FromHistory, Teardown: q.ExtraOnly})
	}
	for i, q := range queue {
		dec := Decide(p, out.After, q.Res)
		if !dec.Admit {
			out.Head = i
			out.HeadDecision = dec
			break
		}
		admit(i, false)
	}
	if out.Head < 0 || !backfill || out.HeadDecision.Blocker.Gate() {
		return out
	}
	shadow, extra, ok := Shadow(now, FreeCapacity(p, out.After), out.Holders, queue[out.Head].Res)
	if !ok {
		return out
	}
	for i := out.Head + 1; i < len(queue); i++ {
		q := queue[i]
		if !Decide(p, out.After, q.Res).Admit {
			continue
		}
		if q.ExtraOnly && !teardownSlotFree(p, out.Holders, shadow) {
			continue
		}
		byTime := !q.ExtraOnly && !now.Add(q.Duration).After(shadow)
		if byTime {
			admit(i, true)
		} else if extra.Fits(q.Res) {
			admit(i, true)
			extra = extra.take(q.Res)
		}
	}
	return out
}
