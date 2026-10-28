package cli

import (
	"agent-queue/internal/client"
	"agent-queue/internal/daemon"
	"agent-queue/internal/units"
	"fmt"
	"os"
)

func cmdEstimate(c *cmdCtx, args []string) error {
	fs := c.newFlagSet("estimate")
	o := client.EstimateOptions{}
	fs.StringVar(&o.Memory, "memory", "", "memory `SIZE` limit (default: from the --name profile)")
	fs.StringVar(&o.Expected, "expected", "", "expected memory `SIZE`")
	fs.StringVar(&o.Timeout, "timeout", "", "timeout `DURATION`; bounds the duration when there is no history")
	fs.StringVar(&o.Priority, "priority", "normal", "`P`riority: low, normal or high")
	fs.StringVar(&o.Profile, "profile", "", "explicit profile `KEY`")
	fs.StringVar(&o.Name, "name", "", "task `NAME`; selects the profile <repo root>:<name>")
	fs.StringVar(&o.Project, "project", "", "project `DIR`")
	fs.IntVar(&o.CPUs, "cpus", 1, "`N` CPUs")
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
	if c.ui.pretty {
		c.prettyEstimate(v)
		return nil
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
	fmt.Fprintf(c.stdout, "memory: %s limit, %s expected\nduration: %s (%s)\n", units.FormatSize(v.Limit), units.FormatSize(v.Expected), secs(v.DurationSeconds), v.DurationSource)
	if v.BlockedBy != "" {
		fmt.Fprintf(c.stdout, "blocked by: %s\n", blockedText(v.BlockedBy, v.BlockedDetail))
	}
	fmt.Fprintf(c.stdout, "confidence: %s\n", v.Confidence)
	return nil
}
