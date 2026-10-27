package linux

import (
	"os"
	"testing"

	"resource-dispatcher/internal/platform"
)

var _ platform.HostMetrics = (*Host)(nil)

func open(t *testing.T, name string) *os.File {
	t.Helper()
	f, err := os.Open("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func TestParseMeminfo(t *testing.T) {
	m, err := parseMeminfo(open(t, "meminfo"))
	if err != nil {
		t.Fatal(err)
	}
	want := platform.MemInfo{Total: 33348911104, Available: 12000000 * 1024, SwapTotal: 8388604 * 1024, SwapFree: 248196 * 1024}
	if m != want {
		t.Fatalf("got %+v want %+v", m, want)
	}
}

func TestParseMeminfoMissingTotal(t *testing.T) {
	if _, err := parseMeminfo(open(t, "vmstat")); err == nil {
		t.Fatal("want error")
	}
}

func TestParsePSI(t *testing.T) {
	v, err := parsePSIFullAvg10(open(t, "psi_memory"))
	if err != nil || v != 7.25 {
		t.Fatalf("got %v, %v", v, err)
	}
}

func TestParseVMStat(t *testing.T) {
	v, err := parseVMStatOOMKill(open(t, "vmstat"))
	if err != nil || v != 8 {
		t.Fatalf("got %v, %v", v, err)
	}
	v, err = parseVMStatOOMKill(open(t, "vmstat_nooom"))
	if err != nil || v != 0 {
		t.Fatalf("missing line: got %v, %v", v, err)
	}
}

func TestHostFromFixtures(t *testing.T) {
	h := NewHost("testdata/proc")
	if m, err := h.Memory(); err != nil || m.Total != 33348911104 {
		t.Fatalf("%+v %v", m, err)
	}
	if v, err := h.PressureFullAvg10(); err != nil || v != 7.25 {
		t.Fatalf("%v %v", v, err)
	}
	if v, err := h.KernelOOMKills(); err != nil || v != 8 {
		t.Fatalf("%v %v", v, err)
	}
}
