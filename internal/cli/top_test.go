package cli

import (
	"strings"
	"testing"
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
