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
