//go:build integration

package e2e

import (
	"encoding/json"
	"fmt"
	"github.com/creatorofuniverses/agent-queue/internal/daemon"
	"github.com/creatorofuniverses/agent-queue/internal/model"
	"github.com/creatorofuniverses/agent-queue/internal/stats"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestE2EProfileFromHistory(t *testing.T) {
	if _, e := exec.LookPath("python3"); e != nil {
		t.Skip("python3 unavailable")
	}
	h := newHarness(t, "")
	argv := []string{"python3", "-c", "b=bytearray(100<<20); import time; time.sleep(2)"}
	for i := 0; i < 2; i++ {
		id := h.submit([]string{"--name", "alloc", "--memory", "512M", "--timeout", "2m"}, argv...)
		if code := h.wait(id); code != 0 {
			t.Fatal(code, h.status(id))
		}
	}
	name := h.dir + ":alloc"
	show := func(n int) daemon.ProfileView {
		t.Helper()
		out, errout, code := h.aq("profile", "show", name, "--json")
		var v daemon.ProfileView
		if code != 0 || json.Unmarshal([]byte(out), &v) != nil || v.Estimate == nil || v.Estimate.Runs != n {
			t.Fatal(code, out, errout)
		}
		return v
	}
	show(2)
	id := h.submit([]string{"--name", "alloc", "--timeout", "2m"}, argv...)
	v := h.status(id)
	if !v.MemoryFromProfile || v.MemoryLimit < 120<<20 || v.MemoryLimit > 200<<20 {
		t.Fatal(v)
	}
	if code := h.wait(id); code != 0 {
		t.Fatal(code, h.status(id))
	}
	show(3)
}
func TestE2EBackfill(t *testing.T) {
	h := newHarness(t, "")
	a := h.submit([]string{"--memory", "400M", "--timeout", "20s"}, "sleep", "3")
	h.waitState(a, model.StateRunning, 10*time.Second)
	b := h.submit([]string{"--memory", "800M", "--timeout", "30s"}, "sleep", "2")
	c := h.submit([]string{"--memory", "200M", "--timeout", "5s"}, "sleep", "1")
	e := h.submit([]string{"--memory", "300M", "--timeout", "120s"}, "sleep", "1")
	for _, id := range []model.ItemID{a, b, c, e} {
		if code := h.wait(id); code != 0 {
			t.Fatal(code, h.status(id))
		}
	}
	av, bv, cv, ev := h.status(a), h.status(b), h.status(c), h.status(e)
	if av.EndedAt == nil || bv.StartedAt == nil || cv.StartedAt == nil || ev.StartedAt == nil || !cv.StartedAt.Before(*av.EndedAt) || bv.StartedAt.Before(*av.EndedAt) || ev.StartedAt.Before(*bv.StartedAt) {
		t.Fatal(av, bv, cv, ev)
	}
}
func TestE2ETopStatsSmoke(t *testing.T) {
	h := newHarness(t, "")
	id := h.submit([]string{"--memory", "100M", "--timeout", "1m"}, "true")
	if h.wait(id) != 0 {
		t.Fatal(h.status(id))
	}
	out, errout, code := h.aq("top", "--json")
	var top daemon.TopView
	if code != 0 || json.Unmarshal([]byte(out), &top) != nil || top.Physical.Used == 0 || top.Commitments.PoolMemory != poolMemory {
		t.Fatal(code, out, errout)
	}
	out, errout, code = h.aq("stats", "--json")
	var s stats.Summary
	if code != 0 || json.Unmarshal([]byte(out), &s) != nil || len(s.Items) == 0 {
		t.Fatal(code, out, errout)
	}
	out, errout, code = h.aq("stats", "export", "--format", "jsonl")
	if code != 0 || !strings.Contains(out, `"table":"items"`) {
		t.Fatal(code, out, errout)
	}
	out, errout, code = h.aq("estimate", "--memory", "100M", "--timeout", "1m", "--json")
	var v daemon.EstimateView
	if code != 0 || json.Unmarshal([]byte(out), &v) != nil || !v.StartsNow {
		t.Fatal(code, out, errout)
	}
}
func TestDaemonCPUStatParsing(t *testing.T) {
	if n, e := daemonCPUTicks([]byte("42 (name with space) S 0 0 0 0 0 0 0 0 0 0 12 8 0")); e != nil || n != 20 {
		t.Fatal(n, e)
	}
}

// daemonCPUTicks parses fields after the final ')' because comm may contain spaces.
func daemonCPUTicks(b []byte) (uint64, error) {
	i := strings.LastIndexByte(string(b), ')')
	if i < 0 {
		return 0, fmt.Errorf("invalid proc stat")
	}
	fields := strings.Fields(string(b[i+1:]))
	if len(fields) < 13 {
		return 0, fmt.Errorf("short proc stat")
	}
	u, e := strconv.ParseUint(fields[11], 10, 64)
	if e != nil {
		return 0, e
	}
	s, e := strconv.ParseUint(fields[12], 10, 64)
	return u + s, e
}
func measureDaemonCPU(t *testing.T, pid int, label string) {
	t.Helper()
	out, e := exec.Command("getconf", "CLK_TCK").Output()
	if e != nil {
		t.Fatal(e)
	}
	hz, e := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if e != nil || hz <= 0 {
		t.Fatal(string(out), e)
	}
	read := func() uint64 {
		b, e := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if e != nil {
			t.Fatal(e)
		}
		n, e := daemonCPUTicks(b)
		if e != nil {
			t.Fatal(e)
		}
		return n
	}
	start := time.Now()
	before := read()
	time.Sleep(30 * time.Second)
	seconds := float64(read()-before) / hz
	elapsed := time.Since(start).Seconds()
	t.Logf("daemon CPU %s: %.3fs CPU / %.3fs wall = %.3f%% of one CPU", label, seconds, elapsed, 100*seconds/elapsed)
}
