// Package cli implements the aq command line.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"agent-queue/internal/client"
	"agent-queue/internal/config"
	"agent-queue/internal/daemon"
	"agent-queue/internal/model"
	"agent-queue/internal/protocol"
)

// usageError is a command line mistake; it exits with 64.
type usageError = client.UsageError

func usagef(format string, a ...any) error { return &usageError{Message: fmt.Sprintf(format, a...)} }

// exitError carries a specific exit code with an optional message.
type exitError struct {
	code int
	msg  string
}

func (e *exitError) Error() string { return e.msg }

// cmdCtx is what every command receives.
type cmdCtx struct {
	ctx    context.Context
	env    func(string) string
	stdout io.Writer
	stderr io.Writer
	ui     ui
	// helpAsked: the user asked for help, so usage goes to stdout.
	helpAsked bool
}

type command func(c *cmdCtx, args []string) error

var commands map[string]command

func init() {
	commands = map[string]command{
		"stats":    cmdStats,
		"profile":  cmdProfile,
		"estimate": cmdEstimate,
		"top":      cmdTop,
		"daemon":   cmdDaemon,
		"submit":   cmdSubmit,
		"status":   cmdStatus,
		"result":   cmdResult,
		"logs":     cmdLogs,
		"cancel":   cmdCancel,
		"list":     cmdList,
		"wait":     cmdWait,
		"ack":      cmdAck,
		"mcp":      cmdMCP,
		"hook":     cmdHook,
		"reserve":  cmdReserve,
		"extend":   cmdExtend,
		"release":  cmdRelease,
		"teardown": cmdTeardown,
		"doctor":   cmdDoctor,
		"version":  cmdVersion,
		"help":     cmdHelpCommand,
	}
}

func isHelpArg(a string) bool { return a == "-h" || a == "--help" || a == "-help" }

// Main runs the aq CLI and returns the process exit code.
func Main(args []string, env func(string) string, stdout, stderr io.Writer) int {
	c := &cmdCtx{ctx: context.Background(), env: env, stdout: stdout, stderr: stderr, ui: detectUI(stdout, env)}
	if len(args) == 0 {
		printOverview(c.ui, stderr)
		return exitUsage
	}
	if isHelpArg(args[0]) {
		printOverview(c.ui, stdout)
		return exitOK
	}
	cmd, ok := commands[args[0]]
	if !ok {
		fmt.Fprintf(stderr, "aq: unknown command %q; run 'aq help' for the list\n", args[0])
		return exitUsage
	}
	rest := args[1:]
	if args[0] == "help" && len(rest) > 0 {
		args, rest = rest, []string{"--help"}
		if cmd, ok = commands[args[0]]; !ok {
			fmt.Fprintf(stderr, "aq: help: unknown command %q\n", args[0])
			return exitUsage
		}
	}
	if len(rest) > 0 && isHelpArg(rest[0]) {
		c.helpAsked = true
		switch args[0] {
		case "version", "hook":
			// No FlagSet of their own.
			commandUsage(c.ui, stdout, args[0], flag.NewFlagSet(args[0], flag.ContinueOnError))
			return exitOK
		}
	}
	return exitCode(cmd(c, rest), stderr)
}

func exitCode(err error, stderr io.Writer) int {
	var (
		ue *usageError
		ee *exitError
		pe *protocol.Error
	)
	switch {
	case err == nil:
		return exitOK
	case errors.Is(err, flag.ErrHelp):
		return exitOK
	case errors.As(err, &ee):
		if ee.msg != "" {
			fmt.Fprintln(stderr, "aq:", ee.msg)
		}
		return ee.code
	case errors.As(err, &ue):
		fmt.Fprintln(stderr, "aq:", ue.Error())
		return exitUsage
	case errors.Is(err, protocol.ErrDaemonUnavailable):
		fmt.Fprintln(stderr, "aq: daemon is not running (start it with `aq daemon`)")
		return exitUnavailable
	case errors.As(err, &pe):
		fmt.Fprintln(stderr, "aq:", pe.Message)
		switch pe.Code {
		case protocol.CodeBadRequest:
			return exitUsage
		case protocol.CodeRejected:
			return exitInfra
		}
		return exitFailed
	}
	fmt.Fprintln(stderr, "aq:", err)
	return exitFailed
}

