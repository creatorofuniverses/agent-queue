package daemon

import (
	"context"
	"errors"
	"github.com/creatorofuniverses/agent-queue/internal/admission"
	"github.com/creatorofuniverses/agent-queue/internal/config"
	"github.com/creatorofuniverses/agent-queue/internal/diskacct"
	"github.com/creatorofuniverses/agent-queue/internal/model"
	"github.com/creatorofuniverses/agent-queue/internal/notify"
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

func TestStatsSamplerWritesRow(t *testing.T) {
	st, e := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer st.Close()
	host := fake.NewHost()
	host.Set(platform.MemInfo{Total: 32 * gib, Available: 12 * gib})
	u := &fake.UsageMeter{}
	u.Set(platform.Usage{Agents: []platform.AgentUsage{{Name: "a", Bytes: 5 * gib, Spawned: 2 * gib}}, Docker: 7 * gib, Tasks: 2 * gib})
	ctx := context.Background()
	d := SamplerDeps{Host: host, Usage: u, Store: st, Pass: func() scheduler.PassSummary {
		return scheduler.PassSummary{Commitments: scheduler.Commitments{Memory: 9 * gib}, Queued: 3, Head: admission.Decision{Blocker: admission.BlockPoolMemory}}
	}, Pressure: func() (bool, float64) { return true, 7 }, Now: func() time.Time { return time.Now().UTC() }}
	sampleStep(ctx, &d, 30*time.Second)
	rs, err := st.HostSamples(context.Background(), time.Time{})
	if err != nil || len(rs) != 1 || rs[0].Span != 30*time.Second || rs[0].Spawned != 2*gib || rs[0].Other != 4*gib || rs[0].CommittedMemory != 9*gib || rs[0].Queued != 3 || rs[0].HeadBlocker != "pool_memory" {
		t.Fatal(rs, err)
	}
	u.Fail(errors.New("failed"))
	ctx = context.Background()
	d.Now = func() time.Time { return time.Now().UTC() }
	sampleStep(ctx, &d, 30*time.Second)
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

// diskDeps is a sampler with a disk meter, an agent tree in a session scope
// and a recording Emit; at is the clock the test moves.
func diskDeps(t *testing.T, at *time.Time) (SamplerDeps, *fake.DiskMeter, *fake.UsageMeter, *[]model.Event) {
	t.Helper()
	st, e := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { st.Close() })
	host := fake.NewHost()
	host.Set(platform.MemInfo{Total: 32 * gib, Available: 12 * gib})
	var evs []model.Event
	disk, usage := &fake.DiskMeter{}, &fake.UsageMeter{}
	d := SamplerDeps{Host: host, Usage: usage, Store: st,
		Pass:     func() scheduler.PassSummary { return scheduler.PassSummary{} },
		Pressure: func() (bool, float64) { return false, 0 },
		Now:      func() time.Time { return *at },
		Disk:     disk, Acct: diskacct.New(), Alarm: diskacct.NewAlarm(60*gib, 5*time.Minute),
		Emit:     func(_ context.Context, ev model.Event) { evs = append(evs, ev) },
		LastDisk: &atomic.Pointer[DiskRate]{}, Home: "/home/u"}
	return d, disk, usage, &evs
}

// setDisk makes the host, the session scope (whose agent wrote agent of it)
// and a compose project hold these cumulative counters.
func setDisk(disk *fake.DiskMeter, usage *fake.UsageMeter, session, docker, agent uint64) {
	disk.Set(platform.DiskSnapshot{Host: session + docker,
		Cgroups: []platform.CgroupIO{
			{Source: "session:session-3.scope", ID: "user.slice/user-1000.slice/session-3.scope", Parent: "session", Bytes: session},
			{Source: "docker:runi", ID: "system.slice/docker-abc.scope", Bytes: docker}},
		Parents: map[string]uint64{"session": session}})
	usage.Set(platform.Usage{IO: []platform.ProcIO{{PID: 10, PPID: 1, Start: 10, Agent: "claude", Project: "/home/u/sber/auto-evolve", Within: "session:session-3.scope", Bytes: int64(agent)}}})
}

func TestStatsSamplerDisk(t *testing.T) {
	ctx := context.Background()
	t0 := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	at := t0
	d, disk, usage, evs := diskDeps(t, &at)
	setDisk(disk, usage, 0, 0, 0)
	sampleStep(ctx, &d, time.Minute) // baseline: no rows, no rate
	if rows, _ := d.Store.DiskWritesSince(ctx, time.Time{}); len(rows) != 0 || d.LastDisk.Load() != nil {
		t.Fatal(rows, d.LastDisk.Load())
	}
	// Per minute: the session writes 1.5 GiB (1 GiB of it by claude), docker 0.5 GiB.
	step := func(k uint64) {
		at = t0.Add(time.Duration(k) * time.Minute)
		setDisk(disk, usage, k*3*gib/2, k*gib/2, k*gib)
		sampleStep(ctx, &d, time.Minute)
	}
	step(1)
	rows, err := d.Store.DiskWritesSince(ctx, time.Time{})
	const g = int64(gib)
	want := map[string]int64{"host": 2 * g, "session:session-3.scope": 3 * g / 2, "docker:runi": g / 2, "agent:claude:/home/u/sber/auto-evolve": g}
	if err != nil || len(rows) != len(want) {
		t.Fatal(rows, err)
	}
	for _, r := range rows {
		if !r.Hour.Equal(t0) || want[r.Source] != r.Bytes || (r.Source == "agent:claude:/home/u/sber/auto-evolve") != (r.Within == "session:session-3.scope") {
			t.Fatalf("%+v", r)
		}
	}
	r := d.LastDisk.Load()
	if r == nil || !r.At.Equal(at) || r.BytesPerSec != float64(2*gib)/60 || r.Top != "session:session-3.scope" ||
		r.TopBytesPerSec != float64(3*gib/2)/60 || r.TopAgent != "agent:claude:/home/u/sber/auto-evolve" {
		t.Fatalf("%+v", r)
	}
	// 2 GiB a minute is 120 GiB/h, over the 60 GiB/h threshold: the alarm rises
	// once the 5-minute window is full, and only once.
	for k := uint64(2); k <= 7; k++ {
		step(k)
		if n := len(*evs); (k < 5 && n != 0) || (k >= 5 && n != 1) {
			t.Fatalf("sample %d: %+v", k, *evs)
		}
	}
	ev := (*evs)[0]
	if ev.Kind != model.EventDiskWritesHigh || ev.Audience != model.AudienceOperator || ev.ID == "" || !ev.CreatedAt.Equal(t0.Add(5*time.Minute)) ||
		ev.Detail != "disk writes at 120.0 GiB/h for 5m (10.0 GiB): session:session-3.scope 90.0 GiB/h (≈ claude ~/sber/auto-evolve), docker:runi 30.0 GiB/h" {
		t.Fatalf("%+v", ev)
	}
	// Writing stops: the all-clear comes once the window average drops below half.
	for k := uint64(8); k <= 12 && len(*evs) == 1; k++ {
		at = t0.Add(time.Duration(k) * time.Minute)
		setDisk(disk, usage, 7*3*gib/2, 7*gib/2, 7*gib)
		sampleStep(ctx, &d, time.Minute)
	}
	if len(*evs) != 2 || (*evs)[1].Kind != model.EventDiskWritesNormal || (*evs)[1].Audience != model.AudienceOperator ||
		!strings.HasPrefix((*evs)[1].Detail, "disk writes back to ") || !strings.Contains((*evs)[1].Detail, "most by session:session-3.scope") {
		t.Fatalf("%+v", *evs)
	}
}

// The rate's span is the real time since the last disk sample, not the
// configured interval, and a failed usage scan skips the disk step instead of
// feeding the accountant a snapshot without processes.
func TestStatsSamplerDiskSkipsFailedUsage(t *testing.T) {
	ctx := context.Background()
	t0 := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	at := t0
	d, disk, usage, _ := diskDeps(t, &at)
	setDisk(disk, usage, 0, 0, 0)
	sampleStep(ctx, &d, time.Minute)
	at = t0.Add(time.Minute)
	usage.Fail(errors.New("scan stuck"))
	sampleStep(ctx, &d, time.Minute)
	if rows, _ := d.Store.DiskWritesSince(ctx, time.Time{}); len(rows) != 0 || d.LastDisk.Load() != nil {
		t.Fatal(rows)
	}
	at = t0.Add(2 * time.Minute)
	setDisk(disk, usage, 3*gib, gib, 2*gib)
	sampleStep(ctx, &d, time.Minute)
	rows, _ := d.Store.DiskWritesSince(ctx, time.Time{})
	agent := int64(0)
	for _, r := range rows {
		if r.Source == "agent:claude:/home/u/sber/auto-evolve" {
			agent = r.Bytes
		}
	}
	if r := d.LastDisk.Load(); r == nil || r.BytesPerSec != float64(4*gib)/120 || agent != 2*int64(gib) {
		t.Fatalf("%+v %+v", r, rows)
	}
}

func TestStatsSamplerDiskFailureKeepsMemorySample(t *testing.T) {
	ctx := context.Background()
	at := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	d, disk, usage, evs := diskDeps(t, &at)
	setDisk(disk, usage, 0, 0, 0)
	disk.Fail(errors.New("no diskstats"))
	for i := 0; i < 3; i++ {
		at = at.Add(time.Minute)
		sampleStep(ctx, &d, time.Minute)
	}
	hs, err := d.Store.HostSamples(ctx, time.Time{})
	rows, err2 := d.Store.DiskWritesSince(ctx, time.Time{})
	if err != nil || err2 != nil || len(hs) != 3 || len(rows) != 0 || d.LastDisk.Load() != nil || len(*evs) != 0 {
		t.Fatal(hs, rows, err, err2)
	}
}

func TestAgentLabel(t *testing.T) {
	for key, want := range map[string]string{
		"agent:claude:/home/u/sber/auto-evolve": "claude ~/sber/auto-evolve",
		"agent:codex:/home/u":                   "codex ~",
		"agent:codex:/home/user2/x":             "codex /home/user2/x",
		"agent:kimi:":                           "kimi",
		"docker:runi":                           "docker:runi",
	} {
		if got := AgentLabel(key, "/home/u"); got != want {
			t.Errorf("%s: %q, want %q", key, got, want)
		}
	}
}

// The alarm's events go to the webhook like the daemon's own, or are skipped
// without one.
func TestOperatorEmitter(t *testing.T) {
	ctx := context.Background()
	st, e := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer st.Close()
	cfg := config.Default()
	kicks := 0
	operatorEmitter(st, notify.NewRouter(cfg), func() { kicks++ })(ctx, model.Event{ID: "e1", Kind: model.EventDiskWritesHigh, Detail: "d", CreatedAt: time.Now().UTC()})
	cfg.Webhook.URL = "http://127.0.0.1:1/hook"
	operatorEmitter(st, notify.NewRouter(cfg), func() { kicks++ })(ctx, model.Event{ID: "e2", Kind: model.EventDiskWritesNormal, CreatedAt: time.Now().UTC()})
	evs, err := st.EventsSince(ctx, time.Time{})
	if err != nil || len(evs) != 2 || kicks != 2 {
		t.Fatal(evs, err, kicks)
	}
	for _, ev := range evs {
		want := map[string]model.EventStatus{"e1": model.EventSkipped, "e2": model.EventPending}[ev.ID]
		if ev.Audience != model.AudienceOperator || ev.Status != want || (ev.Status == model.EventPending) != (ev.Channel == model.ChannelWebhook) {
			t.Fatalf("%+v", ev)
		}
	}
}
