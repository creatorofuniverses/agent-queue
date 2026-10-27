package admission

import (
	"errors"
	"testing"

	"resource-dispatcher/internal/model"
)

const (
	mib = uint64(1) << 20
	gib = uint64(1) << 30
)

func basePolicy() Policy {
	return Policy{
		Pool:         model.Resources{MemoryBytes: 16 * gib, CPUs: 16},
		ReserveFloor: 3 * gib,
		MinDiskFree:  2 * gib,
	}
}

func okSnap() Snapshot {
	return Snapshot{MemAvailable: 32 * gib, DiskFree: 100 * gib}
}

func req(mem uint64, cpus int) model.Resources {
	return model.Resources{MemoryBytes: mem, CPUs: cpus}
}

func TestDecideOrder(t *testing.T) {
	p := basePolicy()
	tests := []struct {
		name string
		mod  func(*Snapshot)
		want Blocker
	}{
		{"none", func(s *Snapshot) {}, BlockNone},
		{"recovering+pressure", func(s *Snapshot) { s.Recovering, s.PressureClosed = true, true }, BlockRecovering},
		{"pressure+disk", func(s *Snapshot) { s.PressureClosed, s.DiskFree = true, 0 }, BlockPressure},
		{"disk+pool", func(s *Snapshot) { s.DiskFree = 0; s.Committed.MemoryBytes = 16 * gib }, BlockDisk},
		{"pool mem+pool cpu", func(s *Snapshot) { s.Committed = req(16*gib, 16) }, BlockPoolMemory},
		{"pool cpu+live", func(s *Snapshot) { s.Committed.CPUs = 16; s.MemAvailable = 0 }, BlockPoolCPU},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := okSnap()
			tt.mod(&s)
			d := Decide(p, s, req(1*gib, 1))
			if d.Blocker != tt.want || d.Admit != (tt.want == BlockNone) {
				t.Fatalf("got %+v, want blocker %q", d, tt.want)
			}
			if !d.Admit && d.Detail == "" {
				t.Fatal("empty detail")
			}
		})
	}
}

func TestDecidePoolMemory(t *testing.T) {
	p := basePolicy()
	s := okSnap()
	s.Committed = req(14*gib, 1)
	if d := Decide(p, s, req(3*gib, 1)); d.Blocker != BlockPoolMemory {
		t.Fatalf("3G: %+v", d)
	}
	if d := Decide(p, s, req(2*gib, 1)); d.Blocker == BlockPoolMemory {
		t.Fatalf("2G: %+v", d)
	}
}

func TestDecideLiveMemory(t *testing.T) {
	p := basePolicy()
	s := Snapshot{MemAvailable: 8 * gib, Unconsumed: 2 * gib, DiskFree: 100 * gib}
	if d := Decide(p, s, req(4*gib, 1)); d.Blocker != BlockLiveMemory {
		t.Fatalf("4G: %+v", d)
	}
	if d := Decide(p, s, req(3*gib, 1)); !d.Admit {
		t.Fatalf("3G: %+v", d)
	}
}

func TestDecideUnconsumedExceedsAvailable(t *testing.T) {
	s := Snapshot{MemAvailable: 1 * gib, Unconsumed: 5 * gib, DiskFree: 100 * gib}
	d := Decide(basePolicy(), s, req(1*mib, 1))
	if d.Admit || d.Blocker != BlockLiveMemory {
		t.Fatalf("got %+v", d)
	}
}

func TestValidate(t *testing.T) {
	p := basePolicy()
	tests := []struct {
		name string
		req  model.Resources
		want error
	}{
		{"ok", req(gib, 1), nil},
		{"exact pool", req(16*gib, 16), nil},
		{"zero mem", req(0, 1), ErrInvalid},
		{"zero cpu", req(gib, 0), ErrInvalid},
		{"negative cpu", req(gib, -1), ErrInvalid},
		{"too much mem", req(17*gib, 1), ErrTooLarge},
		{"too many cpu", req(gib, 17), ErrTooLarge},
	}
	for _, tt := range tests {
		if err := Validate(p, tt.req); !errors.Is(err, tt.want) {
			t.Errorf("%s: got %v, want %v", tt.name, err, tt.want)
		}
	}
}
