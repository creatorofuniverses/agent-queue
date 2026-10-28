package cli

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"agent-queue/internal/daemon"
	"agent-queue/internal/model"
	"agent-queue/internal/stats"
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
		t.add(u.dim(shortID(v.ID)), truncate(name, 32), u.stateText(v.State, v.Reason), mem, fmt.Sprint(v.CPUs), fmtSecs(v.WaitSeconds), fmtSecs(v.RunSeconds), note)
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
		parts = append(parts, part{"docker", p.Docker}, part{"aq tasks", p.Tasks}, part{"other", p.Other})
		for _, x := range parts {
			fmt.Fprintf(w, "  %s %s %s\n", pad(x.name, 12), u.bar(x.bytes, p.Used, 24), size(x.bytes))
		}
	}

	fmt.Fprintln(w)
	cm := v.Commitments
	u.heading(w, "aq commitments")
	fmt.Fprintf(w, "  %s %s %s / %s pool\n", pad("memory", 12), u.bar(cm.Memory, cm.PoolMemory, 24), size(cm.Memory), size(cm.PoolMemory))
	fmt.Fprintf(w, "  %s %s %d / %d cpus\n", pad("cpus", 12), u.bar(uint64(cm.CPUs), uint64(cm.PoolCPUs), 24), cm.CPUs, cm.PoolCPUs)
	if cm.Memory > 0 {
		fmt.Fprintf(w, "  %s tasks %s · stands %s · budgets %s · not yet used %s\n", u.dim(pad("of which", 12)), size(cm.TasksExpected), size(cm.Stands), size(cm.Budgets),
			size(cm.TasksUnconsumed+cm.StandsUnconsumed+cm.BudgetsUnconsumed+cm.TeardownUnconsumed))
	}

	fmt.Fprintln(w)
	q := v.Queue
	u.heading(w, "Queue")
	fmt.Fprintf(w, "  %d running · %d held · %d queued\n", q.Running, q.Held, q.Queued)
	if q.Queued > 0 && q.HeadID != "" {
		fmt.Fprintf(w, "  %s %s %s\n", u.dim("next:"), shortID(q.HeadID), u.yellow("blocked by "+blockedText(q.BlockedBy, q.BlockedDetail)))
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

// printStats is the text summary of aq stats; plain and pretty share it.
func (c *cmdCtx) printStats(v stats.Summary) {
	u, w := c.ui, c.stdout
	period := fmt.Sprintf("%s — %s", v.Since.Local().Format("2006-01-02 15:04"), v.Until.Local().Format("2006-01-02 15:04"))
	fmt.Fprintf(w, "%s %s\n", u.bold("aq stats"), u.dim(period))

	section := func(title string) { fmt.Fprintln(w); u.heading(w, title) }
	section("Items")
	if len(v.Items) == 0 {
		fmt.Fprintln(w, u.dim("  none"))
	} else {
		t := &table{head: []string{"KIND", "CLIENT", "COUNT", "OUTCOMES", "WAIT p50/p90", "RUN p50/p90"}, right: map[int]bool{2: true}}
		for _, g := range v.Items {
			t.add(g.Kind, g.ClientType, fmt.Sprint(g.Count), counts(g.Outcomes), fmtSecs(g.WaitP50)+" / "+fmtSecs(g.WaitP90), fmtSecs(g.RunP50)+" / "+fmtSecs(g.RunP90))
		}
		indent(w, t, u)
	}

	if len(v.Profiles) > 0 {
		section("Profiles")
		t := &table{head: []string{"PROFILE", "RUNS", "USABLE", "OOM", "TIMEOUT", "FROM HISTORY", "OVER EXPECTED", "PEAK/EXPECTED p50/p90", "TIME/ESTIMATE p50/p90"}, right: map[int]bool{1: true, 2: true, 3: true, 4: true, 5: true, 6: true}}
		for _, p := range v.Profiles {
			t.add(shortProfile(p.Profile, c.env("HOME")), fmt.Sprint(p.Runs), fmt.Sprint(p.Usable), fmt.Sprint(p.OOM), fmt.Sprint(p.Timeouts), fmt.Sprint(p.MemoryFromProfile), fmt.Sprint(p.PeakOverExpected),
				fmt.Sprintf("%.2f / %.2f", p.PeakToExpectedP50, p.PeakToExpectedP90), fmt.Sprintf("%.2f / %.2f", p.DurationToEstimateP50, p.DurationToEstimateP90))
		}
		indent(w, t, u)
	}
	if v.Profile != "" {
		return
	}

	if q := v.Queue; q != nil {
		section("Queue")
		line := fmt.Sprintf("  not empty %s of %s sampled", fmtSecs(q.NonEmptySeconds), fmtSecs(q.SampledSeconds))
		if len(q.BlockedSeconds) > 0 {
			var parts []string
			for _, k := range sortedKeys(q.BlockedSeconds) {
				parts = append(parts, fmt.Sprintf("%s %s", k, fmtSecs(q.BlockedSeconds[k])))
			}
			line += " · head blocked by " + strings.Join(parts, ", ")
		}
		fmt.Fprintln(w, line)
	}
	if h := v.Host; h != nil && h.Samples > 0 {
		section(fmt.Sprintf("Host memory (%d samples)", h.Samples))
		t := &table{head: []string{"", "AVG", "MAX"}, right: map[int]bool{1: true, 2: true}}
		t.add("agents", size(h.AgentsAvg), size(h.AgentsMax))
		t.add("docker", size(h.DockerAvg), size(h.DockerMax))
		t.add("aq tasks", size(h.TasksAvg), size(h.TasksMax))
		t.add("other", size(h.OtherAvg), size(h.OtherMax))
		t.add("committed", size(h.CommittedAvg), size(h.CommittedMax))
		t.add("not yet used", size(h.UnconsumedAvg), size(h.UnconsumedMax))
		indent(w, t, u)
		fmt.Fprintf(w, "  available min %s · avg %s · swap max %s · PSI max %.2f\n", size(h.MemAvailableMin), size(h.MemAvailableAvg), size(h.SwapUsedMax), h.PSIMax)
	}
	if len(v.Reservations) > 0 {
		section("Reservations")
		t := &table{head: []string{"CLIENT", "COUNT", "ACTIVE", "EXPIRED", "RELEASED", "FORCED", "TORN DOWN", "TEARDOWN FAILED", "UNKNOWN", "EXTENSIONS"}, right: map[int]bool{1: true, 2: true, 3: true, 4: true, 5: true, 6: true, 7: true, 8: true, 9: true}}
		for _, r := range v.Reservations {
			t.add(r.ClientType, fmt.Sprint(r.Count), fmt.Sprint(r.Active), fmt.Sprint(r.Expired), fmt.Sprint(r.Released), fmt.Sprint(r.ForceReleased), fmt.Sprint(r.TornDown), fmt.Sprint(r.TeardownFailed), fmt.Sprint(r.TeardownUnknown), fmt.Sprint(r.Extensions))
		}
		indent(w, t, u)
	}
	if len(v.Delivery) > 0 {
		section("Notifications")
		t := &table{head: []string{"CHANNEL", "EVENTS", "SENT", "ACKED", "PENDING", "FAILED", "SKIPPED", "UNACKED", "LATENCY p50/p90"}, right: map[int]bool{1: true, 2: true, 3: true, 4: true, 5: true, 6: true, 7: true}}
		for _, d := range v.Delivery {
			t.add(d.Channel, fmt.Sprint(d.Events), fmt.Sprint(d.Sent), fmt.Sprint(d.Acked), fmt.Sprint(d.Pending), fmt.Sprint(d.Failed), fmt.Sprint(d.Skipped), fmt.Sprint(d.Unacked), fmtSecs(d.LatencyP50)+" / "+fmtSecs(d.LatencyP90))
		}
		indent(w, t, u)
	}
	if h := v.Hook; h != nil {
		section(fmt.Sprintf("Hook hints (%d)", h.Hits))
		if len(h.ByClient) > 0 {
			fmt.Fprintf(w, "  by client: %s\n", counts(h.ByClient))
		}
		for _, p := range h.ByPattern {
			fmt.Fprintf(w, "  %s %s %s\n", u.dim(fmt.Sprintf("#%d", p.Index+1)), pad(fmt.Sprint(p.Hits), 5), u.dim(p.Pattern))
		}
	}
	if i := v.Incidents; i != nil {
		section("Incidents")
		bad := func(n float64, s string) string {
			if n > 0 {
				return u.red(s)
			}
			return s
		}
		fmt.Fprintf(w, "  kernel OOM kills outside aq: %s\n", bad(i.KernelOOM, fmt.Sprintf("%.0f", i.KernelOOM)))
		fmt.Fprintf(w, "  systemd-oomd kills: %s outside aq, %d of aq tasks\n", bad(float64(i.OOMDOutside), fmt.Sprint(i.OOMDOutside)), i.OOMDTasks)
		fmt.Fprintf(w, "  memory pressure closed admission %d times, %s in total\n", i.PSIEpisodes, fmtSecs(i.PSIClosedSeconds))
	}
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
