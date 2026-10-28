package cli

import (
	"encoding/json"
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
	for _, s := range []string{"items:", "profiles:", "queue:", "host:", "reservations:", "delivery:", "hook:", "incidents:"} {
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
