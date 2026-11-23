package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/creatorofuniverses/agent-queue/internal/admission"
	"github.com/creatorofuniverses/agent-queue/internal/agentsess"
	"github.com/creatorofuniverses/agent-queue/internal/config"
	"github.com/creatorofuniverses/agent-queue/internal/diskacct"
	"github.com/creatorofuniverses/agent-queue/internal/model"
	"github.com/creatorofuniverses/agent-queue/internal/notify"
	"github.com/creatorofuniverses/agent-queue/internal/platform"
	"github.com/creatorofuniverses/agent-queue/internal/platform/fake"
	"github.com/creatorofuniverses/agent-queue/internal/protocol"
	"github.com/creatorofuniverses/agent-queue/internal/scheduler"
	"github.com/creatorofuniverses/agent-queue/internal/store"
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

type startupSampleMeter struct {
	started chan struct{}
	release chan struct{}
	disk    chan struct{}
	calls   atomic.Int32
}

func (m *startupSampleMeter) Usage(ctx context.Context, _ []string) (platform.Usage, error) {
	if m.calls.Add(1) != 1 {
		return platform.Usage{Tasks: 2 * gib}, nil
	}
	close(m.started)
	select {
	case <-m.release:
		return platform.Usage{Tasks: gib}, nil
	case <-ctx.Done():
		return platform.Usage{}, ctx.Err()
	}
}

func (m *startupSampleMeter) Snapshot(ctx context.Context) (platform.DiskSnapshot, error) {
	close(m.disk)
	<-ctx.Done()
	return platform.DiskSnapshot{}, ctx.Err()
}

