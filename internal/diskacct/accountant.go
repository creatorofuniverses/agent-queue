// Package diskacct turns successive raw disk-write snapshots into deltas per
// interval: level-1 sources (cgroups and the "<parent>:ended" tails of the
// ones that ended) and the level-2 estimate of what agent process trees wrote.
// It is pure: no I/O, no clock.
package diskacct

import (
	"cmp"
	"slices"

	"github.com/creatorofuniverses/agent-queue/internal/platform"
)

// AgentDelta is what one agent's tree wrote in an interval, by its process
// counters: an estimate inside its source, not a part of the source sums.
type AgentDelta struct {
	Key    string // "agent:<name>:<project>"
	Within string // the source key the agent root lives in
	Bytes  int64
}

// Delta is what was written between two snapshots. Every value is signed: a
// counter that went backwards (a cgroup re-created under the same path, or
// cancelled process writes) yields a negative delta; reports clamp.
type Delta struct {
	Host    int64
	Sources map[string]int64 // level 1 and "*:ended"; "other" is the report's
	Agents  []AgentDelta     // sorted by Key, Within; zero deltas left out
}

// Accountant remembers the previous snapshot and the agent-tree ledger.
type Accountant struct {
	started bool
	host    uint64
	cgroups map[string]uint64 // by CgroupIO.ID
	parents map[string]uint64
	procs   map[procKey]*proc
}

// procKey identifies a process across PID reuse: a key that reappears is
// the same process, never reaped.
type procKey struct {
	pid   int
	start uint64
}

// tombstoneSamples is how many consecutive samples a vanished process may be
// missing (a failed read, a skipped cgroup, a pass cut short) and still
// resume from its last counter. It outlives every transfer, which lives two.
const tombstoneSamples = 10

// proc is the ledger entry of one process seen in an agent's cgroup.
//
// last is its counter at the last sample and is also what aq has credited of
// it: every interval credits exactly raw = cur - last, part as growth and part
// as a covered transfer. A process that vanishes hands last, and the
// transfers it still holds, to its last known parent, whose counter gets
// them when it reaps the child.
type proc struct {
	last    int64
	parent  procKey // last known parent; zero when it was outside the snapshot
	member  bool    // once in an agent tree, always in it
	root    procKey // the agent root it was first seen under
	key     string  // the root's agent key, as last seen
	within  string  // the root's source, as last seen
	pending []transfer
	gone    int // samples since it vanished; 0 while alive
}

// transfer is credited counter handed over by a vanished process, kept by
// origin and route so that a process that reappears takes back exactly what
// is still pending.
type transfer struct {
	origin   procKey   // whose last counter this is
	carriers []procKey // who held it before the current holder, first holder first
	amount   int64
	aged     bool // waited one interval already: dropped after this one
}

// New returns an Accountant whose first Step is a baseline.
func New() *Accountant {
	return &Accountant{procs: map[procKey]*proc{}}
}

// Step accounts s against the previous snapshot. The first call only records
// the baseline and returns false: what was written before it is nobody's.
func (a *Accountant) Step(s platform.DiskSnapshot) (Delta, bool) {
	if !a.started {
		a.started = true
		a.procStep(s.Procs)
		a.remember(s)
		return Delta{}, false
	}
	d := Delta{Host: int64(s.Host) - int64(a.host), Sources: a.sources(s), Agents: a.procStep(s.Procs)}
	a.remember(s)
	return d, true
}

func (a *Accountant) remember(s platform.DiskSnapshot) {
	a.host = s.Host
	a.cgroups = make(map[string]uint64, len(s.Cgroups))
	for _, c := range s.Cgroups {
		a.cgroups[c.ID] = c.Bytes
	}
	a.parents = make(map[string]uint64, len(s.Parents))
	for p, b := range s.Parents {
		a.parents[p] = b
	}
}

