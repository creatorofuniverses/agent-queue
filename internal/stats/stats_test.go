package stats

import (
	"encoding/json"
	"fmt"
	"github.com/creatorofuniverses/agent-queue/internal/hook"
	"github.com/creatorofuniverses/agent-queue/internal/model"
	"github.com/creatorofuniverses/agent-queue/internal/profile"
	"github.com/creatorofuniverses/agent-queue/internal/store"
	"maps"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSummarizeItems(t *testing.T) {
	now := time.Now()
	in := Input{Since: now.Add(-time.Hour), Until: now, Items: []*model.Item{{Kind: model.KindTask, ClientType: "codex", Priority: model.PriorityHigh, State: model.StateSucceeded, CreatedAt: now.Add(-30 * time.Second), StartedAt: now.Add(-20 * time.Second), EndedAt: now}, {Kind: model.KindTask, ClientType: "codex", State: model.StateFailed, Reason: model.ReasonOOM}, {Kind: model.KindTask, ClientType: "cli", State: model.StateQueued}}}
	s := Summarize(in)
	if len(s.Items) != 2 {
		t.Fatal(s)
	}
	var g ItemGroup
	for _, v := range s.Items {
		if v.ClientType == "codex" {
			g = v
		}
	}
	if g.Count != 2 || g.Outcomes["SUCCEEDED"] != 1 || g.Outcomes["FAILED/OOM"] != 1 || g.ByPriority["high"] != 1 || g.WaitP50 != 10 || g.RunP90 != 20 {
		t.Fatal(g)
	}
}
func TestSummarizeProfiles(t *testing.T) {
	s := Summarize(Input{Runs: []profile.Run{{Profile: "p", State: model.StateSucceeded, MemoryPeak: 200, Expected: 100, Duration: 2 * time.Second, EstDuration: time.Second, MemoryFromProfile: true}, {Profile: "p", State: model.StateFailed, Reason: model.ReasonCommandFailed, MemoryPeak: 50, Expected: 100, Duration: time.Second, EstDuration: 2 * time.Second}, {Profile: "p", State: model.StateFailed, Reason: model.ReasonOOM, MemoryPeak: 100, Expected: 100}, {Profile: "p", State: model.StateFailed, Reason: model.ReasonTimeout}}})
	if len(s.Profiles) != 1 {
		t.Fatal(s)
	}
	v := s.Profiles[0]
	if v.Runs != 4 || v.Usable != 2 || v.OOM != 1 || v.Timeouts != 1 || v.MemoryFromProfile != 1 || v.PeakOverExpected != 1 || v.PeakToExpectedP50 != 0.5 || v.PeakToExpectedP90 != 2 || v.DurationToEstimateP90 != 2 {
		t.Fatal(v)
	}
}
func TestSummarizeQueueAndHost(t *testing.T) {
	s := Summarize(Input{Samples: []store.HostSample{{Span: 30 * time.Second, MemAvailable: 100, Agents: 10, Spawned: 40, Queued: 1, HeadBlocker: "pool_memory", PSIFullAvg10: 1}, {Span: 600 * time.Second, MemAvailable: 200, Agents: 20, Spawned: 2, Queued: 1, HeadBlocker: "pool_memory", PSIFullAvg10: 4, SwapTotal: 100, SwapFree: 50}}})
	if s.Queue.SampledSeconds != 630 || s.Queue.NonEmptySeconds != 630 || s.Queue.BlockedSeconds["pool_memory"] != 630 || s.Host.Samples != 2 || s.Host.MemAvailableMin != 100 || s.Host.MemAvailableAvg != 195 || s.Host.AgentsAvg != 19 || s.Host.AgentsMax != 20 || s.Host.SpawnedAvg != 3 || s.Host.SpawnedMax != 40 || s.Host.SwapUsedMax != 50 || s.Host.PSIMax != 4 {
		t.Fatal(s)
	}
}
func TestSummarizeReservationsDeliveryHookIncidents(t *testing.T) {
	now := time.Now().UTC()
	in := Input{Until: now, SliceName: "aq.slice", TeardownSliceName: "aqtd.slice", Patterns: []string{"x", "y"}, Hits: []hook.Hit{{At: now, Pattern: 1, Client: "codex"}}, Items: []*model.Item{{ID: "r", Kind: model.KindReservation, ClientType: "codex", State: model.StateReleased, ExpiredAt: now, ExtendCount: 2}, {Kind: model.KindReservation, ClientType: "codex", State: model.StateTornDown}, {Kind: model.KindReservation, ClientType: "codex", State: model.StateTeardownFailed}, {Kind: model.KindReservation, ClientType: "codex", State: model.StateTeardownUnknown}}, Events: []model.Event{{ItemID: "r", Kind: model.EventReservationForceReleased, Channel: model.ChannelWebhook, Status: model.EventSent, CreatedAt: now.Add(-time.Second), SentAt: now}, {Channel: model.ChannelClientWait, Audience: model.AudienceSession, Status: model.EventSent, CreatedAt: now.Add(-2 * time.Second), SentAt: now}}, Incidents: []model.Incident{{At: now.Add(-20 * time.Second), Kind: "psi_close"}, {At: now.Add(-10 * time.Second), Kind: "psi_open"}, {At: now.Add(-5 * time.Second), Kind: "psi_close"}, {At: now, Kind: "kernel_oom", Value: 2}, {At: now, Kind: "oomd_kill", Detail: "/x/aqtd.slice/x.service"}, {At: now, Kind: "oomd_kill", Detail: "/outside"}}}
	s := Summarize(in)
	r := s.Reservations[0]
	if r.Count != 4 || r.Active != 2 || r.Expired != 1 || r.Released != 1 || r.ForceReleased != 1 || r.TornDown != 1 || r.TeardownFailed != 1 || r.TeardownUnknown != 1 || r.Extensions != 2 {
		t.Fatal(r)
	}
	if s.Hook.Hits != 1 || s.Hook.ByPattern[1].Hits != 1 || s.Hook.ByClient["codex"] != 1 || s.Incidents.KernelOOM != 2 || s.Incidents.OOMDTasks != 1 || s.Incidents.OOMDOutside != 1 || s.Incidents.PSIEpisodes != 2 || s.Incidents.PSIClosedSeconds != 15 {
		t.Fatal(s)
	}
	for _, d := range s.Delivery {
		if d.Channel == "client_wait" && (d.Unacked != 1 || d.LatencyP50 != 2) {
			t.Fatal(d)
		}
	}
}
func TestSummarizeProfileFilter(t *testing.T) {
	s := Summarize(Input{Profile: "p"})
	if s.Queue != nil || s.Host != nil || s.Hook != nil || s.Incidents != nil || len(s.Reservations) != 0 || len(s.Delivery) != 0 {
		t.Fatal(s)
	}
}
func TestPSIEpisodeOpenAtWindowStart(t *testing.T) {
	since := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	in := Input{Since: since, Until: since.Add(time.Hour), PSIClosedAtSince: true, Incidents: []model.Incident{{At: since.Add(10 * time.Minute), Kind: "psi_open"}}}
	if g := Summarize(in).Incidents; g.PSIClosedSeconds != 600 || g.PSIEpisodes != 1 {
		t.Fatal(g)
	}
	in.Incidents = nil // closed through the whole window
	if g := Summarize(in).Incidents; g.PSIClosedSeconds != 3600 {
		t.Fatal(g)
	}
}
func TestSummarizeDisk(t *testing.T) {
	u := func(n uint64) *uint64 { return &n }
	task := func(prof, name string, w *uint64) *model.Item {
		return &model.Item{Kind: model.KindTask, Profile: prof, Name: name, State: model.StateSucceeded, IOWriteBytes: w}
	}
	in := Input{Items: []*model.Item{
		task("/r:tests", "tests", u(10)), task("/r:tests", "tests", u(30)), task("/r:tests", "tests", u(20)),
		task("/r:db", "db", u(500)),
		task("", "adhoc", u(1)),
		task("/r:lint", "lint", nil), // unknown: not counted
		{Kind: model.KindReservation, Name: "stand", IOWriteBytes: u(9999)},
	}}
	s := Summarize(in)
	if len(s.Disk) != 3 {
		t.Fatal(s.Disk)
	}
	if d := s.Disk[0]; d.Profile != "/r:db" || d.Runs != 1 || d.Written != 500 {
		t.Fatal(d)
	}
	if d := s.Disk[1]; d.Profile != "/r:tests" || d.Runs != 3 || d.Written != 60 || d.P50 != 20 || d.Max != 30 {
		t.Fatal(d)
	}
	if d := s.Disk[2]; d.Profile != "adhoc" || d.Written != 1 {
		t.Fatal(d)
	}
	in.Profile = "/r:tests"
	if s := Summarize(in); len(s.Disk) != 1 || s.Disk[0].Runs != 3 {
		t.Fatal(s.Disk)
	}
}

func TestSummarizeKernelOOMByGroup(t *testing.T) {
	now := time.Now().UTC()
	s := Summarize(Input{Until: now, Incidents: []model.Incident{
		{At: now, Kind: "kernel_oom", Value: 3, Detail: "aqtest.slice"},
		{At: now, Kind: "kernel_oom", Value: 1, Detail: "aqtest.slice"},
		{At: now, Kind: "kernel_oom", Value: 2},
		{At: now, Kind: "kernel_oom", Value: 1, Detail: "unattributed"},
	}})
	if g := s.Incidents; g.KernelOOM != 7 || !maps.Equal(g.KernelOOMBy, map[string]float64{"aqtest.slice": 4}) {
		t.Fatalf("%+v", g)
	}
}

func TestSummarizeReservationFit(t *testing.T) {
	const gib = 1 << 30
	u := func(n uint64) *uint64 { return &n }
	t0 := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	res := func(name, docker string, stand, standPeak, budget uint64, budgetPeak *uint64, held time.Duration) *model.Item {
		return &model.Item{Kind: model.KindReservation, Project: "/r", Name: name, State: model.StateReleased, DockerProject: docker,
			Limit: stand, Expected: stand, MemoryPeak: standPeak, TaskMemory: budget, TaskMemoryPeak: budgetPeak,
			AdmittedAt: t0, EndedAt: t0.Add(held)}
	}
	torn := res("db", "p", 2*gib, gib/2, 0, u(0), 3*time.Hour)
	torn.State, torn.TeardownStartedAt = model.StateTornDown, t0.Add(time.Hour) // held until the teardown began
	torn.Expected = gib                                                         // history charged less than declared
	s := Summarize(Input{Items: []*model.Item{
		res("lab", "p", gib, gib/4, 4*gib, u(gib), 2*time.Hour),
		res("lab", "p", gib, gib/2, 4*gib, u(2*gib), time.Hour),
		res("lab", "", gib, 0, 4*gib, u(gib), time.Hour), // no docker project: the stand was not measured
		torn,
		res("old", "p", gib, 0, gib, nil, time.Hour),                                                          // ended before peaks were recorded
		{Kind: model.KindReservation, Project: "/r", Name: "held", State: model.StateGranted, AdmittedAt: t0}, // still held
	}})
	if len(s.ReservationFit) != 2 {
		t.Fatalf("%+v", s.ReservationFit)
	}
	// lab: stands 0.75·2+0.5·1 GiB·h unused, budgets 3·2+2·1+3·1 GiB·h unused.
	lab := ReservationFitStats{Name: "/r:lab", Reservations: 3, StandsMeasured: 2, StandPeakP50: 0.25, StandPeakP90: 0.5,
		BudgetsMeasured: 3, BudgetPeakP50: 0.25, BudgetPeakP90: 0.5, UnusedGiBHours: 2 + 11}
	// db: the peak against the 2 GiB declared, the unused part against the 1 GiB charged.
	db := ReservationFitStats{Name: "/r:db", Reservations: 1, StandsMeasured: 1, StandPeakP50: 0.25, StandPeakP90: 0.25, UnusedGiBHours: 0.5}
	if s.ReservationFit[0] != lab || s.ReservationFit[1] != db {
		t.Fatalf("%+v\nwant %+v\n     %+v", s.ReservationFit, lab, db)
	}
}

func TestSummarizeGuardEngagements(t *testing.T) {
	t0 := time.Date(2026, 10, 30, 22, 0, 0, 0, time.UTC)
	var incs []model.Incident
	for i := range 5 {
		incs = append(incs, model.Incident{At: t0.Add(time.Duration(i) * time.Hour), Kind: "guard_engaged", Value: float64(i), Detail: fmt.Sprintf("snapshot %d", i)})
	}
	s := Summarize(Input{Until: t0.Add(6 * time.Hour), Incidents: incs})
	g := s.Incidents
	want := []GuardSnapshot{{At: t0.Add(4 * time.Hour), Detail: "snapshot 4"}, {At: t0.Add(3 * time.Hour), Detail: "snapshot 3"}, {At: t0.Add(2 * time.Hour), Detail: "snapshot 2"}}
	if g.GuardEngaged != 5 || !reflect.DeepEqual(g.GuardSnapshots, want) {
		t.Fatalf("%+v", g)
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

const gib = 1 << 30

func dw(h time.Time, src, within string, b int64) store.DiskWrite {
	return store.DiskWrite{Hour: h, Source: src, Within: within, Bytes: b}
}

func TestSummarizeDiskHost(t *testing.T) {
	h0 := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	h1 := h0.Add(time.Hour)
	in := Input{Since: h0, Until: h1.Add(time.Hour), Items: []*model.Item{
		{Kind: model.KindReservation, Project: "P", Name: "old", DockerProject: "runi", CreatedAt: h0},
		{Kind: model.KindReservation, Project: "P", Name: "new", DockerProject: "runi", CreatedAt: h1},
	}, DiskWrites: []store.DiskWrite{
		dw(h0, "host", "", 100*gib), dw(h1, "host", "", 50*gib),
		dw(h0, "docker:runi", "", 60*gib), dw(h1, "docker:runi", "", 20*gib),
		dw(h0, "aq", "", 10*gib), dw(h1, "aq", "", -2*gib), // total 8
		dw(h0, "session:ended", "", 5*gib),
		dw(h0, "agent:claude:/p", "docker:runi", 70*gib), dw(h1, "agent:claude:/p", "docker:runi", 20*gib), // 90 > 80: clamped
		dw(h0, "agent:codex:/q", "docker:runi", 3*gib),
		dw(h0, "agent:neg:/q", "docker:runi", -5*gib), // negative: dropped
	}}
	s := Summarize(in)
	d := s.DiskHost
	if d == nil || d.Written != 150*gib || d.Hours != 2 {
		t.Fatalf("%+v", d)
	}
	var got []string
	for _, x := range d.Sources {
		got = append(got, x.Source)
	}
	// other = 150 - (80 + 8 + 5) = 57
	if !reflect.DeepEqual(got, []string{"docker:runi", "other", "aq", "session:ended"}) {
		t.Fatal(got)
	}
	if d.Sources[1].Written != 57*gib || d.Sources[2].Written != 8*gib {
		t.Fatal(d.Sources)
	}
	r := d.Sources[0]
	if r.Written != 80*gib || r.Reservation != "P:new" || len(r.Agents) != 2 || r.Agents[0].Agent != "agent:claude:/p" || r.Agents[0].Written != 80*gib || r.Agents[1].Written != 3*gib {
		t.Fatalf("%+v", r)
	}
	if len(d.Busiest) != 2 || d.Busiest[0].Hour != h0 || d.Busiest[0].Written != 100*gib || d.Busiest[0].Top != "docker:runi" || d.Busiest[0].TopWritten != 60*gib || d.Busiest[0].TopAgent != "agent:claude:/p" || d.Busiest[1].Written != 50*gib {
		t.Fatalf("%+v", d.Busiest)
	}
}

func TestSummarizeDiskEmpty(t *testing.T) {
	s := Summarize(Input{})
	if s.DiskHost != nil {
		t.Fatal(s.DiskHost)
	}
	if s.Overview == nil || s.Overview.DiskWritten != 0 {
		t.Fatal(s.Overview)
	}
}

func TestOverview(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	task := func(st model.State, r model.Reason, wait time.Duration) *model.Item {
		it := &model.Item{Kind: model.KindTask, State: st, Reason: r, CreatedAt: now}
		if wait >= 0 {
			it.AdmittedAt = now.Add(wait)
		}
		return it
	}
	nested := task(model.StateSucceeded, "", 10*time.Minute)
	nested.ParentID = "r1"
	h := now.Truncate(time.Hour)
	in := Input{Since: now, Until: now.Add(time.Hour), Items: []*model.Item{
		task(model.StateSucceeded, "", 2*time.Second),
		task(model.StateSucceeded, "", 90*time.Second),
		task(model.StateFailed, model.ReasonCommandFailed, -1),
		task(model.StateFailed, model.ReasonOOM, 4*time.Second),
		task(model.StateFailed, model.ReasonOOMHostGuard, 6*time.Second),
		task(model.StateRejected, "", -1),
		nested,
		{Kind: model.KindReservation, State: model.StateReleased},
	}, Samples: []store.HostSample{{Span: time.Minute, MemAvailable: 7, Tasks: 9}, {Span: time.Minute, MemAvailable: 3, Tasks: 4}},
		Incidents: []model.Incident{{At: now, Kind: "psi_close"}, {At: now.Add(10 * time.Second), Kind: "psi_open"}},
		Patterns:  []string{"x"}, Hits: []hook.Hit{{Pattern: 0, Client: "c"}, {Pattern: 0, Client: "c"}},
		Events:     []model.Event{{Channel: model.ChannelWebhook, Status: model.EventSent}, {Channel: model.ChannelWebhook, Status: model.EventAcked}, {Channel: model.ChannelWebhook, Status: model.EventFailed}, {Channel: model.ChannelWebhook, Status: model.EventSkipped}},
		DiskWrites: []store.DiskWrite{dw(h, "host", "", 100*gib), dw(h, "aq", "", 10*gib), dw(h, "session:s.scope", "", 60*gib), dw(h, "agent:claude:/home/u/x", "session:s.scope", 59*gib)}, Home: "/home/u",
	}
	o := Summarize(in).Overview
	if o == nil {
		t.Fatal("no overview")
	}
	// waits of admitted top-level tasks: 2, 90, 4, 6
	if o.Tasks != 7 || o.Reservations != 1 || o.WaitedOverMinute != 1 || o.WaitP50 != 4 || o.WaitP90 != 90 {
		t.Fatalf("%+v", o)
	}
	if o.Succeeded != 3 || o.Failed != 1 || o.KilledOwnLimit != 1 || o.KilledHostGuard != 1 || o.Rejected != 1 {
		t.Fatalf("%+v", o)
	}
	if o.TasksPeak != 9 || o.AvailableMin != 3 || o.PressureClosures != 1 || o.PressureSeconds != 10 || o.HookNudges != 2 {
		t.Fatalf("%+v", o)
	}
	if o.DiskWritten != 100*gib || o.DiskPerHour != 100*gib || o.AQShare != 0.1 || o.TopWriter != "session:s.scope" || o.TopWriterAgent != "agent:claude:/home/u/x" || o.TopWriterBytes != 60*gib {
		t.Fatalf("%+v", o)
	}
	if o.DeliveredShare != 2.0/3 {
		t.Fatal(o.DeliveredShare)
	}
}

func TestFindingsOrderAndCap(t *testing.T) {
	h := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	w := uint64(30 * gib)
	items := []*model.Item{
		{Kind: model.KindTask, Name: "heavy", State: model.StateSucceeded, IOWriteBytes: &w},
		{Kind: model.KindTask, State: model.StateFailed, Reason: model.ReasonOOMHostGuard},
		{Kind: model.KindTask, State: model.StateRejected, Diagnostic: "task budget: memory 1 GiB expected plus 2 GiB of nested tasks exceeds task memory 2 GiB of reservation r"},
		{Kind: model.KindTask, State: model.StateRejected, Diagnostic: "task budget: memory limit 3 GiB exceeds task memory 2 GiB of reservation r"},
	}
	in := Input{Since: h, Until: h.Add(time.Hour), Items: items,
		Runs:       []profile.Run{{Profile: "p", State: model.StateFailed, Reason: model.ReasonOOM, MemoryPeak: 100, Expected: 100}},
		Incidents:  []model.Incident{{At: h, Kind: "kernel_oom", Value: 1, Detail: "app.slice"}, {At: h, Kind: "guard_engaged", Detail: "who held it"}},
		Events:     []model.Event{{Channel: model.ChannelWebhook, Status: model.EventFailed}},
		DiskWrites: []store.DiskWrite{dw(h, "host", "", 100*gib), dw(h, "session:s.scope", "", 60*gib), dw(h, "agent:claude:/home/u/x", "session:s.scope", 59*gib)}, Home: "/home/u",
	}
	s := Summarize(in)
	// disk source, run, guard, profile, budget, notify, kernel OOM = 7 candidates; add a reservation
	s2 := in
	s2.Items = append(append([]*model.Item(nil), items...), &model.Item{Kind: model.KindReservation, Project: "P", Name: "r", Expected: 10 * gib, Limit: 10 * gib, DockerProject: "d", AdmittedAt: h, EndedAt: h.Add(3 * time.Hour), MemoryPeak: 1, TaskMemoryPeak: new(uint64)})
	f := Summarize(s2).Findings
	if len(f) != 7 {
		t.Fatalf("%d: %+v", len(f), f)
	}
	for i := 1; i < len(f); i++ {
		if f[i].Weight > f[i-1].Weight {
			t.Fatalf("not sorted: %+v", f)
		}
	}
	areas := map[string]string{}
	for _, x := range s.Findings {
		areas[x.Area] += x.Text + "\n"
	}
	if len(s.Findings) != 6 && len(s.Findings) != 7 {
		t.Fatalf("%+v", s.Findings)
	}
	if want := "session:s.scope wrote 60.0 GiB, ≈ all of it claude ~/x"; !strings.Contains(areas["disk"], want) {
		t.Fatalf("%q", areas["disk"])
	}
	if !strings.Contains(areas["budget"], "1 nested task") {
		t.Fatalf("%q", areas["budget"])
	}
}

func TestFindingsNone(t *testing.T) {
	s := Summarize(Input{})
	if s.Findings == nil || len(s.Findings) != 0 {
		t.Fatal(s.Findings)
	}
	b, _ := json.Marshal(s)
	if !strings.Contains(string(b), `"findings":[]`) {
		t.Fatal(string(b))
	}
	if p := Summarize(Input{Profile: "x"}); p.Findings == nil {
		t.Fatal("nil findings with a profile")
	}
}