func TestTopWaitsForInitialUsageScan(t *testing.T) {
	e := newEnv(t)
	m := &startupSampleMeter{started: make(chan struct{}), release: make(chan struct{}), disk: make(chan struct{})}
	e.opts = Options{Usage: m, Disk: m}
	e.host.Set(platform.MemInfo{Total: 32 * gib, Available: 12 * gib})
	e.start(t)
	await := func(ch <-chan struct{}, phase string) {
		t.Helper()
		select {
		case <-ch:
		case <-time.After(2 * time.Second):
			t.Fatalf("sampler did not reach %s", phase)
		}
	}
	await(m.started, "initial usage scan")

	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	type result struct {
		view TopView
		err  error
	}
	done := make(chan result, 1)
	go func() {
		var r result
		r.err = protocol.Call(ctx, e.paths.Socket(), "top", nil, &r.view)
		done <- r
	}()
	select {
	case r := <-done:
		t.Fatalf("top returned before the initial usage scan completed: physical = %+v, err = %v", r.view.Physical, r.err)
	case <-time.After(250 * time.Millisecond): // Give top a chance to reach the held scan.
	}

	close(m.release)
	await(m.disk, "disk sampling")
	r := <-done
	if r.err != nil || r.view.Physical.Error != "" || r.view.Physical.Tasks != 2*gib {
		t.Fatalf("top did not get fresh usage while sampler was processing its first scan: physical = %+v, err = %v", r.view.Physical, r.err)
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
// and a compose project hold these cumulative counters; a nil usage keeps
// the usage meter as it is (failing, say).
func setDisk(disk *fake.DiskMeter, usage *fake.UsageMeter, session, docker, agent uint64) {
	disk.Set(platform.DiskSnapshot{Host: session + docker,
		Cgroups: []platform.CgroupIO{
			{Source: "session:session-3.scope", ID: "user.slice/user-1000.slice/session-3.scope", Parent: "session", Bytes: session},
			{Source: "docker:shop", ID: "system.slice/docker-abc.scope", Bytes: docker}},
		Parents: map[string]uint64{"session": session}})
	if usage != nil {
		usage.Set(platform.Usage{IO: []platform.ProcIO{{PID: 10, PPID: 1, Start: 10, Agent: "claude", Project: "/home/u/src/shop", Within: "session:session-3.scope", Bytes: int64(agent)}}})
	}
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
	want := map[string]int64{"host": 2 * g, "session:session-3.scope": 3 * g / 2, "docker:shop": g / 2, "agent:claude:/home/u/src/shop": g}
	if err != nil || len(rows) != len(want) {
		t.Fatal(rows, err)
	}
	for _, r := range rows {
		if !r.Hour.Equal(t0) || want[r.Source] != r.Bytes || (r.Source == "agent:claude:/home/u/src/shop") != (r.Within == "session:session-3.scope") {
			t.Fatalf("%+v", r)
		}
	}
	r := d.LastDisk.Load()
	if r == nil || !r.At.Equal(at) || r.BytesPerSec != float64(2*gib)/60 || r.Top != "session:session-3.scope" ||
		r.TopBytesPerSec != float64(3*gib/2)/60 || r.TopAgent != "agent:claude:/home/u/src/shop" {
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
		ev.Detail != "disk writes at 120.0 GiB/h for 5m (10.0 GiB): session:session-3.scope 90.0 GiB/h (≈ claude ~/src/shop), docker:shop 30.0 GiB/h" {
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
// configured interval. A failed usage scan (a /proc read stuck behind D-state
// processes during heavy writing) still steps the sources and the host, so
// rows, the rate and the alarm keep up; only the agent estimate waits for the
// next scan, which covers the gap.
func TestStatsSamplerDiskFailedUsageStepsSources(t *testing.T) {
	ctx := context.Background()
	t0 := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	at := t0
	d, disk, usage, evs := diskDeps(t, &at)
	setDisk(disk, usage, 0, 0, 0)
	sampleStep(ctx, &d, time.Minute)
	usage.Fail(errors.New("scan stuck"))
	// 2 GiB a minute while every scan fails: the alarm still rises at 5 minutes.
	for k := uint64(1); k <= 5; k++ {
		at = t0.Add(time.Duration(k) * time.Minute)
		setDisk(disk, nil, k*3*gib/2, k*gib/2, k*gib)
		sampleStep(ctx, &d, time.Minute)
	}
	if len(*evs) != 1 || (*evs)[0].Kind != model.EventDiskWritesHigh {
		t.Fatalf("alarm while the usage scan fails: %+v", *evs)
	}
	sum := func() map[string]int64 {
		rows, _ := d.Store.DiskWritesSince(ctx, time.Time{})
		out := map[string]int64{}
		for _, r := range rows {
			out[r.Source] += r.Bytes
		}
		return out
	}
	const g = int64(gib)
	if got := sum(); got["host"] != 10*g || got["session:session-3.scope"] != 15*g/2 || got["docker:shop"] != 5*g/2 || got["agent:claude:/home/u/src/shop"] != 0 {
		t.Fatalf("rows while the usage scan fails: %v", got)
	}
	if r := d.LastDisk.Load(); r == nil || !r.At.Equal(at) || r.BytesPerSec != float64(2*gib)/60 {
		t.Fatalf("%+v", r)
	}
	// The scan is back: the agent's whole growth since the baseline arrives once.
	at = t0.Add(6 * time.Minute)
	setDisk(disk, usage, 6*3*gib/2, 6*gib/2, 6*gib)
	sampleStep(ctx, &d, time.Minute)
	if got := sum(); got["agent:claude:/home/u/src/shop"] != 6*g || got["host"] != 12*g {
		t.Fatalf("rows after the scan is back: %v", got)
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

// An agent that lives in no source has nowhere to be shown: no row, so the
// report cannot take it for a level-1 source.
func TestDiskRowsSkipAgentOutsideSources(t *testing.T) {
	at := time.Date(2026, 11, 4, 10, 30, 0, 0, time.UTC)
	rows := diskRows(at, diskacct.Delta{Host: 5, Agents: []diskacct.AgentDelta{
		{Key: "agent:claude:/p", Within: "", Bytes: 3},
		{Key: "agent:codex:/q", Within: "session:s.scope", Bytes: 2}}})
	if len(rows) != 2 || rows[0].Source != "host" || rows[1].Source != "agent:codex:/q" || rows[1].Within != "session:s.scope" {
		t.Fatalf("%+v", rows)
	}
}

func sessionSampler(t *testing.T, at *time.Time) (SamplerDeps, *fake.Host, *fake.UsageMeter) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "samples.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	host, usage := fake.NewHost(), &fake.UsageMeter{}
	return SamplerDeps{
		Host: host, Usage: usage, Store: st,
		Pass:     func() scheduler.PassSummary { return scheduler.PassSummary{} },
		Pressure: func() (bool, float64) { return false, 0 },
		Now:      func() time.Time { return *at },
	}, host, usage
}

func TestSamplerSessions(t *testing.T) {
	at := time.Unix(1795464773, 0)
	d, _, usage := sessionSampler(t, &at)
	dir, proc := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "1.json"), []byte(`{"pid":1,"procStart":"123","sessionId":"one","name":"shop","status":"busy"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(proc, "1"), 0700); err != nil {
		t.Fatal(err)
	}
	stat := "1 (claude) S 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 123 456\n"
	if err := os.WriteFile(filepath.Join(proc, "1", "stat"), []byte(stat), 0600); err != nil {
		t.Fatal(err)
	}
	d.Sessions = agentsess.Reader{Dir: dir, ProcRoot: proc}
	d.LastSessions = &atomic.Pointer[[]agentsess.Session]{}
	usage.Set(platform.Usage{Sessions: []platform.SessionUsage{
		{PID: 1, Start: 123, Client: "claude", AgentBytes: 321},
		{PID: 2, Client: "codex", AgentBytes: 123},
	}})
	sampleStep(t.Context(), &d, 30*time.Second)
	last := d.LastSessions.Load()
	if last == nil || len(*last) != 2 {
		t.Fatalf("want two sampled sessions, got %v", last)
	}
	first, second := (*last)[0], (*last)[1]
	if first.PID != 1 || first.Name != "shop" || first.SessionID != "one" || first.AgentBytes != 321 ||
		first.IdleSeconds == nil || *first.IdleSeconds != 0 || second.PID != 2 || second.IdleSeconds != nil {
		t.Fatalf("sampled sessions: %+v", *last)
	}
	usage.Fail(errors.New("usage scan failed"))
	at = at.Add(30 * time.Second)
	sampleStep(t.Context(), &d, 30*time.Second)
	if d.LastSessions.Load() != last {
		t.Fatal("a failed scan replaced the last successful sessions")
	}
	rows, err := d.Store.HostSamples(t.Context(), time.Time{})
	if err != nil || len(rows) != 2 {
		t.Fatalf("memory samples after failed usage scan: %v, %v", rows, err)
	}
}

func TestSamplerLevelAlerts(t *testing.T) {
	for _, tc := range []struct {
		name string
		fs   platform.FSUsage
	}{
		{"bytes", platform.FSUsage{Size: 100, Free: 20, Inodes: 100, InodesFree: 90}},
		{"inodes", platform.FSUsage{Size: 100, Free: 90, Inodes: 100, InodesFree: 20}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const step = 30 * time.Second
			at := time.Unix(1795464773, 0)
			d, host, usage := sessionSampler(t, &at)
			host.Set(platform.MemInfo{Total: 32 * gib, Available: 12 * gib, SwapTotal: 10 * gib, SwapFree: gib})
			host.SetFS(tc.fs, nil)
			usage.Fail(errors.New("usage scan failed"))
			d.TmpRoot = t.TempDir()
			d.Alerts = newAlertSet(config.Default())
			var events []model.Event
			d.Emit = func(_ context.Context, ev model.Event) { events = append(events, ev) }
			sampleStep(t.Context(), &d, step) // The window starts at this sample.
			for i := 1; i <= 20; i++ {
				at = at.Add(step)
				sampleStep(t.Context(), &d, step)
				if i < 20 && len(events) != 0 {
					t.Fatalf("alarm before the window elapsed at step %d: %+v", i, events)
				}
				if i == 20 && len(events) != 2 {
					t.Fatalf("want both alarms after the window elapsed, got %+v", events)
				}
			}
			for range 2 {
				at = at.Add(step)
				sampleStep(t.Context(), &d, step)
			}
			if len(events) != 2 {
				t.Fatalf("want two alarm events, got %+v", events)
			}
			want := map[model.EventKind]string{model.EventTmpFull: "80%", model.EventSwapFull: "90%"}
			for _, ev := range events {
				value := want[ev.Kind]
				if value == "" || ev.Audience != model.AudienceOperator || !strings.Contains(ev.Detail, value) {
					t.Fatalf("unexpected alarm event: %+v", ev)
				}
				delete(want, ev.Kind)
			}
		})
	}
}
