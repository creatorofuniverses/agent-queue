package cli

import (
	"fmt"
	"github.com/creatorofuniverses/agent-queue/internal/daemon"
	"github.com/creatorofuniverses/agent-queue/internal/stats"
	"github.com/creatorofuniverses/agent-queue/internal/units"
	"time"
)

func cmdTop(c *cmdCtx, args []string) error {
	fs := c.newFlagSet("top")
	asJSON := fs.Bool("json", false, "JSON output")
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
	if c.ui.pretty {
		c.prettyTop(v)
		return nil
	}
	printTop(c, v)
	return nil
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
	if t := v.Tmp; t != nil {
		fmt.Fprintf(c.stdout, "tmp: %s\n", tmpText(t))
	}
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
	return fmt.Sprintf("%s: %s free of %s, %d of %d inodes free", s, units.FormatSize(t.Free), units.FormatSize(t.Size), t.InodesFree, t.Inodes)
}
