package diskacct

import (
	"maps"
	"slices"
	"testing"

	"github.com/creatorofuniverses/agent-queue/internal/platform"
)

const (
	mib = int64(1) << 20
	gib = int64(1) << 30

	umib = uint64(mib)
	ugib = uint64(gib)
)

// agentProc is an agent process; Start is its PID so tests read simply.
func agentProc(pid, ppid int, name, project, within string, bytes int64) platform.ProcIO {
	return platform.ProcIO{PID: pid, PPID: ppid, Start: uint64(pid), Agent: name, Project: project, Within: within, Bytes: bytes}
}

func plain(pid, ppid int, bytes int64) platform.ProcIO {
	return platform.ProcIO{PID: pid, PPID: ppid, Start: uint64(pid), Within: "session:session-3.scope", Bytes: bytes}
}

func procs(ps ...platform.ProcIO) platform.DiskSnapshot {
	return platform.DiskSnapshot{Procs: ps}
}

// agentTotal steps through the snapshots after the first (the baseline) and
// sums every agent delta, failing on a delta under another key.
func agentTotal(t *testing.T, key string, snaps ...platform.DiskSnapshot) (total int64, steps []int64) {
	t.Helper()
	a := New()
	if _, ok := a.Step(snaps[0]); ok {
		t.Fatal("the first Step must be a baseline")
	}
	for i, s := range snaps[1:] {
		d, ok := a.Step(s)
		if !ok {
			t.Fatalf("step %d: not ok", i+1)
		}
		var step int64
		for _, ad := range d.Agents {
			if ad.Key != key {
				t.Fatalf("step %d: delta under %q, want only %q: %+v", i+1, ad.Key, key, d.Agents)
			}
			step += ad.Bytes
		}
		steps = append(steps, step)
		total += step
	}
	return total, steps
}

func TestStepBaseline(t *testing.T) {
	s := platform.DiskSnapshot{
		Host:    5000,
		Cgroups: []platform.CgroupIO{{Source: "system:a.service", ID: "system.slice/a.service", Parent: "system", Bytes: 100}, {Source: "aq", ID: "aq.slice", Bytes: 70}},
		Parents: map[string]uint64{"system": 900},
		Procs:   []platform.ProcIO{agentProc(10, 1, "claude", "/p", "session:s.scope", 300), plain(11, 10, 40)},
	}
	a := New()
	if d, ok := a.Step(s); ok {
		t.Fatalf("first Step = %+v, true; want a baseline", d)
	}
	d, ok := a.Step(s)
	if !ok {
		t.Fatal("second Step not ok")
	}
	if d.Host != 0 {
		t.Errorf("Host = %d", d.Host)
	}
	want := map[string]int64{"system:a.service": 0, "aq": 0, "system:ended": 0}
	if !maps.Equal(d.Sources, want) {
		t.Errorf("Sources = %v, want %v", d.Sources, want)
	}
	for _, ad := range d.Agents {
		if ad.Bytes != 0 {
			t.Errorf("agent delta %+v, want 0", ad)
		}
	}
}

func TestSourcesAndEnded(t *testing.T) {
	prev := platform.DiskSnapshot{
		Host: 10000,
		Cgroups: []platform.CgroupIO{
			{Source: "docker:a", ID: "system.slice/docker-a.scope", Parent: "system", Bytes: 300},
			{Source: "system:j", ID: "system.slice/j.service", Parent: "system", Bytes: 100},
			{Source: "system:old", ID: "system.slice/old.service", Parent: "system", Bytes: 50},
		},
		Parents: map[string]uint64{"system": 900},
	}
	cur := platform.DiskSnapshot{
		Host: 11000,
		Cgroups: []platform.CgroupIO{
			{Source: "docker:a", ID: "system.slice/docker-a.scope", Parent: "system", Bytes: 500},
			{Source: "system:j", ID: "system.slice/j.service", Parent: "system", Bytes: 150},
			{Source: "docker:b", ID: "system.slice/docker-b.scope", Parent: "system", Bytes: 120},
		},
		Parents: map[string]uint64{"system": 1500},
	}
	a := New()
	a.Step(prev)
	d, _ := a.Step(cur)
	want := map[string]int64{"docker:a": 200, "system:j": 50, "docker:b": 120, "system:ended": 230}
	if !maps.Equal(d.Sources, want) {
		t.Errorf("Sources = %v, want %v", d.Sources, want)
	}
	if d.Host != 1000 {
		t.Errorf("Host = %d, want 1000", d.Host)
	}
}

