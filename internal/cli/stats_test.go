package cli

import (
	"encoding/json"
	"fmt"
	"github.com/creatorofuniverses/agent-queue/internal/daemon"
	"github.com/creatorofuniverses/agent-queue/internal/model"
	"github.com/creatorofuniverses/agent-queue/internal/stats"
	"strings"
	"testing"
	"time"
)

func TestStatsText(t *testing.T) {
	h := newHarness(t)
	h.startDaemon(t)
	code, out, errout := h.run("stats", "--all")
	if code != 0 {
		t.Fatal(code, out, errout)
	}
	for _, s := range []string{"aq stats", "Items", "Queue", "Hook hints", "Incidents"} {
		if !strings.Contains(out, s) {
			t.Fatal(out)
		}
	}
	for _, args := range [][]string{{"stats", "--since", "7x"}, {"stats", "export", "--format", "csv"}} {
		if code, _, _ = h.run(args...); code != 64 {
			t.Fatal(code)
		}
	}
	h.run("submit", "--memory", "100M", "--timeout", "1m", "--", "true")
	code, out, errout = h.run("stats", "export", "--format", "jsonl")
	if code != 0 {
		t.Fatal(code, out, errout)
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		var m map[string]any
		if e := json.Unmarshal([]byte(line), &m); e != nil || m["table"] == nil {
			t.Fatal(line, e)
		}
	}
}

func TestStatsCSVEmptyTableHasHeader(t *testing.T) {
	h := newHarness(t)
	h.startDaemon(t)
	code, out, errout := h.run("stats", "export", "--format", "csv", "--table", "incidents")
	if code != 0 || strings.TrimSpace(out) != "at,kind,value,detail" {
		t.Fatal(code, out, errout)
	}
}

func TestStatsCSVIntegersAreNotExponents(t *testing.T) {
	h := newHarness(t)
	h.startDaemon(t)
	h.run("submit", "--memory", "512M", "--timeout", "1m", "--", "true")
	code, out, errout := h.run("stats", "export", "--format", "csv", "--table", "items")
	if code != 0 || !strings.Contains(out, "536870912") || strings.Contains(out, "e+") {
		t.Fatal(code, out, errout)
	}
}

func TestPrettyStatsOOMGroupsAndMissingEstimate(t *testing.T) {
	var b strings.Builder
	c := &cmdCtx{stdout: &b, env: func(string) string { return "" }}
	c.printStats(stats.Summary{
		Profiles:  []stats.ProfileStats{{Profile: "/r:tests", Runs: 1, Usable: 1, PeakToExpectedP50: 0.5, PeakToExpectedP90: 0.5}},
		Incidents: &stats.IncidentStats{KernelOOM: 7, KernelOOMBy: map[string]float64{"aqtest.slice": 4, "app.slice": 1}},
	}, "", true)
	out := b.String()
	if !strings.Contains(out, "kernel OOM kills outside aq: 7 (aqtest.slice 4, app.slice 1)") || !strings.Contains(out, "0.50 / 0.50            —") {
		t.Fatal(out)
	}
}

func TestPrettyStatsReservationFit(t *testing.T) {
	var b strings.Builder
	c := &cmdCtx{stdout: &b, env: func(string) string { return "/home/u" }}
	c.printStats(stats.Summary{ReservationFit: []stats.ReservationFitStats{
		{Name: "/home/u/r:lab-stand", Reservations: 3, StandsMeasured: 3, StandPeakP50: 0, StandPeakP90: 0, BudgetsMeasured: 3, BudgetPeakP50: 0.25, BudgetPeakP90: 0.5, UnusedGiBHours: 13},
		{Name: "/home/u/r:db", Reservations: 1, UnusedGiBHours: 0},
	}}, "", true)
	out := b.String()
	for _, want := range []string{"Reservation fit", "~/r:lab-stand", "0.00 / 0.00", "0.25 / 0.50", "13.0", "~/r:db"} {
		if !strings.Contains(out, want) {
			t.Fatalf("no %q in\n%s", want, out)
		}
	}
}

