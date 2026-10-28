package cli

import (
	"flag"
	"fmt"
	"io"
	"strings"
)

// cmdHelp is the help text of one command; flags come from its FlagSet.
type cmdHelp struct {
	usage, summary, details string
	examples                []string
}

var helpGroups = []struct {
	title string
	cmds  []string
}{
	{"Tasks", []string{"submit", "wait", "status", "result", "logs", "cancel", "list"}},
	{"Reservations (docker stands and other long-lived memory)", []string{"reserve", "extend", "release", "teardown"}},
	{"Planning and insight", []string{"top", "estimate", "profile", "stats"}},
	{"Setup and integration", []string{"doctor", "daemon", "ack", "hook", "mcp", "version", "help"}},
}

var helps = map[string]cmdHelp{
	"submit": {"aq submit [flags] -- COMMAND [ARGS...]", "queue a command with a memory and CPU budget",
		"The command waits in the queue until its memory fits, then runs in its own systemd unit with that\nlimit. Prints the task ID. With --name the memory can come from the history of earlier runs\n(the profile <repo root>:<name>); otherwise --memory is required. Name the workload, not the run:\nkeep the name the same across releases (no version, date or commit in it) so the history adds up.",
		[]string{"aq submit --memory 4G --timeout 30m --name tests -- pytest -n 8", "aq submit --name tests --timeout 30m -- pytest -n 8   # memory from history", "aq submit --within RES_ID --memory 2G --timeout 10m -- make e2e"}},
	"wait": {"aq wait [--until expiring] ID", "block until a task ends or a reservation is granted",
		"Prints one line with the outcome. Exit codes: 0 success, granted or expiry warning; 1 the command\nfailed; 2 OOM; 3 timeout; 4 cancelled; 5 infrastructure or rejected; 6 reservation no longer\nheld; 64 usage; 69 daemon not running; 130 interrupted.\nFor a reservation, --until expiring returns when its TTL is about to run out.",
		[]string{"aq wait 5a0e71f2-6f3d-45a0-b6a5-291c7af58dca", "aq wait RES_ID --until expiring"}},
	"status": {"aq status [--json] ID", "show one task or reservation", "", []string{"aq status ID"}},
	"result": {"aq result [--json] ID", "show a finished item with its exit code and log paths", "Includes memory peak and, when the io controller is delegated, the bytes the task wrote\nto and read from physical disks (disk: line, io_write_bytes/io_read_bytes in --json).", []string{"aq result ID"}},
	"logs": {"aq logs [--stream stdout|stderr] [--tail N] [--follow] ID", "print a task's output", "",
		[]string{"aq logs ID --tail 50", "aq logs ID --stream stderr --follow"}},
	"cancel": {"aq cancel [--reason TEXT] ID", "cancel a queued or running task", "", []string{"aq cancel ID --reason 'wrong branch'"}},
	"list": {"aq list [--all] [--mine | --session ID] [--json]", "list the queue and running items",
		"Without --all only items that are not finished yet. --mine limits to the current agent session and\nalso shows its unacknowledged events.",
		[]string{"aq list", "aq list --all", "aq list --mine"}},
	"reserve": {"aq reserve --memory SIZE --ttl DURATION --teardown FILE [flags]", "hold memory for a stand (docker compose) and an optional task budget",
		"The reservation is granted when its memory fits and is held for --ttl. Nested tasks run inside it\nwith aq submit --within. On expiry or release the teardown script stops the stand.",
		[]string{"aq reserve --memory 6G --ttl 2h --docker-project mystand --teardown ./stand-down.sh", "aq reserve --memory 4G --task-memory 4G --task-cpus 4 --ttl 1h --teardown ./down.sh"}},
	"extend":   {"aq extend --ttl DURATION ID", "extend a reservation's TTL", "", []string{"aq extend RES_ID --ttl 1h"}},
	"release":  {"aq release [--force] ID", "free a reservation whose stand you have shut down (no teardown script runs)", "", []string{"aq release RES_ID"}},
	"teardown": {"aq teardown --retry ID", "run a failed teardown again", "", []string{"aq teardown --retry RES_ID"}},
	"top":      {"aq top [--json]", "host memory, what uses it, and what aq has committed", "", []string{"aq top"}},
	"estimate": {"aq estimate [flags]", "forecast when a task would start, without submitting it", "",
		[]string{"aq estimate --memory 8G --timeout 1h", "aq estimate --name tests --timeout 30m"}},
	"profile": {"aq profile list | aq profile show NAME", "memory and duration history of named tasks",
		"NAME is a task name from this repository (resolved like aq submit --name) or a full profile key.",
		[]string{"aq profile list", "aq profile show tests"}},
	"stats": {"aq stats [--since 7d] [--profile P] [--json] | aq stats export --format jsonl|csv [--table T]", "usage history: waits, outcomes, profiles, disk writes, host memory, incidents", "Disk writes lists the profiles whose tasks wrote the most to physical disks (total, median\nand max per run); a suite that writes gigabytes per run is worth fixing.",
		[]string{"aq stats", "aq stats --since 1d", "aq stats export --format csv --table items > items.csv"}},
	"doctor":  {"aq doctor [--json]", "check that the host, systemd and the daemon are set up", "A warn line is not a failure: IO_NOT_DELEGATED means tasks run but record no disk bytes.", []string{"aq doctor"}},
	"daemon":  {"aq daemon", "run the scheduler daemon (normally started by systemd)", "", nil},
	"ack":     {"aq ack [--json] EVENT_ID", "acknowledge a session event", "", nil},
	"hook":    {"aq hook pretooluse", "agent PreToolUse hook: hints to queue heavy commands", "", nil},
	"mcp":     {"aq mcp", "serve the aq tools to an agent over MCP stdio", "", nil},
	"version": {"aq version", "print the version", "", nil},
	"help":    {"aq help [COMMAND]", "show help", "", nil},
}