// Several containers of one compose project share a source key: their deltas add up.
func TestSourcesShareKey(t *testing.T) {
	snap := func(x, y uint64) platform.DiskSnapshot {
		return platform.DiskSnapshot{Cgroups: []platform.CgroupIO{
			{Source: "docker:proj", ID: "system.slice/docker-x.scope", Parent: "system", Bytes: x},
			{Source: "docker:proj", ID: "system.slice/docker-y.scope", Parent: "system", Bytes: y},
		}}
	}
	a := New()
	a.Step(snap(10, 20))
	d, _ := a.Step(snap(15, 60))
	if want := map[string]int64{"docker:proj": 45}; !maps.Equal(d.Sources, want) {
		t.Errorf("Sources = %v, want %v", d.Sources, want)
	}
}

// A counter that went backwards belongs to a cgroup re-created under the
// same path (a unit restart): it counts from zero, so the old one's lifetime
// does not land in "<parent>:ended". The ended remainder stays signed.
func TestSourcesRecreatedCountFromZero(t *testing.T) {
	a := New()
	a.Step(platform.DiskSnapshot{Host: 100,
		Cgroups: []platform.CgroupIO{{Source: "system:j.service", ID: "system.slice/j.service", Parent: "system", Bytes: 104 * ugib}},
		Parents: map[string]uint64{"system": 110 * ugib}})
	d, _ := a.Step(platform.DiskSnapshot{Host: 100 + 30*umib,
		Cgroups: []platform.CgroupIO{{Source: "system:j.service", ID: "system.slice/j.service", Parent: "system", Bytes: 20 * umib}},
		Parents: map[string]uint64{"system": 110*ugib + 30*umib}})
	if want := map[string]int64{"system:j.service": 20 * mib, "system:ended": 10 * mib}; !maps.Equal(d.Sources, want) {
		t.Errorf("Sources = %v, want %v", d.Sources, want)
	}
	d, _ = a.Step(platform.DiskSnapshot{Host: 100 + 30*umib,
		Cgroups: []platform.CgroupIO{{Source: "system:j.service", ID: "system.slice/j.service", Parent: "system", Bytes: 30 * umib}},
		Parents: map[string]uint64{"system": 110*ugib + 30*umib}})
	if want := map[string]int64{"system:j.service": 10 * mib, "system:ended": -10 * mib}; !maps.Equal(d.Sources, want) {
		t.Errorf("signed ended: Sources = %v, want %v", d.Sources, want)
	}
}

// A cgroup missing from one snapshot (a failed io.stat read) continues from
// its last counter when it comes back, instead of counting its lifetime again;
// what its parent showed meanwhile as ended is taken back from ended.
func TestSourcesMissingOneSample(t *testing.T) {
	a := New()
	s3 := func(b uint64, ok bool, parent uint64) platform.DiskSnapshot {
		s := platform.DiskSnapshot{Parents: map[string]uint64{"session": parent}}
		if ok {
			s.Cgroups = []platform.CgroupIO{{Source: "session:session-3.scope", ID: "user.slice/user-1000.slice/session-3.scope", Parent: "session", Bytes: b}}
		}
		return s
	}
	a.Step(s3(1200*ugib, true, 1300*ugib))
	d1, _ := a.Step(s3(0, false, 1300*ugib+ugib))
	d2, _ := a.Step(s3(1200*ugib+3*ugib, true, 1300*ugib+3*ugib))
	if got := d2.Sources["session:session-3.scope"]; got != 3*gib {
		t.Errorf("session-3 after a miss = %d, want %d", got, 3*gib)
	}
	sum := d1.Sources["session:ended"] + d2.Sources["session:ended"] + d2.Sources["session:session-3.scope"]
	if sum != 3*gib || d1.Sources["session:ended"]+d2.Sources["session:ended"] != 0 {
		t.Errorf("over both samples: ended %d + %d, session-3 %d; want the parent's 3 GiB, ended 0",
			d1.Sources["session:ended"], d2.Sources["session:ended"], d2.Sources["session:session-3.scope"])
	}
}

// A cgroup missing longer than missSamples is forgotten: when it comes back it
// is a new cgroup and counts from zero.
func TestSourcesMissingLongIsNew(t *testing.T) {
	a := New()
	c := []platform.CgroupIO{{Source: "user:x.service", ID: "u/x.service", Parent: "user", Bytes: 500}}
	a.Step(platform.DiskSnapshot{Cgroups: c})
	for range missSamples + 1 {
		a.Step(platform.DiskSnapshot{})
	}
	d, _ := a.Step(platform.DiskSnapshot{Cgroups: []platform.CgroupIO{{Source: "user:x.service", ID: "u/x.service", Parent: "user", Bytes: 600}}})
	if got := d.Sources["user:x.service"]; got != 600 {
		t.Errorf("x after a long absence = %d, want 600", got)
	}
}

