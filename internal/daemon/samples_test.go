package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"resource-dispatcher/internal/admission"
	"resource-dispatcher/internal/platform"
	"resource-dispatcher/internal/platform/fake"
	"resource-dispatcher/internal/scheduler"
	"resource-dispatcher/internal/store"
	"testing"
	"time"
)

func TestStatsSamplerWritesRow(t *testing.T) {
	st, e := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer st.Close()
	host := fake.NewHost()
	host.Set(platform.MemInfo{Total: 32 * gib, Available: 12 * gib})
	u := &fake.UsageMeter{}
	u.Set(platform.Usage{Agents: []platform.AgentUsage{{Name: "a", Bytes: 5 * gib}}, Docker: 7 * gib, Tasks: 2 * gib})
	ctx := context.Background()
	d := SamplerDeps{Host: host, Usage: u, Store: st, Pass: func() scheduler.PassSummary {
		return scheduler.PassSummary{Commitments: scheduler.Commitments{Memory: 9 * gib}, Queued: 3, Head: admission.Decision{Blocker: admission.BlockPoolMemory}}
	}, Pressure: func() (bool, float64) { return true, 7 }, Now: func() time.Time { return time.Now().UTC() }}
	sampleStep(ctx, d, 30*time.Second)
	rs, err := st.HostSamples(context.Background(), time.Time{})
	if err != nil || len(rs) != 1 || rs[0].Span != 30*time.Second || rs[0].Other != 6*gib || rs[0].CommittedMemory != 9*gib || rs[0].Queued != 3 || rs[0].HeadBlocker != "pool_memory" {
		t.Fatal(rs, err)
	}
	u.Fail(errors.New("failed"))
	ctx = context.Background()
	d.Now = func() time.Time { return time.Now().UTC() }
	sampleStep(ctx, d, 30*time.Second)
	rs, err = st.HostSamples(context.Background(), time.Time{})
	if err != nil || len(rs) != 2 || rs[1].Agents != 0 || rs[1].Other != 0 {
		t.Fatal(rs, err)
	}
}

func TestStatsSamplerRunsImmediately(t *testing.T) {
	st, e := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer st.Close()
	host := fake.NewHost()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunStatsSampler(ctx, SamplerDeps{Host: host, Store: st, Pass: func() scheduler.PassSummary { return scheduler.PassSummary{} }, Pressure: func() (bool, float64) { return false, 0 }}, time.Hour)
	}()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(time.Second)
	for {
		rs, e := st.HostSamples(context.Background(), time.Time{})
		if e != nil {
			t.Fatal(e)
		}
		if len(rs) == 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no immediate sample")
		}
		time.Sleep(time.Millisecond)
	}
}
