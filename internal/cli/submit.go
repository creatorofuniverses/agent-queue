package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/creatorofuniverses/agent-queue/internal/client"
	"github.com/creatorofuniverses/agent-queue/internal/daemon"
)

// stringList is a repeatable string flag.
type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

func cmdSubmit(c *cmdCtx, args []string) error {
	// Everything after the first "--" is the task argv.
	var argv []string
	for i, a := range args {
		if a == "--" {
			args, argv = args[:i], args[i+1:]
			break
		}
	}
	fs := c.newFlagSet("submit")
	memory := fs.String("memory", "", "memory `SIZE` limit, e.g. 4G (required unless --name has run history)")
	expected := fs.String("expected", "", "expected memory `SIZE` use, for admission (default: the limit)")
	cpus := fs.Int("cpus", 1, "`N` CPUs")
	priority := fs.String("priority", "normal", "`P`riority: low, normal or high")
	timeout := fs.String("timeout", "", "task timeout `DURATION`, e.g. 30m (required)")
	profile := fs.String("profile", "", "explicit profile `KEY` instead of <repo root>:<name>")
	name := fs.String("name", "", "task `NAME`; also selects the memory/duration profile")
	key := fs.String("key", "", "idempotency `KEY`: resubmitting with it returns the same task")
	flagClient := fs.String("client", "", "agent adapter `NAME` (claude, codex) or cli")
	noNotify := fs.Bool("no-notify", false, "disable session notifications")
	session := fs.String("session", "", "agent session `ID` to notify (default: detected)")
	project := fs.String("project", "", "project `DIR` (default: git root of cwd)")
	within := fs.String("within", "", "run inside reservation `ID` (uses its task budget)")
	asJSON := fs.Bool("json", false, "print the item as JSON")
	var envs stringList
	fs.Var(&envs, "env", "extra environment `K=V` (repeatable)")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usagef("submit: unexpected argument %q before --", pos[0])
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	cfg := c.clientConfig()
	identity, err := client.DetectIdentity(cfg.Agents, c.env, *flagClient, *session)
	if err != nil {
		return err
	}
	request, err := client.BuildSubmit(c.ctx, client.SubmitOptions{Profile: *profile, Cwd: cwd, Name: *name, Argv: argv, Memory: *memory, Expected: *expected, CPUs: *cpus, Timeout: *timeout, Priority: *priority, Key: *key, Project: *project, Within: *within, Env: envs, NoNotify: *noNotify, Identity: identity}, c.env)
	if err != nil {
		return err
	}
	var v daemon.ItemView
	err = c.call(c.ctx, "submit", request, &v)
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(c, v)
	}
	fmt.Fprintln(c.stdout, v.ID)
	waitHint(c, cfg, identity, v.ID)
	return nil
}

func printJSON(c *cmdCtx, v any) error {
	enc := json.NewEncoder(c.stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
