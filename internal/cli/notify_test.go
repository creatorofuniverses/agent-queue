package cli

import (
	"context"
	"encoding/json"
	"github.com/creatorofuniverses/agent-queue/internal/daemon"
	"github.com/creatorofuniverses/agent-queue/internal/model"
	"github.com/creatorofuniverses/agent-queue/internal/protocol"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func socketHarness(t *testing.T, f protocol.Handler) *harness {
	t.Helper()
	h := newHarness(t)
	os.MkdirAll(h.paths.RuntimeDir, 0700)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- protocol.Serve(ctx, h.paths.Socket(), f) }()
	t.Cleanup(func() { cancel(); <-done })
	deadline := time.Now().Add(time.Second)
	for {
		if _, e := os.Stat(h.paths.Socket()); e == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no socket")
		}
		time.Sleep(time.Millisecond)
	}
	return h
}
func TestSubmitSendsIdentityAndNoNotify(t *testing.T) {
	h := socketHarness(t, func(ctx context.Context, op string, raw json.RawMessage, w protocol.StreamWriter) (any, error) {
		var a daemon.SubmitArgs
		json.Unmarshal(raw, &a)
		if op != "submit" || a.ClientType != "claude" || a.SessionID != "c" || !a.NoNotify {
			t.Error(op, a)
		}
		return daemon.ItemView{ID: "i"}, nil
	})
	h.vars["CLAUDE_CODE_SESSION_ID"] = "c"
	for _, j := range []bool{false, true} {
		args := []string{"submit", "--memory", "64M", "--timeout", "1m", "--no-notify"}
		if j {
			args = append(args, "--json")
		}
		args = append(args, "--", "true")
		code, _, err := h.run(args...)
		if code != 0 || strings.Contains(err, "hint: to be woken") == j {
			t.Fatal(code, err)
		}
	}
}
func TestReserveSendsIdentity(t *testing.T) {
	h := socketHarness(t, func(ctx context.Context, op string, raw json.RawMessage, w protocol.StreamWriter) (any, error) {
		var a daemon.ReserveArgs
		json.Unmarshal(raw, &a)
		if op != "reserve" || a.ClientType != "codex" || a.SessionID != "t" {
			t.Error(op, a)
		}
		return daemon.ItemView{ID: "r", State: model.StateGranted}, nil
	})
	h.vars["CODEX_THREAD_ID"] = "t"
	p := filepath.Join(h.cwd, "teardown")
	os.WriteFile(p, []byte("true"), 0600)
	code, _, err := h.run("reserve", "--memory", "64M", "--ttl", "1h", "--teardown", p)
	if code != 0 {
		t.Fatal(code, err)
	}
}
func TestSubmitAmbiguousIdentity(t *testing.T) {
	h := socketHarness(t, func(ctx context.Context, op string, raw json.RawMessage, w protocol.StreamWriter) (any, error) {
		var a daemon.SubmitArgs
		json.Unmarshal(raw, &a)
		if a.ClientType != "claude" || a.SessionID != "mine" {
			t.Error(a)
		}
		return daemon.ItemView{ID: "i"}, nil
	})
	h.vars["CODEX_THREAD_ID"] = "parent"
	h.vars["CLAUDE_CODE_SESSION_ID"] = "mine"
	code, _, err := h.run("submit", "--memory", "64M", "--timeout", "1m", "--", "true")
	if code != 64 || !strings.Contains(err, "--client NAME") {
		t.Fatal(code, err)
	}
	code, _, err = h.run("submit", "--memory", "64M", "--timeout", "1m", "--client", "claude", "--", "true")
	if code != 0 {
		t.Fatal(code, err)
	}
}
func TestListMineShowsEvents(t *testing.T) {
	h := socketHarness(t, func(ctx context.Context, op string, raw json.RawMessage, w protocol.StreamWriter) (any, error) {
		var a map[string]any
		json.Unmarshal(raw, &a)
		if a["session_id"] != "c" {
			t.Error(a)
		}
		return []daemon.ItemView{{ID: "i", Events: []daemon.EventView{{ID: "e", Kind: model.EventTaskFinished, Status: model.EventSent}}}}, nil
	})
	code, _, err := h.run("list", "--mine")
	if code != 64 || !strings.Contains(err, "--mine needs a session") {
		t.Fatal(code, err)
	}
	h.vars["CLAUDE_CODE_SESSION_ID"] = "c"
	code, out, err := h.run("list", "--mine")
	if code != 0 || !strings.Contains(out, "EVENT") || !strings.Contains(out, "task_finished") {
		t.Fatal(code, out, err)
	}
	h.vars["CODEX_THREAD_ID"] = "parent" // an explicit session needs no detection
	if code, _, err = h.run("list", "--mine", "--session", "c"); code != 0 {
		t.Fatal(code, err)
	}
}
func TestWaitUntilExpiringAndEventIDs(t *testing.T) {
	h := socketHarness(t, func(ctx context.Context, op string, raw json.RawMessage, w protocol.StreamWriter) (any, error) {
		var a map[string]any
		json.Unmarshal(raw, &a)
		if op == "wait" && a["until"] != "expiring" {
			t.Error(a)
		}
		return daemon.ItemView{ID: "r", Kind: model.KindReservation, State: model.StateExpiring, Events: []daemon.EventView{{ID: "e", Kind: model.EventReservationExpiring}}}, nil
	})
	code, out, err := h.run("wait", "r", "--until", "expiring")
	if code != 0 || !strings.Contains(out, "event=e:reservation_expiring") || !strings.Contains(err, "after aq extend, re-run in background: aq wait r --until expiring") {
		t.Fatal(code, out, err)
	}
}
func TestAckCommand(t *testing.T) {
	h := socketHarness(t, func(ctx context.Context, op string, raw json.RawMessage, w protocol.StreamWriter) (any, error) {
		var a map[string]any
		json.Unmarshal(raw, &a)
		if op != "ack" || a["event_id"] != "E1" {
			t.Error(op, a)
		}
		return nil, &protocol.Error{Code: protocol.CodeNotFound, Message: "absent"}
	})
	code, _, _ := h.run("ack", "E1")
	if code != 1 {
		t.Fatal(code)
	}
}
func TestDoctorAdapterUnavailable(t *testing.T) {
	checks := notificationChecks([]daemon.AdapterStatus{{Name: "codex", Delivery: "push", OK: false, Detail: "codex not found in daemon PATH"}}, daemon.WebhookStatus{})
	if len(checks) != 2 || checks[0].OK || checks[0].Code != "ADAPTER_UNAVAILABLE" {
		t.Fatal(checks)
	}
}

