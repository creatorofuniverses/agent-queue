package cli

import (
	"encoding/json"
	"github.com/creatorofuniverses/agent-queue/internal/stats"
	"strings"
	"testing"
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