// The host and the cgroups are counted per disk: a disk that goes away adds
// nothing (its counter does not turn the total negative), one that comes back
// re-attached counts from zero.
func TestDevicesVanishAndReturn(t *testing.T) {
	snap := func(nvme, usb uint64, withUSB bool) platform.DiskSnapshot {
		dev := map[string]uint64{"259:0": nvme}
		if withUSB {
			dev["8:16"] = usb
		}
		var sum uint64
		for _, v := range dev {
			sum += v
		}
		return platform.DiskSnapshot{Host: sum, Devices: dev,
			Cgroups: []platform.CgroupIO{{Source: "session:s.scope", ID: "user.slice/s.scope", Parent: "session", Bytes: sum, Devices: dev}},
			Parents: map[string]uint64{"session": sum}, ParentDevices: map[string]map[string]uint64{"session": dev}}
	}
	a := New()
	a.Step(snap(1000*ugib, 400*ugib, true))
	d, _ := a.Step(snap(1000*ugib+ugib, 0, false))
	if d.Host != gib || d.Sources["session:s.scope"] != gib || d.Sources["session:ended"] != 0 {
		t.Errorf("disk removed: Host %d, Sources %v; want 1 GiB each, ended 0", d.Host, d.Sources)
	}
	d, _ = a.Step(snap(1000*ugib+2*ugib, 5*umib, true))
	if d.Host != gib+5*mib || d.Sources["session:s.scope"] != gib+5*mib || d.Sources["session:ended"] != 0 {
		t.Errorf("disk back: Host %d, Sources %v; want 1 GiB + 5 MiB each, ended 0", d.Host, d.Sources)
	}
}

// aq's slices are children of the user manager: one re-created (its counter
// back near zero) counts from zero and leaves user:ended alone, where a parent
// pre-reduced by aq's counter would jump by aq's whole lifetime.
func TestSourcesRecreatedChildOfParent(t *testing.T) {
	snap := func(aq, x, user uint64) platform.DiskSnapshot {
		return platform.DiskSnapshot{Cgroups: []platform.CgroupIO{
			{Source: "aq", ID: "m/aq.slice", Parent: "user", Bytes: aq},
			{Source: "user:x.service", ID: "m/app.slice/x.service", Parent: "user", Bytes: x}},
			Parents: map[string]uint64{"user": user}}
	}
	a := New()
	a.Step(snap(600*umib, 300*umib, 1000*umib))
	d, _ := a.Step(snap(5*umib, 305*umib, 1010*umib))
	if want := map[string]int64{"aq": 5 * mib, "user:x.service": 5 * mib, "user:ended": 0}; !maps.Equal(d.Sources, want) {
		t.Errorf("Sources = %v, want %v", d.Sources, want)
	}
}

// No ended bucket when the parent is missing from either snapshot, and none
// for children without a parent (aq, cgroupfs containers).
func TestEndedNeedsParentInBoth(t *testing.T) {
	a := New()
	a.Step(platform.DiskSnapshot{
		Cgroups: []platform.CgroupIO{{Source: "aq", ID: "aq.slice", Bytes: 5}, {Source: "session:s.scope", ID: "user.slice/s.scope", Parent: "session", Bytes: 10}},
		Parents: map[string]uint64{"system": 100}})
	d, _ := a.Step(platform.DiskSnapshot{
		Cgroups: []platform.CgroupIO{{Source: "aq", ID: "aq.slice", Bytes: 9}, {Source: "session:s.scope", ID: "user.slice/s.scope", Parent: "session", Bytes: 30}},
		Parents: map[string]uint64{"session": 400}})
	if want := map[string]int64{"aq": 4, "session:s.scope": 20}; !maps.Equal(d.Sources, want) {
		t.Errorf("Sources = %v, want %v", d.Sources, want)
	}
}

