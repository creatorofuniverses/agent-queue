package cli

import (
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

	env := map[string]string{}
	for _, k := range capturedEnv {
		if v := c.env(k); v != "" {
			env[k] = v
		}
	}
	for _, kv := range envs {
		k, v, ok := strings.Cut(kv, "=")
		// Never echo kv: the value may be a secret.
		if !ok {
			return usagef("submit: --env must be K=V")
		}
		if !validEnvName(k) {
			return usagef("submit: --env has an invalid variable name %q", k)
		}
		env[k] = v
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	proj := *project
	if proj == "" {
		proj = gitOut(cwd, "rev-parse", "--show-toplevel")
		if proj == "" {
			proj = cwd
		}
	}
	rev := gitOut(cwd, "rev-parse", "HEAD")
	dirty := rev != "" && gitOut(cwd, "status", "--porcelain") != ""

	var v daemon.ItemView
	err = c.call(c.ctx, "submit", daemon.SubmitArgs{
		Name: *name, Argv: argv, Cwd: cwd, Env: env, Limit: limit, Expected: exp, CPUs: *cpus,
		TimeoutSeconds: int64(d / time.Second), Priority: *priority, SessionID: *session,
		ClientType: "cli", Project: proj, IdemKey: *key, GitRev: rev, GitDirty: dirty,
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

// gitOut runs git in dir and returns trimmed stdout; any error yields "".
func gitOut(dir string, args ...string) string {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
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
