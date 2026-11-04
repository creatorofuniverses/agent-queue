package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/creatorofuniverses/agent-queue/internal/stats"
)

// statsAreas are the names `aq stats AREA` accepts, in the order help lists them.
var statsAreas = []string{"tasks", "profiles", "disk", "memory", "reservations", "notify", "incidents"}

const (
	// statsAreaRows is how many rows a table keeps in an area view.
	statsAreaRows = 10
	// statsLineWidth is the width the overview lines stay within in plain mode.
	statsLineWidth = 100
)

// printStats is the text of aq stats; plain and pretty share it. Without an
// area or all it is the one-screen overview; --profile has no overview and
// shows the per-profile sections instead.
func (c *cmdCtx) printStats(v stats.Summary, area string, all bool) {
	u, w := c.ui, c.stdout
	period := fmt.Sprintf("%s — %s", v.Since.Local().Format("2006-01-02 15:04"), v.Until.Local().Format("2006-01-02 15:04"))
	fmt.Fprintf(w, "%s %s\n", u.bold("aq stats"), u.dim(period))
	limit := statsAreaRows
	if all {
		limit = 0
	}
	switch {
	case area != "":
		c.statsArea(v, area, limit)
	case v.Profile != "" || (v.Overview == nil && !all):
		c.statsEverything(v, limit)
	case all:
		c.statsEverything(v, 0)
	default:
		c.statsOverview(v)
	}
}

// statsArea prints the sections of one area; an area view never has more than
// statsAreaRows rows per table unless --all lifts the limit.
func (c *cmdCtx) statsArea(v stats.Summary, area string, limit int) {
	switch area {
	case "tasks":
		c.statsItems(v, limit)
		c.statsQueue(v, limit)
	case "profiles":
		c.statsProfiles(v, limit)
	case "disk":
		c.statsDiskSources(v, limit)
		c.statsDiskHours(v, limit)
		c.statsTaskDisk(v, limit)
	case "memory":
		c.statsHost(v, limit)
		c.statsIncidents(v, limit)
	case "reservations":
		c.statsReservations(v, limit)
		c.statsFit(v, limit)
	case "notify":
		c.statsNotify(v, limit)
		c.statsHook(v, limit)
	case "incidents":
		c.statsIncidents(v, limit)
	}
}

// statsEverything is every section, in the order of the old long report.
func (c *cmdCtx) statsEverything(v stats.Summary, limit int) {
	c.statsItems(v, limit)
	c.statsProfiles(v, limit)
	c.statsTaskDisk(v, limit)
	if v.Profile != "" {
		return
	}
	c.statsDiskSources(v, limit)
	c.statsDiskHours(v, limit)
	c.statsQueue(v, limit)
	c.statsHost(v, limit)
	c.statsReservations(v, limit)
	c.statsFit(v, limit)
	c.statsNotify(v, limit)
	c.statsHook(v, limit)
	c.statsIncidents(v, limit)
}

func (c *cmdCtx) section(title string) {
	fmt.Fprintln(c.stdout)
	c.ui.heading(c.stdout, title)
}

// capRows keeps the first limit of n rows (limit 0 keeps all) and prints the
// hint for the rest after the table has been written.
func capRows(n, limit int) int {
	if limit > 0 && n > limit {
		return limit
	}
	return n
}

func (c *cmdCtx) more(hidden int, area string) {
	if hidden > 0 {
		fmt.Fprintf(c.stdout, "  %s\n", c.ui.dim(fmt.Sprintf("%d more: aq stats %s --all", hidden, area)))
	}
}

func (c *cmdCtx) statsItems(v stats.Summary, limit int) {
	w, u := c.stdout, c.ui
	c.section("Items")
	if len(v.Items) == 0 {
		fmt.Fprintln(w, u.dim("  none"))
		return
	}
	n := capRows(len(v.Items), limit)
	t := &table{head: []string{"KIND", "CLIENT", "COUNT", "OUTCOMES", "WAIT p50/p90", "RUN p50/p90"}, right: map[int]bool{2: true}}
	for _, g := range v.Items[:n] {
		t.add(g.Kind, g.ClientType, fmt.Sprint(g.Count), counts(g.Outcomes), fmtSecs(g.WaitP50)+" / "+fmtSecs(g.WaitP90), fmtSecs(g.RunP50)+" / "+fmtSecs(g.RunP90))
	}
	indent(w, t, u)
	c.more(len(v.Items)-n, "tasks")
}

