package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/creatorofuniverses/agent-queue/internal/agentsess"
	"github.com/creatorofuniverses/agent-queue/internal/client"
	"github.com/creatorofuniverses/agent-queue/internal/config"
	"github.com/creatorofuniverses/agent-queue/internal/daemon"
	"github.com/creatorofuniverses/agent-queue/internal/hook"
	"github.com/creatorofuniverses/agent-queue/internal/model"
	"github.com/creatorofuniverses/agent-queue/internal/protocol"
)

// underAncestor is a command context that detects its session through the fake OpenClaude
// tree of fakeAgentTree, the way Main builds the lookup from the config; out takes both
// streams. The harness env holds no identity variable: only the lookup knows the session.
func underAncestor(t *testing.T, h *harness, out *bytes.Buffer) *cmdCtx {
	t.Helper()
	proc, home := fakeAgentTree(t)
	session := client.Parent{ProcRoot: proc, Self: 100, Names: []string{"openclaude"}, Sessions: agentsess.Reader{Sources: agentsess.Sources(home), ProcRoot: proc}}
	return &cmdCtx{ctx: context.Background(), env: func(k string) string { return h.vars[k] }, stdout: out, stderr: out, parent: func(config.Config) client.Parent { return session }}
}

// feedStdin replaces os.Stdin, which aq hook and aq mcp read directly, until the test ends.
func feedStdin(t *testing.T, input string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.WriteString(input); err != nil {
		t.Fatal(err)
	}
	w.Close()
	old := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = old; r.Close() })
}

// Under an agent ancestor submit and reserve send the session of its session file; from
// the environment alone both would send cli with no session.
func TestSubmitReserveFromParentSession(t *testing.T) {
	p := filepath.Join(t.TempDir(), "teardown")
	if err := os.WriteFile(p, []byte("true"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		cmd  command
		args []string
	}{
		{"submit", cmdSubmit, []string{"--memory", "64M", "--timeout", "1m", "--", "true"}},
		{"reserve", cmdReserve, []string{"--memory", "64M", "--ttl", "1h", "--teardown", p}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sent client.Identity
			h := socketHarness(t, func(_ context.Context, op string, raw json.RawMessage, _ protocol.StreamWriter) (any, error) {
				var a struct {
					ClientType string `json:"client_type"`
					SessionID  string `json:"session_id"`
				}
				if e := json.Unmarshal(raw, &a); e != nil {
					t.Error(e)
				}
				if op != tc.name {
					t.Errorf("op %s, want %s", op, tc.name)
				}
				sent = client.Identity{ClientType: a.ClientType, SessionID: a.SessionID}
				return daemon.ItemView{ID: "i", State: model.StateGranted}, nil
			})
			if err := tc.cmd(underAncestor(t, h, &bytes.Buffer{}), tc.args); err != nil {
				t.Fatal(err)
			}
			if sent != (client.Identity{ClientType: "claude", SessionID: "oc-session"}) {
				t.Fatalf("sent %+v, want claude with the ancestor's oc-session", sent)
			}
		})
	}
}

// After aq extend only a client-wait session needs a fresh waiter: the hint is printed
// for the ancestor's adapter, and nothing else tells the command who is asking.
func TestExtendFromParentSession(t *testing.T) {
	id := "0123456789abcdef0123456789abcdef0123" // a full ID: a prefix costs a list call first
	h := socketHarness(t, func(_ context.Context, op string, _ json.RawMessage, _ protocol.StreamWriter) (any, error) {
		if op != "extend" {
			t.Errorf("op %s", op)
		}
		return daemon.ItemView{ID: model.ItemID(id), State: model.StateGranted}, nil
	})
	var out bytes.Buffer
	if err := cmdExtend(underAncestor(t, h, &out), []string{id, "--ttl", "1h"}); err != nil {
		t.Fatal(err)
	}
	if want := "hint: to get the next expiry warning, run in background: aq wait " + id + " --until expiring"; !strings.Contains(out.String(), want) {
		t.Fatal(out.String())
	}
}

// The hook records the client of the ancestor's session file, not the bare cli.
func TestHookFromParentSession(t *testing.T) {
	h := newHarness(t)
	feedStdin(t, `{"tool_name":"Bash","tool_input":{"command":"docker run ubuntu"}}`+"\n")
	if err := cmdHook(underAncestor(t, h, &bytes.Buffer{}), []string{"pretooluse"}); err != nil {
		t.Fatal(err)
	}
	hits, err := hook.ReadHits(h.paths.StateDir, time.Time{})
	if err != nil || len(hits) != 1 || hits[0].Client != "claude" {
		t.Fatal(hits, err)
	}
}

