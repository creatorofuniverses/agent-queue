package cli

import (
	"github.com/creatorofuniverses/agent-queue/internal/daemon"
	"strings"
	"testing"
	"time"
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
		{&daemon.DiskRate{At: now, BytesPerSec: 12 << 20, Top: "session:session-3.scope", TopBytesPerSec: 9 << 20, TopAgent: "agent:claude:/home/u/sber/auto-evolve"},
			"12 MiB/s — session:session-3.scope 9 MiB/s (≈ claude ~/sber/auto-evolve)"},
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
