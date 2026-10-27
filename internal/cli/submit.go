package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"resource-dispatcher/internal/daemon"
	"resource-dispatcher/internal/model"
	"resource-dispatcher/internal/units"
)

// stringList is a repeatable string flag.
type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

// capturedEnv are the caller variables passed to the task; LC_* is an explicit
// list because env is a lookup function, not an enumeration.
var capturedEnv = []string{
	"PATH", "HOME", "LANG", "VIRTUAL_ENV",
	"LC_ALL", "LC_CTYPE", "LC_NUMERIC", "LC_TIME", "LC_COLLATE", "LC_MONETARY", "LC_MESSAGES",
	"LC_PAPER", "LC_NAME", "LC_ADDRESS", "LC_TELEPHONE", "LC_MEASUREMENT", "LC_IDENTIFICATION",
}

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
	name := fs.String("name", "", "task name")
	key := fs.String("key", "", "idempotency key")
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
	if len(argv) == 0 {
		return usagef("submit: command required after --")
	}
	if *timeout == "" {
		return usagef("submit: --timeout is required")
	}
	d, err := time.ParseDuration(*timeout)
	if err != nil || d < time.Second {
		return usagef("submit: invalid --timeout %q", *timeout)
	}
	if *memory == "" {
		return usagef("submit: --memory is required")
	}
	limit, err := units.ParseSize(*memory)
	if err != nil {
		return usagef("submit: --memory: %v", err)
	}
	var exp uint64
	if *expected != "" {
		if exp, err = units.ParseSize(*expected); err != nil {
			return usagef("submit: --expected: %v", err)
		}
	}
	if *cpus < 1 {
		return usagef("submit: --cpus must be at least 1")
	}
	if _, err := model.ParsePriority(*priority); err != nil {
		return usagef("submit: %v", err)
	}

	env, err := captureEnv(c, "submit", capturedEnv, envs)
	if err != nil {
		return err
	}
	cwd, proj, rev, dirty, err := detectProject(c, *project)
	if err != nil {
		return err
	}
	var v daemon.ItemView
	err = c.call(c.ctx, "submit", daemon.SubmitArgs{
		Name: *name, Argv: argv, Cwd: cwd, Env: env, Limit: limit, Expected: exp, CPUs: *cpus,
		TimeoutSeconds: int64(d / time.Second), Priority: *priority, SessionID: *session,
		ClientType: "cli", Project: proj, IdemKey: *key, GitRev: rev, GitDirty: dirty, Within: model.ItemID(*within),
	}, &v)
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(c, v)
	}
	fmt.Fprintln(c.stdout, v.ID)
	return nil
}

// captureEnv collects the caller variables named in names plus the --env K=V pairs.
// It never echoes a pair: the value may be a secret.
func captureEnv(c *cmdCtx, cmd string, names []string, extra stringList) (map[string]string, error) {
	env := map[string]string{}
	for _, k := range names {
		if v := c.env(k); v != "" {
			env[k] = v
		}
	}
	for _, kv := range extra {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return nil, usagef("%s: --env must be K=V", cmd)
		}
		if !validEnvName(k) {
			return nil, usagef("%s: --env has an invalid variable name %q", cmd, k)
		}
		env[k] = v
	}
	return env, nil
}

// detectProject returns the cwd, the project (flag, else git root, else cwd) and git state.
func detectProject(c *cmdCtx, flagProject string) (cwd, proj, rev string, dirty bool, err error) {
	if cwd, err = os.Getwd(); err != nil {
		return
	}
	gctx, cancel := context.WithTimeout(c.ctx, gitTimeout)
	defer cancel()
	proj = flagProject
	if proj == "" {
		proj = gitOut(gctx, cwd, "rev-parse", "--show-toplevel")
		if proj == "" {
			proj = cwd
		}
	}
	rev = gitOut(gctx, cwd, "rev-parse", "HEAD")
	dirty = rev != "" && gitOut(gctx, cwd, "status", "--porcelain") != ""
	return
}

// gitTimeout bounds all git calls of one submit.
var gitTimeout = 5 * time.Second

// gitOut runs git in dir and returns trimmed stdout; any error yields "".
// --no-optional-locks keeps status from taking index.lock.
func gitOut(ctx context.Context, dir string, args ...string) string {
	cmd := exec.CommandContext(ctx, "git", append([]string{"--no-optional-locks"}, args...)...)
	cmd.Dir = dir
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func printJSON(c *cmdCtx, v any) error {
	enc := json.NewEncoder(c.stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// validEnvName reports whether k is a portable environment variable name.
func validEnvName(k string) bool {
	if k == "" {
		return false
	}
	for i, r := range k {
		switch {
		case r == '_', r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}
