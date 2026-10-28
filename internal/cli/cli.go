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

	"resource-dispatcher/internal/client"
	"resource-dispatcher/internal/config"
	"resource-dispatcher/internal/model"
	"resource-dispatcher/internal/protocol"
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
	}
}

const usageText = `usage: aq <command> [flags]

commands: stats top estimate profile daemon submit status result logs cancel list wait reserve extend release teardown doctor ack hook mcp version
`

// Main runs the aq CLI and returns the process exit code.
func Main(args []string, env func(string) string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usageText)
		return exitUsage
	}
	cmd, ok := commands[args[0]]
	if !ok {
		fmt.Fprintf(stderr, "aq: unknown command %q\n%s", args[0], usageText)
		return exitUsage
	}
	c := &cmdCtx{ctx: context.Background(), env: env, stdout: stdout, stderr: stderr}
	return exitCode(cmd(c, args[1:]), stderr)
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
	fs.SetOutput(c.stderr)
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
