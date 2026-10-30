package daemon

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/creatorofuniverses/agent-queue/internal/platform"
	"github.com/creatorofuniverses/agent-queue/internal/store"
)

func TestCulpritText(t *testing.T) {
	v := platform.MemorySnapshot{
		Available: 3 * gib, Uncharged: 9 * gib,
		Cgroups:   []platform.CgroupMemory{{Path: "user.slice/user-1000.slice/user@1000.service/app.slice/app-big.scope", Bytes: 6 * gib}, {Path: "system.slice/docker-x.scope", Bytes: 2 * gib}},
		Processes: []platform.ProcessMemory{{PID: 10, Name: "python", Cgroup: "user.slice/user-1000.slice/user@1000.service/app.slice/app-big.scope", RSS: 4 * gib, Agent: "claude 5"}},
	}
	const want = "available 3.0 GiB; in no cgroup (kernel, drivers, zram) 9.0 GiB; cgroups: app-big.scope 6.0 GiB, docker-x.scope 2.0 GiB; processes: python 4.0 GiB (pid 10, app-big.scope, run by claude 5)"
	if got := culpritText(v); got != want {
		t.Fatalf("%q\nwant %q", got, want)
	}
}

func TestCulpritsRecordedAtMostEveryFiveMinutes(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "aq.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	at := time.Date(2026, 10, 30, 22, 26, 0, 0, time.UTC)
	r := &culpritRecorder{st: st, snapshot: func() (platform.MemorySnapshot, error) {
		return platform.MemorySnapshot{Available: gib}, nil
	}}
	ctx := context.Background()
	for _, d := range []time.Duration{0, time.Minute, 4 * time.Minute, 5 * time.Minute} {
		if r.due(at.Add(d)) {
			r.record(ctx, at.Add(d))
		}
	}
	incs, err := st.Incidents(ctx, at.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(incs) != 2 || incs[0].Kind != "guard_engaged" || incs[0].Value != float64(gib) || incs[1].At != at.Add(5*time.Minute) {
		t.Fatalf("%+v", incs)
	}
}