func TestPrintViewReservationPeaks(t *testing.T) {
	var b strings.Builder
	c := &cmdCtx{stdout: &b, env: func(string) string { return "" }}
	peak := uint64(768 << 20)
	v := daemon.ItemView{Kind: model.KindReservation, State: model.StateReleased, MemoryLimit: 1 << 30, MemoryPeak: 256 << 20, TaskMemory: 3 << 30, TaskCPUs: 2, TaskMemoryPeak: &peak}
	printView(c, v)
	out := b.String()
	if !strings.Contains(out, "stand:     1.0 GiB memory, 0 cpus, peak 256 MiB\n") || !strings.Contains(out, "budget:    3.0 GiB memory, 2 cpus, peak 768 MiB\n") {
		t.Fatal(out)
	}
	b.Reset()
	v.TaskMemoryPeak = nil // still held, or ended before aq recorded peaks
	printView(c, v)
	if out := b.String(); strings.Contains(out, ", peak") {
		t.Fatal(out)
	}
}

func TestPrintViewReservationChargedByHistory(t *testing.T) {
	var b strings.Builder
	c := &cmdCtx{stdout: &b, env: func(string) string { return "" }}
	v := daemon.ItemView{Kind: model.KindReservation, State: model.StateGranted, MemoryLimit: 2 << 30, MemoryExpected: 360 << 20, CPUs: 1,
		TaskMemory: 3 << 29, TaskMemoryExpected: 600 << 20, TaskCPUs: 1, MemoryFromProfile: true}
	printView(c, v)
	out := b.String()
	if !strings.Contains(out, "stand:     2.0 GiB memory, 1 cpus (charged 360 MiB by history)\n") ||
		!strings.Contains(out, "budget:    1.5 GiB memory, 1 cpus (charged 600 MiB by history)\n") {
		t.Fatal(out)
	}
}

func TestPrettyStatsGuardSnapshots(t *testing.T) {
	var b strings.Builder
	c := &cmdCtx{stdout: &b, env: func(string) string { return "" }}
	at := time.Date(2026, 10, 30, 22, 26, 0, 0, time.UTC)
	c.printStats(stats.Summary{Incidents: &stats.IncidentStats{GuardEngaged: 2, GuardSnapshots: []stats.GuardSnapshot{{At: at, Detail: "available 3.0 GiB; processes: python 4.0 GiB (pid 10, app-big.scope)"}}}}, "", true)
	out := b.String()
	if !strings.Contains(out, "host guard cut the aq slice 2 times; who held memory then:\n") ||
		!strings.Contains(out, at.Local().Format("01-02 15:04")+" available 3.0 GiB; processes: python 4.0 GiB (pid 10, app-big.scope)\n") {
		t.Fatal(out)
	}
}

