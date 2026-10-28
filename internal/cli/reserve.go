package cli

import (
	"fmt"
	"os"
	"time"

	"resource-dispatcher/internal/client"
	"resource-dispatcher/internal/config"
	"resource-dispatcher/internal/daemon"
	"resource-dispatcher/internal/model"
)

func cmdReserve(c *cmdCtx, args []string) error {
	fs := c.newFlagSet("reserve")
	memory := fs.String("memory", "", "stand memory `SIZE`, e.g. 2G (required)")
	cpus := fs.Int("cpus", 0, "`N` stand CPUs")
	taskMemory := fs.String("task-memory", "", "memory `SIZE` of the budget for nested tasks (with --task-cpus)")
	taskCPUs := fs.Int("task-cpus", 0, "`N` CPUs of the budget for nested tasks (with --task-memory)")
	ttl := fs.String("ttl", "", "time to live `DURATION`, e.g. 2h (required)")
	teardown := fs.String("teardown", "", "teardown script `FILE` that stops the stand (required)")
	dockerProject := fs.String("docker-project", "", "docker compose `PROJECT` of the stand (measures its memory)")
	priority := fs.String("priority", "normal", "`P`riority: low, normal or high")
	name := fs.String("name", "", "reservation `NAME`")
	key := fs.String("key", "", "idempotency `KEY`")
	flagClient := fs.String("client", "", "agent adapter `NAME` (claude, codex) or cli")
	noNotify := fs.Bool("no-notify", false, "disable session notifications")
	session := fs.String("session", "", "agent session `ID` to notify (default: detected)")
	project := fs.String("project", "", "project `DIR` (default: git root of cwd)")
	asJSON := fs.Bool("json", false, "print the item as JSON")
	var envs stringList
	fs.Var(&envs, "env", "extra environment K=V (repeatable)")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usagef("reserve: unexpected argument %q", pos[0])
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
	request, err := client.BuildReserve(c.ctx, client.ReserveOptions{Cwd: cwd, Name: *name, Memory: *memory, CPUs: *cpus, TaskMemory: *taskMemory, TaskCPUs: *taskCPUs, TTL: *ttl, TeardownFile: *teardown, DockerProject: *dockerProject, Priority: *priority, Key: *key, Project: *project, Env: envs, NoNotify: *noNotify, Identity: identity}, c.env)
	if err != nil {
		return err
	}
	var v daemon.ItemView
	err = c.call(c.ctx, "reserve", request, &v)
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(c, v)
	}
	line := fmt.Sprintf("%s %s", v.ID, v.State)
	if v.State == model.StateQueued {
		line += fmt.Sprintf(" position=%d blocked_by=%s", v.Position, v.BlockedBy)
	}
	fmt.Fprintln(c.stdout, line)
	waitHint(c, cfg, identity, v.ID)
	return nil
}

func cmdExtend(c *cmdCtx, args []string) error {
	fs := c.newFlagSet("extend")
	ttl := fs.String("ttl", "", "new time to live `DURATION` from now, e.g. 1h (required)")
	asJSON := fs.Bool("json", false, "print the item as JSON")
	id, err := c.parseItemID(fs, args)
	if err != nil {
		return err
	}
	d, err := time.ParseDuration(*ttl)
	if *ttl == "" || err != nil || d < time.Second {
		return usagef("extend: --ttl is required and must be a duration of at least 1s")
	}
	if err := itemOp(c, "extend", map[string]any{"id": id, "ttl_seconds": int64(d / time.Second)}, *asJSON); err != nil {
		return err
	}
	// The next expiry warning only reaches a client-wait session through a fresh waiter.
	cfg := c.clientConfig()
	if who, err := client.DetectIdentity(cfg.Agents, c.env, "", ""); err == nil {
		if a, ok := cfg.Agent(who.ClientType); ok && a.Delivery == config.DeliveryClientWait {
			fmt.Fprintf(c.stderr, "hint: to get the next expiry warning, run in background: aq wait %s --until expiring\n", id)
		}
	}
	return nil
}

func cmdRelease(c *cmdCtx, args []string) error {
	fs := c.newFlagSet("release")
	force := fs.Bool("force", false, "also release a failed or unknown teardown")
	asJSON := fs.Bool("json", false, "print the item as JSON")
	id, err := c.parseItemID(fs, args)
	if err != nil {
		return err
	}
	return itemOp(c, "release", map[string]any{"id": id, "force": *force}, *asJSON)
}

func cmdTeardown(c *cmdCtx, args []string) error {
	fs := c.newFlagSet("teardown")
	retry := fs.Bool("retry", false, "run the teardown script again (required)")
	asJSON := fs.Bool("json", false, "print the item as JSON")
	id, err := c.parseItemID(fs, args)
	if err != nil {
		return err
	}
	if !*retry {
		return usagef("teardown: --retry is required")
	}
	return itemOp(c, "teardown_retry", map[string]string{"id": id}, *asJSON)
}

// itemOp calls op and prints the resulting item.
func itemOp(c *cmdCtx, op string, args any, asJSON bool) error {
	var v daemon.ItemView
	if err := c.call(c.ctx, op, args, &v); err != nil {
		return err
	}
	if asJSON {
		return printJSON(c, v)
	}
	fmt.Fprintf(c.stdout, "%s %s\n", v.ID, v.State)
	return nil
}
