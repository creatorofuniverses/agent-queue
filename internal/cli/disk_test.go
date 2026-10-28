package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/creatorofuniverses/agent-queue/internal/platform"
	"github.com/creatorofuniverses/agent-queue/internal/stats"
)

// finishTask ends the n-th launched task with ei and waits until aq result shows it finished.
func finishTask(t *testing.T, h *harness, n int, ei platform.ExitInfo) string {
	t.Helper()
	spec := h.launched(t, n)[n-1]
	h.ex.Finish(spec.ID, ei)
	h.ex.SetAlive(spec.ID, false)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, out, _ := h.run("result", string(spec.ID)); strings.Contains(out, "SUCCEEDED") {
			return string(spec.ID)
		}
		if time.Now().After(deadline) {
			t.Fatal("task did not finish")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestResultShowsDiskIO(t *testing.T) {
	h := newHarness(t)
	h.startDaemon(t)
	ok := platform.ExitInfo{ServiceResult: "success", ExitCode: "exited", ExitStatus: "0", MemoryPeak: 10 << 20, EndedAt: time.Now()}

	h.run("submit", "--memory", "100M", "--timeout", "1m", "--name", "db", "--", "true")
	w, r := uint64(3<<30), uint64(1<<20)
	io := ok
	io.IOWriteBytes, io.IOReadBytes = &w, &r
	id := finishTask(t, h, 1, io)
	_, out, _ := h.run("result", id)
	if !strings.Contains(out, "disk:      3.0 GiB written, 1 MiB read\n") {
		t.Fatal(out)
	}
	_, js, _ := h.run("result", id, "--json")
	if !strings.Contains(js, `"io_write_bytes": 3221225472`) || !strings.Contains(js, `"io_read_bytes": 1048576`) {
		t.Fatal(js)
	}
	_, prof, _ := h.run("profile", "show", "db")
	if !strings.Contains(prof, "disk writes: 3.0 GiB per run (median of 1)") {
		t.Fatal(prof)
	}

	// Without an io controller the bytes are unknown: no line, no JSON field.
	h.run("submit", "--memory", "100M", "--timeout", "1m", "--", "true")
	id = finishTask(t, h, 2, ok)
	_, out, _ = h.run("result", id)
	_, js, _ = h.run("result", id, "--json")
	if strings.Contains(out, "disk:") || strings.Contains(js, "io_write_bytes") {
		t.Fatal(out, js)
	}
}

func TestStatsShowsDiskWrites(t *testing.T) {
	var b strings.Builder
	c := &cmdCtx{stdout: &b, env: func(string) string { return "" }}
	c.printStats(stats.Summary{Disk: []stats.DiskStats{{Profile: "/r:db", Runs: 2, Written: 3 << 30, P50: 1 << 30, Max: 2 << 30}, {Runs: 1, Written: 1}}})
	out := b.String()
	if !strings.Contains(out, "Disk writes") || !strings.Contains(out, "/r:db") || !strings.Contains(out, "3.0 GiB") || !strings.Contains(out, "(unnamed)") {
		t.Fatal(out)
	}
}
