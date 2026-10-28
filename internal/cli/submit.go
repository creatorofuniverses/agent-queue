package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"resource-dispatcher/internal/client"
	"resource-dispatcher/internal/daemon"
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
	memory := fs.String("memory", "", "memory limit, e.g. 4G (required)")
	expected := fs.String("expected", "", "expected memory use (default: --memory)")
	cpus := fs.Int("cpus", 1, "CPUs")
	priority := fs.String("priority", "normal", "low|normal|high")
	timeout := fs.String("timeout", "", "task timeout, e.g. 30m (required)")
	profile := fs.String("profile", "", "explicit profile name")
	name := fs.String("name", "", "task name")
	key := fs.String("key", "", "idempotency key")
	flagClient := fs.String("client", "", "agent adapter name or cli")
	noNotify := fs.Bool("no-notify", false, "disable session notifications")
	session := fs.String("session", "", "session id")
	project := fs.String("project", "", "project (default: git root of cwd)")
	within := fs.String("within", "", "run inside this reservation (task budget)")
	asJSON := fs.Bool("json", false, "print the item as JSON")
	var envs stringList
	fs.Var(&envs, "env", "extra environment K=V (repeatable)")
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
