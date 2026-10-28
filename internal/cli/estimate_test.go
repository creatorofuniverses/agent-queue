package cli

import (
	"strings"
	"testing"
)

func TestEstimateText(t *testing.T) {
	h := newHarness(t)
	h.startDaemon(t)
	code, out, errout := h.run("estimate", "--memory", "100M", "--timeout", "1m")
	if code != 0 {
		t.Fatal(code, out, errout)
	}
	for _, s := range []string{"starts now", "memory:", "duration:", "confidence:"} {
		if !strings.Contains(out, s) {
			t.Fatal(out)
		}
	}
	code, _, _ = h.run("estimate")
	if code != 64 {
		t.Fatal(code)
	}
}
