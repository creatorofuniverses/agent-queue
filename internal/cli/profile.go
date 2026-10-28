package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/creatorofuniverses/agent-queue/internal/client"
	"github.com/creatorofuniverses/agent-queue/internal/daemon"
	"github.com/creatorofuniverses/agent-queue/internal/units"
	"text/tabwriter"
	"time"
)

func cmdProfile(c *cmdCtx, args []string) error {
	fs := c.newFlagSet("profile")
	asJSON := fs.Bool("json", false, "JSON output")
	if len(args) > 0 && isHelpArg(args[0]) {
		_, e := parseFlags(fs, args)
		return e
	}
	if len(args) == 0 {
		return usagef("profile: expected show NAME or list")
	}
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
		if e = c.call(c.ctx, "profile_show", map[string]string{"profile": c.profileKey(pos[0])}, &v); e != nil {
			return e
		}
		if *asJSON {
			return printJSON(c, v)
		}
		if c.ui.pretty {
			c.prettyProfile(v)
			return nil
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
		if c.ui.pretty {
			c.prettyProfiles(vs)
			return nil
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
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%t\n", r.EndedAt.Format(time.RFC3339), stateReason(r.State, r.Reason), secs(r.DurationSeconds), units.FormatSize(r.MemoryPeak), units.FormatSize(r.SampleP90), r.Usable)
	}
	w.Flush()
}

// profileKey resolves a bare task name to <repo root>:<name> like aq submit
// --name does; a full key (containing ':') is used as given.
func (c *cmdCtx) profileKey(name string) string {
	if name == "" || strings.Contains(name, ":") {
		return name
	}
	cwd, err := os.Getwd()
	if err != nil {
		return name
	}
	return client.DefaultProfile(c.ctx, cwd, "", name)
}
