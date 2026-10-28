package daemon

import (
	"context"
	"errors"
	"github.com/creatorofuniverses/agent-queue/internal/platform"
	"github.com/creatorofuniverses/agent-queue/internal/platform/fake"
	"strings"
	"testing"
	"time"
)

func TestTopView(t *testing.T) {
	e := newEnv(t)
	m := &fake.UsageMeter{}
	m.Set(platform.Usage{Agents: []platform.AgentUsage{{Name: "claude", Bytes: 5 * gib, Procs: 2}}, Docker: 7 * gib, Tasks: 2 * gib})
	e.opts = Options{Usage: m}
	e.host.Set(platform.MemInfo{Total: 32 * gib, Available: 12 * gib})
	e.start(t)
	var v TopView
	if err := e.call(t, "top", nil, &v); err != nil || v.Physical.Used != 20*gib || v.Physical.Other != 6*gib {
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
