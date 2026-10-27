//go:build integration

package linux

import (
	"runtime"
	"testing"
)

func TestHostLive(t *testing.T) {
	h := NewHost("/proc")
	m, err := h.Memory()
	if err != nil {
		t.Fatal(err)
	}
	if m.Total == 0 || m.Available > m.Total {
		t.Fatalf("bad meminfo %+v", m)
	}
	if _, err := h.PressureFullAvg10(); err != nil {
		t.Fatal(err)
	}
	if _, err := h.KernelOOMKills(); err != nil {
		t.Fatal(err)
	}
	if free, err := h.DiskFree(t.TempDir()); err != nil || free == 0 {
		t.Fatalf("disk free %d %v", free, err)
	}
	if h.NumCPU() != runtime.NumCPU() {
		t.Fatalf("NumCPU %d", h.NumCPU())
	}
}
