package client

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/creatorofuniverses/agent-queue/internal/agentsess"
	"github.com/creatorofuniverses/agent-queue/internal/config"
	"github.com/creatorofuniverses/agent-queue/internal/platform"
)

// Parent finds the agent process aq was started from and resolves the identity
// from it. The zero value looks at nothing: identity then comes from the
// environment alone, as before agents were looked up up the process tree.
type Parent struct {
	ProcRoot string           // defaults to /proc
	Self     int              // the aq process; 0 is os.Getpid()
	Names    []string         // agent process names: stats.agent_processes
	Sessions agentsess.Reader // the agent's session file
	Off      bool             // AQ_NO_PARENT_SESSION is set
}

// ParentFrom builds the lookup from the config and the environment aq runs in.
func ParentFrom(cfg config.Config, env func(string) string) Parent {
	return Parent{
		Names:    cfg.Stats.AgentProcesses,
		Sessions: agentsess.Reader{Sources: agentsess.Sources(env("HOME")), ProcRoot: "/proc"},
		Off:      env("AQ_NO_PARENT_SESSION") != "",
	}
}

func (p Parent) procRoot() string {
	if p.ProcRoot == "" {
		return "/proc"
	}
	return p.ProcRoot
}

// nearestAgent walks up from aq's parent and returns the nearest agent: processes
// outside Names are skipped, whatever session file sits on their pid. The walk
// covers at most 64 levels and stops at a parent that is gone (PPid 0) or init.
func (p Parent) nearestAgent() (int, bool) {
	if p.Off || len(p.Names) == 0 {
		return 0, false
	}
	self := p.Self
	if self == 0 {
		self = os.Getpid()
	}
	pid := parentPID(p.procRoot(), self)
	for depth := 0; depth < 64 && pid > 1; depth++ {
		dir := filepath.Join(p.procRoot(), strconv.Itoa(pid))
		comm, _ := os.ReadFile(filepath.Join(dir, "comm"))
		cmdline, _ := os.ReadFile(filepath.Join(dir, "cmdline"))
		if platform.AgentIndex(p.Names, string(comm), string(cmdline)) >= 0 {
			return pid, true
		}
		pid = parentPID(p.procRoot(), pid)
	}
	return 0, false
}

// identity resolves the session of the nearest agent ancestor: its own identity
// variable, else its verified session file, else cli.
func (p Parent) identity(pid int, agents []config.AgentConfig, env func(string) string, flagSession string) (Identity, error) {
	id := Identity{ClientType: "cli", SessionID: flagSession}
	// /proc shows the environment a process was started with. A variable the agent
	// itself received from an outer agent is not its own session; when the
	// environment cannot be read no variable is trusted with identity.
	agentEnv, readable := execEnv(filepath.Join(p.procRoot(), strconv.Itoa(pid), "environ"))
	var own []config.AgentConfig
	for _, a := range agents {
		if v := env(a.IdentityEnv); v != "" && readable && agentEnv[a.IdentityEnv] != v {
			own = append(own, a)
		}
	}
	switch {
	case len(own) > 1:
		return Identity{}, ambiguous(own)
	case len(own) == 1:
		id.ClientType = own[0].Name
		if id.SessionID == "" {
			id.SessionID = env(own[0].IdentityEnv)
		}
		if isCodex(own[0]) {
			id.SessionID = codexRootThread(env, id.SessionID)
		}
	default:
		// The agent wrote its session file under the home it was started with: a
		// command without HOME (env -i, unset) or with another one still finds it
		// there, after the command's own home.
		sessions := p.Sessions
		if home := agentEnv["HOME"]; home != "" && home != env("HOME") {
			sessions.Sources = slices.Concat(sessions.Sources, agentsess.Sources(home))
		}
		src, info, ok := sessions.Lookup(pid)
		if !ok || info.SessionID == "" {
			return id, nil
		}
		var matches []config.AgentConfig
		for _, a := range agents {
			if a.IdentityEnv == src.IdentityEnv {
				matches = append(matches, a)
			}
		}
		if len(matches) == 1 { // two adapters sharing one variable give no identity
			id.ClientType = matches[0].Name
			if id.SessionID == "" {
				id.SessionID = info.SessionID
			}
		}
	}
	return id, nil
}

// parentPID reads PPid from /proc/<pid>/status; 0 when the process is gone.
func parentPID(procRoot string, pid int) int {
	b, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "status"))
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "PPid:") {
			n, _ := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "PPid:")))
			return n
		}
	}
	return 0
}

// execEnv is the environment a process was started with, as /proc shows it.
func execEnv(path string) (map[string]string, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	m := make(map[string]string)
	for _, kv := range strings.Split(string(b), "\x00") {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	return m, true
}
