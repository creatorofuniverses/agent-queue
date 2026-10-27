package cli

import (
	"fmt"
	"os"
	"time"
	"unicode/utf8"

	"resource-dispatcher/internal/daemon"
	"resource-dispatcher/internal/model"
	"resource-dispatcher/internal/units"
)

// maxTeardownScript is the largest teardown script the client sends.
const maxTeardownScript = 64 << 10

// reserveCapturedEnv is the submit env list plus the Docker client variables.
var reserveCapturedEnv = append(append([]string{}, capturedEnv...), "DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_CONFIG")

func cmdReserve(c *cmdCtx, args []string) error {
	fs := c.newFlagSet("reserve")
	memory := fs.String("memory", "", "stand memory, e.g. 2G (required)")
	cpus := fs.Int("cpus", 0, "stand CPUs")
	taskMemory := fs.String("task-memory", "", "task budget memory (with --task-cpus)")
	taskCPUs := fs.Int("task-cpus", 0, "task budget CPUs (with --task-memory)")
	ttl := fs.String("ttl", "", "time to live, e.g. 2h (required)")
	teardown := fs.String("teardown", "", "teardown script file (required)")
	dockerProject := fs.String("docker-project", "", "compose project of the stand")
	priority := fs.String("priority", "normal", "low|normal|high")
	name := fs.String("name", "", "reservation name")
	key := fs.String("key", "", "idempotency key")
	session := fs.String("session", "", "session id")
	project := fs.String("project", "", "project (default: git root of cwd)")
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
	if *memory == "" {
		return usagef("reserve: --memory is required")
	}
	mem, err := units.ParseSize(*memory)
	if err != nil || mem == 0 {
		return usagef("reserve: invalid --memory %q", *memory)
	}
	if *cpus < 0 {
		return usagef("reserve: --cpus must not be negative")
	}
	if *ttl == "" {
		return usagef("reserve: --ttl is required")
	}
	d, err := time.ParseDuration(*ttl)
	if err != nil || d < time.Second {
		return usagef("reserve: invalid --ttl %q", *ttl)
	}
	if *teardown == "" {
		return usagef("reserve: --teardown is required")
	}
	if (*taskMemory == "") != (*taskCPUs == 0) {
		return usagef("reserve: --task-memory and --task-cpus must be given together")
	}
	var tmem uint64
	if *taskMemory != "" {
		if tmem, err = units.ParseSize(*taskMemory); err != nil {
			return usagef("reserve: --task-memory: %v", err)
		}
		if *taskCPUs < 1 {
			return usagef("reserve: --task-cpus must be at least 1")
		}
	}
	if _, err := model.ParsePriority(*priority); err != nil {
		return usagef("reserve: %v", err)
	}
	script, err := os.ReadFile(*teardown)
	if err != nil {
		return usagef("reserve: cannot read --teardown file: %v", err)
	}
	if len(script) == 0 || len(script) > maxTeardownScript {
		return usagef("reserve: --teardown script must be 1 byte to 64 KiB")
	}
	if !utf8.Valid(script) {
		return usagef("reserve: teardown script must be valid UTF-8")
	}
	env, err := captureEnv(c, "reserve", reserveCapturedEnv, envs)
	if err != nil {
		return err
	}
	cwd, proj, _, _, err := detectProject(c, *project)
	if err != nil {
		return err
	}
	var v daemon.ItemView
	err = c.call(c.ctx, "reserve", daemon.ReserveArgs{
		Name: *name, Cwd: cwd, Env: env, Memory: mem, CPUs: *cpus, TaskMemory: tmem, TaskCPUs: *taskCPUs,
		TTLSeconds: int64(d / time.Second), Teardown: string(script), DockerProject: *dockerProject,
		Priority: *priority, SessionID: *session, ClientType: "cli", Project: proj, IdemKey: *key,
	}, &v)
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
	return nil
}

func cmdExtend(c *cmdCtx, args []string) error {
	fs := c.newFlagSet("extend")
	ttl := fs.String("ttl", "", "new time to live from now, e.g. 1h (required)")
	asJSON := fs.Bool("json", false, "print the item as JSON")
	id, err := parseID(fs, args)
	if err != nil {
		return err
	}
	d, err := time.ParseDuration(*ttl)
	if *ttl == "" || err != nil || d < time.Second {
		return usagef("extend: --ttl is required and must be a duration of at least 1s")
	}
	return itemOp(c, "extend", map[string]any{"id": id, "ttl_seconds": int64(d / time.Second)}, *asJSON)
}

func cmdRelease(c *cmdCtx, args []string) error {
	fs := c.newFlagSet("release")
	force := fs.Bool("force", false, "also release a failed or unknown teardown")
	asJSON := fs.Bool("json", false, "print the item as JSON")
	id, err := parseID(fs, args)
	if err != nil {
		return err
	}
	return itemOp(c, "release", map[string]any{"id": id, "force": *force}, *asJSON)
}

func cmdTeardown(c *cmdCtx, args []string) error {
	fs := c.newFlagSet("teardown")
	retry := fs.Bool("retry", false, "run the teardown script again (required)")
	asJSON := fs.Bool("json", false, "print the item as JSON")
	id, err := parseID(fs, args)
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
