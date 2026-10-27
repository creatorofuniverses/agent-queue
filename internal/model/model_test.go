package model

import (
	"regexp"
	"testing"
)

func TestStateTerminal(t *testing.T) {
	term := map[State]bool{
		StateSucceeded: true, StateFailed: true, StateCancelled: true, StateRejected: true,
		StateQueued: false, StateStarting: false, StateRunning: false, StateCleaning: false, StateRecovering: false,
	}
	for s, want := range term {
		if s.Terminal() != want {
			t.Errorf("%s.Terminal() = %v, want %v", s, !want, want)
		}
	}
}

func TestParsePriority(t *testing.T) {
	cases := map[string]Priority{"low": -10, "normal": 0, "high": 100, "": 0}
	for in, want := range cases {
		got, err := ParsePriority(in)
		if err != nil || got != want {
			t.Errorf("ParsePriority(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	if _, err := ParsePriority("urgent"); err == nil {
		t.Error("ParsePriority(urgent) want error")
	}
}

func TestNewIDUnique(t *testing.T) {
	re := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	seen := map[ItemID]bool{}
	for range 1000 {
		id := NewID()
		if !re.MatchString(string(id)) {
			t.Fatalf("bad id format %q", id)
		}
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
	}
}

func TestCommitted(t *testing.T) {
	it := &Item{Expected: 5, CPUs: 2}
	if r := it.Committed(); r != (Resources{MemoryBytes: 5, CPUs: 2}) {
		t.Errorf("Committed() = %+v", r)
	}
}

func TestReservationStatesTerminal(t *testing.T) {
	for _, s := range []State{StateTornDown, StateReleased} {
		if !s.Terminal() || s.Held() {
			t.Errorf("%s: Terminal=%v Held=%v, want true/false", s, s.Terminal(), s.Held())
		}
	}
	for _, s := range []State{StateGranted, StateExpiring, StateTearingDown, StateTeardownFailed, StateTeardownUnknown} {
		if s.Terminal() || !s.Held() {
			t.Errorf("%s: Terminal=%v Held=%v, want false/true", s, s.Terminal(), s.Held())
		}
	}
	if StateQueued.Held() {
		t.Error("QUEUED must not be Held")
	}
	// TerminalStates is the single source for SQL and must agree with Terminal().
	got := map[State]bool{}
	for _, s := range TerminalStates() {
		if !s.Terminal() {
			t.Errorf("TerminalStates has non-terminal %s", s)
		}
		got[s] = true
	}
	if len(got) != 6 {
		t.Errorf("TerminalStates = %v, want 6 distinct states", TerminalStates())
	}
}

func TestCommittedByKind(t *testing.T) {
	const g = uint64(1) << 30
	task := &Item{Kind: KindTask, Expected: 2 * g, CPUs: 2}
	if r := task.Committed(); r != (Resources{2 * g, 2}) {
		t.Errorf("task: %+v", r)
	}
	task.ParentID = "res"
	if r := task.Committed(); r != (Resources{}) {
		t.Errorf("nested task: %+v", r)
	}
	res := &Item{Kind: KindReservation, Expected: g, CPUs: 1, TaskMemory: 3 * g, TaskCPUs: 4}
	if !res.IsReservation() || task.IsReservation() {
		t.Error("IsReservation wrong")
	}
	if r := res.Committed(); r != (Resources{4 * g, 5}) {
		t.Errorf("reservation: %+v", r)
	}
}

func TestTeardownUnitID(t *testing.T) {
	if got := TeardownUnitID("abc"); got != "teardown-abc" {
		t.Errorf("got %q", got)
	}
}

func TestIsTeardownUnitID(t *testing.T) {
	for id, want := range map[ItemID]bool{TeardownUnitID("abc"): true, "abc": false, "teardow-x": false, "": false} {
		if got := IsTeardownUnitID(id); got != want {
			t.Errorf("IsTeardownUnitID(%q) = %v, want %v", id, got, want)
		}
	}
}

func TestChannelHelpers(t *testing.T) {
	c := ExecChannel("codex")
	if c != "exec:codex" {
		t.Fatal(c)
	}
	if a, ok := c.ExecAdapter(); !ok || a != "codex" {
		t.Fatal(a, ok)
	}
	if a, ok := ChannelWebhook.ExecAdapter(); ok || a != "" {
		t.Fatal(a, ok)
	}
	for _, ch := range []Channel{ChannelWebhook, c, ChannelClientWait, ChannelNone} {
		if ch.Retried() != (ch == c || ch == ChannelWebhook) {
			t.Fatal(ch)
		}
	}
}
func TestReasonInfra(t *testing.T) {
	for _, r := range []Reason{ReasonLaunchFailed, ReasonLost, ReasonStorageFull, ReasonOOMHostGuard} {
		if !r.Infra() {
			t.Fatal(r)
		}
	}
	for _, r := range []Reason{ReasonOOM, ReasonTimeout, ReasonCommandFailed, ReasonCancelled, ReasonNone} {
		if r.Infra() {
			t.Fatal(r)
		}
	}
}
