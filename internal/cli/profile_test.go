package cli

import (
	"bytes"
	"github.com/creatorofuniverses/agent-queue/internal/daemon"
	"github.com/creatorofuniverses/agent-queue/internal/model"
	"strings"
	"testing"
)

func TestCLIProfileShowText(t *testing.T) {
	h := newHarness(t)
	h.startDaemon(t)
	code, out, errout := h.run("profile", "list")
	if code != 0 {
		t.Fatal(code, out, errout)
	}
	code, _, _ = h.run("submit", "--profile", "p", "--memory", "100M", "--timeout", "1m", "--", "true")
	if code != 0 {
		t.Fatal(code)
	}
	code, _, _ = h.run("profile", "show", "p")
	if code != 1 {
		t.Fatal(code)
	}
	for _, args := range [][]string{{"profile"}, {"profile", "show"}, {"profile", "bad"}} {
		code, _, errout = h.run(args...)
		if code != 64 || !strings.Contains(errout, "profile") {
			t.Fatal(code, errout)
		}
	}
}

func TestProfileTextFormatting(t *testing.T) {
	for _, usable := range []bool{false, true} {
		var b bytes.Buffer
		c := &cmdCtx{stdout: &b}
		v := daemon.ProfileView{Profile: "p", Runs: []daemon.ProfileRunView{{State: model.StateSucceeded, Usable: true, MemoryPeak: 100 << 20}}}
		if usable {
			v.Estimate = &daemon.ProfileEstimateView{Runs: 2, Limit: 120 << 20, Expected: 100 << 20, DurationSeconds: 60}
		}
		printProfile(c, v)
		for _, s := range []string{"profile:", "estimate:", "ENDED", "STATE", "DURATION", "PEAK", "P90", "USABLE"} {
			if !strings.Contains(b.String(), s) {
				t.Fatal(b.String())
			}
		}
		if !usable && !strings.Contains(b.String(), "no usable runs") {
			t.Fatal(b.String())
		}
	}
}