// aq mcp hands its tools the ancestor lookup: queue_submit sends the ancestor's session
// instead of refusing an unknown identity.
func TestMCPCommandFromParentSession(t *testing.T) {
	var sent client.Identity
	h := socketHarness(t, func(_ context.Context, op string, raw json.RawMessage, _ protocol.StreamWriter) (any, error) {
		var a struct {
			ClientType string `json:"client_type"`
			SessionID  string `json:"session_id"`
		}
		if e := json.Unmarshal(raw, &a); e != nil {
			t.Error(e)
		}
		if op != "submit" {
			t.Errorf("op %s", op)
		}
		sent = client.Identity{ClientType: a.ClientType, SessionID: a.SessionID}
		return daemon.ItemView{ID: "i"}, nil
	})
	req, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{
		"name": "queue_submit", "arguments": map[string]any{"cwd": h.cwd, "argv": []string{"true"}, "memory": "64M", "timeout": "1m"}}})
	feedStdin(t, string(req)+"\n")
	var out bytes.Buffer
	if err := cmdMCP(underAncestor(t, h, &out), nil); err != nil {
		t.Fatal(err)
	}
	if sent != (client.Identity{ClientType: "claude", SessionID: "oc-session"}) || strings.Contains(out.String(), `"isError":true`) {
		t.Fatal(sent, out.String())
	}
}

// fakeOpenCodeTree is a fake /proc of aq (pid 100) started from an OpenCode tool shell:
// opencode-pty (pid 102), which is not an agent, belongs to the OpenCode service (pid 101).
// The service has a readable environ without identity variables: only a variable the command
// holds and the service does not is the agent's own, and without a readable environ none is.
func fakeOpenCodeTree(t *testing.T) (proc string) {
	t.Helper()
	proc = t.TempDir()
	write := func(path, text string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(proc, "100", "status"), "Name:\taq\nPid:\t100\nPPid:\t102\n")
	write(filepath.Join(proc, "102", "comm"), "opencode-pty\n")
	write(filepath.Join(proc, "102", "cmdline"), "/home/u/.cache/opencode/bin/opencode-pty/0.6.0-x/opencode-pty\x00")
	write(filepath.Join(proc, "102", "status"), "Name:\topencode-pty\nPid:\t102\nPPid:\t101\n")
	write(filepath.Join(proc, "101", "comm"), "opencode\n")
	write(filepath.Join(proc, "101", "cmdline"), "/home/u/.opencode/bin/opencode\x00")
	write(filepath.Join(proc, "101", "status"), "Name:\topencode\nPid:\t101\nPPid:\t1\n")
	write(filepath.Join(proc, "101", "environ"), "PATH=/usr/bin\x00")
	return proc
}

// underOpenCode is a command context that detects its session through the fake OpenCode tree
// of fakeOpenCodeTree, the way Main builds the lookup from the config; out takes both streams.
// OpenCode has no session files: the harness env holds OPENCODE_SESSION_ID, the variable it
// gives every tool shell.
func underOpenCode(t *testing.T, h *harness, out *bytes.Buffer) *cmdCtx {
	t.Helper()
	proc := fakeOpenCodeTree(t)
	session := client.Parent{ProcRoot: proc, Self: 100, Names: []string{"opencode"}}
	h.vars["OPENCODE_SESSION_ID"] = "ses_test"
	return &cmdCtx{ctx: context.Background(), env: func(k string) string { return h.vars[k] }, stdout: out, stderr: out, parent: func(config.Config) client.Parent { return session }}
}

