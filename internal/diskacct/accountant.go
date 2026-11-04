// Package diskacct turns successive raw disk-write snapshots into deltas per
// interval: level-1 sources (cgroups and the "<parent>:ended" tails of the
// ones that ended) and the level-2 estimate of what agent process trees wrote.
// It is pure: no I/O, no clock.
package diskacct

import (
	"cmp"
	"maps"
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

// Delta is what was written between two snapshots. Host and the cgroup
// sources are never negative (a counter that went backwards was re-created
// and counts from zero). An "*:ended" remainder and an agent delta are signed:
// a cgroup's writes counted as ended while it was missing are taken back, and
// cancelled process writes shrink an agent; reports clamp.
type Delta struct {
	Host    int64
	Sources map[string]int64 // level 1 and "*:ended"; "other" is the report's
	Agents  []AgentDelta     // sorted by Key, Within; zero deltas left out
}

// Accountant remembers the previous snapshot and the agent-tree ledger.
type Accountant struct {
	started bool
	host    counter
	cgroups map[string]*cgroup // by CgroupIO.ID, the missing ones too
	parents map[string]counter
	procs   map[procKey]*proc
}

// counter is a cumulative write counter by physical disk; the key "" stands
// for the sum when the disks are not known.
type counter map[string]uint64

func counterOf(devices map[string]uint64, sum uint64) counter {
	if devices == nil {
		return counter{"": sum}
	}
	return maps.Clone(devices)
}

// since is what was written from prev to c, disk by disk. A disk whose
// counter went backwards belongs to a counter re-created since (a cgroup
// re-created under the same path, a disk re-attached) and counts from zero,
// as does a disk new to it; a disk that went away adds nothing.
func (c counter) since(prev counter) int64 {
	var d int64
	for dev, cur := range c {
		if p, ok := prev[dev]; ok && cur >= p {
			d += int64(cur - p)
		} else {
			d += int64(cur)
		}
	}
	return d
}

// cgroup is the last counter of a cgroup and how many samples it has been
// missing from.
type cgroup struct {
	last    counter
	missing int
}

// procKey identifies a process across PID reuse: a key that reappears is
// the same process, never reaped.
type procKey struct {
	pid   int
	start uint64
}

// tombstoneSamples is how many consecutive samples a vanished process may be
// missing (a failed read, a skipped cgroup, a pass cut short) and still
// resume from its last counter and take back the transfers it handed over or
// carried: those wait as long as it is a tombstone.
const tombstoneSamples = 10

// missSamples is how many consecutive samples a cgroup may be missing (a
// failed io.stat read) and still continue from its last counter.
const missSamples = 10

// proc is the ledger entry of one process seen in an agent's cgroup.
//
// last is its counter at the last sample and is also what aq has credited of
// it: every interval credits exactly raw = cur - last, part as growth and part
// as a covered transfer. A process that vanishes hands last, and the
// transfers it still holds, to its last known parent, whose counter gets
// them when it reaps the child.
type proc struct {
	last    int64
	parent  procKey // last known parent; zero when it was outside the snapshot and not a tombstone
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
//
// A transfer is held by the nearest live ancestor of its origin, since that
// one's counter gets it on a reap, whether the processes in between were
// reaped too or only skipped. Those in between are its carriers. While a
// carrier is a tombstone, the transfer cannot expire: the carrier may come
// back and take it. With no live ancestor it stays with the topmost tombstone.
type transfer struct {
	origin   procKey   // whose last counter this is
	carriers []procKey // the tombstones between origin and holder, lowest first
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
		a.sources(s)
		return Delta{}, false
	}
	host := counterOf(s.Devices, s.Host)
	d := Delta{Host: host.since(a.host), Sources: a.sources(s), Agents: a.procStep(s.Procs)}
	return d, true
}

// sources sums cgroup deltas by source key and remembers the counters. A
// cgroup new since the last snapshot counts from zero; one that disappeared
// adds nothing here, its tail shows in "<parent>:ended" = Δparent − Σ Δ of
// the parent's live children. A cgroup missing from up to missSamples
// snapshots continues from its last counter when it is back: its writes
// meanwhile showed in ended and are taken back from ended now.
func (a *Accountant) sources(s platform.DiskSnapshot) map[string]int64 {
	a.host = counterOf(s.Devices, s.Host)
	if a.cgroups == nil {
		a.cgroups = map[string]*cgroup{}
	}
	out := map[string]int64{}
	children := map[string]int64{}
	seen := make(map[string]bool, len(s.Cgroups))
	for _, c := range s.Cgroups {
		cur := counterOf(c.Devices, c.Bytes)
		var d int64
		if g := a.cgroups[c.ID]; g != nil {
			d = cur.since(g.last)
		} else {
			d = cur.since(nil)
		}
		a.cgroups[c.ID] = &cgroup{last: cur}
		seen[c.ID] = true
		if c.Source != "" { // "": no source, only a child of its parent
			out[c.Source] += d
		}
		if c.Parent != "" {
			children[c.Parent] += d
		}
	}
	for id, g := range a.cgroups {
		if seen[id] {
			continue
		}
		if g.missing++; g.missing > missSamples {
			delete(a.cgroups, id)
		}
	}
	parents := make(map[string]counter, len(s.Parents))
	for p, sum := range s.Parents {
		cur := counterOf(s.ParentDevices[p], sum)
		parents[p] = cur
		if prev, ok := a.parents[p]; ok {
			out[p+":ended"] = cur.since(prev) - children[p]
		}
	}
	a.parents = parents
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
		if j, ok := index[p.PPID]; ok && p.PPID != p.PID {
			visit(j)
			st.parent = procKey{p.PPID, ps[j].Start}
			pst = a.procs[st.parent]
		} else if e := a.procs[st.parent]; e == nil || e.gone == 0 || st.parent.pid != p.PPID {
			// While the PPID still names the last known parent, now a
			// tombstone, that parent lives (an orphan gets a new PPID): it
			// was only skipped, and the link stays.
			st.parent = procKey{}
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
		credit, rest := a.settle(st.pending, raw)
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
// change raw and returns the credit and what may wait.
// A positive total is credited up to the positive part of raw, a negative
// one down to the negative part: a transfer the counter has not caught up
// with waits instead of turning into growth, so a child that was only
// skipped, not reaped, takes it back without a trace.
func (a *Accountant) settle(pending []transfer, raw int64) (int64, []transfer) {
	var total int64
	for _, t := range pending {
		total += t.amount
	}
	credit := max(total, min(0, raw))
	if total >= 0 {
		credit = min(total, max(0, raw))
	}
	// Of the remainder, transfers of its sign are kept: first the pinned
	// ones (a carrier may take them back, so they are credited last), then
	// this interval's, the freshest first, which age. Aged ones are dropped.
	left := total - credit
	order := make([]int, 0, len(pending))
	for i := len(pending) - 1; i >= 0; i-- {
		if a.pinned(pending[i]) {
			order = append(order, i)
		}
	}
	for i := len(pending) - 1; i >= 0; i-- {
		if !a.pinned(pending[i]) && !pending[i].aged {
			order = append(order, i)
		}
	}
	keep := make([]bool, len(pending))
	out := slices.Clone(pending)
	for _, i := range order {
		t := &out[i]
		if left == 0 {
			break
		}
		if t.amount == 0 || (t.amount > 0) != (left > 0) {
			continue
		}
		if (left > 0 && t.amount > left) || (left < 0 && t.amount < left) {
			t.amount = left
		}
		left -= t.amount
		t.aged = t.aged || !a.pinned(*t)
		keep[i] = true
	}
	kept := out[:0]
	for i, t := range out {
		if keep[i] {
			kept = append(kept, t)
		}
	}
	return credit, kept
}

// pinned reports whether a carrier of t is a tombstone that may come back.
func (a *Accountant) pinned(t transfer) bool {
	for _, c := range t.carriers {
		if e := a.procs[c]; e != nil && e.gone > 0 {
			return true
		}
	}
	return false
}

// handOver turns the processes that vanished since the last sample into
// tombstones. Each hands its last counter, and the transfers it holds, to
// its nearest live ancestor through the tombstones in between (see transfer).
func (a *Accountant) handOver(alive map[procKey]bool) {
	var gone []procKey
	for k, st := range a.procs {
		if st.gone == 0 && !alive[k] {
			gone = append(gone, k)
		}
	}
	slices.SortFunc(gone, compareKeys)
	for _, k := range gone {
		a.procs[k].gone = 1
	}
	for _, k := range gone {
		st := a.procs[k]
		holder, route := k, []procKey(nil) // route: the tombstones above k, below the holder
		for p := st.parent; len(route) <= len(a.procs); {
			e := a.procs[p]
			if e == nil {
				break
			}
			if holder != k {
				route = append(route, holder)
			}
			holder = p
			if e.gone == 0 {
				break
			}
			p = e.parent
		}
		if holder == k {
			continue // nobody above: its transfers wait on its tombstone
		}
		h := a.procs[holder]
		for _, t := range st.pending {
			t.carriers = slices.Concat(t.carriers, []procKey{k}, route)
			t.aged = false // the hand-over is news of this interval
			h.pending = append(h.pending, t)
		}
		st.pending = nil
		if st.last != 0 {
			h.pending = append(h.pending, transfer{origin: k, carriers: route, amount: st.last})
		}
	}
}

// revive brings back a process that was missing from earlier samples but
// never reaped. It takes back what it handed over and is still pending: its
// own counter goes nowhere, and transfers it carried return to it, since
// its counter, not its ancestor's, covers them. What an ancestor already
// credited stays credited.
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
				t.carriers = t.carriers[:i:i] // pinned so far, so never aged
				st.pending = append(st.pending, t)
			default:
				kept = append(kept, t)
			}
		}
		h.pending = kept
	}
}

// age forgets tombstones missing too long, with the transfers they hold.
// Transfers held by a tombstone do not expire before: nobody's counter can
// show them meanwhile.
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
	}
}

func compareKeys(x, y procKey) int {
	return cmp.Or(cmp.Compare(x.pid, y.pid), cmp.Compare(x.start, y.start))
}
