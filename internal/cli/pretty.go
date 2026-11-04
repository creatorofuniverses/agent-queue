package cli

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/creatorofuniverses/agent-queue/internal/daemon"
	"github.com/creatorofuniverses/agent-queue/internal/model"
)

// Pretty renderers: used only when c.ui.pretty (a person at a terminal).

func (c *cmdCtx) prettyView(v daemon.ItemView, res *daemon.ResultView) {
	u, w := c.ui, c.stdout
	title := v.Name
	if title == "" {
		title = string(v.Kind)
	}
	fmt.Fprintf(w, "%s %s  %s  %s\n", u.stateMark(v.State), u.bold(title), u.stateText(v.State, v.Reason), u.dim(string(v.ID)))
	if v.Diagnostic != "" {
		fmt.Fprintf(w, "  %s\n", u.red(strings.ReplaceAll(v.Diagnostic, "\n", "\n  ")))
	}
	fmt.Fprintln(w)
	if v.Profile != "" {
		src := ""
		if v.MemoryFromProfile {
			src = u.dim("  (memory from history)")
		}
		u.field(w, "profile", shortProfile(v.Profile, c.env("HOME"))+src)
	}
	if v.Kind == model.KindReservation {
		u.field(w, "stand", fmt.Sprintf("%s, %d cpus", size(v.MemoryLimit), v.CPUs))
		if v.TaskMemory > 0 || v.TaskCPUs > 0 {
			u.field(w, "task budget", fmt.Sprintf("%s, %d cpus", size(v.TaskMemory), v.TaskCPUs))
		}
		if v.StandMeasured != nil {
			u.field(w, "measured", u.bar(*v.StandMeasured, v.MemoryLimit, 20)+" "+size(*v.StandMeasured))
		}
		if v.DockerProject != "" {
			u.field(w, "docker", v.DockerProject)
		}
		if v.ExpiresAt != nil {
			left := ""
			if v.State.Held() {
				left = u.dim("  (in " + fmtDur(time.Until(*v.ExpiresAt)) + ")")
			}
			held := ""
			if v.TTLHeld {
				held = u.yellow("  held by nested tasks")
			}
			u.field(w, "expires", v.ExpiresAt.Local().Format("2006-01-02 15:04:05")+left+held)
		}
		if v.TeardownAttempt > 0 {
			u.field(w, "teardown", fmt.Sprintf("attempt %d", v.TeardownAttempt))
		}
	} else {
		mem := fmt.Sprintf("%s limit · %s expected", size(v.MemoryLimit), size(v.MemoryExpected))
		if v.MemoryPeak > 0 {
			mem = u.bar(v.MemoryPeak, v.MemoryLimit, 20) + " " + size(v.MemoryPeak) + " peak · " + mem
		}
		u.field(w, "memory", mem)
		u.field(w, "cpus", fmt.Sprint(v.CPUs))
		if d := diskText(v); d != "" {
			u.field(w, "disk", d)
		}
		if v.Within != "" {
			u.field(w, "within", string(v.Within))
		}
	}
	u.field(w, "priority", string(v.Priority))
	timing := fmt.Sprintf("waited %s · ran %s · timeout %s", fmtSecs(v.WaitSeconds), fmtSecs(v.RunSeconds), fmtDur(time.Duration(v.TimeoutSeconds)*time.Second))
	if v.EstimatedSeconds > 0 {
		timing += " · usually " + fmtSecs(v.EstimatedSeconds)
	}
	u.field(w, "time", timing)
	if v.State == model.StateQueued {
		start := startIn(v.ExpectedStart)
		if v.BlockedBy != "" {
			start += u.dim("  blocked by " + blockedText(v.BlockedBy, v.BlockedDetail))
		}
		u.field(w, "start", start)
	}
	if v.ExitCode != nil {
		code := fmt.Sprint(*v.ExitCode)
		if *v.ExitCode != 0 {
			code = u.red(code)
		}
		u.field(w, "exit code", code)
	}
	if v.Signal != "" {
		u.field(w, "signal", u.red(v.Signal))
	}
	if v.Project != "" {
		u.field(w, "project", shortProfile(v.Project, c.env("HOME")))
	}
	if res != nil {
		switch {
		case res.LogsPruned:
			u.field(w, "logs", u.dim("removed by retention"))
		default:
			u.field(w, "stdout", res.StdoutPath)
			u.field(w, "stderr", res.StderrPath)
			if res.LogsTruncated {
				u.field(w, "", u.yellow("logs truncated"))
			}
			fmt.Fprintf(w, "\n  %s aq logs %s\n", u.dim("output:"), shortID(v.ID))
		}
	}
}

