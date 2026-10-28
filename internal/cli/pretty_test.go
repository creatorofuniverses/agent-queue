package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/creatorofuniverses/agent-queue/internal/platform"
)

func TestHelpOverviewAndCommand(t *testing.T) {
	h := newHarness(t)
	for _, args := range [][]string{{"help"}, {"--help"}, {"-h"}} {
		code, out, _ := h.run(args...)
		if code != 0 || !strings.Contains(out, "Reservations") || !strings.Contains(out, "submit") {
			t.Fatal(args, code, out)
		}
	}
	for _, args := range [][]string{{"help", "submit"}, {"submit", "--help"}, {"profile", "--help"}, {"version", "--help"}} {
		code, out, errout := h.run(args...)
		if code != 0 || !strings.Contains(out, "Usage: aq "+args[0]) && !strings.Contains(out, "Usage: aq "+args[1]) {
			t.Fatal(args, code, out, errout)
		}
	}
	_, out, _ := h.run("help", "submit")
	if !strings.Contains(out, "--memory SIZE") || !strings.Contains(out, "Examples:") || strings.Contains(out, "4G (required)") {
		t.Fatal(out)
	}
	if code, _, errout := h.run("help", "nope"); code != 64 || !strings.Contains(errout, "unknown command") {
		t.Fatal(code, errout)
	}
	// A bad flag prints the error once and points to the help.
	code, out, errout := h.run("submit", "--wait")
	if code != 64 || out != "" || strings.Count(errout, "-wait") != 1 || !strings.Contains(errout, "aq help submit") {
		t.Fatal(code, out, errout)
	}
}

func TestPrettyModeOnlyWhenAsked(t *testing.T) {
	h := newHarness(t)
	h.startDaemon(t)
	h.run("submit", "--memory", "100M", "--timeout", "1m", "--name", "build", "--", "true")
	_, plain, _ := h.run("list")
	if !strings.HasPrefix(plain, "ID\tNAME") && !strings.HasPrefix(plain, "ID  ") {
		t.Fatal(plain)
	}
	if strings.Contains(plain, "\x1b[") {
		t.Fatal("plain output has escapes", plain)
	}
	h.vars["AQ_UI"] = "pretty"
	_, pretty, _ := h.run("list")
	if !strings.Contains(pretty, "\x1b[1mID") || !strings.Contains(pretty, "build") {
		t.Fatal(pretty)
	}
	h.vars["NO_COLOR"] = "1"
	if _, out, _ := h.run("list"); strings.Contains(out, "\x1b[") || !strings.Contains(out, "build") {
		t.Fatal(out)
	}
	if _, out, _ := h.run("top"); !strings.Contains(out, "Host memory") || !strings.Contains(out, "aq commitments") {
		t.Fatal(out)
	}
}

func TestItemIDPrefix(t *testing.T) {
	h := newHarness(t)
	h.startDaemon(t)
	_, id, _ := h.run("submit", "--memory", "100M", "--timeout", "1m", "--", "true")
	id = strings.TrimSpace(id)
	code, out, errout := h.run("status", id[:8])
	if code != 0 || !strings.Contains(out, id) {
		t.Fatal(code, out, errout)
	}
	if code, _, _ := h.run("status", "zzzzzzzz"); code == 0 {
		t.Fatal("unknown prefix resolved")
	}
}

func TestProfileShowShortName(t *testing.T) {
	h := newHarness(t)
	h.startDaemon(t)
	h.run("submit", "--memory", "100M", "--timeout", "1m", "--name", "unit", "--", "true")
	spec := h.launched(t, 1)[0]
	h.ex.Finish(spec.ID, platform.ExitInfo{ServiceResult: "success", ExitCode: "exited", ExitStatus: "0", MemoryPeak: 10 << 20, EndedAt: time.Now()})
	h.ex.SetAlive(spec.ID, false)
	deadline := time.Now().Add(5 * time.Second)
	for {
		code, out, _ := h.run("profile", "show", "unit")
		if code == 0 && strings.Contains(out, ":unit") {
			if strings.Contains(out, "SUCCEEDED/") {
				t.Fatal("empty reason printed", out)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal(code, out)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestFmtDur(t *testing.T) {
	for d, want := range map[time.Duration]string{0: "0s", 850 * time.Millisecond: "850ms", 42 * time.Second: "42s", time.Minute: "1m", 185 * time.Second: "3m05s", 2 * time.Hour: "2h", 130 * time.Minute: "2h10m", 76 * time.Hour: "3d4h"} {
		if got := fmtDur(d); got != want {
			t.Errorf("%v: %s, want %s", d, got, want)
		}
	}
}

func TestTableAlignsColoredCells(t *testing.T) {
	u := ui{pretty: true, color: true}
	tb := &table{head: []string{"A", "B"}}
	tb.add(u.green("x"), "1")
	tb.add("long", "2")
	var b strings.Builder
	tb.write(u, &b)
	lines := strings.Split(strings.TrimSpace(b.String()), "\n")
	if visibleLen(lines[1]) != visibleLen(lines[2]) {
		t.Fatalf("%q", lines)
	}
}
