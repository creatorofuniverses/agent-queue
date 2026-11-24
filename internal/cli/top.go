package cli

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/creatorofuniverses/agent-queue/internal/agentsess"
	"github.com/creatorofuniverses/agent-queue/internal/daemon"
	"github.com/creatorofuniverses/agent-queue/internal/platform"
	"github.com/creatorofuniverses/agent-queue/internal/stats"
	"github.com/creatorofuniverses/agent-queue/internal/units"
)

func cmdTop(c *cmdCtx, args []string) error {
	fs := c.newFlagSet("top")
	asJSON := fs.Bool("json", false, "JSON output")
	sessions := fs.Bool("sessions", false, "show the full agent session table")
	pos, e := parseFlags(fs, args)
	if e != nil {
		return e
	}
	if len(pos) != 0 {
		return usagef("top: unexpected arguments")
	}
	var v daemon.TopView
	if e = c.call(c.ctx, "top", nil, &v); e != nil {
		return e
	}
	if *asJSON {
		return printJSON(c, v)
	}
	if *sessions {
		if v.Physical.Error != "" {
			return &exitError{code: exitUnavailable, msg: "sessions unavailable: " + v.Physical.Error}
		}
		printSessions(c, v.Sessions)
		return nil
	}
	if c.ui.pretty {
		c.prettyTop(v)
		return nil
	}
	printTop(c, v)
	return nil
}

// cmdDashboard prints the snapshot panels read (the runtime dir's dashboard.json) as JSON.
func cmdDashboard(c *cmdCtx, args []string) error {
	fs := c.newFlagSet("dashboard")
	fs.Bool("json", true, "JSON output (always)")
	pos, e := parseFlags(fs, args)
	if e != nil {
		return e
	}
	if len(pos) != 0 {
		return usagef("dashboard: unexpected arguments")
	}
	var v daemon.DashboardView
	if e = c.call(c.ctx, "dashboard", nil, &v); e != nil {
		return e
	}
	return printJSON(c, v)
}
func printTop(c *cmdCtx, v daemon.TopView) {
	fmt.Fprintf(c.stdout, "memory: %s total, %s available, swap %s used, PSI %.2f (closed=%t)\n", units.FormatSize(v.Host.MemTotal), units.FormatSize(v.Host.MemAvailable), units.FormatSize(v.Host.SwapTotal-v.Host.SwapFree), v.Host.PSIFullAvg10, v.Host.PressureClosed)
	if v.Physical.Error != "" {
		fmt.Fprintf(c.stdout, "physical: unavailable: %s\n", v.Physical.Error)
	} else {
		var agents uint64
		for _, a := range v.Physical.Agents {
			agents += a.Bytes
		}
		fmt.Fprintf(c.stdout, "physical: agents %s, run by agents %s, docker %s, tasks %s, other %s (used %s)\n", units.FormatSize(agents), units.FormatSize(v.Physical.Spawned), units.FormatSize(v.Physical.Docker), units.FormatSize(v.Physical.Tasks), units.FormatSize(v.Physical.Other), units.FormatSize(v.Physical.Used))
	}
	p := v.Commitments
	fmt.Fprintf(c.stdout, "commitments (pool %s / %d cpus): %s / %d cpus; tasks %s, stands %s, budgets %s; unconsumed %s\n", units.FormatSize(p.PoolMemory), p.PoolCPUs, units.FormatSize(p.Memory), p.CPUs, units.FormatSize(p.TasksExpected), units.FormatSize(p.Stands), units.FormatSize(p.Budgets), units.FormatSize(p.TasksUnconsumed+p.StandsUnconsumed+p.BudgetsUnconsumed+p.TeardownUnconsumed))
	head := "none"
	if v.Queue.HeadID != "" {
		head = string(v.Queue.HeadID) + " blocked by " + blockedText(v.Queue.BlockedBy, v.Queue.BlockedDetail)
	}
	fmt.Fprintf(c.stdout, "queue: %d queued, %d running, %d held; head %s\n", v.Queue.Queued, v.Queue.Running, v.Queue.Held, head)
	fmt.Fprintf(c.stdout, "disk: %s\n", c.diskText(v.Disk, time.Now()))
	printSessionsLine(c, v)
	if t := v.Tmp; t != nil {
		fmt.Fprintf(c.stdout, "tmp: %s%s\n", tmpText(t), tmpInlineSuffix(v))
	}
	printTmpScanLine(c, v)
	printAlertsLine(c, v.Alerts)
}

func printAlertsLine(c *cmdCtx, alerts []daemon.AlertView) {
	if len(alerts) == 0 {
		return
	}
	parts := make([]string, 0, len(alerts))
	for _, alert := range alerts {
		parts = append(parts, fmt.Sprintf("%s since %s (%s)", alert.Name, alert.Since.Local().Format("15:04"), alert.FormattedValue()))
	}
	fmt.Fprintln(c.stdout, "alerts: "+strings.Join(parts, ", "))
}

func tmpInlineSuffix(v daemon.TopView) string {
	if v.TmpSummary != nil && v.TmpSummary.SameFilesystem {
		return tmpSummarySuffix(v.TmpSummary)
	}
	return ""
}