// A snapshot whose usage scan failed steps the host and the sources but not
// the agent ledger: no process is taken for gone, and the next scan's delta
// covers the gap. Without a ledger yet, the next scan is its baseline.
func TestStepProcsUnknown(t *testing.T) {
	const key = "agent:claude:/p"
	snap := func(host uint64, agent int64, known bool) platform.DiskSnapshot {
		s := platform.DiskSnapshot{Host: host,
			Cgroups: []platform.CgroupIO{{Source: "session:s.scope", ID: "user.slice/s.scope", Parent: "session", Bytes: host}},
			Parents: map[string]uint64{"session": host}}
		if known {
			s.Procs = []platform.ProcIO{agentProc(10, 1, "claude", "/p", "session:s.scope", agent), plain(11, 10, agent/2)}
		} else {
			s.ProcsUnknown = true
		}
		return s
	}
	a := New()
	a.Step(snap(0, 0, true))
	d, ok := a.Step(snap(3*ugib, 0, false))
	if !ok || d.Host != 3*gib || d.Sources["session:s.scope"] != 3*gib || len(d.Agents) != 0 {
		t.Fatalf("unknown procs: %+v %v", d, ok)
	}
	d, _ = a.Step(snap(4*ugib, 2*gib, true))
	if len(d.Agents) != 1 || d.Agents[0].Key != key || d.Agents[0].Bytes != 3*gib {
		t.Fatalf("after the gap: agents %+v, want 3 GiB for %s", d.Agents, key)
	}

	a = New()
	a.Step(snap(100*ugib, 0, false)) // the ledger has no baseline yet
	d, _ = a.Step(snap(101*ugib, 50*gib, true))
	if d.Host != gib || len(d.Agents) != 0 {
		t.Fatalf("first scan after an unknown baseline: %+v", d)
	}
	d, _ = a.Step(snap(102*ugib, 51*gib, true))
	if len(d.Agents) != 1 || d.Agents[0].Bytes != gib+gib/2 {
		t.Fatalf("second scan: %+v", d.Agents)
	}
}

func TestAgentMultiGenerationReap(t *testing.T) {
	const key = "agent:claude:/p"
	A := func(b int64) platform.ProcIO { return agentProc(10, 1, "claude", "/p", "session:s.scope", b) }
	total, steps := agentTotal(t, key,
		procs(A(0)),
		procs(A(0), plain(11, 10, 0), plain(12, 11, gib)),
		procs(A(gib)), // B and C were reaped: C's 1G reached A through B
	)
	if total != gib {
		t.Errorf("agent total = %d (steps %v), want 1G", total, steps)
	}
}

func TestAgentReapDuringScan(t *testing.T) {
	const key = "agent:claude:/p"
	A := func(b int64) platform.ProcIO { return agentProc(10, 1, "claude", "/p", "session:s.scope", b) }
	total, steps := agentTotal(t, key,
		procs(A(0)),
		procs(A(0), plain(11, 10, gib)),
		procs(A(0)),   // A was read before it reaped B, B was gone when its turn came
		procs(A(gib)), // the transfer waited one interval
	)
	if total != gib {
		t.Errorf("agent total = %d (steps %v), want 1G", total, steps)
	}
}

// A process that vanishes while it still waits for a child's transfer hands
// that transfer on together with its own credited counter.
func TestAgentInheritedTransferMovesUp(t *testing.T) {
	const key = "agent:claude:/p"
	A := func(b int64) platform.ProcIO { return agentProc(10, 1, "claude", "/p", "session:s.scope", b) }
	total, steps := agentTotal(t, key,
		procs(A(0)),
		procs(A(0), plain(11, 10, 0), plain(12, 11, gib)),
		procs(A(0), plain(11, 10, 0)), // B was read before it reaped C: C's 1G waits at B
		procs(A(gib)),                 // A reaped B, whose total held C's 1G
	)
	if total != gib {
		t.Errorf("agent total = %d (steps %v), want 1G", total, steps)
	}
}

// A transfer the parent's counter never covers waits one interval, then is dropped.
func TestAgentUncoveredTransferExpires(t *testing.T) {
	const key = "agent:claude:/p"
	A := func(b int64) platform.ProcIO { return agentProc(10, 1, "claude", "/p", "session:s.scope", b) }
	total, steps := agentTotal(t, key,
		procs(A(0)),
		procs(A(0), plain(11, 10, gib)),
		procs(A(0)), // B vanished: 1G transferred, uncovered
		procs(A(0)), // still uncovered: dropped after this interval
		procs(A(gib)),
	)
	if total != 2*gib || steps[3] != gib {
		t.Errorf("agent total = %d (steps %v), want 2G with the last step 1G", total, steps)
	}
}

// A partly covered transfer is credited only up to the parent's growth; the
// rest waits.
func TestAgentPartialCover(t *testing.T) {
	const key = "agent:claude:/p"
	A := func(b int64) platform.ProcIO { return agentProc(10, 1, "claude", "/p", "session:s.scope", b) }
	_, steps := agentTotal(t, key,
		procs(A(0)),
		procs(A(0), plain(11, 10, 100*mib)),
		procs(A(40*mib)),  // 40M of B's 100M arrived (e.g. a second reaped child), 60M waits
		procs(A(160*mib)), // 60M of the rest is credited, 60M are A's own new writes
	)
	if want := []int64{100 * mib, 0, 60 * mib}; !slices.Equal(steps, want) {
		t.Errorf("steps = %v, want %v", steps, want)
	}
}