// sources sums cgroup deltas by source key. A cgroup new since the last
// snapshot counts from zero; one that disappeared adds nothing here, its tail
// shows in "<parent>:ended" = Δparent − Σ Δ of the parent's live children.
func (a *Accountant) sources(s platform.DiskSnapshot) map[string]int64 {
	out := map[string]int64{}
	children := map[string]int64{}
	for _, c := range s.Cgroups {
		d := int64(c.Bytes) - int64(a.cgroups[c.ID])
		out[c.Source] += d
		if c.Parent != "" {
			children[c.Parent] += d
		}
	}
	for p, cur := range s.Parents {
		if prev, ok := a.parents[p]; ok {
			out[p+":ended"] = int64(cur) - int64(prev) - children[p]
		}
	}
	return out
}

// procStep updates the ledger with the processes of one sample and returns
// the agents' growth since the previous one.
func (a *Accountant) procStep(ps []platform.ProcIO) []AgentDelta {
	index := make(map[int]int, len(ps)) // PID -> position in ps
	alive := make(map[procKey]bool, len(ps))
	for i, p := range ps {
		index[p.PID] = i
		k := procKey{p.PID, p.Start}
		alive[k] = true
		if st := a.procs[k]; st != nil && st.gone > 0 {
			a.revive(k)
		}
	}
	a.handOver(alive)

	type sum struct{ key, within string }
	sums := map[sum]int64{}
	done := make([]bool, len(ps))
	var visit func(i int)
	visit = func(i int) {
		if done[i] {
			return
		}
		done[i] = true
		p := ps[i]
		k := procKey{p.PID, p.Start}
		st := a.procs[k]
		if st == nil { // new since the last sample: counts from zero
			st = &proc{}
			a.procs[k] = st
		}
		var pst *proc // the parent, already visited
		st.parent = procKey{}
		if j, ok := index[p.PPID]; ok && p.PPID != p.PID {
			visit(j)
			st.parent = procKey{p.PPID, ps[j].Start}
			pst = a.procs[st.parent]
		}
		switch {
		case p.Agent != "": // an agent heads its own tree, even inside another's
			key := "agent:" + p.Agent + ":" + p.Project
			if p.Project != "" || st.root != k || st.key == "" { // keep the project over a failed read
				st.key = key
			}
			st.member, st.root, st.within = true, k, p.Within
		case st.member && st.root == k: // a root that is no agent any more keeps its key
			st.within = p.Within
		case st.member: // follow the root's current key while it lives
			if j, ok := index[st.root.pid]; ok && ps[j].Start == st.root.start {
				visit(j)
				r := a.procs[st.root]
				st.key, st.within = r.key, r.within
			}
		case pst != nil && pst.member:
			st.member, st.root, st.key, st.within = true, pst.root, pst.key, pst.within
		}

		raw := p.Bytes - st.last
		credit, rest := settle(st.pending, raw)
		st.pending, st.last = rest, p.Bytes
		if growth := raw - credit; st.member && growth != 0 {
			sums[sum{st.key, st.within}] += growth
		}
	}
	for i := range ps {
		visit(i)
	}
	a.age()

	var out []AgentDelta
	for s, b := range sums {
		if b != 0 {
			out = append(out, AgentDelta{Key: s.key, Within: s.within, Bytes: b})
		}
	}
	slices.SortFunc(out, func(x, y AgentDelta) int {
		return cmp.Or(cmp.Compare(x.Key, y.Key), cmp.Compare(x.Within, y.Within))
	})
	return out
}

// settle credits a live process's pending transfers against its counter's
// change raw and returns the credit and what may wait one more interval.
// A positive total is credited up to the positive part of raw, a negative
// one down to the negative part: a transfer the counter has not caught up
// with waits instead of turning into growth, so a child that was only
// skipped, not reaped, takes it back without a trace.
func settle(pending []transfer, raw int64) (int64, []transfer) {
	var total int64
	for _, t := range pending {
		total += t.amount
	}
	credit := max(total, min(0, raw))
	if total >= 0 {
		credit = min(total, max(0, raw))
	}
	// The oldest is credited first: of the remainder, only this interval's
	// transfers of its sign wait, the freshest first; the aged rest is dropped.
	left := total - credit
	var out []transfer
	for i := len(pending) - 1; i >= 0 && left != 0; i-- {
		t := pending[i]
		if t.aged || t.amount == 0 || (t.amount > 0) != (left > 0) {
			continue
		}
		if (left > 0 && t.amount > left) || (left < 0 && t.amount < left) {
			t.amount = left
		}
		left -= t.amount
		t.aged = true
		out = append(out, t)
	}
	slices.Reverse(out)
	return credit, out
}

