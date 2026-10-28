package cli

import (
	"fmt"
	"resource-dispatcher/internal/daemon"
	"resource-dispatcher/internal/units"
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
		fmt.Fprintf(c.stdout, "physical: agents %s, docker %s, tasks %s, other %s (used %s)\n", units.FormatSize(agents), units.FormatSize(v.Physical.Docker), units.FormatSize(v.Physical.Tasks), units.FormatSize(v.Physical.Other), units.FormatSize(v.Physical.Used))
	}
	p := v.Commitments
	fmt.Fprintf(c.stdout, "commitments (pool %s / %d cpus): %s / %d cpus; tasks %s, stands %s, budgets %s; unconsumed %s\n", units.FormatSize(p.PoolMemory), p.PoolCPUs, units.FormatSize(p.Memory), p.CPUs, units.FormatSize(p.TasksExpected), units.FormatSize(p.Stands), units.FormatSize(p.Budgets), units.FormatSize(p.TasksUnconsumed+p.StandsUnconsumed+p.BudgetsUnconsumed+p.TeardownUnconsumed))
	head := "none"
	if v.Queue.HeadID != "" {
		head = string(v.Queue.HeadID) + " blocked by " + blockedText(v.Queue.BlockedBy, v.Queue.BlockedDetail)
	}
	fmt.Fprintf(c.stdout, "queue: %d queued, %d running, %d held; head %s\n", v.Queue.Queued, v.Queue.Running, v.Queue.Held, head)
}