func blockedText(b any, detail string) string {
	s := fmt.Sprint(b)
	if detail != "" {
		s += ": " + detail
	}
	return s
}

func (c *cmdCtx) prettyList(views []daemon.ItemView, all bool) {
	u, w := c.ui, c.stdout
	if len(views) == 0 {
		if all {
			fmt.Fprintln(w, u.dim("no items"))
		} else {
			fmt.Fprintln(w, u.dim("queue is empty; aq list --all shows finished items"))
		}
		return
	}
	t := &table{head: []string{"ID", "NAME", "STATE", "MEMORY", "CPU", "WAIT", "RUN", "NOTE"}, right: map[int]bool{3: true, 4: true, 5: true, 6: true}}
	for _, v := range views {
		name := v.Name
		if v.Kind == model.KindReservation {
			name = strings.TrimSpace("▣ " + name)
		}
		// Truncate the name alone so the reservation a task runs in stays visible.
		name = truncate(name, 32)
		if v.Within != "" {
			name = strings.TrimSpace(name + " ⊂ " + shortID(v.Within))
		}
		note := ""
		switch {
		case v.State == model.StateQueued:
			note = "starts " + startIn(v.ExpectedStart)
			if v.BlockedBy != "" {
				note += " · " + blockedText(v.BlockedBy, v.BlockedDetail)
			}
			note = u.yellow(note)
		case v.Kind == model.KindReservation && v.State.Held() && v.ExpiresAt != nil:
			note = "expires in " + fmtDur(time.Until(*v.ExpiresAt))
		case v.Diagnostic != "":
			note = u.red(truncate(strings.ReplaceAll(v.Diagnostic, "\n", " "), 50))
		}
		mem := size(v.MemoryLimit)
		if v.MemoryPeak > 0 && v.Kind == model.KindTask {
			mem = u.dim(size(v.MemoryPeak)+" /") + " " + mem
		}
		t.add(u.dim(shortID(v.ID)), name, u.stateText(v.State, v.Reason), mem, fmt.Sprint(v.CPUs), fmtSecs(v.WaitSeconds), fmtSecs(v.RunSeconds), note)
	}
	t.write(u, w)
}

func (c *cmdCtx) prettyEvents(views []daemon.ItemView) {
	u, w := c.ui, c.stdout
	t := &table{head: []string{"EVENT", "ITEM", "KIND", "STATUS", "AGE"}}
	for _, v := range views {
		for _, e := range v.Events {
			t.add(u.dim(string(e.ID)), shortID(v.ID), string(e.Kind), string(e.Status), fmtDur(time.Since(e.CreatedAt)))
		}
	}
	if len(t.rows) == 0 {
		return
	}
	fmt.Fprintln(w)
	u.heading(w, "Unacknowledged events")
	t.write(u, w)
}