// profileOrder puts the profiles worth a look first: OOM kills, then peaks
// over the forecast, then the busiest. It sorts a copy; JSON keeps its order.
func profileOrder(ps []stats.ProfileStats) []stats.ProfileStats {
	out := append([]stats.ProfileStats(nil), ps...)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.OOM != b.OOM {
			return a.OOM > b.OOM
		}
		if a.PeakOverExpected != b.PeakOverExpected {
			return a.PeakOverExpected > b.PeakOverExpected
		}
		return a.Runs > b.Runs
	})
	return out
}

func (c *cmdCtx) statsProfiles(v stats.Summary, limit int) {
	if len(v.Profiles) == 0 {
		return
	}
	c.section("Profiles")
	ps := profileOrder(v.Profiles)
	n := capRows(len(ps), limit)
	t := &table{head: []string{"PROFILE", "RUNS", "USABLE", "OOM", "TIMEOUT", "FROM HISTORY", "OVER EXPECTED", "PEAK/EXPECTED p50/p90", "TIME/ESTIMATE p50/p90"}, right: map[int]bool{1: true, 2: true, 3: true, 4: true, 5: true, 6: true}}
	for _, p := range ps[:n] {
		t.add(shortProfile(p.Profile, c.env("HOME")), fmt.Sprint(p.Runs), fmt.Sprint(p.Usable), fmt.Sprint(p.OOM), fmt.Sprint(p.Timeouts), fmt.Sprint(p.MemoryFromProfile), fmt.Sprint(p.PeakOverExpected),
			ratios(p.PeakToExpectedP50, p.PeakToExpectedP90), ratios(p.DurationToEstimateP50, p.DurationToEstimateP90))
	}
	indent(c.stdout, t, c.ui)
	c.more(len(ps)-n, "profiles")
}

func (c *cmdCtx) statsTaskDisk(v stats.Summary, limit int) {
	if len(v.Disk) == 0 {
		return
	}
	c.section("Disk writes (tasks, physical disks)")
	n := capRows(len(v.Disk), limit)
	t := &table{head: []string{"PROFILE", "RUNS", "WRITTEN", "PER RUN p50", "MAX"}, right: map[int]bool{1: true, 2: true, 3: true, 4: true}}
	for _, d := range v.Disk[:n] {
		name := shortProfile(d.Profile, c.env("HOME"))
		if name == "" {
			name = "(unnamed)"
		}
		t.add(name, fmt.Sprint(d.Runs), size(d.Written), size(d.P50), size(d.Max))
	}
	indent(c.stdout, t, c.ui)
	c.more(len(v.Disk)-n, "disk")
}

func (c *cmdCtx) statsDiskSources(v stats.Summary, limit int) {
	w, u := c.stdout, c.ui
	d := v.DiskHost
	if d == nil {
		fmt.Fprintln(w)
		fmt.Fprintln(w, u.dim("disk: no samples yet"))
		return
	}
	c.section("Disk writes (host, by source)")
	line := fmt.Sprintf("  %s written", size(d.Written))
	if d.Hours > 0 {
		line += fmt.Sprintf(" in %.0f h with data (%s/h)", d.Hours, size(uint64(float64(d.Written)/d.Hours)))
	}
	fmt.Fprintln(w, line)
	if len(d.Sources) == 0 {
		return
	}
	n := capRows(len(d.Sources), limit)
	t := &table{head: []string{"SOURCE", "WRITTEN", "SHARE", "INCLUDING (estimate)"}, right: map[int]bool{1: true, 2: true}}
	for _, s := range d.Sources[:n] {
		name := s.Source
		if s.Reservation != "" {
			name += " (" + shortProfile(s.Reservation, c.env("HOME")) + ")"
		}
		share := ""
		if d.Written > 0 {
			share = fmt.Sprintf("%.0f%%", 100*float64(s.Written)/float64(d.Written))
		}
		var agents []string
		for i, a := range s.Agents {
			if i == 3 {
				break
			}
			agents = append(agents, fmt.Sprintf("%s %s", stats.AgentLabel(a.Agent, c.env("HOME")), size(a.Written)))
		}
		t.add(name, size(s.Written), share, strings.Join(agents, ", "))
	}
	indent(w, t, u)
	c.more(len(d.Sources)-n, "disk")
}

