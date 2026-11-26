package cli

import (
	"fmt"

	"github.com/creatorofuniverses/agent-queue/internal/config"
)

// opencodeIdentityEnv is the variable of the OpenCode adapter, the one aq mcp
// takes the session of a call from when exactly one adapter has it.
const opencodeIdentityEnv = "OPENCODE_SESSION_ID"

// cmdInstallHints is what `just install` prints about an existing config. It
// has no entry in the help: the hints are the installer's, and the installer
// asks the binary it just built so that they see the config as the daemon and
// aq mcp will, in every form of TOML. A config aq cannot read is said on
// stderr and is no error here: the daemon refuses it, the installer goes on.
func cmdInstallHints(c *cmdCtx, args []string) error {
	fs := c.newFlagSet("install-hints")
	path := fs.String("config", "", "config file to read")
	if pos, err := parseFlags(fs, args); err != nil {
		return err
	} else if len(pos) > 0 {
		return usagef("install-hints: unexpected argument %q", pos[0])
	}
	if *path == "" {
		return usagef("install-hints: --config is required")
	}
	cfg, err := config.Load(*path)
	if err != nil {
		fmt.Fprintf(c.stderr, "warning: %v; the daemon will not start with this config\n", err)
		return nil
	}
	for _, h := range installHints(cfg, *path) {
		fmt.Fprint(c.stdout, h)
	}
	return nil
}

// installHints is what the config lacks for OpenClaude and OpenCode, one hint
// of two lines each. A process name counts only as a whole string, as aq
// matches process names. The OpenCode adapter is one whose identity_env is
// opencodeIdentityEnv, and only one: two give an OpenCode call no session, as
// none does. The installer never edits the config.
func installHints(cfg config.Config, path string) []string {
	var hints []string
	listed := map[string]bool{}
	for _, n := range cfg.Stats.AgentProcesses {
		listed[n] = true
	}
	for _, h := range []struct{ name, without string }{
		{"openclaude", "OpenClaude's sessions are not counted and its commands belong to cli or to the session of an agent it was started from"},
		{"opencode", "OpenCode's processes are not counted and its commands are identified by the environment alone: an inherited variable of another agent can file them under that agent's session"},
	} {
		if !listed[h.name] {
			hints = append(hints,
				fmt.Sprintf("hint: agent_processes in %s does not list %s.\n", path, h.name),
				fmt.Sprintf("      add it, or %s.\n", h.without))
		}
	}
	var opencode int
	for _, a := range cfg.Agents {
		if a.IdentityEnv == opencodeIdentityEnv {
			opencode++
		}
	}
	const unknown = `rejected with "session identity unknown".`
	switch opencode {
	case 0:
		hints = append(hints,
			fmt.Sprintf("hint: [[agents]] in %s has no adapter for OpenCode.\n", path),
			fmt.Sprintf("      add one with identity_env = %q and delivery = %q, or OpenCode's commands belong to cli and its MCP submissions are %s\n", opencodeIdentityEnv, config.DeliveryClientWait, unknown))
	case 1:
	default:
		hints = append(hints,
			fmt.Sprintf("hint: [[agents]] in %s has %d adapters with identity_env = %q.\n", path, opencode, opencodeIdentityEnv),
			fmt.Sprintf("      keep exactly one, or OpenCode's MCP submissions are %s\n", unknown))
	}
	return hints
}