func TestAgentCancellation(t *testing.T) {
	const key = "agent:claude:/p"
	A := func(b int64) platform.ProcIO { return agentProc(10, 1, "claude", "/p", "session:s.scope", b) }
	total, steps := agentTotal(t, key, procs(), procs(A(100*mib)), procs(A(0)))
	if total != 0 || steps[1] != -100*mib {
		t.Errorf("agent total = %d (steps %v), want 0 with the second step -100M", total, steps)
	}
}

func TestAgentReparentedKeepsAttribution(t *testing.T) {
	const key = "agent:claude:/p"
	A := agentProc(10, 1, "claude", "/p", "session:s.scope", 0)
	total, steps := agentTotal(t, key,
		procs(),
		procs(A, plain(12, 10, 0)),
		procs(A, plain(12, 1, 50*mib)), // setsid nohup: C now belongs to init
	)
	if total != 50*mib || steps[1] != 50*mib {
		t.Errorf("agent total = %d (steps %v), want 50M in the second step", total, steps)
	}
}

func TestAgentUnseenChildViaParent(t *testing.T) {
	const key = "agent:claude:/p"
	A := func(b int64) platform.ProcIO { return agentProc(10, 1, "claude", "/p", "session:s.scope", b) }
	total, _ := agentTotal(t, key, procs(A(0)), procs(A(30*mib)))
	if total != 30*mib {
		t.Errorf("agent total = %d, want 30M", total)
	}
}

func TestAgentPIDReuse(t *testing.T) {
	const key = "agent:claude:/p"
	A := func(b int64) platform.ProcIO { return agentProc(10, 1, "claude", "/p", "session:s.scope", b) }
	reused := func(ppid int, b int64) platform.ProcIO {
		return platform.ProcIO{PID: 20, PPID: ppid, Start: 999, Within: "session:s.scope", Bytes: b}
	}
	// The new process under A counts from zero, not from the old one's 100M.
	// A has not reaped the old one yet: its 100M wait as a transfer.
	_, steps := agentTotal(t, key,
		procs(A(0)),
		procs(A(0), plain(20, 10, 100*mib)),
		procs(A(0), reused(10, 10*mib)),
	)
	if want := []int64{100 * mib, 10 * mib}; !slices.Equal(steps, want) {
		t.Errorf("under A: steps = %v, want %v", steps, want)
	}
	// Outside the tree, the new process does not inherit the old one's membership.
	_, steps = agentTotal(t, key,
		procs(A(0)),
		procs(A(0), plain(20, 10, 100*mib)),
		procs(A(0), reused(1, 300*mib)),
	)
	if want := []int64{100 * mib, 0}; !slices.Equal(steps, want) {
		t.Errorf("outside the tree: steps = %v, want %v", steps, want)
	}
}

// A process outside every agent tree is ignored, even in an agent's cgroup.
func TestAgentStrangerIgnored(t *testing.T) {
	a := New()
	a.Step(procs(agentProc(10, 1, "claude", "/p", "session:s.scope", 0), plain(30, 1, 0)))
	d, _ := a.Step(procs(agentProc(10, 1, "claude", "/p", "session:s.scope", 0), plain(30, 1, gib)))
	if len(d.Agents) != 0 {
		t.Errorf("Agents = %+v, want none", d.Agents)
	}
}

// An agent started by another agent heads its own tree.
func TestAgentNestedAgentOwnTree(t *testing.T) {
	a := New()
	a.Step(procs())
	d, _ := a.Step(procs(
		agentProc(10, 1, "claude", "/p", "session:s.scope", 1*mib),
		agentProc(11, 10, "codex", "/q", "session:s.scope", 2*mib),
		plain(12, 11, 4*mib),
		plain(13, 10, 8*mib),
	))
	want := []AgentDelta{
		{Key: "agent:claude:/p", Within: "session:s.scope", Bytes: 9 * mib},
		{Key: "agent:codex:/q", Within: "session:s.scope", Bytes: 6 * mib},
	}
	if !slices.Equal(d.Agents, want) {
		t.Errorf("Agents = %+v, want %+v", d.Agents, want)
	}
}