// cmdHelpCommand is bare "aq help"; "aq help CMD" is routed by Main to CMD --help.
func cmdHelpCommand(c *cmdCtx, args []string) error {
	printOverview(c.ui, c.stdout)
	return nil
}

func printOverview(u ui, w io.Writer) {
	fmt.Fprintf(w, "%s — local resource dispatcher: heavy commands wait for memory instead of hanging the host\n\n", u.bold("aq"))
	fmt.Fprintf(w, "%s aq <command> [flags]\n", u.bold("Usage:"))
	for _, g := range helpGroups {
		fmt.Fprintf(w, "\n%s\n", u.bold(g.title))
		for _, name := range g.cmds {
			fmt.Fprintf(w, "  %s %s\n", u.cyan(pad(name, 10)), helps[name].summary)
		}
	}
	fmt.Fprintf(w, "\nRun %s for a command's flags and examples. Add --json for machine output.\n", u.bold("aq help <command>"))
}

// commandUsage is the FlagSet.Usage of command name.
func commandUsage(u ui, w io.Writer, name string, fs *flag.FlagSet) {
	h, ok := helps[name]
	if !ok {
		h = cmdHelp{usage: "aq " + name + " [flags]"}
	}
	fmt.Fprintf(w, "%s %s\n", u.bold("Usage:"), h.usage)
	if h.summary != "" {
		fmt.Fprintf(w, "\n%s%s\n", strings.ToUpper(h.summary[:1]), h.summary[1:]+".")
	}
	if h.details != "" {
		fmt.Fprintf(w, "\n%s\n", h.details)
	}
	var flags []*flag.Flag
	fs.VisitAll(func(f *flag.Flag) { flags = append(flags, f) })
	if len(flags) > 0 {
		fmt.Fprintf(w, "\n%s\n", u.bold("Flags:"))
		for _, f := range flags {
			name, usage := flag.UnquoteUsage(f)
			left := "--" + f.Name
			if name != "" {
				left += " " + name
			}
			def := ""
			if f.DefValue != "" && f.DefValue != "false" && f.DefValue != "0" {
				def = u.dim(" (default " + f.DefValue + ")")
			}
			fmt.Fprintf(w, "  %s %s%s\n", u.cyan(pad(left, 24)), usage, def)
		}
	}
	if len(h.examples) > 0 {
		fmt.Fprintf(w, "\n%s\n", u.bold("Examples:"))
		for _, e := range h.examples {
			fmt.Fprintf(w, "  %s\n", e)
		}
	}
}