// weekSummary is a week of a busy machine, shaped like a real report.
func weekSummary() stats.Summary {
	home := "/home/u"
	at := time.Date(2026, 11, 3, 14, 0, 0, 0, time.UTC)
	s := stats.Summary{
		Since: time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC), Until: time.Date(2026, 11, 3, 23, 0, 0, 0, time.UTC),
		Items:    []stats.ItemGroup{{Kind: "task", ClientType: "claude", Count: 800, Outcomes: map[string]int{"SUCCEEDED": 600}}},
		Queue:    &stats.QueueStats{NonEmptySeconds: 600, SampledSeconds: 6000},
		Host:     &stats.HostStats{Samples: 100},
		Delivery: []stats.ChannelStats{{Channel: "codex", Events: 10}},
		Hook:     &stats.HookStats{Hits: 259, ByClient: map[string]int{"claude": 259}},
		Incidents: &stats.IncidentStats{PSIEpisodes: 2, PSIClosedSeconds: 140,
			GuardEngaged: 1, GuardSnapshots: []stats.GuardSnapshot{{At: at, Detail: "available 3.0 GiB"}}},
		DiskHost: &stats.DiskHostStats{Written: 3 << 40, Hours: 150, Sources: []stats.DiskSourceStats{
			{Source: "session:session-3.scope", Written: 1 << 40, Agents: []stats.DiskAgentStats{{Agent: "agent:claude:" + home + "/sber/auto-evolve", Written: 1 << 40}}},
			{Source: "aq", Written: 60 << 30},
		}, Busiest: []stats.DiskHourStats{{Hour: at, Written: 200 << 30, Top: "session:session-3.scope", TopWritten: 150 << 30, TopAgent: "agent:claude:" + home + "/sber/auto-evolve"}}},
		Overview: &stats.Overview{Tasks: 1061, Reservations: 263, WaitedOverMinute: 27, WaitP50: 0.002, WaitP90: 0.042,
			Succeeded: 617, Failed: 362, KilledOwnLimit: 7, KilledHostGuard: 7, Rejected: 32,
			TasksPeak: 11 << 30, AvailableMin: 3 << 30, PressureClosures: 2, PressureSeconds: 140,
			DiskWritten: 3 << 40, DiskPerHour: 20 << 30, AQShare: 0.02, TopWriter: "session:session-3.scope", TopWriterBytes: 1 << 40,
			TopWriterAgent: "agent:claude:" + home + "/sber/auto-evolve", HookNudges: 259, DeliveredShare: 0.99},
		Findings: []stats.Finding{
			{Area: "disk", Text: "session:session-3.scope wrote 1.0 TiB, ≈ all of it claude ~/sber/auto-evolve"},
			{Area: "profiles", Text: "12 profiles peak >20% over their --memory"},
			{Area: "reservations", Text: "guardrails-qualification-full held 7 GiB·h it never used"},
		},
	}
	return s
}

func statsOut(s stats.Summary, area string, all bool) string {
	var b strings.Builder
	c := &cmdCtx{stdout: &b, env: func(string) string { return "/home/u" }}
	c.printStats(s, area, all)
	return b.String()
}

