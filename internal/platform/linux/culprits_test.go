package linux

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/creatorofuniverses/agent-queue/internal/platform"
)

func TestMemorySnapshotNamesTheLargest(t *testing.T) {
	dir := t.TempDir()
	proc, cg := filepath.Join(dir, "proc"), filepath.Join(dir, "cg")
	write := func(p, s string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	const gib = 1 << 30
	write(filepath.Join(proc, "meminfo"), "MemTotal:       31457280 kB\nMemFree:         2097152 kB\nMemAvailable:    3145728 kB\n")
	// 30 - 2 = 28 GiB in use, cgroups account for 6+5+3 = 14: 14 GiB is in none.
	write(filepath.Join(cg, "memory.stat"), "anon 6442450944\nfile 5368709120\nkernel 3221225472\nsock 0\n")
	write(filepath.Join(cg, "user.slice/memory.current"), "9000000000\n") // not a leaf: its children count
	write(filepath.Join(cg, "user.slice/app-big.scope/memory.current"), "6442450944\n")
	write(filepath.Join(cg, "user.slice/app-small.scope/memory.current"), "104857600\n")
	write(filepath.Join(cg, "system.slice/docker-x.scope/memory.current"), "2147483648\n")
	proc1 := func(pid, name, rssKB, cgroup string) {
		write(filepath.Join(proc, pid, "status"), "Name:\t"+name+"\nVmRSS:\t"+rssKB+" kB\n")
		write(filepath.Join(proc, pid, "cgroup"), "0::/"+cgroup+"\n")
	}
	proc1("10", "python", "4194304", "user.slice/app-big.scope")
	proc1("20", "dockerd", "300000", "system.slice/docker.service")
	proc1("30", "kthreadd", "0", "")
	got, err := MemorySnapshot(proc, cg, 2)
	if err != nil {
		t.Fatal(err)
	}
	want := platform.MemorySnapshot{
		Available: 3 * gib, Uncharged: 14 * gib,
		Cgroups:   []platform.CgroupMemory{{Path: "user.slice/app-big.scope", Bytes: 6 * gib}, {Path: "system.slice/docker-x.scope", Bytes: 2 * gib}},
		Processes: []platform.ProcessMemory{{PID: 10, Name: "python", Cgroup: "user.slice/app-big.scope", RSS: 4 * gib}, {PID: 20, Name: "dockerd", Cgroup: "system.slice/docker.service", RSS: 300000 << 10}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
}
