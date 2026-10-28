// Package client shares request validation and identity between CLI and MCP.
package client

import (
	"context"
	"fmt"
	"github.com/creatorofuniverses/agent-queue/internal/daemon"
	"github.com/creatorofuniverses/agent-queue/internal/model"
	"github.com/creatorofuniverses/agent-queue/internal/profile"
	"github.com/creatorofuniverses/agent-queue/internal/units"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

type UsageError struct {
	Message string
	Err     error // optional cause, for errors.Is
}

func (e *UsageError) Error() string        { return e.Message }
func (e *UsageError) Unwrap() error        { return e.Err }
func usagef(format string, a ...any) error { return &UsageError{Message: fmt.Sprintf(format, a...)} }

type SubmitOptions struct {
	Profile                                                              string
	Cwd, Name, Memory, Expected, Timeout, Priority, Key, Project, Within string
	CPUs                                                                 int
	Argv, Env                                                            []string
	NoNotify                                                             bool
	Identity                                                             Identity
}
type ReserveOptions struct {
	Cwd, Name, Memory, TaskMemory, TTL, TeardownFile, DockerProject, Priority, Key, Project string
	CPUs, TaskCPUs                                                                          int
	Env                                                                                     []string
	NoNotify                                                                                bool
	Identity                                                                                Identity
}

const maxTeardownScript = 64 << 10

// capturedEnv are the caller variables passed to the task; LC_* is an explicit
// list because env is a lookup function, not an enumeration.
var capturedEnv = []string{
	"PATH", "HOME", "LANG", "VIRTUAL_ENV",
	"LC_ALL", "LC_CTYPE", "LC_NUMERIC", "LC_TIME", "LC_COLLATE", "LC_MONETARY", "LC_MESSAGES",
	"LC_PAPER", "LC_NAME", "LC_ADDRESS", "LC_TELEPHONE", "LC_MEASUREMENT", "LC_IDENTIFICATION",
}

var reserveCapturedEnv = append(append([]string{}, capturedEnv...), "DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_CONFIG")

func BuildSubmit(ctx context.Context, o SubmitOptions, getenv func(string) string) (daemon.SubmitArgs, error) {
	if len(o.Argv) == 0 {
		return daemon.SubmitArgs{}, usagef("submit: command required after --")
	}
	if o.Timeout == "" {
		return daemon.SubmitArgs{}, usagef("submit: --timeout is required")
	}
	d, err := time.ParseDuration(o.Timeout)
	if err != nil || d < time.Second {
		return daemon.SubmitArgs{}, usagef("submit: invalid --timeout %q", o.Timeout)
	}
	name := o.Profile
	derived := name == "" && o.Name != ""
	if name == "" {
		name = DefaultProfile(ctx, o.Cwd, o.Project, o.Name)
	}
	if name != "" {
		if e := profile.ValidName(name); e != nil {
			return daemon.SubmitArgs{}, usagef("submit: %v", e)
		}
	}
	if o.Memory == "" && name == "" {
		return daemon.SubmitArgs{}, usagef("submit: --memory is required (or --name/--profile with history)")
	}
	var limit uint64
	if o.Memory != "" {
		limit, err = units.ParseSize(o.Memory)
		if err != nil {
			return daemon.SubmitArgs{}, usagef("submit: --memory: %v", err)
		}
	}
	var exp uint64
	if o.Expected != "" {
		if exp, err = units.ParseSize(o.Expected); err != nil {
			return daemon.SubmitArgs{}, usagef("submit: --expected: %v", err)
		}
	}
	if o.CPUs < 1 {
		return daemon.SubmitArgs{}, usagef("submit: --cpus must be at least 1")
	}
	if _, err := model.ParsePriority(o.Priority); err != nil {
		return daemon.SubmitArgs{}, usagef("submit: %v", err)
	}

	env, err := captureEnv(getenv, "submit", capturedEnv, o.Env)
	if err != nil {
		return daemon.SubmitArgs{}, err
	}
	if !filepath.IsAbs(o.Cwd) {
		return daemon.SubmitArgs{}, usagef("submit: cwd must be absolute")
	}
	proj, rev, dirty := DetectProject(ctx, o.Cwd, o.Project)
	return daemon.SubmitArgs{
		Profile: name, ProfileDerived: derived, NoNotify: o.NoNotify, Name: o.Name, Argv: o.Argv, Cwd: o.Cwd, Env: env, Limit: limit, Expected: exp, CPUs: o.CPUs,
		TimeoutSeconds: int64(d / time.Second), Priority: o.Priority, SessionID: o.Identity.SessionID,
		ClientType: o.Identity.ClientType, Project: proj, IdemKey: o.Key, GitRev: rev, GitDirty: dirty, Within: model.ItemID(o.Within),
	}, nil
}
func BuildReserve(ctx context.Context, o ReserveOptions, getenv func(string) string) (daemon.ReserveArgs, error) {
	if o.Memory == "" {
		return daemon.ReserveArgs{}, usagef("reserve: --memory is required")
	}
	mem, err := units.ParseSize(o.Memory)
	if err != nil || mem == 0 {
		return daemon.ReserveArgs{}, usagef("reserve: invalid --memory %q", o.Memory)
	}
	if o.CPUs < 0 {
		return daemon.ReserveArgs{}, usagef("reserve: --cpus must not be negative")
	}
	if o.TTL == "" {
		return daemon.ReserveArgs{}, usagef("reserve: --ttl is required")
	}
	d, err := time.ParseDuration(o.TTL)
	if err != nil || d < time.Second {
		return daemon.ReserveArgs{}, usagef("reserve: invalid --ttl %q", o.TTL)
	}
	if o.TeardownFile == "" {
		return daemon.ReserveArgs{}, usagef("reserve: --teardown is required")
	}
	if (o.TaskMemory == "") != (o.TaskCPUs == 0) {
		return daemon.ReserveArgs{}, usagef("reserve: --task-memory and --task-cpus must be given together")
	}
	var tmem uint64
	if o.TaskMemory != "" {
		if tmem, err = units.ParseSize(o.TaskMemory); err != nil {
			return daemon.ReserveArgs{}, usagef("reserve: --task-memory: %v", err)
		}
		if o.TaskCPUs < 1 {
			return daemon.ReserveArgs{}, usagef("reserve: --task-cpus must be at least 1")
		}
	}
	if _, err := model.ParsePriority(o.Priority); err != nil {
		return daemon.ReserveArgs{}, usagef("reserve: %v", err)
	}
	script, err := os.ReadFile(o.TeardownFile)
	if err != nil {
		return daemon.ReserveArgs{}, usagef("reserve: cannot read --teardown file: %v", err)
	}
	if len(script) == 0 || len(script) > maxTeardownScript {
		return daemon.ReserveArgs{}, usagef("reserve: --teardown script must be 1 byte to 64 KiB")
	}
	if !utf8.Valid(script) {
		return daemon.ReserveArgs{}, usagef("reserve: teardown script must be valid UTF-8")
	}
	env, err := captureEnv(getenv, "reserve", reserveCapturedEnv, o.Env)
	if err != nil {
		return daemon.ReserveArgs{}, err
	}
	if !filepath.IsAbs(o.Cwd) {
		return daemon.ReserveArgs{}, usagef("reserve: cwd must be absolute")
	}
	proj, _, _ := DetectProject(ctx, o.Cwd, o.Project)
	return daemon.ReserveArgs{
		NoNotify: o.NoNotify, Name: o.Name, Cwd: o.Cwd, Env: env, Memory: mem, CPUs: o.CPUs, TaskMemory: tmem, TaskCPUs: o.TaskCPUs,
		TTLSeconds: int64(d / time.Second), Teardown: string(script), DockerProject: o.DockerProject,
		Priority: o.Priority, SessionID: o.Identity.SessionID, ClientType: o.Identity.ClientType, Project: proj, IdemKey: o.Key,
	}, nil
}

// captureEnv collects the caller variables named in names plus the --env K=V pairs.
// It never echoes a pair: the value may be a secret.
func captureEnv(getenv func(string) string, cmd string, names []string, extra []string) (map[string]string, error) {
	env := map[string]string{}
	for _, k := range names {
		if v := getenv(k); v != "" {
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

type EstimateOptions struct {
	Cwd, Memory, Expected, Timeout, Priority, Profile, Name, Project string
	CPUs                                                             int
}

func BuildEstimate(ctx context.Context, o EstimateOptions) (daemon.EstimateArgs, error) {
	var a daemon.EstimateArgs
	var e error
	a.Profile = o.Profile
	if a.Profile == "" {
		a.Profile = DefaultProfile(ctx, o.Cwd, o.Project, o.Name)
	}
	if a.Profile != "" {
		if e = profile.ValidName(a.Profile); e != nil {
			return a, usagef("estimate: %v", e)
		}
	}
	if o.Memory == "" && a.Profile == "" {
		return a, usagef("estimate: --memory or --name/--profile is required")
	}
	if o.Memory != "" {
		if a.Limit, e = units.ParseSize(o.Memory); e != nil {
			return a, usagef("estimate: --memory: %v", e)
		}
	}
	if o.Expected != "" {
		if a.Expected, e = units.ParseSize(o.Expected); e != nil {
			return a, usagef("estimate: --expected: %v", e)
		}
	}
	if o.Timeout != "" {
		d, err := units.ParseDuration(o.Timeout)
		if err != nil || d < time.Second {
			return a, usagef("estimate: invalid --timeout")
		}
		a.TimeoutSeconds = int64(d / time.Second)
	}
	if o.CPUs < 1 {
		return a, usagef("estimate: --cpus must be at least 1")
	}
	a.CPUs = o.CPUs
	if _, e = model.ParsePriority(o.Priority); e != nil {
		return a, usagef("estimate: %v", e)
	}
	a.Priority = o.Priority
	return a, nil
}