func (c *cmdCtx) statsDiskHours(v stats.Summary, limit int) {
	d := v.DiskHost
	if d == nil || len(d.Busiest) == 0 {
		return
	}
	c.section("Disk writes (heaviest hours)")
	n := capRows(len(d.Busiest), limit)
	t := &table{head: []string{"HOUR", "WRITTEN", "TOP SOURCE", "TOP WRITTEN", "ESTIMATED AGENT"}, right: map[int]bool{1: true, 3: true}}
	for _, h := range d.Busiest[:n] {
		agent := ""
		if h.TopAgent != "" {
			agent = "≈ " + stats.AgentLabel(h.TopAgent, c.env("HOME"))
		}
		t.add(h.Hour.Local().Format("01-02 15:04"), size(h.Written), h.Top, size(h.TopWritten), agent)
	}
	indent(c.stdout, t, c.ui)
	c.more(len(d.Busiest)-n, "disk")
}

func (c *cmdCtx) statsQueue(v stats.Summary, _ int) {
	q := v.Queue
	if q == nil {
		return
	}
	c.section("Queue")
	line := fmt.Sprintf("  not empty %s of %s sampled", fmtSecs(q.NonEmptySeconds), fmtSecs(q.SampledSeconds))
	if len(q.BlockedSeconds) > 0 {
		var parts []string
		for _, k := range sortedKeys(q.BlockedSeconds) {
			parts = append(parts, fmt.Sprintf("%s %s", k, fmtSecs(q.BlockedSeconds[k])))
		}
		line += " · head blocked by " + strings.Join(parts, ", ")
	}
	fmt.Fprintln(c.stdout, line)
}

func (c *cmdCtx) statsHost(v stats.Summary, _ int) {
	h := v.Host
	if h == nil || h.Samples == 0 {
		return
	}
	c.section(fmt.Sprintf("Host memory (%d samples)", h.Samples))
	t := &table{head: []string{"", "AVG", "MAX"}, right: map[int]bool{1: true, 2: true}}
	t.add("agents", size(h.AgentsAvg), size(h.AgentsMax))
	t.add("run by agents", size(h.SpawnedAvg), size(h.SpawnedMax))
	t.add("docker", size(h.DockerAvg), size(h.DockerMax))
	t.add("aq tasks", size(h.TasksAvg), size(h.TasksMax))
	t.add("other", size(h.OtherAvg), size(h.OtherMax))
	t.add("committed", size(h.CommittedAvg), size(h.CommittedMax))
	t.add("not yet used", size(h.UnconsumedAvg), size(h.UnconsumedMax))
	indent(c.stdout, t, c.ui)
	fmt.Fprintf(c.stdout, "  available min %s · avg %s · swap max %s · PSI max %.2f\n", size(h.MemAvailableMin), size(h.MemAvailableAvg), size(h.SwapUsedMax), h.PSIMax)
}

func (c *cmdCtx) statsReservations(v stats.Summary, limit int) {
	if len(v.Reservations) == 0 {
		return
	}
	c.section("Reservations")
	n := capRows(len(v.Reservations), limit)
	t := &table{head: []string{"CLIENT", "COUNT", "ACTIVE", "EXPIRED", "RELEASED", "FORCED", "TORN DOWN", "TEARDOWN FAILED", "UNKNOWN", "EXTENSIONS"}, right: map[int]bool{1: true, 2: true, 3: true, 4: true, 5: true, 6: true, 7: true, 8: true, 9: true}}
	for _, r := range v.Reservations[:n] {
		t.add(r.ClientType, fmt.Sprint(r.Count), fmt.Sprint(r.Active), fmt.Sprint(r.Expired), fmt.Sprint(r.Released), fmt.Sprint(r.ForceReleased), fmt.Sprint(r.TornDown), fmt.Sprint(r.TeardownFailed), fmt.Sprint(r.TeardownUnknown), fmt.Sprint(r.Extensions))
	}
	indent(c.stdout, t, c.ui)
	c.more(len(v.Reservations)-n, "reservations")
}

