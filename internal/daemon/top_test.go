package daemon

import (
	"context"
	"errors"
	"github.com/creatorofuniverses/agent-queue/internal/platform"
	"github.com/creatorofuniverses/agent-queue/internal/platform/fake"
	"github.com/creatorofuniverses/agent-queue/internal/scheduler"
	"github.com/creatorofuniverses/agent-queue/internal/store"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestTopView(t *testing.T) {
	e := newEnv(t)
	m := &fake.UsageMeter{}
	m.Set(platform.Usage{Agents: []platform.AgentUsage{{Name: "claude", Bytes: 5 * gib, Procs: 2, Spawned: 4 * gib}}, Docker: 7 * gib, Tasks: 2 * gib})
	e.opts = Options{Usage: m}
	e.host.Set(platform.MemInfo{Total: 32 * gib, Available: 12 * gib})
	e.start(t)
	var v TopView
	if err := e.call(t, "top", nil, &v); err != nil || v.Physical.Used != 20*gib || v.Physical.Spawned != 4*gib || v.Physical.Other != 2*gib {
		t.Fatal(v, err)
	}
	m.Fail(errors.New("unavailable"))
	if err := e.call(t, "top", nil, &v); err != nil || v.Physical.Error == "" || v.Physical.Used != 0 || v.Host.MemTotal != 32*gib {
		t.Fatal(v, err)
	}
}

type blockedUsage struct{ release chan struct{} }

func (b *blockedUsage) Usage(ctx context.Context, agents []string) (platform.Usage, error) {
	<-b.release // ignores ctx, like a read stuck on a D-state process
	return platform.Usage{Tasks: 1}, nil
}

func TestBoundedUsageAbandonsBlockedScan(t *testing.T) {
	inner := &blockedUsage{release: make(chan struct{})}
	b := &boundedUsage{inner: inner}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if _, e := b.Usage(ctx, nil); !errors.Is(e, context.DeadlineExceeded) {
		t.Fatal(e)
	}
	if _, e := b.Usage(t.Context(), nil); e == nil || errors.Is(e, context.DeadlineExceeded) {
		t.Fatal("second scan must not start while the first is stuck", e)
	}
	close(inner.release)
	for b.busy.Load() {
		time.Sleep(time.Millisecond)
	}
	if u, e := b.Usage(t.Context(), nil); e != nil || u.Tasks != 1 {
		t.Fatal(u, e)
	}
}

// aq top shows where task temp dirs live and how full that filesystem is.
func TestTopViewTmp(t *testing.T) {
	e := newEnv(t)
	e.cfg.TmpDir = t.TempDir()
	e.host.SetFS(platform.FSUsage{Size: 16 * gib, Free: 10 * gib, Inodes: 1 << 20, InodesFree: 900_000, Tmpfs: true}, nil)
	e.start(t)
	var v TopView
	if err := e.call(t, "top", nil, &v); err != nil || v.Tmp == nil {
		t.Fatal(v, err)
	}
	if !strings.HasPrefix(v.Tmp.Root, e.cfg.TmpDir+"/aq-") || v.Tmp.Free != 10*gib || v.Tmp.InodesFree != 900_000 || !v.Tmp.Tmpfs {
		t.Fatalf("%+v", v.Tmp)
	}
}

// growingDisk is a disk meter whose host and one source write 1 MiB between reads.
type growingDisk struct{ n atomic.Uint64 }

func (g *growingDisk) Snapshot(ctx context.Context) (platform.DiskSnapshot, error) {
	b := g.n.Add(1) << 20
	return platform.DiskSnapshot{Host: b, Cgroups: []platform.CgroupIO{{Source: "system:journald.service", ID: "system.slice/journald.service", Parent: "system", Bytes: b}}, Parents: map[string]uint64{"system": b}}, nil
}

// aq top shows the disk write rate of the last sample, and nothing until the
// sampler has a delta (the first sample of a start is only a baseline).
func TestTopViewDisk(t *testing.T) {
	e := newEnv(t)
	disk := &growingDisk{}
	e.opts = Options{Usage: &fake.UsageMeter{}, Disk: disk}
	e.start(t)
	deadline := time.Now().Add(5 * time.Second)
	for disk.n.Load() == 0 { // the baseline sample was taken
		if time.Now().After(deadline) {
			t.Fatal("no baseline sample")
		}
		time.Sleep(time.Millisecond)
	}
	var v TopView
	if err := e.call(t, "top", nil, &v); err != nil || v.Disk != nil {
		t.Fatal(v.Disk, err)
	}

	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	sched := scheduler.New(scheduler.Deps{Store: st, Exec: e.ex, Host: e.host, Cfg: e.cfg, Paths: e.paths})
	d := &server{host: e.host, sched: sched, st: st, paths: e.paths, cfg: e.cfg, lastDisk: &atomic.Pointer[DiskRate]{}}
	want := DiskRate{At: time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC), BytesPerSec: 12e6, Top: "session:session-3.scope", TopBytesPerSec: 9e6, TopAgent: "agent:claude:/home/u/p"}
	d.lastDisk.Store(&want)
	got, err := d.top(context.Background())
	if err != nil || got.(TopView).Disk == nil || *got.(TopView).Disk != want {
		t.Fatal(got, err)
	}
}