func (c *cmdCtx) prettyTop(v daemon.TopView) {
	u, w := c.ui, c.stdout
	h := v.Host
	used := h.MemTotal - min(h.MemAvailable, h.MemTotal)
	u.heading(w, "Host memory")
	fmt.Fprintf(w, "  %s %s used of %s · %s available\n", u.bar(used, h.MemTotal, 30), size(used), size(h.MemTotal), u.bold(size(h.MemAvailable)))
	swap := h.SwapTotal - min(h.SwapFree, h.SwapTotal)
	psi := fmt.Sprintf("%.2f", h.PSIFullAvg10)
	if h.PressureClosed {
		psi = u.red(psi + " — admission closed")
	}
	fmt.Fprintf(w, "  %s %s · %s %s\n", u.dim("swap"), size(swap), u.dim("pressure (PSI full avg10)"), psi)

	fmt.Fprintln(w)
	u.heading(w, "Who uses it")
	if v.Physical.Error != "" {
		fmt.Fprintf(w, "  %s\n", u.dim("breakdown unavailable: "+v.Physical.Error))
	} else {
		p := v.Physical
		type part struct {
			name  string
			bytes uint64
		}
		var parts []part
		for _, a := range p.Agents {
			label := a.Name
			if a.Procs > 0 {
				label = fmt.Sprintf("%s (%d)", a.Name, a.Procs)
			}
			parts = append(parts, part{label, a.Bytes})
		}
		parts = append(parts, part{"run by agents", p.Spawned}, part{"docker", p.Docker}, part{"aq tasks", p.Tasks}, part{"other", p.Other})
		for _, x := range parts {
			fmt.Fprintf(w, "  %s %s %s\n", pad(x.name, 14), u.bar(x.bytes, p.Used, 24), size(x.bytes))
		}
	}

	fmt.Fprintln(w)
	cm := v.Commitments
	u.heading(w, "aq commitments")
	fmt.Fprintf(w, "  %s %s %s / %s pool\n", pad("memory", 14), u.bar(cm.Memory, cm.PoolMemory, 24), size(cm.Memory), size(cm.PoolMemory))
	fmt.Fprintf(w, "  %s %s %d / %d cpus\n", pad("cpus", 14), u.bar(uint64(cm.CPUs), uint64(cm.PoolCPUs), 24), cm.CPUs, cm.PoolCPUs)
	if cm.Memory > 0 {
		fmt.Fprintf(w, "  %s tasks %s · stands %s · budgets %s · not yet used %s\n", u.dim(pad("of which", 14)), size(cm.TasksExpected), size(cm.Stands), size(cm.Budgets),
			size(cm.TasksUnconsumed+cm.StandsUnconsumed+cm.BudgetsUnconsumed+cm.TeardownUnconsumed))
	}

	fmt.Fprintln(w)
	q := v.Queue
	u.heading(w, "Queue")
	fmt.Fprintf(w, "  %d running · %d held · %d queued\n", q.Running, q.Held, q.Queued)
	if q.Queued > 0 && q.HeadID != "" {
		fmt.Fprintf(w, "  %s %s %s\n", u.dim("next:"), shortID(q.HeadID), u.yellow("blocked by "+blockedText(q.BlockedBy, q.BlockedDetail)))
	}

	fmt.Fprintln(w)
	u.heading(w, "Disk writes")
	fmt.Fprintf(w, "  %s\n", c.diskText(v.Disk, time.Now()))

	if t := v.Tmp; t != nil {
		fmt.Fprintln(w)
		u.heading(w, "Temp dirs")
		switch {
		case t.Root == "":
			fmt.Fprintf(w, "  %s %s\n", u.yellow("in the state dir:"), t.Reason)
		case t.Error != "":
			fmt.Fprintf(w, "  %s %s\n", t.Root, u.red(t.Error))
		default:
			used := t.Size - min(t.Free, t.Size)
			fmt.Fprintf(w, "  %s %s %s used of %s · %d inodes free\n", pad("space", 14), u.bar(used, t.Size, 24), size(used), size(t.Size), t.InodesFree)
			note := t.Root
			if t.Tmpfs {
				note += " · tmpfs: temp files count as task memory"
			}
			fmt.Fprintf(w, "  %s\n", u.dim(note))
			if t.Reason != "" {
				fmt.Fprintf(w, "  %s\n", u.yellow(t.Reason))
			}
		}
	}
}

func (c *cmdCtx) prettyProfiles(vs []daemon.ProfileListEntry) {
	u, w := c.ui, c.stdout
	if len(vs) == 0 {
		fmt.Fprintln(w, u.dim("no profiles yet: run tasks with aq submit --name"))
		return
	}
	t := &table{head: []string{"PROFILE", "RUNS", "USABLE", "LAST RUN"}, right: map[int]bool{1: true, 2: true}}
	for _, v := range vs {
		t.add(shortProfile(v.Profile, c.env("HOME")), fmt.Sprint(v.Runs), fmt.Sprint(v.Usable), u.dim(fmtDur(time.Since(v.LastEnded))+" ago"))
	}
	t.write(u, w)
}

