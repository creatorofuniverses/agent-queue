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

// procKey identifies a process across PID reuse.
type procKey struct {
	pid   int
	start uint64
}

// proc is the ledger entry of one process seen in an agent's cgroup.
//
// last is its counter at the last sample and is also what aq has credited of
// it: every interval credits exactly raw = cur - last, part as growth and part
// as a covered transfer. A process that vanishes hands last plus its
// uncovered inherited transfer to its last known parent, whose counter gets
// the total when it reaps the child.
type proc struct {
	last   int64
	parent procKey // last known parent; zero when it was outside the snapshot
	member bool    // once in an agent tree, always in it
	root   procKey // the agent root it was first seen under
	key    string  // the root's agent key, as last seen
	within string  // the root's source, as last seen
	// waiting is the uncovered transfer from the previous interval: its last
	// chance before it is dropped. fresh is the transfer of this interval.
	waiting, fresh int64
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
		alive[procKey{p.PID, p.Start}] = true
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
		transfer := st.waiting + st.fresh
		credit := min(transfer, max(0, raw))
		left := transfer - credit
		st.waiting = 0
		if left > 0 { // only this interval's transfer may wait one more
			st.waiting = min(left, max(0, st.fresh))
		}
		st.fresh, st.last = 0, p.Bytes
		if growth := raw - credit; st.member && growth != 0 {
			sums[sum{st.key, st.within}] += growth
		}
	}
	for i := range ps {
		visit(i)
	}

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

// handOver drops the processes that vanished since the last sample. Bottom-up
// through every generation that vanished in this interval, each hands what
// was credited for it (last) plus what it inherited and its counter did not
// yet cover to its last known parent, as that parent's fresh transfer.
func (a *Accountant) handOver(alive map[procKey]bool) {
	kids := map[procKey][]procKey{}
	var tops []procKey
	for k, st := range a.procs {
		if alive[k] {
			continue
		}
		if _, tracked := a.procs[st.parent]; tracked && !alive[st.parent] {
			kids[st.parent] = append(kids[st.parent], k)
		} else {
			tops = append(tops, k)
		}
	}
	seen := map[procKey]bool{}
	var total func(k procKey) int64
	total = func(k procKey) int64 {
		if seen[k] { // a stale link loop; cannot happen with start times
			return 0
		}
		seen[k] = true
		st := a.procs[k]
		t := st.last + st.waiting + st.fresh
		for _, c := range kids[k] {
			t += total(c)
		}
		return t
	}
	for _, k := range tops {
		t := total(k)
		if pst, ok := a.procs[a.procs[k].parent]; ok {
			pst.fresh += t
		}
	}
	for k := range a.procs {
		if !alive[k] {
			delete(a.procs, k)
		}
	}
}
