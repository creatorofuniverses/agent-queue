package cli

import (
	"encoding/json"
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
	code, out, errout := h.run("stats")
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
	})
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
	}})
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
	c.printStats(stats.Summary{Incidents: &stats.IncidentStats{GuardEngaged: 2, GuardSnapshots: []stats.GuardSnapshot{{At: at, Detail: "available 3.0 GiB; processes: python 4.0 GiB (pid 10, app-big.scope)"}}}})
	out := b.String()
	if !strings.Contains(out, "host guard cut the aq slice 2 times; who held memory then:\n") ||
		!strings.Contains(out, at.Local().Format("01-02 15:04")+" available 3.0 GiB; processes: python 4.0 GiB (pid 10, app-big.scope)\n") {
		t.Fatal(out)
	}
}
