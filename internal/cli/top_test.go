package cli

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/creatorofuniverses/agent-queue/internal/daemon"
)

func TestTopText(t *testing.T) {
	h := newHarness(t)
	h.startDaemon(t)
	code, out, errout := h.run("top")
	if code != 0 {
		t.Fatal(code, out, errout)
	}
	for _, s := range []string{"memory:", "physical: unavailable:", "commitments (pool", "queue:", "tmp: state dir ("} {
		if !strings.Contains(out, s) {
			t.Fatal(out)
		}
	}
}

func TestTopDiskLine(t *testing.T) {
	var b strings.Builder
	c := &cmdCtx{stdout: &b, env: func(string) string { return "/home/u" }}
	now := time.Now()
	for _, tc := range []struct {
		d    *daemon.DiskRate
		want string
	}{
		{nil, "no samples yet"},
		{&daemon.DiskRate{At: now, BytesPerSec: 12 << 20, Top: "session:session-3.scope", TopBytesPerSec: 9 << 20, TopAgent: "agent:claude:/home/u/src/shop"},
			"12 MiB/s — session:session-3.scope 9 MiB/s (≈ claude ~/src/shop)"},
		{&daemon.DiskRate{At: now, BytesPerSec: 1 << 20}, "1 MiB/s"},
	} {
		if got := c.diskText(tc.d, now); got != tc.want {
			t.Fatalf("%q != %q", got, tc.want)
		}
	}
	if got := c.diskText(&daemon.DiskRate{At: now.Add(-10 * time.Minute), BytesPerSec: 1 << 20}, now); got != "1 MiB/s (as of 10m ago)" {
		t.Fatal(got)
	}
	if got := c.diskText(&daemon.DiskRate{At: now.Add(-2 * time.Minute), BytesPerSec: 1 << 20}, now); strings.Contains(got, "as of") {
		t.Fatal(got)
	}
	// Both renderers carry the line.
	v := daemon.TopView{}
	printTop(c, v)
	if !strings.Contains(b.String(), "disk: no samples yet\n") {
		t.Fatal(b.String())
	}
	b.Reset()
	c.ui = ui{pretty: true}
	c.prettyTop(v)
	if !strings.Contains(b.String(), "no samples yet") {
		t.Fatal(b.String())
	}
}

// aq dashboard prints the panel snapshot as JSON, with or without --json.
func TestDashboardJSON(t *testing.T) {
	h := newHarness(t)
	h.startDaemon(t)
	for _, args := range [][]string{{"dashboard"}, {"dashboard", "--json"}} {
		code, out, errout := h.run(args...)
		var v daemon.DashboardView
		if code != 0 || json.Unmarshal([]byte(out), &v) != nil || v.Version != daemon.DashboardVersion || v.Items == nil {
			t.Fatal(args, code, out, errout)
		}
	}
	if code, _, _ := h.run("dashboard", "extra"); code != 64 {
		t.Fatal(code)
	}
}

func TestTopSessionsLine(t *testing.T) {
	h := newHarness(t)
	sessions := []string{
		`{"pid":4,"name":"fourth","idle_seconds":72000}`,
		`{"pid":1,"name":"ruchat-75","agent_bytes":533725184,"idle_seconds":158400}`,
		`{"pid":2,"name":"qa","run_bytes":1073741824,"idle_seconds":108000}`,
		`{"pid":3,"name":"main","agent_bytes":1048576,"idle_seconds":86400}`,
		`{"pid":5,"name":"unknown","run_bytes":2902458368,"idle_seconds":null}`,
		`{"pid":6,"idle_seconds":43200}`, `{"pid":7}`, `{"pid":8}`, `{"pid":9}`, `{"pid":10}`,
	}
	payload := json.RawMessage(fmt.Sprintf(`{"sessions_idle_after_seconds":43200,"sessions":[%s]}`, strings.Join(sessions, ",")))
	stubInsight(t, h, "top", "", payload, nil)
	code, out, errout := h.run("top")
	want := "sessions: 10 (4.2 GiB); idle >=12h: ruchat-75 509M 44h, qa 1.0G 30h, main 1M 24h\n"
	if code != 0 || errout != "" || !strings.Contains(out, want) {
		t.Fatalf("want %q, got exit %d, stdout %q, stderr %q", want, code, out, errout)
	}
}

func TestTopSessionsLineIncludesIdleThreshold(t *testing.T) {
	h := newHarness(t)
	payload := json.RawMessage(`{"sessions_idle_after_seconds":43200,"sessions":[{"pid":1,"name":"at-threshold","idle_seconds":43200},{"pid":2,"name":"below-threshold","idle_seconds":43199}]}`)
	stubInsight(t, h, "top", "", payload, nil)
	code, out, errout := h.run("top")
	if code != 0 || errout != "" || !strings.Contains(out, "; idle >=12h: at-threshold 0B 12h\n") {
		t.Fatalf("want the session at the idle threshold, got exit %d, stdout %q, stderr %q", code, out, errout)
	}
}

func TestDashboardHelpDescribesPanelFields(t *testing.T) {
	h := newHarness(t)
	code, out, errout := h.run("help", "dashboard")
	if code != 0 || errout != "" {
		t.Fatal(code, out, errout)
	}
	for _, want := range []string{"sessions", "tmp", "complete", "partial_items", "alerts", "an alert opens or clears"} {
		if !strings.Contains(out, want) {
			t.Errorf("dashboard help missing %q", want)
		}
	}
}

