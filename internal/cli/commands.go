package cli

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"resource-dispatcher/internal/config"
	"resource-dispatcher/internal/daemon"
	"resource-dispatcher/internal/model"
	"resource-dispatcher/internal/platform/linux"
	"resource-dispatcher/internal/units"
)

func cmdVersion(c *cmdCtx, args []string) error {
	fmt.Fprintln(c.stdout, "aq", daemon.Version)
	return nil
}

func cmdDaemon(c *cmdCtx, args []string) error {
	fs := c.newFlagSet("daemon")
	if pos, err := parseFlags(fs, args); err != nil {
		return err
	} else if len(pos) > 0 {
		return usagef("daemon: unexpected argument %q", pos[0])
	}
	paths, err := c.paths()
	if err != nil {
		return err
	}
	cfg, err := config.Load(paths.ConfigFile)
	if err != nil {
		return err
	}
	bin, err := os.Executable()
	if err != nil {
		return err
	}
	ex, err := linux.NewExecutor(linux.ExecutorConfig{SliceName: cfg.SliceName, TeardownSlice: cfg.TeardownSliceName(),
		UnitPrefix: cfg.UnitPrefix, AqBinary: bin})
	if err != nil {
		return err
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(c.stderr, nil)))
	ctx, stop := signal.NotifyContext(c.ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return daemon.Run(ctx, paths, cfg, ex, linux.NewHost("/proc"))
}

func cmdStatus(c *cmdCtx, args []string) error {
	fs := c.newFlagSet("status")
	asJSON := fs.Bool("json", false, "print JSON")
	id, err := parseID(fs, args)
	if err != nil {
		return err
	}
	var v daemon.ItemView
	if err := c.call(c.ctx, "status", map[string]string{"id": id}, &v); err != nil {
		return err
	}
	if *asJSON {
		return printJSON(c, v)
	}
	printView(c, v)
	return nil
}

func cmdResult(c *cmdCtx, args []string) error {
	fs := c.newFlagSet("result")
	asJSON := fs.Bool("json", false, "print JSON")
	id, err := parseID(fs, args)
	if err != nil {
		return err
	}
	var v daemon.ResultView
	if err := c.call(c.ctx, "result", map[string]string{"id": id}, &v); err != nil {
		return err
	}
	if *asJSON {
		return printJSON(c, v)
	}
	printView(c, v.ItemView)
	fmt.Fprintf(c.stdout, "stdout:    %s\nstderr:    %s\n", v.StdoutPath, v.StderrPath)
	if v.LogsTruncated {
		fmt.Fprintln(c.stdout, "logs:      truncated")
	}
	return nil
}

func printView(c *cmdCtx, v daemon.ItemView) {
	w := c.stdout
	fmt.Fprintf(w, "id:        %s\n", v.ID)
	if v.Name != "" {
		fmt.Fprintf(w, "name:      %s\n", v.Name)
	}
	state := string(v.State)
	if v.Reason != "" {
		state += " (" + string(v.Reason) + ")"
	}
	fmt.Fprintf(w, "state:     %s\n", state)
	fmt.Fprintf(w, "priority:  %s\n", v.Priority)
	fmt.Fprintf(w, "memory:    %s limit, %s expected, %s peak\n",
		units.FormatSize(v.MemoryLimit), units.FormatSize(v.MemoryExpected), units.FormatSize(v.MemoryPeak))
	fmt.Fprintf(w, "cpus:      %d\n", v.CPUs)
	fmt.Fprintf(w, "timeout:   %s\n", time.Duration(v.TimeoutSeconds)*time.Second)
	fmt.Fprintf(w, "waited:    %s\n", secs(v.WaitSeconds))
	fmt.Fprintf(w, "ran:       %s\n", secs(v.RunSeconds))
	if v.ExitCode != nil {
		fmt.Fprintf(w, "exit code: %d\n", *v.ExitCode)
	}
	if v.Signal != "" {
		fmt.Fprintf(w, "signal:    %s\n", v.Signal)
	}
	if v.Project != "" {
		fmt.Fprintf(w, "project:   %s\n", v.Project)
	}
	if v.Diagnostic != "" {
		fmt.Fprintf(w, "diagnostic: %s\n", v.Diagnostic)
	}
}

func secs(s float64) string {
	return (time.Duration(s * float64(time.Second))).Round(100 * time.Millisecond).String()
}