// The key and Within follow the root agent as it is now: a descendant's
// writes go to the root's current project and source.
func TestAgentKeyFollowsRoot(t *testing.T) {
	a := New()
	a.Step(procs(agentProc(10, 1, "claude", "/p", "session:s.scope", 0), plain(11, 10, 0)))
	d, _ := a.Step(procs(agentProc(10, 1, "claude", "/q", "user:tmux.scope", 0), plain(11, 10, 5*mib)))
	want := []AgentDelta{{Key: "agent:claude:/q", Within: "user:tmux.scope", Bytes: 5 * mib}}
	if !slices.Equal(d.Agents, want) {
		t.Errorf("Agents = %+v, want %+v", d.Agents, want)
	}
	// The root is gone: an orphan keeps the root's last key.
	d, _ = a.Step(procs(platform.ProcIO{PID: 11, PPID: 1, Start: 11, Within: "session:s.scope", Bytes: 7 * mib}))
	want = []AgentDelta{{Key: "agent:claude:/q", Within: "user:tmux.scope", Bytes: 2 * mib}}
	if !slices.Equal(d.Agents, want) {
		t.Errorf("after the root left: Agents = %+v, want %+v", d.Agents, want)
	}
}

// A live agent skipped for one sample (a failed read, a timed-out pass)
// reappears with the same PID+Start: it was never reaped, so it resumes from
// its last counter instead of counting its whole life again.
func TestAgentMissedSampleResumes(t *testing.T) {
	const key = "agent:claude:/p"
	A := agentProc(10, 1, "claude", "/p", "session:s.scope", 20*gib)
	total, steps := agentTotal(t, key, procs(A), procs(A), procs(), procs(A))
	if total != 0 {
		t.Errorf("agent total = %d (steps %v), want 0", total, steps)
	}
}

// A member child skipped for one sample takes back the transfer it handed
// its parent.
func TestAgentMissedChildTakesTransferBack(t *testing.T) {
	const key = "agent:claude:/p"
	A := agentProc(10, 1, "claude", "/p", "session:s.scope", 0)
	C := plain(11, 10, 10*gib)
	total, steps := agentTotal(t, key, procs(A, C), procs(A), procs(A, C), procs(A, C))
	if total != 0 {
		t.Errorf("agent total = %d (steps %v), want 0", total, steps)
	}
	// The parent's own writes while the child is back are not taken for the
	// child's transfer.
	total, steps = agentTotal(t, key, procs(A, C), procs(A), procs(agentProc(10, 1, "claude", "/p", "session:s.scope", 5*gib), C))
	if total != 5*gib {
		t.Errorf("with the parent writing: agent total = %d (steps %v), want 5G", total, steps)
	}
	// The same after the transfer waited its interval and was dropped: the
	// child still resumes from its last counter.
	total, steps = agentTotal(t, key, procs(A, C), procs(A), procs(A), procs(A), procs(A, C))
	if total != 0 {
		t.Errorf("after the transfer expired: agent total = %d (steps %v), want 0", total, steps)
	}
}

// A skipped process gets back the transfers it carried for its own reaped
// children: its counter, not its parent's, covers them.
func TestAgentMissedCarrierGetsTransfersBack(t *testing.T) {
	const key = "agent:claude:/p"
	A := agentProc(10, 1, "claude", "/p", "session:s.scope", 0)
	total, steps := agentTotal(t, key,
		procs(A),
		procs(A, plain(11, 10, 0), plain(12, 11, gib)),
		procs(A, plain(11, 10, 0)), // B was read before it reaped C: C's 1G waits at B
		procs(A),                   // B skipped: what it carried went up to A
		procs(A, plain(11, 10, gib)),
	)
	if total != gib {
		t.Errorf("agent total = %d (steps %v), want 1G", total, steps)
	}
}

// The whole tree skipped for one sample (its cgroup dropped from the pass).
func TestAgentMissedTreeResumes(t *testing.T) {
	const key = "agent:claude:/p"
	A := agentProc(10, 1, "claude", "/p", "session:s.scope", 5*gib)
	B := plain(11, 10, 3*gib)
	total, steps := agentTotal(t, key, procs(), procs(A, B), procs(), procs(A, B), procs(A, plain(11, 10, 4*gib)))
	if want := []int64{8 * gib, 0, 0, gib}; !slices.Equal(steps, want) || total != 9*gib {
		t.Errorf("steps = %v, want %v", steps, want)
	}
}

// A child with a negative net counter (it cancelled dirty pages of others)
// is netted when its parent reaps it, now or one interval later, and a skip
// of it adds no phantom writes.
func TestAgentNegativeTransfer(t *testing.T) {
	const key = "agent:claude:/p"
	A := func(b int64) platform.ProcIO { return agentProc(10, 1, "claude", "/p", "session:s.scope", b) }
	R := plain(11, 10, -10*mib)
	for name, snaps := range map[string][]platform.DiskSnapshot{
		"reaped at once":    {procs(A(0)), procs(A(0), R), procs(A(-10 * mib)), procs(A(-10 * mib))},
		"reaped next":       {procs(A(0)), procs(A(0), R), procs(A(0)), procs(A(-10 * mib))},
		"skipped, not gone": {procs(A(0)), procs(A(0), R), procs(A(0)), procs(A(0), R)},
	} {
		total, steps := agentTotal(t, key, snaps...)
		if total != -10*mib {
			t.Errorf("%s: agent total = %d (steps %v), want -10M", name, total, steps)
		}
	}
}