func printTmpScanLine(c *cmdCtx, v daemon.TopView) {
	summary := v.TmpSummary
	if summary == nil || v.Tmp != nil && summary.SameFilesystem {
		return
	}
	space := "filesystem usage unavailable"
	if summary.FS != nil {
		space = fsSpaceText(*summary.FS)
	}
	fmt.Fprintf(c.stdout, "tmp scan: %s: %s%s\n", summary.Root, space, tmpSummarySuffix(summary))
}

func printSessionsLine(c *cmdCtx, v daemon.TopView) {
	if len(v.Sessions) == 0 && v.SessionsIdleAfterSeconds == 0 {
		return
	}
	var bytes uint64
	var idle []agentsess.Session
	for _, s := range v.Sessions {
		bytes += s.AgentBytes + s.RunBytes
		if s.IdleSeconds != nil && *s.IdleSeconds >= v.SessionsIdleAfterSeconds {
			idle = append(idle, s)
		}
	}
	fmt.Fprintf(c.stdout, "sessions: %d (%s)", len(v.Sessions), size(bytes))
	if len(idle) > 0 {
		slices.SortStableFunc(idle, func(a, b agentsess.Session) int {
			return cmp.Compare(*b.IdleSeconds, *a.IdleSeconds)
		})
		entries := make([]string, 0, min(len(idle), 3))
		for _, s := range idle[:min(len(idle), 3)] {
			entries = append(entries, fmt.Sprintf("%s %s %s", sessionName(s), compactSize(s.AgentBytes+s.RunBytes), fmtSecs(*s.IdleSeconds)))
		}
		fmt.Fprintf(c.stdout, "; idle >=%s: %s", fmtSecs(v.SessionsIdleAfterSeconds), strings.Join(entries, ", "))
	}
	fmt.Fprintln(c.stdout)
}

func printSessions(c *cmdCtx, sessions []agentsess.Session) {
	t := table{head: []string{"NAME", "CLIENT", "PROJECT", "AGENT", "RUN", "SWAP", "IDLE"}, right: map[int]bool{3: true, 4: true, 5: true, 6: true}}
	home := c.env("HOME")
	for _, s := range sessions {
		project := s.Cwd
		switch {
		case project == "":
			project = "-"
		case home != "" && home != "/" && project == home:
			project = "~"
		case home != "" && strings.HasPrefix(project, home+"/"):
			project = "~" + project[len(home):]
		}
		idle := "-"
		if s.IdleSeconds != nil {
			idle = fmtSecs(*s.IdleSeconds)
		}
		t.add(sessionName(s), s.Client, project, size(s.AgentBytes), size(s.RunBytes), size(s.SwapBytes), idle)
	}
	t.write(c.ui, c.stdout)
}

func sessionName(s agentsess.Session) string {
	if s.Name != "" {
		return s.Name
	}
	return fmt.Sprintf("pid %d", s.PID)
}

var compactSizeUnits = strings.NewReplacer(" TiB", "T", " GiB", "G", " MiB", "M", " KiB", "K", " B", "B")

func compactSize(bytes uint64) string { return compactSizeUnits.Replace(size(bytes)) }

func tmpSummarySuffix(summary *daemon.TmpSummary) string {
	if summary == nil {
		return ""
	}
	orphans, candidates := tmpSizes(summary.OrphanBytes, summary.CandidateBytes)
	s := fmt.Sprintf("; orphans %s, candidates %s", orphans, candidates)
	if len(summary.Items) > 0 {
		items := make([]string, 0, len(summary.Items))
		for _, item := range summary.Items {
			items = append(items, item.Path+" "+compactSize(item.Bytes))
		}
		s += "; largest: " + strings.Join(items, ", ")
	}
	return s
}

// diskStale is how old the last disk rate may be before aq top says so.
const diskStale = 3 * time.Minute

// diskText is the host write rate of the last sample, its top source and the
// agent estimated behind it. A rate older than diskStale carries its age, so a
// stuck sampler is not read as a quiet disk.
func (c *cmdCtx) diskText(d *daemon.DiskRate, now time.Time) string {
	if d == nil {
		return "no samples yet"
	}
	s := size(uint64(max(d.BytesPerSec, 0))) + "/s"
	if d.Top != "" {
		s += fmt.Sprintf(" — %s %s/s", d.Top, size(uint64(max(d.TopBytesPerSec, 0))))
		if d.TopAgent != "" {
			s += " (≈ " + stats.AgentLabel(d.TopAgent, c.env("HOME")) + ")"
		}
	}
	if age := now.Sub(d.At); age > diskStale {
		s += " (as of " + fmtDur(age.Round(time.Second)) + " ago)"
	}
	return s
}

// tmpText is where task temp dirs live and how full that filesystem is.
func tmpText(t *daemon.TmpView) string {
	if t.Root == "" {
		return "state dir (" + t.Reason + ")"
	}
	s := t.Root
	if t.Tmpfs {
		s += " (tmpfs, counts as task memory)"
	}
	if t.Error != "" {
		return s + ": " + t.Error
	}
	if t.Reason != "" {
		s += " [" + t.Reason + "]"
	}
	return s + ": " + fsSpaceText(t.FSUsage)
}

func fsSpaceText(fs platform.FSUsage) string {
	return fmt.Sprintf("%s free of %s, %d of %d inodes free", size(fs.Free), size(fs.Size), fs.InodesFree, fs.Inodes)
}
