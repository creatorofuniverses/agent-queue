package cli

import (
	"fmt"
	"resource-dispatcher/internal/daemon"
	"resource-dispatcher/internal/units"
	"text/tabwriter"
	"time"
)

func cmdProfile(c *cmdCtx, args []string) error {
	if len(args) == 0 {
		return usagef("profile: expected show P or list")
	}
	fs := c.newFlagSet("profile")
	asJSON := fs.Bool("json", false, "JSON output")
	pos, e := parseFlags(fs, args[1:])
	if e != nil {
		return e
	}
	switch args[0] {
	case "show":
		if len(pos) != 1 {
			return usagef("profile show: expected profile name")
		}
		var v daemon.ProfileView
		if e = c.call(c.ctx, "profile_show", map[string]string{"profile": pos[0]}, &v); e != nil {
			return e
		}
		if *asJSON {
			return printJSON(c, v)
		}
		printProfile(c, v)
		return nil
	case "list":
		if len(pos) != 0 {
			return usagef("profile list: unexpected arguments")
		}
		var vs []daemon.ProfileListEntry
		if e = c.call(c.ctx, "profile_list", nil, &vs); e != nil {
			return e
		}
		if *asJSON {
			return printJSON(c, vs)
		}
		w := tabwriter.NewWriter(c.stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "PROFILE\tRUNS\tUSABLE\tLAST ENDED")
		for _, v := range vs {
			fmt.Fprintf(w, "%s\t%d\t%d\t%s\n", v.Profile, v.Runs, v.Usable, v.LastEnded.Format(time.RFC3339))
		}
		return w.Flush()
	default:
		return usagef("profile: expected show or list")
	}
}
func printProfile(c *cmdCtx, v daemon.ProfileView) {
	fmt.Fprintf(c.stdout, "profile: %s\n", v.Profile)
	if v.Estimate == nil {
		fmt.Fprintln(c.stdout, "estimate: no usable runs")
	} else {
		e := v.Estimate
		fmt.Fprintf(c.stdout, "estimate: %s limit, %s expected, %s duration (%d runs)\n", units.FormatSize(e.Limit), units.FormatSize(e.Expected), secs(e.DurationSeconds), e.Runs)
	}
	w := tabwriter.NewWriter(c.stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ENDED\tSTATE\tDURATION\tPEAK\tP90\tUSABLE")
	for _, r := range v.Runs {
		fmt.Fprintf(w, "%s\t%s/%s\t%s\t%s\t%s\t%t\n", r.EndedAt.Format(time.RFC3339), r.State, r.Reason, secs(r.DurationSeconds), units.FormatSize(r.MemoryPeak), units.FormatSize(r.SampleP90), r.Usable)
	}
	w.Flush()
}
