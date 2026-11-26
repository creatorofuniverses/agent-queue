package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/creatorofuniverses/agent-queue/internal/agentsess"
	"github.com/creatorofuniverses/agent-queue/internal/client"
	"github.com/creatorofuniverses/agent-queue/internal/config"
	"github.com/creatorofuniverses/agent-queue/internal/daemon"
	"github.com/creatorofuniverses/agent-queue/internal/protocol"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func serveText(t *testing.T, input string, call Caller, vars map[string]string) []map[string]any {
	t.Helper()
	return serveParent(t, client.Parent{}, input, call, vars)
}

// serveParent is serveText under the ancestor lookup of a fake process tree.
func serveParent(t *testing.T, parent client.Parent, input string, call Caller, vars map[string]string) []map[string]any {
	t.Helper()
	return serve(t, parent, config.Default().Agents, input, call, vars)
}

// serveAgents is serveText with an explicit adapter list.
func serveAgents(t *testing.T, agents []config.AgentConfig, input string, call Caller, vars map[string]string) []map[string]any {
	t.Helper()
	return serve(t, client.Parent{}, agents, input, call, vars)
}

// serve answers the requests with the given ancestor lookup and adapters, and decodes
// the responses.
func serve(t *testing.T, parent client.Parent, agents []config.AgentConfig, input string, call Caller, vars map[string]string) []map[string]any {
	t.Helper()
	var b bytes.Buffer
	if call == nil {
		call = func(context.Context, string, any, any) error { t.Error("unexpected call"); return nil }
	}
	if e := Serve(context.Background(), strings.NewReader(input), &b, call, parent, agents, func(k string) string { return vars[k] }, "test"); e != nil {
		t.Fatal(e)
	}
	var out []map[string]any
	dec := json.NewDecoder(&b)
	for {
		var v map[string]any
		e := dec.Decode(&v)
		if e == io.EOF {
			break
		}
		if e != nil {
			t.Fatal(e)
		}
		out = append(out, v)
	}
	return out
}
func req(method string, params any) string {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	return string(b) + "\n"
}
func tool(name string, args any) string {
	return req("tools/call", map[string]any{"name": name, "arguments": args})
}

// toolMeta is a tools/call request with _meta, the metadata OpenCode sends with a call.
func toolMeta(name string, args, meta map[string]any) string {
	return req("tools/call", map[string]any{"name": name, "arguments": args, "_meta": meta})
}