func (c *cmdCtx) statsFit(v stats.Summary, limit int) {
	if len(v.ReservationFit) == 0 {
		return
	}
	c.section("Reservation fit (peak / declared)")
	n := capRows(len(v.ReservationFit), limit)
	t := &table{head: []string{"RESERVATION", "COUNT", "STAND p50/p90", "BUDGET p50/p90", "UNUSED GiB·h"}, right: map[int]bool{1: true, 4: true}}
	for _, r := range v.ReservationFit[:n] {
		t.add(shortProfile(r.Name, c.env("HOME")), fmt.Sprint(r.Reservations), measuredRatios(r.StandsMeasured, r.StandPeakP50, r.StandPeakP90),
			measuredRatios(r.BudgetsMeasured, r.BudgetPeakP50, r.BudgetPeakP90), fmt.Sprintf("%.1f", r.UnusedGiBHours))
	}
	indent(c.stdout, t, c.ui)
	c.more(len(v.ReservationFit)-n, "reservations")
}

func (c *cmdCtx) statsNotify(v stats.Summary, limit int) {
	if len(v.Delivery) == 0 {
		return
	}
	c.section("Notifications")
	n := capRows(len(v.Delivery), limit)
	t := &table{head: []string{"CHANNEL", "EVENTS", "SENT", "ACKED", "PENDING", "FAILED", "SKIPPED", "UNACKED", "LATENCY p50/p90"}, right: map[int]bool{1: true, 2: true, 3: true, 4: true, 5: true, 6: true, 7: true}}
	for _, d := range v.Delivery[:n] {
		t.add(d.Channel, fmt.Sprint(d.Events), fmt.Sprint(d.Sent), fmt.Sprint(d.Acked), fmt.Sprint(d.Pending), fmt.Sprint(d.Failed), fmt.Sprint(d.Skipped), fmt.Sprint(d.Unacked), fmtSecs(d.LatencyP50)+" / "+fmtSecs(d.LatencyP90))
	}
	indent(c.stdout, t, c.ui)
	c.more(len(v.Delivery)-n, "notify")
}

func (c *cmdCtx) statsHook(v stats.Summary, limit int) {
	h := v.Hook
	if h == nil {
		return
	}
	w, u := c.stdout, c.ui
	c.section(fmt.Sprintf("Hook hints (%d)", h.Hits))
	if len(h.ByClient) > 0 {
		fmt.Fprintf(w, "  by client: %s\n", counts(h.ByClient))
	}
	n := capRows(len(h.ByPattern), limit)
	for _, p := range h.ByPattern[:n] {
		fmt.Fprintf(w, "  %s %s %s\n", u.dim(fmt.Sprintf("#%d", p.Index+1)), pad(fmt.Sprint(p.Hits), 5), u.dim(p.Pattern))
	}
	c.more(len(h.ByPattern)-n, "notify")
}

func (c *cmdCtx) statsIncidents(v stats.Summary, limit int) {
	i := v.Incidents
	if i == nil {
		return
	}
	w, u := c.stdout, c.ui
	c.section("Incidents")
	bad := func(n float64, s string) string {
		if n > 0 {
			return u.red(s)
		}
		return s
	}
	fmt.Fprintf(w, "  kernel OOM kills outside aq: %s%s\n", bad(i.KernelOOM, fmt.Sprintf("%.0f", i.KernelOOM)), oomGroups(i.KernelOOMBy))
	fmt.Fprintf(w, "  systemd-oomd kills: %s outside aq, %d of aq tasks\n", bad(float64(i.OOMDOutside), fmt.Sprint(i.OOMDOutside)), i.OOMDTasks)
	fmt.Fprintf(w, "  memory pressure closed admission %d times, %s in total\n", i.PSIEpisodes, fmtSecs(i.PSIClosedSeconds))
	if i.GuardEngaged > 0 {
		fmt.Fprintf(w, "  host guard cut the aq slice %s times; who held memory then:\n", u.red(fmt.Sprint(i.GuardEngaged)))
		n := capRows(len(i.GuardSnapshots), limit)
		for _, g := range i.GuardSnapshots[:n] {
			fmt.Fprintf(w, "    %s %s\n", u.dim(g.At.Local().Format("01-02 15:04")), g.Detail)
		}
		c.more(len(i.GuardSnapshots)-n, "incidents")
	}
}

