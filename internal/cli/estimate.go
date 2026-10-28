package cli

import (
	"fmt"
	"os"
	"resource-dispatcher/internal/client"
	"resource-dispatcher/internal/daemon"
	"resource-dispatcher/internal/units"
)

func cmdEstimate(c *cmdCtx, args []string) error {
	fs := c.newFlagSet("estimate")
	o := client.EstimateOptions{}
	fs.StringVar(&o.Memory, "memory", "", "memory limit")
	fs.StringVar(&o.Expected, "expected", "", "expected memory")
	fs.StringVar(&o.Timeout, "timeout", "", "duration bound")
	fs.StringVar(&o.Priority, "priority", "normal", "priority")
	fs.StringVar(&o.Profile, "profile", "", "profile name")
	fs.StringVar(&o.Name, "name", "", "task name")
	fs.StringVar(&o.Project, "project", "", "project")
	fs.IntVar(&o.CPUs, "cpus", 1, "CPUs")
	asJSON := fs.Bool("json", false, "JSON output")
	pos, e := parseFlags(fs, args)
	if e != nil {
		return e
	}
	if len(pos) != 0 {
		return usagef("estimate: unexpected argument")
	}
	o.Cwd, e = os.Getwd()
	if e != nil {
		return e
	}
	a, e := client.BuildEstimate(c.ctx, o)
	if e != nil {
		return e
	}
	var v daemon.EstimateView
	if e = c.call(c.ctx, "estimate", a, &v); e != nil {
		return e
	}
	if *asJSON {
		return printJSON(c, v)
	}
	if v.StartsNow {
		if v.Backfill {
			fmt.Fprintln(c.stdout, "starts now (backfill)")
		} else {
			fmt.Fprintln(c.stdout, "starts now")
		}
	} else {
		fmt.Fprintf(c.stdout, "queued: position %d, expected start %s\n", v.Position, startText(v.ExpectedStart))
		if v.ExpectedStart == nil {
			fmt.Fprintln(c.stdout, "expected start unknown")
		}
	}
	fmt.Fprintf(c.stdout, "memory: %s limit, %s expected\nduration: %s (%s)\nblocked by: %s %s\nconfidence: %s\n", units.FormatSize(v.Limit), units.FormatSize(v.Expected), secs(v.DurationSeconds), v.DurationSource, v.BlockedBy, v.BlockedDetail, v.Confidence)
	return nil
}