// From an OpenCode tool shell submit and reserve send the session of the shell's
// OPENCODE_SESSION_ID with the opencode adapter; with the variable unset both would send cli.
func TestSubmitReserveFromOpenCode(t *testing.T) {
	p := filepath.Join(t.TempDir(), "teardown")
	if err := os.WriteFile(p, []byte("true"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		cmd  command
		args []string
	}{
		{"submit", cmdSubmit, []string{"--memory", "64M", "--timeout", "1m", "--", "true"}},
		{"reserve", cmdReserve, []string{"--memory", "64M", "--ttl", "1h", "--teardown", p}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sent client.Identity
			h := socketHarness(t, func(_ context.Context, op string, raw json.RawMessage, _ protocol.StreamWriter) (any, error) {
				var a struct {
					ClientType string `json:"client_type"`
					SessionID  string `json:"session_id"`
				}
				if e := json.Unmarshal(raw, &a); e != nil {
					t.Error(e)
				}
				if op != tc.name {
					t.Errorf("op %s, want %s", op, tc.name)
				}
				sent = client.Identity{ClientType: a.ClientType, SessionID: a.SessionID}
				return daemon.ItemView{ID: "i", State: model.StateGranted}, nil
			})
			if err := tc.cmd(underOpenCode(t, h, &bytes.Buffer{}), tc.args); err != nil {
				t.Fatal(err)
			}
			if sent != (client.Identity{ClientType: "opencode", SessionID: "ses_test"}) {
				t.Fatalf("sent %+v, want opencode with the shell's ses_test", sent)
			}
		})
	}
}

// After aq extend only a client-wait session needs a fresh waiter, and the opencode adapter
// delivers client-wait: the hint names the command OpenCode runs in the background.
func TestExtendFromOpenCode(t *testing.T) {
	id := "0123456789abcdef0123456789abcdef0123" // a full ID: a prefix costs a list call first
	h := socketHarness(t, func(_ context.Context, op string, _ json.RawMessage, _ protocol.StreamWriter) (any, error) {
		if op != "extend" {
			t.Errorf("op %s", op)
		}
		return daemon.ItemView{ID: model.ItemID(id), State: model.StateGranted}, nil
	})
	var out bytes.Buffer
	if err := cmdExtend(underOpenCode(t, h, &out), []string{id, "--ttl", "1h"}); err != nil {
		t.Fatal(err)
	}
	if want := "hint: to get the next expiry warning, run in background: aq wait " + id + " --until expiring"; !strings.Contains(out.String(), want) {
		t.Fatal(out.String())
	}
}

// The hook records the client of the shell's own variable, not the bare cli.
func TestHookFromOpenCode(t *testing.T) {
	h := newHarness(t)
	feedStdin(t, `{"tool_name":"Bash","tool_input":{"command":"docker run ubuntu"}}`+"\n")
	if err := cmdHook(underOpenCode(t, h, &bytes.Buffer{}), []string{"pretooluse"}); err != nil {
		t.Fatal(err)
	}
	hits, err := hook.ReadHits(h.paths.StateDir, time.Time{})
	if err != nil || len(hits) != 1 || hits[0].Client != "opencode" {
		t.Fatal(hits, err)
	}
}

// From an OpenCode tool shell list --mine asks for the session of the shell's variable.
func TestListMineFromOpenCode(t *testing.T) {
	h := newHarness(t)
	stubInsight(t, h, "list", `{"all":false,"session_id":"ses_test"}`, json.RawMessage("[]"), nil)
	var out bytes.Buffer
	if err := cmdList(underOpenCode(t, h, &out), []string{"--mine"}); err != nil { // the stub fails the test on other arguments
		t.Fatal(err)
	}
}

// aq mcp hands its tools the ancestor lookup: a call without _meta sends the session of the
// shell's variable instead of refusing an unknown identity.
func TestMCPCommandFromOpenCode(t *testing.T) {
	var sent client.Identity
	h := socketHarness(t, func(_ context.Context, op string, raw json.RawMessage, _ protocol.StreamWriter) (any, error) {
		var a struct {
			ClientType string `json:"client_type"`
			SessionID  string `json:"session_id"`
		}
		if e := json.Unmarshal(raw, &a); e != nil {
			t.Error(e)
		}
		if op != "submit" {
			t.Errorf("op %s", op)
		}
		sent = client.Identity{ClientType: a.ClientType, SessionID: a.SessionID}
		return daemon.ItemView{ID: "i"}, nil
	})
	req, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{
		"name": "queue_submit", "arguments": map[string]any{"cwd": h.cwd, "argv": []string{"true"}, "memory": "64M", "timeout": "1m"}}})
	feedStdin(t, string(req)+"\n")
	var out bytes.Buffer
	if err := cmdMCP(underOpenCode(t, h, &out), nil); err != nil {
		t.Fatal(err)
	}
	if sent != (client.Identity{ClientType: "opencode", SessionID: "ses_test"}) || strings.Contains(out.String(), `"isError":true`) {
		t.Fatal(sent, out.String())
	}
}
