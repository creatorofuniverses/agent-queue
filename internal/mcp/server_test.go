package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"resource-dispatcher/internal/client"
	"resource-dispatcher/internal/config"
	"resource-dispatcher/internal/daemon"
	"resource-dispatcher/internal/protocol"
	"strings"
	"testing"
	"time"
)

func serveText(t *testing.T, input string, call Caller, vars map[string]string) []map[string]any {
	t.Helper()
	var b bytes.Buffer
	if call == nil {
		call = func(context.Context, string, any, any) error { t.Error("unexpected call"); return nil }
	}
	if e := Serve(context.Background(), strings.NewReader(input), &b, call, config.Default().Agents, func(k string) string { return vars[k] }, "test"); e != nil {
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
	want := []string{"queue_submit", "queue_reserve", "queue_extend", "queue_release", "queue_cancel", "queue_status", "queue_result", "queue_list", "queue_ack", "queue_logs"}
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
		done <- Serve(ctx, r, io.Discard, nil, config.Default().Agents, func(string) string { return "" }, "t")
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stdin read ignored cancellation")
	}
}
