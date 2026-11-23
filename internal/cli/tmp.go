package cli

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/creatorofuniverses/agent-queue/internal/platform/linux"
	"github.com/creatorofuniverses/agent-queue/internal/protocol"
	"github.com/creatorofuniverses/agent-queue/internal/tmpscan"
)

func cmdTmp(c *cmdCtx, args []string) error {
	fs := c.newFlagSet("tmp")
	asJSON := fs.Bool("json", false, "show the scan report as JSON")
	rescan := fs.Bool("rescan", false, "request a scan if the last report is at least one minute old")
	all := fs.Bool("all", false, "show every entry in the inventory")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usagef("tmp: unexpected arguments")
	}
	request := struct {
		Rescan bool `json:"rescan"`
	}{*rescan}
	var raw json.RawMessage
	if err := c.call(c.ctx, "tmp", request, &raw); err != nil {
		var pe *protocol.Error
		if errors.As(err, &pe) && pe.Code == protocol.CodeUnavailable && pe.Message == "no /tmp scan available yet" {
			return &exitError{code: exitUnavailable, msg: "no /tmp scan yet; try again in a minute or use --rescan"}
		}
		return err
	}
	if *asJSON {
		_, err := fmt.Fprintln(c.stdout, string(raw))
		return err
	}
	var report tmpscan.Report
	if err := json.Unmarshal(raw, &report); err != nil {
		return err
	}
	printTmp(c, report, *all)
	return nil
}

func printTmp(c *cmdCtx, report tmpscan.Report, all bool) {
	now := time.Now()
	occupancy := "filesystem usage unavailable"
	if fs, err := linux.NewHost("/proc").FSUsage(report.Root); err == nil {
		used, total := tmpSizes(fs.Size-min(fs.Free, fs.Size), fs.Size)
		_, unit, _ := strings.Cut(total, " ")
		occupancy = strings.TrimSuffix(used, " "+unit) + " of " + total
	}
	orphans, candidates := tmpSizes(report.OrphanBytes, report.CandidateBytes)
	fmt.Fprintf(c.stdout, "tmp: %s; orphans %s; candidates %s; scanned %s; partial items: %d",
		occupancy, orphans, candidates, tmpAge(now, report.At), report.PartialItems)
	if !report.Complete {
		fmt.Fprint(c.stdout, "; incomplete pass: tmp_orphans paused")
	}
	fmt.Fprintln(c.stdout)

	t := table{head: []string{"PATH", "SIZE", "LAST WRITE", "OWNER", "TYPE", "STATE"}, right: map[int]bool{1: true}}
	slices.SortStableFunc(report.Items, func(a, b tmpscan.Item) int {
		return cmp.Compare(b.Bytes, a.Bytes)
	})
	shown := len(report.Items)
	if !all {
		shown = min(shown, 30)
	}
	partial := false
	for _, item := range report.Items[:shown] {
		formattedSize := size(item.Bytes)
		if item.Partial {
			formattedSize += "*"
			partial = true
		}
		t.add(item.Path, formattedSize, tmpAge(now, item.LastWrite), tmpOwner(item.Owner), string(item.Kind), string(item.State))
	}
	t.write(c.ui, c.stdout)
	if partial {
		fmt.Fprintln(c.stdout, "* partial scan; size may be underestimated")
	}
	printTmpStateTotals(c, report.Items)
	if more := len(report.Items) - shown; more > 0 {
		fmt.Fprintf(c.stdout, "… and %d more (use --all)\n", more)
	}
}

func printTmpStateTotals(c *cmdCtx, items []tmpscan.Item) {
	type total struct {
		count int
		bytes uint64
	}
	byState := make(map[tmpscan.State]total)
	for _, item := range items {
		t := byState[item.State]
		t.count++
		t.bytes += item.Bytes
		byState[item.State] = t
	}
	var parts []string
	for _, state := range []tmpscan.State{"aq_task", "in_use", "live", "candidate", "orphan", "recent"} {
		t := byState[state]
		parts = append(parts, fmt.Sprintf("%s %d (%s)", state, t.count, size(t.bytes)))
	}
	fmt.Fprintln(c.stdout, "states: "+strings.Join(parts, ", "))
}

func tmpAge(now, at time.Time) string {
	if at.IsZero() {
		return "-"
	}
	return fmtDur(now.Sub(at).Round(time.Second)) + " ago"
}

func tmpOwner(owner tmpscan.Owner) string {
	switch owner.Kind {
	case "process":
		return fmt.Sprintf("pid %d", owner.PID)
	case "claude_project":
		if owner.Session == "" {
			return owner.Project
		}
		return owner.Project + "/" + owner.Session
	case "":
		return "unknown"
	default:
		return owner.Kind
	}
}

// Keep the same unit for related GiB totals, including sub-GiB amounts.
func tmpSizes(a, b uint64) (string, string) {
	const gibibyte = 1 << 30
	if max(a, b) >= gibibyte && min(a, b) < gibibyte && max(a, b) < 1<<40 {
		return fmt.Sprintf("%.1f GiB", float64(a)/gibibyte), fmt.Sprintf("%.1f GiB", float64(b)/gibibyte)
	}
	return size(a), size(b)
}