// handOver turns the processes that vanished since the last sample into
// tombstones. Bottom-up through every generation that vanished together, each
// hands its last counter and the transfers it holds to its last known parent
// while that one is tracked and was alive; otherwise the transfers stay with
// the tombstone and expire there.
func (a *Accountant) handOver(alive map[procKey]bool) {
	var gone []procKey
	for k, st := range a.procs {
		if st.gone == 0 && !alive[k] {
			gone = append(gone, k)
		}
	}
	vanishing := make(map[procKey]bool, len(gone))
	for _, k := range gone {
		vanishing[k] = true
	}
	depth := map[procKey]int{}
	var depthOf func(k procKey, hops int) int
	depthOf = func(k procKey, hops int) int {
		if d, ok := depth[k]; ok {
			return d
		}
		d := 0
		if p := a.procs[k].parent; vanishing[p] && hops < len(gone) { // hops: a stale link loop cannot recurse forever
			d = 1 + depthOf(p, hops+1)
		}
		depth[k] = d
		return d
	}
	slices.SortFunc(gone, func(x, y procKey) int {
		return cmp.Or(cmp.Compare(depthOf(y, 0), depthOf(x, 0)), compareKeys(x, y))
	})
	for _, k := range gone {
		st := a.procs[k]
		st.gone = 1
		h := a.procs[st.parent]
		if h == nil || h.gone > 0 {
			continue
		}
		for _, t := range st.pending {
			t.carriers = append(slices.Clone(t.carriers), k)
			t.aged = false // the hand-over is news of this interval
			h.pending = append(h.pending, t)
		}
		st.pending = nil
		if st.last != 0 {
			h.pending = append(h.pending, transfer{origin: k, amount: st.last})
		}
	}
}

// revive brings back a process that was missing from earlier samples but
// never reaped. It takes back what it handed over and is still pending: its
// own counter goes nowhere, and transfers it carried return to it, since
// its counter, not its parent's, covers them. What a parent already credited
// stays credited.
func (a *Accountant) revive(k procKey) {
	st := a.procs[k]
	st.gone = 0
	keys := make([]procKey, 0, len(a.procs))
	for hk := range a.procs {
		if hk != k {
			keys = append(keys, hk)
		}
	}
	slices.SortFunc(keys, compareKeys)
	for _, hk := range keys {
		h := a.procs[hk]
		kept := h.pending[:0]
		for _, t := range h.pending {
			i := slices.Index(t.carriers, k)
			switch {
			case t.origin == k:
			case i >= 0:
				t.carriers = t.carriers[:i:i]
				st.pending = append(st.pending, t)
			default:
				kept = append(kept, t)
			}
		}
		h.pending = kept
	}
}

// age ends the interval for tombstones: their aged transfers are dropped,
// the rest wait one more, and a tombstone missing too long is forgotten.
func (a *Accountant) age() {
	for k, st := range a.procs {
		if st.gone == 0 {
			continue
		}
		if st.gone > tombstoneSamples {
			delete(a.procs, k)
			continue
		}
		st.gone++
		kept := st.pending[:0]
		for _, t := range st.pending {
			if !t.aged {
				t.aged = true
				kept = append(kept, t)
			}
		}
		st.pending = kept
	}
}

func compareKeys(x, y procKey) int {
	return cmp.Or(cmp.Compare(x.pid, y.pid), cmp.Compare(x.start, y.start))
}