// sessionMeta is the _meta of a call OpenCode makes on behalf of a session.
func sessionMeta(session any) map[string]any {
	return map[string]any{"ai.opencode/sessionID": session}
}
func TestInitializeNegotiates(t *testing.T) {
	for _, v := range []string{"2025-03-26", "1999-01-01"} {
		out := serveText(t, req("initialize", map[string]string{"protocolVersion": v}), nil, nil)
		r := out[0]["result"].(map[string]any)
		want := v
		if v == "1999-01-01" {
			want = "2025-06-18"
		}
		if r["protocolVersion"] != want || r["serverInfo"].(map[string]any)["name"] != "aq" || r["capabilities"].(map[string]any)["tools"] == nil {
			t.Fatal(r)
		}
	}
}
func TestToolsListNames(t *testing.T) {
	out := serveText(t, req("tools/list", nil), nil, nil)
	list := out[0]["result"].(map[string]any)["tools"].([]any)
	names := map[string]bool{}
	for _, x := range list {
		v := x.(map[string]any)
		names[v["name"].(string)] = true
		if v["inputSchema"].(map[string]any)["type"] != "object" {
			t.Fatal(v)
		}
	}
	want := []string{"queue_submit", "queue_reserve", "queue_extend", "queue_release", "queue_cancel", "queue_status", "queue_result", "queue_list", "queue_ack", "queue_logs", "queue_estimate"}
	if len(names) != len(want) {
		t.Fatal(names)
	}
	for _, n := range want {
		if !names[n] {
			t.Fatal(n)
		}
	}
}
func TestUnknownMethod(t *testing.T) {
	out := serveText(t, req("foo/bar", nil)+`{"jsonrpc":"2.0","method":"notifications/initialized"}`+"\n", nil, nil)
	if len(out) != 1 || out[0]["error"].(map[string]any)["code"] != float64(-32601) || out[0]["id"] != float64(1) {
		t.Fatal(out)
	}
}
func submitArgs() map[string]any {
	return map[string]any{"cwd": "/tmp", "argv": []string{"true"}, "memory": "64M", "timeout": "1m"}
}
func TestSubmitToolBuildsSameArgsAsCLI(t *testing.T) {
	args := submitArgs()
	args["session_id"] = "explicit"
	env := func(k string) string {
		if k == "CODEX_THREAD_ID" {
			return "t"
		}
		return ""
	}
	want, e := client.BuildSubmit(context.Background(), client.SubmitOptions{Cwd: "/tmp", Argv: []string{"true"}, Memory: "64M", Timeout: "1m", CPUs: 1, Identity: client.Identity{ClientType: "codex", SessionID: "explicit"}}, env)
	if e != nil {
		t.Fatal(e)
	}
	called := false
	out := serveText(t, tool("queue_submit", args), func(ctx context.Context, op string, a, out any) error {
		called = true
		if op != "submit" || !reflect.DeepEqual(a, want) {
			t.Errorf("%s %+v != %+v", op, a, want)
		}
		b, _ := json.Marshal(daemon.ItemView{ID: "i"})
		return json.Unmarshal(b, out)
	}, map[string]string{"CODEX_THREAD_ID": "t"})
	if !called || out[0]["result"].(map[string]any)["isError"] == true {
		t.Fatal(out)
	}
}
func TestMCPIdentityRequired(t *testing.T) {
	// A Claude session started from a Codex shell sees both variables: same gate as no identity.
	for _, env := range []map[string]string{nil, {"CODEX_THREAD_ID": "parent", "CLAUDE_CODE_SESSION_ID": "mine"}} {
		testMCPIdentityRequired(t, env)
	}
}
func TestMCPExplicitClientNeedsItsSession(t *testing.T) {
	// The environment's Codex thread must not vouch for an explicit Claude client with no session.
	args := submitArgs()
	args["client"] = "claude"
	called := false
	out := serveText(t, tool("queue_submit", args), func(context.Context, string, any, any) error { called = true; return nil }, map[string]string{"CODEX_THREAD_ID": "parent"})
	r := out[0]["result"].(map[string]any)
	if called || r["isError"] != true || !strings.Contains(r["content"].([]any)[0].(map[string]any)["text"].(string), "session identity unknown") {
		t.Fatal(called, r)
	}
}