// Vanished processes are kept only a bounded number of samples.
func TestAgentTombstonesExpire(t *testing.T) {
	A := agentProc(10, 1, "claude", "/p", "session:s.scope", 0)
	a := New()
	a.Step(procs(A))
	for pid := 100; pid < 100+3*tombstoneSamples; pid++ {
		a.Step(procs(A, plain(pid, 10, 1)))
	}
	for range tombstoneSamples + 1 {
		a.Step(procs(A))
	}
	if len(a.procs) != 1 {
		t.Errorf("ledger holds %d entries, want only the agent", len(a.procs))
	}
}

// A process skipped around the reaps of its children: whatever happens while
// it is unseen, its children's writes are counted once. Each case is a tree
// A (agent) -> B -> D with D's 1G seen first.
func TestAgentSkippedReaper(t *testing.T) {
	const key = "agent:claude:/p"
	A := func(b int64) platform.ProcIO { return agentProc(10, 1, "claude", "/p", "session:s.scope", b) }
	B := func(ppid int, b int64) platform.ProcIO { return plain(11, ppid, b) }
	D := func(ppid int) platform.ProcIO { return plain(12, ppid, gib) }
	for name, c := range map[string]struct {
		snaps []platform.DiskSnapshot
		want  int64
	}{
		"B skipped while D seen, D reaped as B returns": {[]platform.DiskSnapshot{
			procs(A(0)), procs(A(0), B(10, 0), D(11)), procs(A(0), D(11)), procs(A(0), B(10, gib)),
		}, gib},
		"B skipped twice, D reaped meanwhile": {[]platform.DiskSnapshot{
			procs(A(0)), procs(A(0), B(10, 0), D(11)), procs(A(0), D(11)), procs(A(0)), procs(A(0), B(10, gib)),
		}, gib},
		"B and D skipped, D reaped, B back two samples later": {[]platform.DiskSnapshot{
			procs(A(0)), procs(A(0), B(10, 0), D(11)), procs(A(0)), procs(A(0)), procs(A(0), B(10, gib)),
		}, gib},
		"B and D skipped long, D reaped, B back": {[]platform.DiskSnapshot{
			procs(A(0)), procs(A(0), B(10, 0), D(11)), procs(A(0)), procs(A(0)), procs(A(0)), procs(A(0)), procs(A(0), B(10, gib)),
		}, gib},
		"B skipped, then D and B reaped before the next sample": {[]platform.DiskSnapshot{
			procs(A(0)), procs(A(0), B(10, 0), D(11)), procs(A(0), D(11)), procs(A(gib)),
		}, gib},
		"B skipped, D reaped, A writes on its own meanwhile": {[]platform.DiskSnapshot{
			procs(A(0)), procs(A(0), B(10, 0), D(11)), procs(A(0), D(11)), procs(A(3 * gib)), procs(A(3*gib), B(10, gib)),
		}, 4 * gib},
		"whole tree skipped, D reaped during the skip": {[]platform.DiskSnapshot{
			procs(A(0)), procs(A(0), B(10, 0), D(11)), procs(), procs(A(0), B(10, gib)),
		}, gib},
		"whole tree skipped long, D reaped during the skip": {[]platform.DiskSnapshot{
			procs(A(0)), procs(A(0), B(10, 0), D(11)), procs(), procs(), procs(), procs(), procs(A(0), B(10, gib)),
		}, gib},
	} {
		total, steps := agentTotal(t, key, c.snaps...)
		if total != c.want {
			t.Errorf("%s: agent total = %d (steps %v), want %d", name, total, steps, c.want)
		}
	}
}