// statsOverview is the default screen: a line per area and what to look at.
func (c *cmdCtx) statsOverview(v stats.Summary) {
	w, u := c.stdout, c.ui
	o := v.Overview
	row := func(label, text string) {
		fmt.Fprintf(w, "%s %s\n", u.dim(pad(label, 10)), truncate(text, statsLineWidth-11))
	}
	fmt.Fprintln(w)

	queue := fmt.Sprintf("%d tasks, %d reservations · wait p50 %s, p90 %s", o.Tasks, o.Reservations, fmtSecs(o.WaitP50), fmtSecs(o.WaitP90))
	if o.WaitedOverMinute > 0 {
		queue += fmt.Sprintf(" · %d waited >1m", o.WaitedOverMinute)
	}
	row("Queue", queue)

	out := fmt.Sprintf("%d ok · %d failed", o.Succeeded, o.Failed)
	if k := o.KilledOwnLimit + o.KilledHostGuard; k > 0 {
		out += fmt.Sprintf(" · %d killed for memory (%d own limit, %d host guard)", k, o.KilledOwnLimit, o.KilledHostGuard)
	}
	if o.Rejected > 0 {
		out += fmt.Sprintf(" · %d rejected", o.Rejected)
	}
	row("Outcomes", out)

	mem := "no memory samples yet"
	if o.TasksPeak > 0 || o.AvailableMin > 0 {
		mem = fmt.Sprintf("tasks peak %s · host available min %s", size(o.TasksPeak), size(o.AvailableMin))
		if o.PressureClosures > 0 {
			mem += fmt.Sprintf(" · pressure closed admission %d× (%s)", o.PressureClosures, fmtSecs(o.PressureSeconds))
		} else {
			mem += " · no memory pressure"
		}
	}
	row("Memory", mem)

	disk, top := c.overviewDisk(v)
	row("Disk", disk)
	if top != "" {
		row("", top)
	}

	agents := fmt.Sprintf("%d hook nudges", o.HookNudges)
	if len(v.Delivery) > 0 {
		agents += fmt.Sprintf(" · notifications delivered %.0f%%", 100*o.DeliveredShare)
	}
	row("Agents", agents)

	fmt.Fprintln(w)
	u.heading(w, "Worth a look")
	if len(v.Findings) == 0 {
		fmt.Fprintln(w, "  nothing stands out")
	}
	for _, f := range v.Findings {
		fmt.Fprintf(w, "  %s %s %s\n", u.yellow("!"), pad(f.Area, 13), truncate(f.Text, statsLineWidth-18))
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, u.dim("More: aq stats "+strings.Join(statsAreas, "|")+" · aq stats --all"))
}

// unattributedShown is the share of the host's writes no source measured
// from which the Disk row names it.
const unattributedShown = 0.10

// overviewDisk is the Disk row, total and rate with aq's share (and the
// unattributed one when it is significant), and a second row for the top
// writer and the agent estimated behind it. Long names are cut by truncate in
// row, so neither row wraps.
func (c *cmdCtx) overviewDisk(v stats.Summary) (string, string) {
	o := v.Overview
	if v.DiskHost == nil {
		return "no samples yet", ""
	}
	head := fmt.Sprintf("%s written (%s/h) · aq tasks %.0f%%", size(o.DiskWritten), size(uint64(o.DiskPerHour)), 100*o.AQShare)
	if o.OtherShare >= unattributedShown {
		head += fmt.Sprintf(" · unattributed %.0f%%", 100*o.OtherShare)
	}
	if o.TopWriter == "" {
		return head, ""
	}
	top := fmt.Sprintf("top: %s %s", truncate(o.TopWriter, 40), size(o.TopWriterBytes))
	if o.TopWriterAgent != "" {
		top += fmt.Sprintf(" (≈ %s)", truncate(stats.AgentLabel(o.TopWriterAgent, c.env("HOME")), 38))
	}
	return head, top
}