func TestStatsOverviewFitsScreen(t *testing.T) {
	out := statsOut(weekSummary(), "", false)
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) > 25 {
		t.Fatalf("%d lines:\n%s", len(lines), out)
	}
	for _, l := range lines {
		if n := len([]rune(l)); n > 100 {
			t.Fatalf("%d columns: %q", n, l)
		}
	}
	for _, want := range []string{"Queue ", "Outcomes ", "Memory ", "Disk ", "Agents ", "Worth a look", "  ! disk ", "More: aq stats tasks|profiles|disk|memory|reservations|notify|incidents · aq stats --all", "≈ claude ~/sber/auto-evolve"} {
		if !strings.Contains(out, want) {
			t.Fatalf("no %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "Items") {
		t.Fatal(out)
	}
	t.Log("\n" + out)
}

func TestStatsOverviewCutsLongNames(t *testing.T) {
	s := weekSummary()
	s.Overview.TopWriter = "docker:" + strings.Repeat("compose-project-", 6)
	s.Overview.TopWriterAgent = "agent:claude:/home/u/" + strings.Repeat("deep/", 12)
	s.Findings = []stats.Finding{{Area: "disk", Text: strings.Repeat("long finding ", 20)}}
	for _, l := range strings.Split(statsOut(s, "", false), "\n") {
		if len([]rune(l)) > 100 {
			t.Fatalf("%d columns: %q", len([]rune(l)), l)
		}
	}
}

func TestStatsOverviewNothingStandsOutAndNoDisk(t *testing.T) {
	s := weekSummary()
	s.Findings, s.DiskHost = nil, nil
	out := statsOut(s, "", false)
	if !strings.Contains(out, "nothing stands out") || !strings.Contains(out, "no samples yet") {
		t.Fatal(out)
	}
	if out := statsOut(s, "disk", false); !strings.Contains(out, "disk: no samples yet") {
		t.Fatal(out)
	}
}

func TestStatsArea(t *testing.T) {
	heads := map[string]string{
		"Items": "tasks", "Queue": "tasks", "Profiles": "profiles", "Disk writes (tasks": "disk", "Disk writes (host": "disk",
		"Disk writes (heaviest": "disk", "Host memory": "memory", "Reservations": "reservations", "Reservation fit": "reservations",
		"Notifications": "notify", "Hook hints": "notify", "Incidents": "memory,incidents",
	}
	s := weekSummary()
	s.Profiles = []stats.ProfileStats{{Profile: "/r:a", Runs: 1}}
	s.Disk = []stats.DiskStats{{Profile: "/r:a", Runs: 1, Written: 1}}
	s.Reservations = []stats.ReservationGroup{{ClientType: "claude", Count: 1}}
	s.ReservationFit = []stats.ReservationFitStats{{Name: "/r:a", Reservations: 1}}
	for _, area := range statsAreas {
		out := statsOut(s, area, false)
		for head, areas := range heads {
			has := strings.Contains("\n"+out, "\n"+head)
			if want := strings.Contains(","+areas+",", ","+area+","); has != want {
				t.Fatalf("area %s, heading %q: present=%t want=%t\n%s", area, head, has, want, out)
			}
		}
	}
}

func TestStatsAreaTruncates(t *testing.T) {
	s := weekSummary()
	for i := 0; i < 12; i++ {
		s.Profiles = append(s.Profiles, stats.ProfileStats{Profile: fmt.Sprintf("/r:p%02d", i), Runs: i + 1})
	}
	s.Profiles[3].OOM = 1 // an OOM profile is shown first, whatever its place in JSON
	out := statsOut(s, "profiles", false)
	if strings.Count(out, "/r:p") != 10 || !strings.Contains(out, "2 more: aq stats profiles --all") || !strings.Contains(out, "/r:p03") {
		t.Fatal(out)
	}
	if lines := strings.Split(out, "\n"); !strings.Contains(lines[4], "/r:p03") {
		t.Fatal(out)
	}
	if s.Profiles[0].Profile != "/r:p00" {
		t.Fatal("sorted in place")
	}
	if out := statsOut(s, "profiles", true); strings.Count(out, "/r:p") != 12 || strings.Contains(out, "more:") {
		t.Fatal(out)
	}
}

func TestStatsAll(t *testing.T) {
	s := weekSummary()
	s.Profiles = []stats.ProfileStats{{Profile: "/r:a", Runs: 1}}
	s.Disk = []stats.DiskStats{{Profile: "/r:a", Runs: 1, Written: 1}}
	s.Reservations = []stats.ReservationGroup{{ClientType: "claude", Count: 1}}
	s.ReservationFit = []stats.ReservationFitStats{{Name: "/r:a", Reservations: 1}}
	out := statsOut(s, "", true)
	for _, h := range []string{"Items", "Profiles", "Disk writes (tasks", "Disk writes (host", "Disk writes (heaviest", "Queue", "Host memory", "Reservations", "Reservation fit", "Notifications", "Hook hints", "Incidents"} {
		if !strings.Contains("\n"+out, "\n"+h) {
			t.Fatalf("no %q in\n%s", h, out)
		}
	}
	if strings.Contains(out, "more:") || strings.Contains(out, "Worth a look") {
		t.Fatal(out)
	}
}

func TestStatsUnknownArea(t *testing.T) {
	h := newHarness(t)
	h.startDaemon(t)
	code, _, errout := h.run("stats", "bogus")
	if code != 64 || !strings.Contains(errout, "tasks, profiles, disk, memory, reservations, notify, incidents") {
		t.Fatal(code, errout)
	}
}

func TestStatsOverviewFromDaemon(t *testing.T) {
	h := newHarness(t)
	h.startDaemon(t)
	code, out, errout := h.run("stats")
	if code != 0 || !strings.Contains(out, "Worth a look") {
		t.Fatal(code, out, errout)
	}
}

func TestStatsJSONIgnoresArea(t *testing.T) {
	h := newHarness(t)
	h.startDaemon(t)
	_, full, _ := h.run("stats", "--json")
	code, area, errout := h.run("stats", "disk", "--json")
	if code != 0 {
		t.Fatal(code, errout)
	}
	var a, b map[string]any
	if json.Unmarshal([]byte(full), &a) != nil || json.Unmarshal([]byte(area), &b) != nil || len(a) != len(b) || a["items"] == nil {
		t.Fatal(full, area)
	}
	if _, ok := b["findings"]; !ok {
		t.Fatal(area)
	}
}