// newFlagSet makes a FlagSet that reports errors through the returned error.
func (c *cmdCtx) newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet("aq "+name, flag.ContinueOnError)
	// The flag package's own messages are dropped: exitCode prints the error once.
	fs.SetOutput(io.Discard)
	fs.Usage = func() {
		if c.helpAsked {
			commandUsage(c.ui, c.stdout, name, fs)
		} else {
			fmt.Fprintf(c.stderr, "run 'aq help %s' for usage\n", name)
		}
	}
	return fs
}

// parseFlags parses args, allowing flags before and after positionals.
func parseFlags(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, err
			}
			return nil, &usageError{Message: err.Error()}
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return pos, nil
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}

// parseID parses args that must hold exactly one positional ID.
func parseID(fs *flag.FlagSet, args []string) (string, error) {
	pos, err := parseFlags(fs, args)
	if err != nil {
		return "", err
	}
	if len(pos) != 1 {
		return "", usagef("%s: expected exactly one ID", fs.Name())
	}
	return pos[0], nil
}

// parseItemID is parseID for a task or reservation ID; a unique prefix of at
// least 4 characters (as aq list shows) stands for the full ID.
func (c *cmdCtx) parseItemID(fs *flag.FlagSet, args []string) (string, error) {
	id, err := parseID(fs, args)
	if err != nil || len(id) >= 36 || len(id) < 4 {
		return id, err
	}
	var views []daemon.ItemView
	if err := c.call(c.ctx, "list", map[string]any{"all": true}, &views); err != nil {
		return id, nil // the real request reports the daemon error
	}
	var match []string
	for _, v := range views {
		if strings.HasPrefix(string(v.ID), id) {
			match = append(match, string(v.ID))
		}
	}
	switch len(match) {
	case 0:
		return id, nil
	case 1:
		return match[0], nil
	}
	return "", usagef("ID prefix %s matches %d items; give more characters", id, len(match))
}

// paths resolves directories from the injected env (AQ_STATE_DIR, AQ_RUNTIME_DIR, AQ_CONFIG).
func (c *cmdCtx) paths() (config.Paths, error) {
	p := config.Paths{StateDir: c.env("AQ_STATE_DIR"), RuntimeDir: c.env("AQ_RUNTIME_DIR"), ConfigFile: c.env("AQ_CONFIG"),
		SliceLockDir: config.SliceLockDirFor(c.env("XDG_RUNTIME_DIR"))}
	if p.StateDir == "" || p.ConfigFile == "" {
		home := c.env("HOME")
		if home == "" {
			return p, errors.New("HOME is not set; set AQ_STATE_DIR and AQ_CONFIG")
		}
		if p.StateDir == "" {
			p.StateDir = filepath.Join(home, ".local", "state", "agent-queue")
		}
		if p.ConfigFile == "" {
			p.ConfigFile = filepath.Join(home, ".config", "agent-queue", "config.toml")
		}
	}
	if p.RuntimeDir == "" {
		xdg := c.env("XDG_RUNTIME_DIR")
		if xdg == "" {
			return p, errors.New("XDG_RUNTIME_DIR is not set; set AQ_RUNTIME_DIR")
		}
		p.RuntimeDir = filepath.Join(xdg, "agent-queue")
	}
	return p, nil
}

// call performs one daemon request against the resolved socket.
func (c *cmdCtx) call(ctx context.Context, op string, args, out any) error {
	p, err := c.paths()
	if err != nil {
		return err
	}
	return callOn(ctx, p, op, args, out)
}

func callOn(ctx context.Context, p config.Paths, op string, args, out any) error {
	return protocol.Call(ctx, p.Socket(), op, args, out)
}

func callStreamOn(ctx context.Context, p config.Paths, op string, args any, onStream func(json.RawMessage) error) error {
	return protocol.CallStream(ctx, p.Socket(), op, args, onStream, nil)
}

// clientConfig warns once and keeps CLI usable with defaults on config errors.
func (c *cmdCtx) clientConfig() config.Config {
	p, err := c.paths()
	if err != nil {
		fmt.Fprintf(c.stderr, "aq: config: %v; using defaults\n", err)
		return config.Default()
	}
	cfg, err := config.Load(p.ConfigFile)
	if err != nil {
		fmt.Fprintf(c.stderr, "aq: %v; using defaults\n", err)
		return config.Default()
	}
	return cfg
}
func waitHint(c *cmdCtx, cfg config.Config, id client.Identity, item model.ItemID) {
	if a, ok := cfg.Agent(id.ClientType); ok && a.Delivery == config.DeliveryClientWait {
		fmt.Fprintf(c.stderr, "hint: to be woken, run in background: aq wait %s\n", item)
	}
}