func (c *cmdCtx) prettyProfile(v daemon.ProfileView) {
	u, w := c.ui, c.stdout
	fmt.Fprintln(w, u.bold(shortProfile(v.Profile, c.env("HOME"))))
	if v.Estimate == nil {
		fmt.Fprintf(w, "  %s\n", u.yellow("no usable runs yet: pass --memory"))
	} else {
		e := v.Estimate
		fmt.Fprintf(w, "  next run gets %s limit · %s expected · about %s  %s\n", u.bold(size(e.Limit)), size(e.Expected), fmtSecs(e.DurationSeconds), u.dim(fmt.Sprintf("(from %d runs)", e.Runs)))
	}
	if v.IOWriteP50 != nil {
		fmt.Fprintf(w, "  writes %s to disk per run  %s\n", u.bold(size(*v.IOWriteP50)), u.dim(fmt.Sprintf("(median of %d)", v.IOWriteRuns)))
	}
	if len(v.Runs) == 0 {
		return
	}
	fmt.Fprintln(w)
	t := &table{head: []string{"ENDED", "OUTCOME", "TIME", "PEAK", "P90", ""}, right: map[int]bool{2: true, 3: true, 4: true}}
	for _, r := range v.Runs {
		ok := u.green("✓")
		if !r.Usable {
			ok = u.dim("unused")
		}
		t.add(u.dim(r.EndedAt.Local().Format("01-02 15:04")), u.stateText(r.State, r.Reason), fmtSecs(r.DurationSeconds), size(r.MemoryPeak), size(r.SampleP90), ok)
	}
	t.write(u, w)
}

func (c *cmdCtx) prettyEstimate(v daemon.EstimateView) {
	u, w := c.ui, c.stdout
	switch {
	case v.StartsNow && v.Backfill:
		fmt.Fprintf(w, "%s %s\n", u.green("●"), u.bold("starts now")+u.dim(" (backfill: fits before the queue head needs the memory)"))
	case v.StartsNow:
		fmt.Fprintf(w, "%s %s\n", u.green("●"), u.bold("starts now"))
	default:
		fmt.Fprintf(w, "%s %s %s\n", u.yellow("○"), u.bold(fmt.Sprintf("queued at position %d,", v.Position)), "starts "+startIn(v.ExpectedStart))
		if v.BlockedBy != "" {
			u.field(w, "blocked by", blockedText(v.BlockedBy, v.BlockedDetail))
		}
	}
	src := ""
	if v.MemoryFromProfile {
		src = u.dim("  (from history)")
	}
	u.field(w, "memory", fmt.Sprintf("%s limit · %s expected%s", size(v.Limit), size(v.Expected), src))
	u.field(w, "duration", fmt.Sprintf("%s %s", fmtSecs(v.DurationSeconds), u.dim("("+v.DurationSource+")")))
	u.field(w, "confidence", v.Confidence)
}

// ratios is a p50/p90 pair, or "—" when there was nothing to compare (no estimate).
func ratios(p50, p90 float64) string {
	if p50 == 0 && p90 == 0 {
		return "—"
	}
	return fmt.Sprintf("%.2f / %.2f", p50, p90)
}

// measuredRatios is ratios for n measurements, where 0 / 0 is a real peak of zero.
func measuredRatios(n int, p50, p90 float64) string {
	if n == 0 {
		return "—"
	}
	return fmt.Sprintf("%.2f / %.2f", p50, p90)
}

// oomGroups names the cgroups the kills were attributed to, most first.
func oomGroups(by map[string]float64) string {
	if len(by) == 0 {
		return ""
	}
	names := make([]string, 0, len(by))
	for n := range by {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		if by[names[i]] != by[names[j]] {
			return by[names[i]] > by[names[j]]
		}
		return names[i] < names[j]
	})
	parts := make([]string, len(names))
	for i, n := range names {
		parts[i] = fmt.Sprintf("%s %.0f", n, by[n])
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

func indent(w io.Writer, t *table, u ui) {
	var b strings.Builder
	t.write(u, &b)
	for _, l := range strings.Split(strings.TrimRight(b.String(), "\n"), "\n") {
		fmt.Fprintln(w, "  "+l)
	}
}

func counts(m map[string]int) string {
	var parts []string
	for _, k := range sortedKeys(m) {
		parts = append(parts, fmt.Sprintf("%s %d", k, m[k]))
	}
	return strings.Join(parts, ", ")
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
