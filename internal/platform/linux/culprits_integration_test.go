//go:build integration

package linux

import "testing"

func TestMemorySnapshotOnThisHost(t *testing.T) {
	v, err := MemorySnapshot("/proc", "/sys/fs/cgroup", 5)
	if err != nil {
		t.Fatal(err)
	}
	if v.Available == 0 || len(v.Cgroups) == 0 || len(v.Processes) == 0 || v.Processes[0].RSS == 0 {
		t.Fatalf("%+v", v)
	}
	t.Logf("%+v", v)
}