// Cases around the skipped-reaper rules that must not change: PID reuse
// while the old process is a tombstone, a skipped child back under init
// after its holder was reaped, a reparent to the agent.
func TestAgentSkipNeighbours(t *testing.T) {
	const key = "agent:claude:/p"
	A := func(b int64) platform.ProcIO { return agentProc(10, 1, "claude", "/p", "session:s.scope", b) }
	for name, c := range map[string]struct {
		snaps []platform.DiskSnapshot
		want  int64
	}{
		"PID reuse while the old one is a tombstone": {[]platform.DiskSnapshot{
			procs(A(0)), procs(A(0), plain(11, 10, gib)), procs(A(gib)),
			procs(A(gib), platform.ProcIO{PID: 11, PPID: 10, Start: 99}),
			procs(A(gib), platform.ProcIO{PID: 11, PPID: 10, Start: 99, Bytes: 50 * mib}),
		}, gib + 50*mib},
		"C skipped, its holder B reaped, C back under init": {[]platform.DiskSnapshot{
			procs(A(0)), procs(A(0), plain(11, 10, 0), plain(12, 11, gib)), procs(A(0), plain(11, 10, 0)), procs(A(0)), procs(A(0), plain(12, 1, gib)),
		}, gib},
		"reparent to A after B reaped, then C reaped": {[]platform.DiskSnapshot{
			procs(A(0)), procs(A(0), plain(11, 10, 0), plain(12, 11, gib)), procs(A(0), plain(12, 10, gib)), procs(A(gib)), procs(A(gib)),
		}, gib},
	} {
		total, steps := agentTotal(t, key, c.snaps...)
		if total != c.want {
			t.Errorf("%s: agent total = %d (steps %v), want %d", name, total, steps, c.want)
		}
	}
}

// Transfers of opposite signs are not netted against each other while they
// wait: each stays with its origin, so a child that was only skipped takes
// back exactly its own, whatever its sibling's did.
func TestAgentMixedSignTransfersKeepOrigin(t *testing.T) {
	const key = "agent:claude:/p"
	A := func(b int64) platform.ProcIO { return agentProc(10, 1, "claude", "/p", "session:s.scope", b) }
	W := plain(11, 10, 100*mib)
	C := plain(12, 10, -50*mib)
	for name, snaps := range map[string][]platform.DiskSnapshot{
		"W reaped, C skipped and back": {procs(A(0)), procs(A(0), W, C), procs(A(0)), procs(A(100*mib), C), procs(A(100*mib), C)},
		"C reaped, W skipped and back": {procs(A(0)), procs(A(0), W, C), procs(A(0)), procs(A(-50*mib), W), procs(A(-50*mib), W)},
		"both reaped at once":          {procs(A(0)), procs(A(0), W, C), procs(A(50 * mib)), procs(A(50 * mib))},
		"both reaped a sample late":    {procs(A(0)), procs(A(0), W, C), procs(A(0)), procs(A(50 * mib))},
	} {
		total, steps := agentTotal(t, key, snaps...)
		if total != 50*mib {
			t.Errorf("%s: agent total = %d (steps %v), want 50M", name, total, steps)
		}
	}
}

// An agent's tree stops at its cgroup: a child started in another cgroup
// (another agent's, here) is that cgroup's source's, not the agent's, and so
// is what it starts there; a member that moves to another cgroup is no
// longer counted from then on. Reaping such a child adds nothing either.
func TestAgentTreeStopsAtCgroup(t *testing.T) {
	const (
		cgA = "user.slice/user-1000.slice/session-a.scope"
		cgB = "user.slice/user-1000.slice/user@1000.service/app.slice/b.scope"
	)
	in := func(p platform.ProcIO, cg string) platform.ProcIO {
		p.Cgroup = cg
		p.Within = map[string]string{cgA: "session:session-a.scope", cgB: "user:b.scope"}[cg]
		return p
	}
	A := func(b int64) platform.ProcIO { return in(agentProc(10, 1, "claude", "/p", "", b), cgA) }
	B := func(b int64) platform.ProcIO { return in(plain(11, 10, b), cgB) } // A's shell, in b.scope
	D := func(b int64) platform.ProcIO { return in(plain(12, 11, b), cgB) } // what B runs there
	C := func(b int64) platform.ProcIO { return in(agentProc(20, 1, "codex", "/q", "", b), cgB) }
	M := func(cg string, b int64) platform.ProcIO { return in(plain(13, 10, b), cg) } // A's member, moved later
	a := New()
	steps := [][]platform.ProcIO{
		{A(0), B(0), D(0), C(0), M(cgA, 0)},
		{A(0), B(100 * mib), D(10 * mib), C(1 * mib), M(cgA, 5*mib)},
		{A(0), B(100 * mib), D(10 * mib), C(1 * mib), M(cgB, 8*mib)},
		{A(110 * mib), C(1 * mib), M(cgB, 9*mib)}, // A reaped B, which had reaped D
	}
	want := [][]AgentDelta{
		nil,
		{{Key: "agent:claude:/p", Within: "session:session-a.scope", Bytes: 5 * mib}, {Key: "agent:codex:/q", Within: "user:b.scope", Bytes: 1 * mib}},
		nil,
		nil,
	}
	for i, ps := range steps {
		d, _ := a.Step(platform.DiskSnapshot{Procs: ps})
		if !slices.Equal(d.Agents, want[i]) {
			t.Errorf("step %d: Agents = %+v, want %+v", i, d.Agents, want[i])
		}
	}
}
