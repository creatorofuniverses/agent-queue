//go:build integration

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/creatorofuniverses/agent-queue/internal/daemon"
	"github.com/creatorofuniverses/agent-queue/internal/model"
	"github.com/creatorofuniverses/agent-queue/internal/store"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// First notification test: inherited live identities must never reach base CLI/daemon env.
func TestE2EHarnessScrubsIdentity(t *testing.T) {
	t.Setenv("CODEX_THREAD_ID", "x")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "y")
	t.Setenv("FAKE_SESSION", "z")
	h := newHarness(t, "")
	id := h.submit(mem("64M"), "true")
	v := h.status(id)
	if v.SessionID != "" || v.ClientType != "cli" {
		t.Fatal(v)
	}
	for _, kv := range h.env {
		if strings.HasPrefix(kv, "CODEX_THREAD_ID=") || strings.HasPrefix(kv, "CLAUDE_CODE_SESSION_ID=") || strings.HasPrefix(kv, "FAKE_SESSION=") {
			t.Fatal("identity leaked to base environment")
		}
	}
	if c := h.wait(id); c != 0 {
		t.Fatal(c)
	}
}
func fakeAdapter(script string) string {
	return fmt.Sprintf("\n[[agents]]\nname=\"fakecodex\"\nidentity_env=\"FAKE_SESSION\"\ndelivery=\"push\"\ncommand=[%q,\"{session}\",\"{event_id}\",\"{text}\"]\n[[agents]]\nname=\"claude\"\nidentity_env=\"CLAUDE_CODE_SESSION_ID\"\ndelivery=\"client-wait\"\n", script)
}
func notifyScript(t *testing.T, dir, body string) string {
	t.Helper()
	p := filepath.Join(dir, "send")
	if e := os.WriteFile(p, []byte("#!/bin/bash\nset -eu\n"+body), 0700); e != nil {
		t.Fatal(e)
	}
	return p
}
func (h *harness) submitEnv(vars map[string]string, argv ...string) model.ItemID {
	h.t.Helper()
	args := append([]string{"submit", "--json", "--memory", "64M", "--timeout", "1m", "--"}, argv...)
	out, err, code := h.aqEnv(vars, args...)
	if code != 0 {
		h.t.Fatal(out, err, code)
	}
	var v daemon.ItemView
	if e := json.Unmarshal([]byte(out), &v); e != nil {
		h.t.Fatal(e)
	}
	return v.ID
}
func (h *harness) sessionItems(vars map[string]string, session string) []daemon.ItemView {
	h.t.Helper()
	args := []string{"list", "--json"}
	if session == "" {
		args = append(args, "--mine")
	} else {
		args = append(args, "--session", session)
	}
	out, err, code := h.aqEnv(vars, args...)
	if code != 0 {
		h.t.Fatal(out, err, code)
	}
	var v []daemon.ItemView
	if e := json.Unmarshal([]byte(out), &v); e != nil {
		h.t.Fatal(e)
	}
	return v
}
func (h *harness) sentEvent(session string) (daemon.EventView, bool) {
	for _, v := range h.sessionItems(nil, session) {
		for _, e := range v.Events {
			if e.Kind == model.EventTaskFinished && e.Status == model.EventSent {
				return e, true
			}
		}
	}
	return daemon.EventView{}, false
}
func TestE2EExecTemplateDelivery(t *testing.T) {
	dir := t.TempDir()
	attempts := filepath.Join(dir, "attempts")
	text := filepath.Join(dir, "text")
	script := notifyScript(t, dir, fmt.Sprintf("printf '%%s %%s\\n' \"$1\" \"$2\" >> %q\nprintf '%%s' \"$3\" > %q\n", attempts, text))
	h := newHarness(t, fakeAdapter(script))
	id := h.submitEnv(map[string]string{"FAKE_SESSION": "s1"}, "true")
	var event daemon.EventView
	eventually(t, 30*time.Second, "exec delivery", func() bool { var ok bool; event, ok = h.sentEvent("s1"); return ok })
	if got := lines(t, attempts); len(got) != 1 || got[0] != "s1 "+event.ID {
		t.Fatal(got, event)
	}
	b, _ := os.ReadFile(text)
	if !strings.Contains(string(b), string(id)) || !strings.Contains(string(b), "aq ack "+event.ID) {
		t.Fatal(string(b))
	}
	if _, err, c := h.aq("ack", event.ID); c != 0 {
		t.Fatal(err)
	}
	if vs := h.sessionItems(nil, "s1"); len(vs) != 0 {
		t.Fatal(vs)
	}
}
func TestE2EDeliveryRetrySameEventID(t *testing.T) {
	dir := t.TempDir()
	attempts := filepath.Join(dir, "attempts")
	script := notifyScript(t, dir, fmt.Sprintf("printf '%%s\\n' \"$2\" >> %q\nn=$(wc -l < %q)\nif [ \"$n\" -lt 3 ]; then exit 3; fi\n", attempts, attempts))
	h := newHarness(t, fakeAdapter(script)+"\n[notify]\nretry_min=\"1s\"\nretry_max=\"2s\"\n")
	h.submitEnv(map[string]string{"FAKE_SESSION": "s1"}, "true")
	var event daemon.EventView
	eventually(t, 30*time.Second, "third delivery", func() bool { var ok bool; event, ok = h.sentEvent("s1"); return ok })
	got := lines(t, attempts)
	if len(got) != 3 {
		t.Fatal(got)
	}
	for _, s := range got {
		if s != event.ID {
			t.Fatal(got, event)
		}
	}
}
func TestE2EDeliveryAfterDaemonKill(t *testing.T) {
	dir := t.TempDir()
	attempts := filepath.Join(dir, "attempts")
	flag := filepath.Join(dir, "release")
	script := notifyScript(t, dir, fmt.Sprintf("printf '%%s\\n' \"$2\" >> %q\nwhile [ ! -e %q ]; do sleep .1; done\n", attempts, flag))
	h := newHarness(t, fakeAdapter(script))
	t.Cleanup(func() { os.WriteFile(flag, nil, 0600) })
	h.submitEnv(map[string]string{"FAKE_SESSION": "s1"}, "true")
	eventually(t, 30*time.Second, "claimed delivery", func() bool { return len(lines(t, attempts)) > 0 })
	first := lines(t, attempts)[0]
	h.killDaemon()
	os.WriteFile(flag, nil, 0600)
	h.startDaemon()
	eventually(t, 30*time.Second, "recovered delivery", func() bool { e, ok := h.sentEvent("s1"); return ok && e.ID == first })
	got := lines(t, attempts)
	if len(got) != 2 {
		t.Fatal(got)
	}
	for _, id := range got {
		if id != first {
			t.Fatal(got)
		}
	}
}
func TestE2EClaudeClientWait(t *testing.T) {
	h := newHarness(t, "")
	vars := map[string]string{"CLAUDE_CODE_SESSION_ID": "c1"}
	id := h.submitEnv(vars, "sleep", "2")
	done := make(chan struct {
		out, err string
		code     int
	}, 1)
	go func() {
		o, e, c := h.aqEnv(vars, "wait", string(id))
		done <- struct {
			out, err string
			code     int
		}{o, e, c}
	}()
	select {
	case r := <-done:
		if r.code != 0 || !strings.Contains(r.out, "event=") {
			t.Fatal(r)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("client wait did not finish")
	}
	vs := h.sessionItems(vars, "")
	if len(vs) != 1 || len(vs[0].Events) != 1 || vs[0].Events[0].Status != model.EventSent {
		t.Fatal(vs)
	}
	if _, err, c := h.aqEnv(vars, "ack", vs[0].Events[0].ID); c != 0 {
		t.Fatal(err)
	}
}
func TestE2ETwoSessionsIsolated(t *testing.T) {
	h := newHarness(t, "")
	ids := map[string]model.ItemID{}
	for _, s := range []string{"c1", "c2"} {
		vars := map[string]string{"CLAUDE_CODE_SESSION_ID": s}
		id := h.submitEnv(vars, "true")
		ids[s] = id
		if _, err, c := h.aqEnv(vars, "wait", string(id)); c != 0 {
			t.Fatal(err)
		}
	}
	for _, s := range []string{"c1", "c2"} {
		vs := h.sessionItems(map[string]string{"CLAUDE_CODE_SESSION_ID": s}, "")
		if len(vs) != 1 || vs[0].ID != ids[s] || len(vs[0].Events) != 1 {
			t.Fatal(s, vs)
		}
	}
}
func TestE2EWebhookOperatorAlert(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var v map[string]any
		if e := json.Unmarshal(b, &v); e != nil {
			t.Error(e)
		}
		if r.Header.Get("Idempotency-Key") != v["event_id"] {
			t.Error("idempotency key mismatch")
		}
		if strings.Contains(string(b), "secret-value") {
			t.Error("secret leaked")
		}
		mu.Lock()
		seen[fmt.Sprint(v["kind"])] = fmt.Sprint(v["event_id"])
		mu.Unlock()
		w.WriteHeader(204)
	}))
	t.Cleanup(server.Close)
	h := newHarness(t, fmt.Sprintf("\n[webhook]\nurl=%q\n", server.URL))
	td := writeScript(t, h.dir, "exit 3\n")
	res := h.reserve(h.dir, "--memory", "64M", "--ttl", "3s", "--teardown", td, "--env", "SECRET=secret-value")
	h.waitState(res.ID, model.StateTeardownFailed, 30*time.Second)
	eventually(t, 10*time.Second, "operator notifications", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return seen["reservation_expiring"] != "" && seen["teardown_failed"] != ""
	})
	h.itemOp("release", string(res.ID), "--force")
}
func TestE2ERealCodexQueue(t *testing.T) {
	thread := os.Getenv("AQ_E2E_CODEX_THREAD")
	if thread == "" {
		t.Skip("controller-only real Codex queue opt-in")
	}
	h := newHarness(t, "\n[[agents]]\nname=\"codex\"\nidentity_env=\"CODEX_THREAD_ID\"\ndelivery=\"push\"\ncommand=[\"codex\",\"queue\",\"--thread\",\"{session}\",\"--message\",\"{text}\"]\n")
	h.submit(append(mem("64M"), "--client", "codex", "--session", thread), "true")
	eventually(t, 30*time.Second, "real Codex delivery", func() bool { _, ok := h.sentEvent(thread); return ok })
}
func TestE2ENotificationRSS(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })
	h := newHarness(t, fmt.Sprintf("\n[webhook]\nurl=%q\ntimeout=\"60s\"\n[notify]\ndelivery_timeout=\"60s\"\n", server.URL))
	rss := func() string {
		out, e := exec.Command("ps", "-o", "rss=", "-p", fmt.Sprint(h.daemon.Process.Pid)).Output()
		if e != nil {
			t.Fatal(e)
		}
		return strings.TrimSpace(string(out))
	}
	measureDaemonCPU(t, h.daemon.Process.Pid, "idle")
	baseline := rss()
	h.killDaemon()
	st, e := store.Open(filepath.Join(h.dir, "state", "aq.db"))
	if e != nil {
		t.Fatal(e)
	}
	at := time.Now()
	var evs []model.Event
	for i := 0; i < 11; i++ {
		evs = append(evs, model.Event{ID: string(model.NewID()), Kind: model.EventAdmissionClosed, Audience: model.AudienceOperator, Channel: model.ChannelWebhook, Status: model.EventPending, CreatedAt: at})
	}
	if e = st.AddEvents(context.Background(), evs); e != nil {
		t.Fatal(e)
	}
	st.Close()
	h.startDaemon()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("webhook not entered")
	}

	st, e = store.Open(filepath.Join(h.dir, "state", "aq.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer st.Close()
	// 10 of the 11 wait behind the one in flight, plus the operator's
	// daemon_restarted event for the killed daemon.
	n, e := st.CountPending(context.Background())
	if e != nil || n != 11 {
		t.Fatal(n, e)
	}
	holder := h.submit([]string{"--memory", "64M", "--cpus", "4", "--timeout", "2m"}, "sleep", "90")
	h.waitState(holder, model.StateRunning, 10*time.Second)
	for i := 0; i < 10; i++ {
		h.submit([]string{"--memory", "64M", "--timeout", "1m"}, "true")
	}
	eventually(t, 5*time.Second, "ten queued tasks", func() bool {
		vs, e := h.list()
		if e != nil {
			return false
		}
		n := 0
		for _, v := range vs {
			if v.State == model.StateQueued {
				n++
			}
		}
		return n == 10
	})
	measureDaemonCPU(t, h.daemon.Process.Pid, "10 queued tasks + 10 pending webhook events")
	t.Logf("daemon RSS idle=%s KiB; with 10 queued tasks + 10 pending webhook events + one in flight=%s KiB", baseline, rss())
}
