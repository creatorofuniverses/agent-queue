package admission

import "testing"

func TestGuardCeiling(t *testing.T) {
	tests := []struct {
		name   string
		in     GuardInput
		want   uint64
		active bool
	}{
		{"tight", GuardInput{Pool: 16 * gib, SliceCurrent: 2 * gib, MemAvailable: 4 * gib, Floor: 3 * gib, Min: 512 * mib}, 3 * gib, true},
		{"roomy", GuardInput{Pool: 16 * gib, SliceCurrent: 2 * gib, MemAvailable: 20 * gib, Floor: 3 * gib, Min: 512 * mib}, 16 * gib, false},
		{"floor min", GuardInput{Pool: 16 * gib, MemAvailable: 1 * gib, Floor: 3 * gib, Min: 512 * mib}, 512 * mib, true},
	}
	for _, tt := range tests {
		got, active := GuardCeiling(tt.in)
		if got != tt.want || active != tt.active {
			t.Errorf("%s: got %d,%v want %d,%v", tt.name, got, active, tt.want, tt.active)
		}
	}
}

func TestGuardApply(t *testing.T) {
	if !GuardApply(10*gib, 9*gib) {
		t.Error("decrease must apply")
	}
	if GuardApply(10*gib, 10*gib+32*mib) {
		t.Error("small increase must not apply")
	}
	if !GuardApply(10*gib, 10*gib+64*mib) {
		t.Error("64M increase must apply")
	}
}
