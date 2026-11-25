package client

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/creatorofuniverses/agent-queue/internal/agentsess"
	"github.com/creatorofuniverses/agent-queue/internal/config"
)

// aq is pid selfPID in every fake tree and chain gives its ancestors the next pids.
// startTicks is field 22 of every fake /proc/<pid>/stat: with btime 1000000 the process
// began at 1000000500 ms, which the session files below are written against.
const (
	selfPID    = 100
	nearest    = selfPID + 1
	startTicks = 50
)

// noVars is a readable start environment without identity variables.
var noVars = map[string]string{}

type fakeProc struct {
	name    string
	ppid    int               // 1 ends the walk
	environ map[string]string // nil: /proc/<pid>/environ cannot be read
}

// chain makes a fake tree: aq is pid selfPID, the processes follow as its parent,
// grandparent and so on, and the last one belongs to init.
func chain(procs ...fakeProc) map[int]fakeProc {
	tree := map[int]fakeProc{selfPID: {name: "aq", ppid: nearest}}
	for i, p := range procs {
		p.ppid = 1
		if i+1 < len(procs) {
			p.ppid = selfPID + i + 2
		}
		tree[selfPID+i+1] = p
	}
	return tree
}

// sleepers is a chain of n processes that are not agents.
func sleepers(n int) []fakeProc {
	procs := make([]fakeProc, n)
	for i := range procs {
		procs[i] = fakeProc{name: "sleep", environ: noVars}
	}
	return procs
}

// writeTree writes the fake /proc: the boot time and, per process, comm, cmdline,
// status and stat; environ only for a process that has a readable one.
func writeTree(t *testing.T, tree map[int]fakeProc) string {
	t.Helper()
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "stat"), "btime 1000000\n")
	for pid, p := range tree {
		dir := filepath.Join(root, strconv.Itoa(pid))
		mustWrite(t, filepath.Join(dir, "comm"), p.name+"\n")
		mustWrite(t, filepath.Join(dir, "cmdline"), "/usr/bin/"+p.name+"\x00")
		mustWrite(t, filepath.Join(dir, "status"), fmt.Sprintf("Name:\t%s\nPid:\t%d\nPPid:\t%d\n", p.name, pid, p.ppid))
		// Field 22 of stat is the start time: the 18 fields between it and the state are zero.
		mustWrite(t, filepath.Join(dir, "stat"), fmt.Sprintf("%d (%s) S 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 %d 0\n", pid, p.name, startTicks))
		if p.environ != nil {
			var b strings.Builder
			for k, v := range p.environ {
				b.WriteString(k + "=" + v + "\x00")
			}
			mustWrite(t, filepath.Join(dir, "environ"), b.String())
		}
	}
	return root
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// sessionFile is one client's session file for a pid of the fake tree.
type sessionFile struct {
	client string // the directory under home: .claude or .openclaude
	pid    int
	body   string
}

// ocSession is the file OpenClaude writes for pid: no procStart, startedAt right
// after the process began.
func ocSession(pid int) []sessionFile {
	return []sessionFile{{"openclaude", pid, fmt.Sprintf(`{"pid":%d,"sessionId":"oc-session","status":"shell","updatedAt":1000000500,"startedAt":1000000503}`, pid)}}
}

// claudeSession is the file Claude Code writes for pid: the exact procStart.
func claudeSession(pid int) []sessionFile {
	return []sessionFile{{"claude", pid, fmt.Sprintf(`{"pid":%d,"sessionId":"claude-session","procStart":"%d"}`, pid, startTicks)}}
}

// fakeParent is the lookup over a fake tree, with the session files of a fake home.
func fakeParent(t *testing.T, tree map[int]fakeProc, files []sessionFile) Parent {
	t.Helper()
	home := t.TempDir()
	for _, f := range files {
		mustWrite(t, filepath.Join(home, "."+f.client, "sessions", strconv.Itoa(f.pid)+".json"), f.body)
	}
	proc := writeTree(t, tree)
	return Parent{
		ProcRoot: proc,
		Self:     selfPID,
		Names:    []string{"claude", "openclaude", "codex"},
		Sessions: agentsess.Reader{Sources: agentsess.Sources(home), ProcRoot: proc},
	}
}

func envOf(env map[string]string) func(string) string { return func(k string) string { return env[k] } }

