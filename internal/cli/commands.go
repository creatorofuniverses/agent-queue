package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"resource-dispatcher/internal/client"
	"resource-dispatcher/internal/config"
	"resource-dispatcher/internal/daemon"
	"resource-dispatcher/internal/hook"
	"resource-dispatcher/internal/mcp"
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
	return daemon.Run(ctx, paths, cfg, ex, linux.NewHost("/proc"), linux.NewDockerMeter(linux.DockerMeterConfig{}))
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
	if v.Profile != "" {
		fmt.Fprintf(w, "profile:   %s\n", v.Profile)
	}
	if v.EstimatedSeconds > 0 {
		fmt.Fprintf(w, "estimate:  %s\n", secs(v.EstimatedSeconds))
	}
	if v.MemoryFromProfile {
		fmt.Fprintln(w, "memory source: (from profile)")
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
	if v.Kind == model.KindReservation {
		fmt.Fprintf(w, "kind:      reservation\n")
		fmt.Fprintf(w, "stand:     %s memory, %d cpus\n", units.FormatSize(v.MemoryLimit), v.CPUs)
		fmt.Fprintf(w, "budget:    %s memory, %d cpus\n", units.FormatSize(v.TaskMemory), v.TaskCPUs)
		if v.ExpiresAt != nil {
			fmt.Fprintf(w, "expires:   %s\n", v.ExpiresAt.Format(time.RFC3339))
		}
		if v.TTLHeld {
			fmt.Fprintf(w, "ttl held:  yes\n")
		}
		if v.DockerProject != "" {
			fmt.Fprintf(w, "docker:    %s\n", v.DockerProject)
		}
		if v.StandMeasured != nil {
			fmt.Fprintf(w, "measured:  %s\n", units.FormatSize(*v.StandMeasured))
		}
		if v.TeardownAttempt > 0 {
			fmt.Fprintf(w, "teardown attempt: %d\n", v.TeardownAttempt)
		}
	} else if v.Within != "" {
		fmt.Fprintf(w, "within:    %s\n", v.Within)
	}
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
	mine := fs.Bool("mine", false, "only this session, including unacknowledged events")
	all := fs.Bool("all", false, "include finished items")
	asJSON := fs.Bool("json", false, "print JSON")
	if pos, err := parseFlags(fs, args); err != nil {
		return err
	} else if len(pos) > 0 {
		return usagef("list: unexpected argument %q", pos[0])
	}
	if *mine && *session == "" {
		id, err := client.DetectIdentity(c.clientConfig().Agents, c.env, "", "")
		if err != nil {
			return err
		}
		if id.SessionID == "" {
			return usagef("list: --mine needs a session (CODEX_THREAD_ID, CLAUDE_CODE_SESSION_ID or --session)")
		}
		*session = id.SessionID
	}
	var views []daemon.ItemView
	if err := c.call(c.ctx, "list", map[string]any{"session_id": *session, "all": *all}, &views); err != nil {
		return err
	}
	if *asJSON {
		return printJSON(c, views)
	}
	tw := tabwriter.NewWriter(c.stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tKIND\tSTATE\tPRIORITY\tMEMORY\tEXPIRES\tWAIT\tRUN\tBLOCKED_BY")
	for _, v := range views {
		state := string(v.State)
		if v.Reason != "" {
			state += "/" + string(v.Reason)
		}
		blocked := string(v.BlockedBy)
		if blocked != "" && v.BlockedDetail != "" {
			blocked += " (" + v.BlockedDetail + ")"
		}
		kind, name, expires := "task", v.Name, ""
		if v.Kind == model.KindReservation {
			kind = "res"
			if v.State.Held() && v.ExpiresAt != nil {
				expires = time.Until(*v.ExpiresAt).Round(time.Second).String()
			}
		}
		if v.Within != "" {
			name = strings.TrimSpace(name + " (in " + shortID(v.Within) + ")")
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", v.ID, name, kind, state, v.Priority,
			units.FormatSize(v.MemoryLimit), expires, secs(v.WaitSeconds), secs(v.RunSeconds), blocked)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if *session != "" {
		fmt.Fprintln(c.stdout)
		tw = tabwriter.NewWriter(c.stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "EVENT\tITEM\tKIND\tSTATUS\tAGE")
		for _, v := range views {
			for _, e := range v.Events {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", e.ID, v.ID, e.Kind, e.Status, time.Since(e.CreatedAt).Round(time.Second))
			}
		}
		return tw.Flush()
	}
	return nil
}

func cmdWait(c *cmdCtx, args []string) error {
	fs := c.newFlagSet("wait")
	until := fs.String("until", "", "wait condition: expiring (reservation only)")
	asJSON := fs.Bool("json", false, "print JSON")
	id, err := parseID(fs, args)
	if err != nil {
		return err
	}
	if *until != "" && *until != "expiring" {
		return usagef("wait: --until must be expiring")
	}
	ctx, stop := signal.NotifyContext(c.ctx, os.Interrupt)
	defer stop()
	var v daemon.ItemView
	err = c.call(ctx, "wait", map[string]string{"id": id, "until": *until}, &v)
	if ctx.Err() != nil {
		return &exitError{code: exitInterrupted, msg: "interrupted"}
	}
	if err != nil {
		return err
	}
	if *asJSON {
		if err := printJSON(c, v); err != nil {
			return err
		}
		return &exitError{code: ExitCodeFor(v)}
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
	for _, e := range v.Events {
		line += fmt.Sprintf(" event=%s:%s", e.ID, e.Kind)
	}
	fmt.Fprintln(c.stdout, line)
	switch {
	case *until == "expiring" && v.State == model.StateGranted:
		fmt.Fprintf(c.stderr, "hint: reservation still held; re-run in background: aq wait %s --until expiring\n", v.ID)
	case *until == "expiring" && v.State == model.StateExpiring:
		fmt.Fprintf(c.stderr, "hint: after aq extend, re-run in background: aq wait %s --until expiring\n", v.ID)
	}
	if code := ExitCodeFor(v); code != exitOK {
		return &exitError{code: code}
	}
	return nil
}

func shortID(id model.ItemID) string {
	if len(id) > 8 {
		return string(id[:8])
	}
	return string(id)
}

func cmdAck(c *cmdCtx, args []string) error {
	fs := c.newFlagSet("ack")
	asJSON := fs.Bool("json", false, "print JSON")
	id, err := parseID(fs, args)
	if err != nil {
		return err
	}
	var e daemon.EventView
	if err = c.call(c.ctx, "ack", map[string]string{"event_id": id}, &e); err != nil {
		return err
	}
	if *asJSON {
		return printJSON(c, e)
	}
	fmt.Fprintf(c.stdout, "%s %s\n", e.ID, e.Status)
	return nil
}

func cmdMCP(c *cmdCtx, args []string) error {
	fs := c.newFlagSet("mcp")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usagef("mcp: unexpected argument %q", pos[0])
	}
	p, err := c.paths()
	if err != nil {
		return err
	}
	cfg := c.clientConfig()
	ctx, cancel := signal.NotifyContext(c.ctx, syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	return mcp.Serve(ctx, os.Stdin, c.stdout, func(ctx context.Context, op string, args, out any) error { return callOn(ctx, p, op, args, out) }, cfg.Agents, c.env, daemon.Version)
}

func cmdHook(c *cmdCtx, args []string) error {
	if len(args) != 1 || args[0] != "pretooluse" {
		return usagef("hook: expected pretooluse")
	}
	p, err := c.paths()
	cfg := config.Default()
	if err == nil {
		if loaded, e := config.Load(p.ConfigFile); e == nil {
			cfg = loaded
		}
	}
	patterns, e := hook.Compile(cfg.Hook.Patterns)
	if e != nil {
		patterns, _ = hook.Compile(config.Default().Hook.Patterns)
	}
	id, _ := client.DetectIdentity(cfg.Agents, c.env, "", "")
	hook.PreToolUse(os.Stdin, c.stdout, patterns, func(idx int) {
		if err == nil {
			_ = hook.AppendHit(p.StateDir, time.Now(), idx, id.ClientType)
		}
	})
	return nil
}