func TestDoctorAdapterUnavailableOutput(t *testing.T) {
	h := newHarness(t)
	h.vars["XDG_RUNTIME_DIR"] = "/run/user/1000"
	old := newDoctorSystem
	newDoctorSystem = func() doctorSystem {
		s := newFakeSystem().system()
		s.PingDetails = func(context.Context, string) ([]daemon.AdapterStatus, daemon.WebhookStatus, error) {
			return []daemon.AdapterStatus{{Name: "codex", Delivery: "push", Detail: "codex not found in daemon PATH"}}, daemon.WebhookStatus{}, nil
		}
		return s
	}
	t.Cleanup(func() { newDoctorSystem = old })
	code, out, err := h.run("doctor")
	if code != 1 || !strings.Contains(out, "FAIL") || !strings.Contains(out, "ADAPTER_UNAVAILABLE") {
		t.Fatal(code, out, err)
	}
}

// codexSubagentHome writes the rollout of a Codex multi-agent v2 sub-agent "sub" spawned by
// thread "root", the way Codex records it under $CODEX_HOME/sessions.
func codexSubagentHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, "sessions", "2026", "11", "06")
	os.MkdirAll(dir, 0o755)
	meta := `{"type":"session_meta","payload":{"id":"sub","session_id":"root","parent_thread_id":"root","thread_source":"subagent","multi_agent_version":"v2"}}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "rollout-2026-11-06T17-09-19-sub.jsonl"), []byte(meta), 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

// A Codex sub-agent's own thread refuses queued messages; its stands notify the root thread
// and its listings find them there.
func TestCodexSubagentFilesUnderRootThread(t *testing.T) {
	var sessions []string
	h := socketHarness(t, func(ctx context.Context, op string, raw json.RawMessage, w protocol.StreamWriter) (any, error) {
		var a map[string]any
		json.Unmarshal(raw, &a)
		sessions = append(sessions, op+":"+a["session_id"].(string))
		if op == "list" {
			return []daemon.ItemView{}, nil
		}
		return daemon.ItemView{ID: "r", State: model.StateGranted}, nil
	})
	h.vars["CODEX_HOME"] = codexSubagentHome(t)
	h.vars["CODEX_THREAD_ID"] = "sub"
	p := filepath.Join(h.cwd, "teardown")
	os.WriteFile(p, []byte("true"), 0600)
	for _, args := range [][]string{
		{"reserve", "--client", "codex", "--session", "sub", "--memory", "64M", "--ttl", "1h", "--teardown", p},
		{"list", "--session", "sub"},
		{"list", "--mine"},
	} {
		if code, _, err := h.run(args...); code != 0 {
			t.Fatal(args, code, err)
		}
	}
	if want := []string{"reserve:root", "list:root", "list:root"}; strings.Join(sessions, " ") != strings.Join(want, " ") {
		t.Fatal(sessions)
	}
}
