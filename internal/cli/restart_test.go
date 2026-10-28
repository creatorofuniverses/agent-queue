package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/creatorofuniverses/agent-queue/internal/platform"
)

func TestWaitSurvivesDaemonRestart(t *testing.T) {
	h := newHarness(t)
	stop := h.startDaemon(t)
	_, out, _ := h.run("submit", "--memory", "100M", "--timeout", "1m", "--", "true")
	id := strings.TrimSpace(out)
	spec := h.launched(t, 1)[0]
	h.ex.SetAlive(spec.ID, true)
	type result struct {
		code        int
		out, errout string
	}
	done := make(chan result, 1)
	go func() {
		c, o, e := h.run("wait", id)
		done <- result{c, o, e}
	}()
	time.Sleep(100 * time.Millisecond)
	stop() // the waiter loses its connection
	time.Sleep(300 * time.Millisecond)
	h.startDaemon(t) // recovery adopts the still-running unit
	h.ex.Finish(spec.ID, platform.ExitInfo{ServiceResult: "success", ExitCode: "exited", ExitStatus: "0", EndedAt: time.Now()})
	h.ex.SetAlive(spec.ID, false)
	select {
	case r := <-done:
		if r.code != 0 || !strings.Contains(r.out, "SUCCEEDED") || !strings.Contains(r.errout, "reconnecting") {
			t.Fatalf("%+v", r)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("wait did not return after the restart")
	}
}

func TestWaitGivesUpWithoutDaemon(t *testing.T) {
	old := waitReconnect
	waitReconnect = 300 * time.Millisecond
	defer func() { waitReconnect = old }()
	h := newHarness(t)
	code, _, errout := h.run("wait", "5a0e71f2-6f3d-45a0-b6a5-291c7af58dca")
	if code != exitUnavailable || !strings.Contains(errout, "reconnecting") {
		t.Fatal(code, errout)
	}
}