func TestTmpHelpDescribesDefaultLimit(t *testing.T) {
	h := newHarness(t)
	code, out, errout := h.run("help", "tmp")
	if code != 0 || errout != "" || !strings.Contains(out, "[--all]") || !strings.Contains(out, "30 largest") {
		t.Fatalf("want help with default limit and --all, got exit %d, stdout %q, stderr %q", code, out, errout)
	}
}

func TestTopSessionsLineWithoutIdle(t *testing.T) {
	h := newHarness(t)
	payload := json.RawMessage(`{"sessions_idle_after_seconds":86400,"sessions":[{"pid":1,"idle_seconds":46800},{"pid":2,"idle_seconds":null}]}`)
	stubInsight(t, h, "top", "", payload, nil)
	code, out, errout := h.run("top")
	if code != 0 || errout != "" || !strings.Contains(out, "sessions: 2 (0 B)\n") {
		t.Fatalf("want sessions count without idle suffix, got exit %d, stdout %q, stderr %q", code, out, errout)
	}
}

func TestTopTmpLine(t *testing.T) {
	for _, tc := range []struct {
		name     string
		same     bool
		wantScan string
	}{
		{name: "same filesystem", same: true},
		{name: "different filesystems", wantScan: "tmp scan: /tmp: 37 B free of 100 B, 8 of 25 inodes free"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := fmt.Sprintf(`{"tmp":{"root":"/tasks/aq","size":200,"free":60,"inodes":50,"inodes_free":19},"tmp_summary":{"root":"/tmp","fs":{"size":100,"free":37,"inodes":25,"inodes_free":8},"same_filesystem":%t,"orphan_bytes":1181116006,"candidate_bytes":429496730,"scanned_at":"2026-11-24T00:00:00Z","items":[{"path":"/tmp/build","bytes":1181116006}]}}`, tc.same)
			var v daemon.TopView
			if err := json.Unmarshal([]byte(payload), &v); err != nil {
				t.Fatal(err)
			}
			var b strings.Builder
			c := &cmdCtx{stdout: &b}
			for _, pretty := range []bool{false, true} {
				b.Reset()
				c.ui.pretty = pretty
				if pretty {
					c.prettyTop(v)
				} else {
					printTop(c, v)
				}
				summary := "; orphans 1.1 GiB, candidates 0.4 GiB; largest: /tmp/build 1.1G\n"
				task := "tmp: /tasks/aq: 60 B free of 200 B, 19 of 50 inodes free"
				if pretty {
					task = "140 B used of 200 B · 19 inodes free"
				}
				if tc.same {
					if !strings.Contains(b.String(), task+summary) || strings.Contains(b.String(), "tmp scan:") {
						t.Errorf("pretty=%t: want summary on task filesystem line, got %q", pretty, b.String())
					}
				} else if !strings.Contains(b.String(), task+"\n") || !strings.Contains(b.String(), tc.wantScan+summary) {
					t.Errorf("pretty=%t: want task and scan filesystem lines, got %q", pretty, b.String())
				}
			}
		})
	}
}

func TestTopAlertsLine(t *testing.T) {
	payload := `{"alerts":[{"name":"swap_full","since":"2026-11-24T12:03:00Z","value":82,"threshold":70},{"name":"tmp_orphans","since":"2026-11-24T12:07:00Z","value":3221225472,"threshold":2147483648}]}`
	var v daemon.TopView
	if err := json.Unmarshal([]byte(payload), &v); err != nil {
		t.Fatal(err)
	}
	for _, render := range []struct {
		name string
		fn   func(*cmdCtx, daemon.TopView)
	}{
		{name: "plain", fn: printTop},
		{name: "pretty", fn: (*cmdCtx).prettyTop},
	} {
		t.Run(render.name, func(t *testing.T) {
			var b strings.Builder
			c := &cmdCtx{stdout: &b, ui: ui{pretty: render.name == "pretty"}}
			render.fn(c, daemon.TopView{})
			if strings.Contains(b.String(), "alerts:") {
				t.Errorf("unexpected alerts line in %q", b.String())
			}
			b.Reset()
			render.fn(c, v)
			at := time.Date(2026, 11, 24, 12, 3, 0, 0, time.UTC).Local()
			want := fmt.Sprintf("alerts: swap_full since %s (82%%), tmp_orphans since %s (3.0 GiB)\n",
				at.Format("15:04"), at.Add(4*time.Minute).Format("15:04"))
			if strings.Count(b.String(), "alerts:") != 1 || !strings.Contains(b.String(), want) {
				t.Errorf("want %q, got %q", want, b.String())
			}
		})
	}
}

func TestTopSessionsTable(t *testing.T) {
	h := newHarness(t)
	h.vars["HOME"] = "/home/u"
	payload := json.RawMessage(`{"sessions":[{"pid":1,"name":"ruchat-75","client":"claude","cwd":"/home/u/proj","agent_bytes":533725184,"run_bytes":1048576,"swap_bytes":1024,"idle_seconds":158400},{"pid":42,"client":"codex","cwd":"/home/u","idle_seconds":null}]}`)
	stubInsight(t, h, "top", "", payload, nil)
	code, out, errout := h.run("top", "--sessions")
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if code != 0 || errout != "" || len(lines) != 3 ||
		strings.Join(strings.Fields(lines[1]), " ") != "ruchat-75 claude ~/proj 509 MiB 1 MiB 1 KiB 44h" ||
		strings.Join(strings.Fields(lines[2]), " ") != "pid 42 codex ~ 0 B 0 B 0 B -" {
		t.Fatalf("want two session rows, got exit %d, stdout %q, stderr %q", code, out, errout)
	}
}