// The nearest agent ancestor gives the identity: its own variable, else its verified
// session file, else cli. Without an agent the environment rules hold as before.
func TestDetectIdentityUnderAgent(t *testing.T) {
	codexVar := map[string]string{"CODEX_THREAD_ID": "t"}
	claudeVar := map[string]string{"CLAUDE_CODE_SESSION_ID": "c"}
	bothVars := map[string]string{"CODEX_THREAD_ID": "t", "CLAUDE_CODE_SESSION_ID": "c"}
	outerVar := map[string]string{"CLAUDE_CODE_SESSION_ID": "outer"}
	openClaude := chain(fakeProc{name: "openclaude", environ: noVars}, fakeProc{name: "sh", environ: noVars})
	twoAdapters := []config.AgentConfig{
		{Name: "claude", IdentityEnv: "CLAUDE_CODE_SESSION_ID", Delivery: config.DeliveryClientWait},
		{Name: "claude2", IdentityEnv: "CLAUDE_CODE_SESSION_ID", Delivery: config.DeliveryClientWait},
	}
	for _, tc := range []struct {
		name      string
		tree      map[int]fakeProc
		env       map[string]string
		files     []sessionFile
		agents    []config.AgentConfig // nil: the default adapters
		want      Identity
		ambiguous bool // several of aq's own variables: exit 64
	}{
		{"a terminal is no agent", chain(fakeProc{name: "terminal", environ: noVars}), nil, nil, nil, Identity{"cli", ""}, false},
		{"codex above a shell", chain(fakeProc{name: "codex", environ: noVars}, fakeProc{name: "sh", environ: noVars}), codexVar, nil, nil, Identity{"codex", "t"}, false},
		{"claude above a shell", chain(fakeProc{name: "claude", environ: noVars}, fakeProc{name: "sh", environ: noVars}), claudeVar, nil, nil, Identity{"claude", "c"}, false},
		{"claude started without its variable: the file with the exact procStart", chain(fakeProc{name: "claude", environ: noVars}, fakeProc{name: "sh", environ: noVars}), nil, claudeSession(nearest), nil, Identity{"claude", "claude-session"}, false},
		{"codex above claude: only the claude variable is aq's own", chain(fakeProc{name: "claude", environ: codexVar}, fakeProc{name: "codex", environ: noVars}), bothVars, nil, nil, Identity{"claude", "c"}, false},
		{"claude above codex: only the codex variable is aq's own", chain(fakeProc{name: "codex", environ: claudeVar}, fakeProc{name: "claude", environ: noVars}), bothVars, nil, nil, Identity{"codex", "t"}, false},
		{"OpenClaude: the file without procStart", openClaude, nil, ocSession(nearest), nil, Identity{"claude", "oc-session"}, false},
		{"codex above OpenClaude: the inherited variable is not aq's own", chain(fakeProc{name: "openclaude", environ: codexVar}, fakeProc{name: "codex", environ: noVars}), codexVar, ocSession(nearest), nil, Identity{"claude", "oc-session"}, false},
		{"claude above OpenClaude: the inherited variable is not aq's own", chain(fakeProc{name: "openclaude", environ: outerVar}, fakeProc{name: "claude", environ: noVars}), outerVar, ocSession(nearest), nil, Identity{"claude", "oc-session"}, false},
		{"codex above OpenClaude without a file", chain(fakeProc{name: "openclaude", environ: codexVar}, fakeProc{name: "codex", environ: noVars}), codexVar, nil, nil, Identity{"cli", ""}, false},
		{"claude above OpenClaude without a file", chain(fakeProc{name: "openclaude", environ: outerVar}, fakeProc{name: "claude", environ: noVars}), outerVar, nil, nil, Identity{"cli", ""}, false},
		{"a non-agent is skipped whatever file sits on its pid", chain(fakeProc{name: "bash", environ: noVars}), nil, ocSession(nearest), nil, Identity{"cli", ""}, false},
		{"a broken chain keeps the environment rules", chain(fakeProc{name: "tmux", environ: noVars}), codexVar, nil, nil, Identity{"codex", "t"}, false},
		{"a broken chain keeps the ambiguity error", chain(fakeProc{name: "tmux", environ: noVars}), bothVars, nil, nil, Identity{}, true},
		{"a file without a sessionId", openClaude, nil, []sessionFile{{"openclaude", nearest, fmt.Sprintf(`{"pid":%d,"sessionId":"","startedAt":1000000503}`, nearest)}}, nil, Identity{"cli", ""}, false},
		{"the own variable wins over a file saying otherwise", chain(fakeProc{name: "claude", environ: noVars}, fakeProc{name: "sh", environ: noVars}), claudeVar, []sessionFile{{"claude", nearest, fmt.Sprintf(`{"pid":%d,"sessionId":"other","procStart":"%d"}`, nearest, startTicks)}}, nil, Identity{"claude", "c"}, false},
		{"the nearest agent decides: OpenClaude without a file above a claude with one", chain(fakeProc{name: "openclaude", environ: noVars}, fakeProc{name: "claude", environ: noVars}), nil, claudeSession(nearest + 1), nil, Identity{"cli", ""}, false},
		{"an unreadable environment: the file still counts", chain(fakeProc{name: "openclaude"}, fakeProc{name: "sh", environ: noVars}), codexVar, ocSession(nearest), nil, Identity{"claude", "oc-session"}, false},
		{"an unreadable environment: no variable is trusted", chain(fakeProc{name: "openclaude"}, fakeProc{name: "sh", environ: noVars}), codexVar, nil, nil, Identity{"cli", ""}, false},
		{"two adapters on one variable: the file gives no identity", openClaude, nil, ocSession(nearest), twoAdapters, Identity{"cli", ""}, false},
		{"two adapters on one variable: the variable is ambiguous", openClaude, claudeVar, ocSession(nearest), twoAdapters, Identity{}, true},
		{"63 non-agents, then OpenClaude: the 64th ancestor is still found", chain(append(sleepers(63), fakeProc{name: "openclaude", environ: noVars})...), nil, ocSession(nearest + 63), nil, Identity{"claude", "oc-session"}, false},
		{"64 non-agents, then OpenClaude: the walk is over", chain(append(sleepers(64), fakeProc{name: "openclaude", environ: noVars})...), nil, ocSession(nearest + 64), nil, Identity{"cli", ""}, false},
		{"aq's parent is gone", chain(), nil, nil, nil, Identity{"cli", ""}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agents := tc.agents
			if agents == nil {
				agents = config.Default().Agents
			}
			got, err := DetectIdentity(fakeParent(t, tc.tree, tc.files), agents, envOf(tc.env), "", "")
			if tc.ambiguous {
				var ue *UsageError
				if !errors.Is(err, ErrAmbiguousIdentity) || !errors.As(err, &ue) {
					t.Fatalf("DetectIdentity() = %v, %v; want exit 64 on several own variables", got, err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("DetectIdentity() = %v, %v; want %v", got, err, tc.want)
			}
		})
	}
}

// AQ_NO_PARENT_SESSION and the flags keep their meaning under an agent.
func TestDetectIdentityUnderAgentOptions(t *testing.T) {
	openClaude := chain(fakeProc{name: "openclaude", environ: noVars}, fakeProc{name: "sh", environ: noVars})
	for _, tc := range []struct {
		name, client, session string
		off                   bool
		env                   map[string]string
		want                  Identity
		usage                 string // the message of the usage error, if any
	}{
		{"AQ_NO_PARENT_SESSION", "", "", true, nil, Identity{"cli", ""}, ""},
		{"AQ_NO_PARENT_SESSION keeps the environment rules", "", "", true, map[string]string{"CODEX_THREAD_ID": "t"}, Identity{"codex", "t"}, ""},
		{"--session is the session", "", "s", false, nil, Identity{"claude", "s"}, ""},
		{"--client turns the lookup off", "codex", "", false, nil, Identity{}, "--client codex needs a session (--session or CODEX_THREAD_ID)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent := fakeParent(t, openClaude, ocSession(nearest))
			parent.Off = tc.off
			got, err := DetectIdentity(parent, config.Default().Agents, envOf(tc.env), tc.client, tc.session)
			if tc.usage != "" {
				var ue *UsageError
				if !errors.As(err, &ue) || ue.Message != tc.usage {
					t.Fatalf("DetectIdentity() = %v, %v; want the usage error %q", got, err, tc.usage)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("DetectIdentity() = %v, %v; want %v", got, err, tc.want)
			}
		})
	}
}

// The walk changes where the session comes from, not where it is notified: a sub-agent
// thread of a codex ancestor resolves to the root thread, as without an ancestor.
func TestDetectIdentityUnderCodexSubagent(t *testing.T) {
	const (
		root = "01a1b12c-34a4-79e2-9d46-42a2e28309b3"
		sub  = "01a1b131-4886-7ca3-8ec7-7b69b601d554"
	)
	codexHome := t.TempDir()
	writeRollout(t, codexHome, "2026/11/06", root, map[string]any{"session_id": root, "thread_source": "user"})
	writeRollout(t, codexHome, "2026/11/06", sub, subagentMeta(root, root))
	parent := fakeParent(t, chain(fakeProc{name: "codex", environ: noVars}, fakeProc{name: "sh", environ: noVars}), nil)
	env := map[string]string{"CODEX_HOME": codexHome, "CODEX_THREAD_ID": sub}
	want := Identity{"codex", root}
	if got, err := DetectIdentity(parent, config.Default().Agents, envOf(env), "", ""); err != nil || got != want {
		t.Fatalf("DetectIdentity() = %v, %v; want %v", got, err, want)
	}
}

// The agent wrote its session file under the home it was started with. A command
// without HOME (env -i, unset) or with another one still finds it there: the
// agent's home is looked at after the command's own.
func TestDetectIdentityAgentHome(t *testing.T) {
	for _, tc := range []struct {
		name       string
		agent      string
		named      bool          // the agent's start environment names the home with its files
		unreadable bool          // the agent's environment cannot be read
		files      []sessionFile // under the agent's home
		command    string        // the command's HOME: none, other or the agent's
		otherFiles []sessionFile // under that other home
		want       Identity
	}{
		{"OpenClaude under env -i", "openclaude", true, false, ocSession(nearest), "none", nil, Identity{"claude", "oc-session"}},
		{"OpenClaude with another HOME", "openclaude", true, false, ocSession(nearest), "other", nil, Identity{"claude", "oc-session"}},
		{"Claude Code under env -i: the exact procStart", "claude", true, false, claudeSession(nearest), "none", nil, Identity{"claude", "claude-session"}},
		{"the command's home is looked at first", "openclaude", true, false, ocSession(nearest), "other", claudeSession(nearest), Identity{"claude", "claude-session"}},
		{"no HOME in the agent's environment", "openclaude", false, false, ocSession(nearest), "none", nil, Identity{"cli", ""}},
		{"an unreadable environment of the agent", "openclaude", true, true, ocSession(nearest), "none", nil, Identity{"cli", ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			for _, f := range tc.files {
				mustWrite(t, filepath.Join(home, "."+f.client, "sessions", strconv.Itoa(f.pid)+".json"), f.body)
			}
			agent := fakeProc{name: tc.agent}
			switch {
			case tc.unreadable:
			case tc.named:
				agent.environ = map[string]string{"HOME": home}
			default:
				agent.environ = noVars
			}
			var commandHome string
			if tc.command == "other" {
				commandHome = t.TempDir()
				for _, f := range tc.otherFiles {
					mustWrite(t, filepath.Join(commandHome, "."+f.client, "sessions", strconv.Itoa(f.pid)+".json"), f.body)
				}
			}
			proc := writeTree(t, chain(agent, fakeProc{name: "sh", environ: noVars}))
			parent := Parent{ProcRoot: proc, Self: selfPID, Names: []string{"claude", "openclaude", "codex"},
				Sessions: agentsess.Reader{Sources: agentsess.Sources(commandHome), ProcRoot: proc}}
			got, err := DetectIdentity(parent, config.Default().Agents, envOf(nil), "", "")
			if err != nil || got != tc.want {
				t.Fatalf("DetectIdentity() = %v, %v; want %v", got, err, tc.want)
			}
		})
	}
}

func TestParentFrom(t *testing.T) {
	cfg := config.Default()
	env := map[string]string{"HOME": "/home/agent"}
	want := Parent{Names: cfg.Stats.AgentProcesses, Sessions: agentsess.Reader{Sources: agentsess.Sources("/home/agent"), ProcRoot: "/proc"}}
	if got := ParentFrom(cfg, envOf(env)); !reflect.DeepEqual(got, want) {
		t.Fatalf("ParentFrom() = %+v, want %+v", got, want)
	}
	env["AQ_NO_PARENT_SESSION"] = "1"
	if got := ParentFrom(cfg, envOf(env)); !got.Off {
		t.Fatalf("AQ_NO_PARENT_SESSION is set: %+v", got)
	}
	delete(env, "AQ_NO_PARENT_SESSION")
	delete(env, "HOME")
	if got := ParentFrom(cfg, envOf(env)); got.Off || got.Sessions.Sources != nil || got.Sessions.ProcRoot != "/proc" {
		t.Fatalf("no HOME: %+v", got)
	}
}