// OpenCode names the calling session in the _meta of tools/call: the aq mcp process is a
// child of its service and has no identity of its own. The explicit arguments come first,
// then _meta, then the process; without a usable _meta nothing changes.
func TestMCPMetaSession(t *testing.T) {
	const meta = "ses_meta"
	noIdentity := "session identity unknown"
	openCodeInEnv := map[string]string{"OPENCODE_SESSION_ID": "outer"} // the process variable does not beat the call's session
	teardown := filepath.Join(t.TempDir(), "teardown")
	if err := os.WriteFile(teardown, []byte("true"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A Codex sub-agent's rollout: aq files its items under the root thread.
	home := t.TempDir()
	dir := filepath.Join(home, "sessions", "2026", "11", "06")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rollout-2026-11-06T17-09-19-sub.jsonl"), []byte(`{"type":"session_meta","payload":{"id":"sub","session_id":"root","thread_source":"subagent","multi_agent_version":"v2"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	with := func(pairs map[string]any) map[string]any {
		args := submitArgs()
		for k, v := range pairs {
			args[k] = v
		}
		return args
	}
	identity := func(name, session string) map[string]any {
		return map[string]any{"client_type": name, "session_id": session}
	}
	adapter := func(name, variable string) config.AgentConfig {
		return config.AgentConfig{Name: name, IdentityEnv: variable, Delivery: config.DeliveryClientWait}
	}
	noOpenCode := []config.AgentConfig{adapter("codex", "CODEX_THREAD_ID"), adapter("claude", "CLAUDE_CODE_SESSION_ID")}
	twoOpenCode := []config.AgentConfig{adapter("codex", "CODEX_THREAD_ID"), adapter("claude", "CLAUDE_CODE_SESSION_ID"), adapter("opencode", "OPENCODE_SESSION_ID"), adapter("opencode2", "OPENCODE_SESSION_ID")}
	for _, tc := range []struct {
		name    string
		agents  []config.AgentConfig // nil: the default adapters
		vars    map[string]string
		input   string
		refusal string         // the error carries this text, and the daemon is not called
		op      string         // the operation the daemon receives
		want    map[string]any // part of its arguments
	}{
		{"1 queue_submit without identity arguments", nil, openCodeInEnv, toolMeta("queue_submit", submitArgs(), sessionMeta(meta)), "", "submit", identity("opencode", meta)},
		{"1 queue_reserve without identity arguments", nil, openCodeInEnv, toolMeta("queue_reserve", map[string]any{"cwd": "/tmp", "memory": "64M", "ttl": "1h", "teardown_file": teardown}, sessionMeta(meta)), "", "reserve", identity("opencode", meta)},
		{"2 client of the OPENCODE_SESSION_ID adapter without a session", nil, openCodeInEnv, toolMeta("queue_submit", with(map[string]any{"client": "opencode"}), sessionMeta(meta)), "", "submit", identity("opencode", meta)},
		{"3 session without a client", nil, nil, toolMeta("queue_submit", with(map[string]any{"session_id": "S"}), sessionMeta(meta)), "", "submit", identity("opencode", "S")},
		{"4 another adapter without a session", nil, map[string]string{"CLAUDE_CODE_SESSION_ID": "outer"}, toolMeta("queue_submit", with(map[string]any{"client": "claude"}), sessionMeta(meta)), noIdentity, "", nil},
		{"4 another adapter with an explicit session", nil, map[string]string{"CLAUDE_CODE_SESSION_ID": "outer"}, toolMeta("queue_submit", with(map[string]any{"client": "claude", "session_id": "c"}), sessionMeta(meta)), "", "submit", identity("claude", "c")},
		{"4 another adapter with an explicit Codex sub-agent session", nil, map[string]string{"CODEX_HOME": home}, toolMeta("queue_submit", with(map[string]any{"client": "codex", "session_id": "sub"}), sessionMeta(meta)), "", "submit", identity("codex", "root")},
		{"4 a client no adapter has", nil, nil, toolMeta("queue_submit", with(map[string]any{"client": "kimi", "session_id": "k"}), sessionMeta(meta)), "unknown client", "", nil},
		{"5 another adapter without a session, no _meta", nil, map[string]string{"CLAUDE_CODE_SESSION_ID": "outer"}, tool("queue_submit", with(map[string]any{"client": "claude"})), "", "submit", identity("claude", "outer")},
		{"6 queue_list mine", nil, nil, toolMeta("queue_list", map[string]any{"mine": true}, sessionMeta(meta)), "", "list", map[string]any{"session_id": meta}},
		{"6 queue_list mine with a session", nil, nil, toolMeta("queue_list", map[string]any{"mine": true, "session_id": "S"}, sessionMeta(meta)), "", "list", map[string]any{"session_id": "S"}},
		{"6 queue_list without mine", nil, nil, toolMeta("queue_list", map[string]any{}, sessionMeta(meta)), "", "list", map[string]any{"session_id": ""}},
		{"7 empty _meta session", nil, nil, toolMeta("queue_submit", submitArgs(), sessionMeta("")), noIdentity, "", nil},
		{"7 129-byte _meta session", nil, nil, toolMeta("queue_submit", submitArgs(), sessionMeta(strings.Repeat("s", 129))), noIdentity, "", nil},
		{"7 _meta session with a line break", nil, nil, toolMeta("queue_submit", submitArgs(), sessionMeta("ses\nmeta")), noIdentity, "", nil},
		{"7 _meta session that is a number", nil, nil, toolMeta("queue_submit", submitArgs(), sessionMeta(42)), noIdentity, "", nil},
		{"8 no adapter with OPENCODE_SESSION_ID", noOpenCode, nil, toolMeta("queue_submit", submitArgs(), sessionMeta(meta)), noIdentity, "", nil},
		{"8 queue_list mine without OPENCODE_SESSION_ID", noOpenCode, nil, toolMeta("queue_list", map[string]any{"mine": true}, sessionMeta(meta)), "--mine needs a session", "", nil},
		{"9 two adapters with OPENCODE_SESSION_ID", twoOpenCode, nil, toolMeta("queue_submit", submitArgs(), sessionMeta(meta)), noIdentity, "", nil},
		{"9 queue_list mine with two adapters on OPENCODE_SESSION_ID", twoOpenCode, nil, toolMeta("queue_list", map[string]any{"mine": true}, sessionMeta(meta)), "--mine needs a session", "", nil},
		{"10 ambiguous environment", nil, map[string]string{"CODEX_THREAD_ID": "parent", "CLAUDE_CODE_SESSION_ID": "mine"}, toolMeta("queue_submit", submitArgs(), sessionMeta(meta)), "", "submit", identity("opencode", meta)},
		{"11 no_notify", nil, nil, toolMeta("queue_submit", with(map[string]any{"no_notify": true}), sessionMeta(meta)), "", "submit", map[string]any{"client_type": "opencode", "session_id": meta, "no_notify": true}},
		{"12 _meta with other keys only", nil, nil, toolMeta("queue_submit", submitArgs(), map[string]any{"progressToken": "p", "ai.opencode/other": "x"}), noIdentity, "", nil},
		{"13 client cli", nil, nil, toolMeta("queue_submit", with(map[string]any{"client": "cli"}), sessionMeta(meta)), noIdentity, "", nil},
		{"13 client cli with no_notify", nil, nil, toolMeta("queue_submit", with(map[string]any{"client": "cli", "no_notify": true}), sessionMeta(meta)), "", "submit", map[string]any{"client_type": "cli", "session_id": "", "no_notify": true}},
		{"14 queue_status", nil, nil, toolMeta("queue_status", map[string]any{"id": "i"}, sessionMeta(meta)), "", "status", map[string]any{"id": "i"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agents := tc.agents
			if agents == nil {
				agents = config.Default().Agents
			}
			called := false
			var op string
			var args map[string]any
			out := serveAgents(t, agents, tc.input, func(_ context.Context, o string, a, _ any) error {
				called = true
				op = o
				b, e := json.Marshal(a)
				if e != nil {
					t.Error(e)
				}
				return json.Unmarshal(b, &args)
			}, tc.vars)
			r := out[0]["result"].(map[string]any)
			if tc.refusal != "" {
				if called || r["isError"] != true || !strings.Contains(r["content"].([]any)[0].(map[string]any)["text"].(string), tc.refusal) {
					t.Fatal(called, r)
				}
				return
			}
			if !called || r["isError"] == true {
				t.Fatal(called, r)
			}
			if op != tc.op {
				t.Errorf("op %s, want %s", op, tc.op)
			}
			for k, v := range tc.want {
				if !reflect.DeepEqual(args[k], v) {
					t.Errorf("%s: %s = %v, want %v", op, k, args[k], v)
				}
			}
		})
	}
}

// A _meta session is usable up to 128 bytes, not runes; longer, empty, non-string and
// control values are none.
func TestMetaSession(t *testing.T) {
	key := `{"ai.opencode/sessionID":`
	for _, tc := range []struct{ raw, want string }{
		{key + `"ses"}`, "ses"},
		{key + `"` + strings.Repeat("s", 128) + `"}`, strings.Repeat("s", 128)},
		{key + `"` + strings.Repeat("s", 129) + `"}`, ""},
		{key + `"` + strings.Repeat("é", 64) + `"}`, strings.Repeat("é", 64)}, // 128 bytes in 64 runes
		{key + `"` + strings.Repeat("é", 65) + `"}`, ""},                      // 130 bytes in 65 runes
		{`{"ai.opencode/sessionID":"ses","progressToken":"p"}`, "ses"},
		{key + `""}`, ""},
		{key + `42}`, ""},
		{key + `null}`, ""},
		{key + `"ses\nmeta"}`, ""},
		{key + `"\u001b[31m"}`, ""},
		{key + `"\u007f"}`, ""},
		{key + `"\u0085"}`, ""},
		{`{"progressToken":"p","ai.opencode/other":"ses"}`, ""},
		{`{}`, ""},
		{``, ""},
		{`null`, ""},
		{`"ses"`, ""},
	} {
		if got := metaSession(json.RawMessage(tc.raw)); got != tc.want {
			t.Errorf("metaSession(%s) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

// fakeAgentTree is a fake /proc of aq (pid 100) started by OpenClaude (pid 101).
func fakeAgentTree(t *testing.T) string {
	t.Helper()
	proc := t.TempDir()
	write := func(path, text string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(proc, "stat"), "btime 1000000\n")
	write(filepath.Join(proc, "100", "status"), "Name:\taq\nPid:\t100\nPPid:\t101\n")
	write(filepath.Join(proc, "101", "comm"), "openclaude\n")
	write(filepath.Join(proc, "101", "cmdline"), "/usr/bin/openclaude\x00")
	write(filepath.Join(proc, "101", "status"), "Name:\topenclaude\nPid:\t101\nPPid:\t1\n")
	// Field 22 of stat is the start time: the 18 fields between it and the state are zero.
	write(filepath.Join(proc, "101", "stat"), fmt.Sprintf("101 (openclaude) S 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 50 0\n"))
	return proc
}

// ocFile is the session file OpenClaude writes for pid 101 of fakeAgentTree: no procStart.
const ocFile = `{"pid":101,"sessionId":"oc-session","status":"shell","updatedAt":1000000500,"startedAt":1000000503}`

// ocParent is the ancestor lookup over the fake tree, aq started by OpenClaude, with
// the session file content in a fake home or, with "", without one.
func ocParent(t *testing.T, content string) client.Parent {
	t.Helper()
	home := t.TempDir()
	if content != "" {
		path := filepath.Join(home, ".openclaude", "sessions", "101.json")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	proc := fakeAgentTree(t)
	return client.Parent{ProcRoot: proc, Self: 100, Names: []string{"openclaude"}, Sessions: agentsess.Reader{Sources: agentsess.Sources(home), ProcRoot: proc}}
}

// Under an agent ancestor the session comes from its session file; without one
// the identity is unknown as before.
func TestMCPParentSessionFile(t *testing.T) {
	for _, file := range []string{ocFile, ""} {
		called := false
		var sent daemon.SubmitArgs
		out := serveParent(t, ocParent(t, file), tool("queue_submit", submitArgs()), func(_ context.Context, _ string, a, _ any) error {
			called = true
			sent = a.(daemon.SubmitArgs)
			return nil
		}, nil)
		r := out[0]["result"].(map[string]any)
		if file == "" {
			if called || r["isError"] != true || !strings.Contains(r["content"].([]any)[0].(map[string]any)["text"].(string), "session identity unknown") {
				t.Fatal(called, r)
			}
			continue
		}
		if !called || r["isError"] == true || sent.ClientType != "claude" || sent.SessionID != "oc-session" {
			t.Fatal(called, sent, r)
		}
	}
}

// queue_list --mine asks for the ancestor's session when the caller gives none.
func TestMCPParentSessionList(t *testing.T) {
	called := false
	var args map[string]any
	serveParent(t, ocParent(t, ocFile), tool("queue_list", map[string]any{"mine": true}), func(_ context.Context, op string, a, out any) error {
		called = true
		if op != "list" {
			t.Errorf("op %s, want list", op)
		}
		args = a.(map[string]any)
		return json.Unmarshal([]byte(`[]`), out)
	}, nil)
	if !called || args["session_id"] != "oc-session" {
		t.Fatal(called, args)
	}
}
func testMCPIdentityRequired(t *testing.T, env map[string]string) {
	for _, tc := range []struct {
		c, s   string
		no, ok bool
	}{{}, {s: "c"}, {c: "claude", s: "c", ok: true}, {no: true, ok: true}} {
		args := submitArgs()
		if tc.c != "" {
			args["client"] = tc.c
		}
		if tc.s != "" {
			args["session_id"] = tc.s
		}
		args["no_notify"] = tc.no
		called := false
		var sent daemon.SubmitArgs
		out := serveText(t, tool("queue_submit", args), func(_ context.Context, _ string, a, _ any) error {
			called = true
			sent = a.(daemon.SubmitArgs)
			return nil
		}, env)
		r := out[0]["result"].(map[string]any)
		if called != tc.ok || (r["isError"] == true) == tc.ok {
			t.Fatal(env, tc, called, r)
		}
		if tc.c != "" && (sent.ClientType != tc.c || sent.SessionID != tc.s) {
			t.Fatal(env, tc, sent)
		}
		if !tc.ok && !strings.Contains(r["content"].([]any)[0].(map[string]any)["text"].(string), "session identity unknown") {
			t.Fatal(r)
		}
	}
}
func TestToolErrorIsError(t *testing.T) {
	args := submitArgs()
	args["no_notify"] = true
	out := serveText(t, tool("queue_submit", args), func(context.Context, string, any, any) error { return &protocol.Error{Code: "REJECTED", Message: "x"} }, nil)
	r := out[0]["result"].(map[string]any)
	if r["isError"] != true || r["content"].([]any)[0].(map[string]any)["text"] != "REJECTED: x" {
		t.Fatal(r)
	}
	args["memory"] = "oops"
	out = serveText(t, tool("queue_submit", args), nil, nil)
	if out[0]["result"].(map[string]any)["isError"] != true {
		t.Fatal(out)
	}
}
func TestListStructuredContent(t *testing.T) {
	out := serveText(t, tool("queue_list", map[string]any{}), func(ctx context.Context, op string, args, out any) error {
		return json.Unmarshal([]byte(`[{"id":"i"}]`), out)
	}, nil)
	r := out[0]["result"].(map[string]any)
	sc := r["structuredContent"].(map[string]any)
	if len(sc["items"].([]any)) != 1 {
		t.Fatal(r)
	}
	var text any
	json.Unmarshal([]byte(r["content"].([]any)[0].(map[string]any)["text"].(string)), &text)
	if !reflect.DeepEqual(sc, text) {
		t.Fatal(r)
	}
}
func TestLogsTailCapped(t *testing.T) {
	for _, n := range []int{0, 5000} {
		out := serveText(t, tool("queue_logs", map[string]any{"id": "i", "tail": n}), nil, nil)
		if out[0]["result"].(map[string]any)["isError"] != true {
			t.Fatal(out)
		}
	}
	called := false
	serveText(t, tool("queue_logs", map[string]string{"id": "i"}), func(ctx context.Context, op string, a, out any) error {
		called = true
		x := a.(daemon.LogsArgs)
		if x.Tail != 200 || x.Follow || x.MaxBytes != 256<<10 {
			t.Error(x)
		}
		return nil
	}, nil)
	if !called {
		t.Fatal("not forwarded")
	}
}
func TestDaemonUnavailableError(t *testing.T) {
	out := serveText(t, tool("queue_status", map[string]string{"id": "i"}), func(context.Context, string, any, any) error {
		return fmt.Errorf("%w: dial unix /x: connect: no such file", protocol.ErrDaemonUnavailable)
	}, nil)
	r := out[0]["result"].(map[string]any)
	if r["isError"] != true || !strings.HasPrefix(r["content"].([]any)[0].(map[string]any)["text"].(string), "DAEMON_UNAVAILABLE: ") {
		t.Fatal(r)
	}
}
func TestMalformedLine(t *testing.T) {
	out := serveText(t, "{\n"+req("ping", nil), nil, nil)
	if len(out) != 2 || out[0]["id"] != nil || out[0]["error"].(map[string]any)["code"] != float64(-32700) || out[1]["result"] == nil {
		t.Fatal(out)
	}
}
func TestServeCancelsPipe(t *testing.T) {
	r, w := io.Pipe()
	defer w.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, r, io.Discard, nil, client.Parent{}, config.Default().Agents, func(string) string { return "" }, "t")
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stdin read ignored cancellation")
	}
}

func TestMCPSubmitWithoutMemory(t *testing.T) {
	cwd := t.TempDir()
	called := false
	out := serveText(t, tool("queue_submit", map[string]any{"cwd": cwd, "name": "build", "argv": []string{"true"}, "timeout": "1m", "no_notify": true}), func(_ context.Context, op string, args, out any) error {
		r := args.(daemon.SubmitArgs)
		called = true
		if op != "submit" || r.Limit != 0 || r.Profile != cwd+":build" || !r.ProfileDerived {
			t.Fatal(r)
		}
		return nil
	}, nil)
	if !called || out[0]["result"].(map[string]any)["isError"] == true {
		t.Fatal(out)
	}
}

func TestQueueEstimate(t *testing.T) {
	out := serveText(t, tool("queue_estimate", map[string]any{"memory": "100M", "timeout": "1m"}), func(_ context.Context, op string, args, out any) error {
		if op != "estimate" {
			t.Fatal(op)
		}
		b, _ := json.Marshal(map[string]bool{"starts_now": true})
		return json.Unmarshal(b, out)
	}, nil)
	r := out[0]["result"].(map[string]any)
	if r["isError"] == true || r["structuredContent"].(map[string]any)["starts_now"] != true {
		t.Fatal(out)
	}
	out = serveText(t, tool("queue_estimate", map[string]any{}), nil, nil)
	if out[0]["result"].(map[string]any)["isError"] != true {
		t.Fatal(out)
	}
}
func TestListCodexSubagentSession(t *testing.T) {
	home := t.TempDir()
	dir := home + "/sessions/2026/11/06"
	os.MkdirAll(dir, 0o755)
	os.WriteFile(dir+"/rollout-2026-11-06T17-09-19-sub.jsonl", []byte(`{"type":"session_meta","payload":{"id":"sub","session_id":"root","thread_source":"subagent","multi_agent_version":"v2"}}`+"\n"), 0o600)
	for _, args := range []map[string]any{{"session_id": "sub"}, {"mine": true}} {
		var got any
		serveText(t, tool("queue_list", args), func(ctx context.Context, op string, a, out any) error {
			got = a.(map[string]any)["session_id"]
			return json.Unmarshal([]byte(`[]`), out)
		}, map[string]string{"CODEX_HOME": home, "CODEX_THREAD_ID": "sub"})
		if got != "root" {
			t.Fatal(args, got)
		}
	}
}