func cmdLogs(c *cmdCtx, args []string) error {
	fs := c.newFlagSet("logs")
	stream := fs.String("stream", "stdout", "stdout|stderr")
	tail := fs.Int("tail", 100, "number of trailing lines (0 = all)")
	follow := fs.Bool("follow", false, "stream new lines until the task ends")
	id, err := parseID(fs, args)
	if err != nil {
		return err
	}
	if *stream != "stdout" && *stream != "stderr" {
		return usagef("logs: --stream must be stdout or stderr")
	}
	if *tail < 0 {
		return usagef("logs: --tail must not be negative")
	}
	p, err := c.paths()
	if err != nil {
		return err
	}
	a := daemon.LogsArgs{ID: model.ItemID(id), Stream: *stream, Tail: *tail, Follow: *follow}
	print := func(l daemon.LogLines) {
		for _, line := range l.Lines {
			fmt.Fprintln(c.stdout, line)
		}
	}
	if !*follow {
		var out daemon.LogLines
		if err := callOn(c.ctx, p, "logs", a, &out); err != nil {
			return err
		}
		print(out)
		return nil
	}
	ctx, stop := signal.NotifyContext(c.ctx, os.Interrupt)
	defer stop()
	err = callStreamOn(ctx, p, "logs", a, func(raw json.RawMessage) error {
		var l daemon.LogLines
		if err := json.Unmarshal(raw, &l); err != nil {
			return err
		}
		print(l)
		return nil
	})
	if ctx.Err() != nil {
		return &exitError{code: exitInterrupted}
	}
	return err
}

func cmdCancel(c *cmdCtx, args []string) error {
	fs := c.newFlagSet("cancel")
	reason := fs.String("reason", "", "reason")
	id, err := parseID(fs, args)
	if err != nil {
		return err
	}
	var v daemon.ItemView
	if err := c.call(c.ctx, "cancel", map[string]string{"id": id, "reason": *reason}, &v); err != nil {
		return err
	}
	fmt.Fprintf(c.stdout, "%s %s\n", v.ID, v.State)
	return nil
}

func cmdList(c *cmdCtx, args []string) error {
	fs := c.newFlagSet("list")
	session := fs.String("session", "", "only this session")
	all := fs.Bool("all", false, "include finished items")
	asJSON := fs.Bool("json", false, "print JSON")
	if pos, err := parseFlags(fs, args); err != nil {
		return err
	} else if len(pos) > 0 {
		return usagef("list: unexpected argument %q", pos[0])
	}
	var views []daemon.ItemView
	if err := c.call(c.ctx, "list", map[string]any{"session_id": *session, "all": *all}, &views); err != nil {
		return err
	}
	if *asJSON {
		return printJSON(c, views)
	}
	tw := tabwriter.NewWriter(c.stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tSTATE\tPRIORITY\tMEMORY\tWAIT\tRUN\tBLOCKED_BY")
	for _, v := range views {
		state := string(v.State)
		if v.Reason != "" {
			state += "/" + string(v.Reason)
		}
		blocked := string(v.BlockedBy)
		if blocked != "" && v.BlockedDetail != "" {
			blocked += " (" + v.BlockedDetail + ")"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", v.ID, v.Name, state, v.Priority,
			units.FormatSize(v.MemoryLimit), secs(v.WaitSeconds), secs(v.RunSeconds), blocked)
	}
	return tw.Flush()
}

func cmdWait(c *cmdCtx, args []string) error {
	fs := c.newFlagSet("wait")
	id, err := parseID(fs, args)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(c.ctx, os.Interrupt)
	defer stop()
	var v daemon.ItemView
	err = c.call(ctx, "wait", map[string]string{"id": id}, &v)
	if ctx.Err() != nil {
		return &exitError{code: exitInterrupted, msg: "interrupted"}
	}
	if err != nil {
		return err
	}
	line := fmt.Sprintf("%s %s", v.ID, v.State)
	if v.Reason != "" {
		line += " " + string(v.Reason)
	}
	if v.ExitCode != nil {
		line += fmt.Sprintf(" exit=%d", *v.ExitCode)
	}
	line += fmt.Sprintf(" waited=%s ran=%s", secs(v.WaitSeconds), secs(v.RunSeconds))
	if v.Diagnostic != "" {
		line += " " + strings.ReplaceAll(v.Diagnostic, "\n", " ")
	}
	fmt.Fprintln(c.stdout, line)
	if code := ExitCodeFor(v); code != exitOK {
		return &exitError{code: code}
	}
	return nil
}
