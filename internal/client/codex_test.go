package client

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/creatorofuniverses/agent-queue/internal/config"
)

// writeRollout writes a Codex rollout whose first line is the session_meta Codex records.
func writeRollout(t *testing.T, home, day, id string, meta map[string]any) {
	t.Helper()
	dir := filepath.Join(home, "sessions", day)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	meta["id"] = id
	meta["base_instructions"] = map[string]string{"text": strings.Repeat("x", 200<<10)} // first lines are long
	line, _ := json.Marshal(map[string]any{"type": "session_meta", "payload": meta})
	body := string(line) + "\n" + `{"type":"event_msg","payload":{}}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "rollout-2026-11-06T17-09-19-"+id+".jsonl"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func subagentMeta(parent, session string) map[string]any {
	return map[string]any{
		"session_id": session, "parent_thread_id": parent, "thread_source": "subagent", "multi_agent_version": "v2",
		"source": map[string]any{"subagent": map[string]any{"thread_spawn": map[string]any{"parent_thread_id": parent, "depth": 1}}},
	}
}

func TestCodexSubagentNotifiesRootThread(t *testing.T) {
	const (
		root   = "01a1b12c-34a4-79e2-9d46-42a2e28309b3"
		sub    = "01a1b131-4886-7ca3-8ec7-7b69b601d554"
		deep   = "01a1b135-d254-7623-9f6c-d94943a6481b"
		guard  = "01a1b131-48e8-7e31-b15a-aee3dfe6d664"
		v1     = "01a1b140-0000-7000-8000-000000000001"
		orphan = "01a1b150-0000-7000-8000-000000000002"
	)
	home := t.TempDir()
	writeRollout(t, home, "2026/11/06", root, map[string]any{"session_id": root, "thread_source": "user"})
	writeRollout(t, home, "2026/11/06", sub, subagentMeta(root, root))
	// A grandchild whose session_id names its sub-agent parent still reaches the root.
	writeRollout(t, home, "2026/11/07", deep, subagentMeta(sub, sub))
	// Guardian reviews and v1 sub-agents take direct input: keep them.
	writeRollout(t, home, "2026/11/06", guard, map[string]any{"session_id": root, "parent_thread_id": root, "thread_source": "guardian_review", "multi_agent_version": "disabled"})
	writeRollout(t, home, "2026/11/06", v1, map[string]any{"session_id": root, "parent_thread_id": root, "thread_source": "subagent", "multi_agent_version": "v1"})
	// The parent's rollout is gone: it is still the only thread that can take the message.
	writeRollout(t, home, "2026/11/06", orphan, subagentMeta("01a1b1ff-0000-7000-8000-000000000003", "01a1b1ff-0000-7000-8000-000000000003"))

	agents := config.Default().Agents
	for _, tc := range []struct {
		env        map[string]string
		client, id string
		want       Identity
	}{
		{map[string]string{"CODEX_THREAD_ID": sub}, "", "", Identity{"codex", root}},
		{map[string]string{"CODEX_THREAD_ID": sub}, "codex", sub, Identity{"codex", root}}, // the skill passes --session explicitly
		{nil, "codex", deep, Identity{"codex", root}},
		{map[string]string{"CODEX_THREAD_ID": root}, "", "", Identity{"codex", root}},
		{nil, "codex", guard, Identity{"codex", guard}},
		{nil, "codex", v1, Identity{"codex", v1}},
		{nil, "codex", orphan, Identity{"codex", "01a1b1ff-0000-7000-8000-000000000003"}},
		{nil, "codex", "01a1b1ee-0000-7000-8000-000000000004", Identity{"codex", "01a1b1ee-0000-7000-8000-000000000004"}},
		{nil, "codex", "*", Identity{"codex", "*"}},
		{nil, "codex", "../" + sub, Identity{"codex", "../" + sub}},
		{map[string]string{"CLAUDE_CODE_SESSION_ID": sub}, "", "", Identity{"claude", sub}},
	} {
		env := map[string]string{"CODEX_HOME": home}
		for k, v := range tc.env {
			env[k] = v
		}
		got, err := DetectIdentity(Parent{}, agents, func(k string) string { return env[k] }, tc.client, tc.id)
		if err != nil || got != tc.want {
			t.Errorf("%v %q %q: got %v %v, want %v", tc.env, tc.client, tc.id, got, err, tc.want)
		}
	}

	// Without CODEX_HOME the default ~/.codex is read.
	env := map[string]string{"HOME": t.TempDir(), "CODEX_THREAD_ID": sub}
	if err := os.Rename(home, filepath.Join(env["HOME"], ".codex")); err != nil {
		t.Fatal(err)
	}
	if got, _ := DetectIdentity(Parent{}, agents, func(k string) string { return env[k] }, "", ""); got.SessionID != root {
		t.Errorf("default CODEX_HOME: %v", got)
	}

	// A listing by an explicit session finds what the sub-agent's submits recorded.
	envFn := func(k string) string { return env[k] }
	if got := ListSession(agents, envFn, sub); got != root {
		t.Errorf("ListSession(sub) = %q", got)
	}
	if got := ListSession(agents, envFn, "c"); got != "c" {
		t.Errorf("ListSession(c) = %q", got)
	}
	if got := ListSession(agents, envFn, ""); got != "" {
		t.Errorf("ListSession('') = %q", got)
	}
}

func TestUUIDv7Time(t *testing.T) {
	at, ok := uuidV7Time("01a1b131-4886-7ca3-8ec7-7b69b601d554")
	if !ok || !at.Equal(time.Date(2026, 11, 6, 14, 9, 19, 494e6, time.UTC)) {
		t.Fatal(at.UTC(), ok)
	}
	for _, id := range []string{"sub", "01a1b131-4886-4ca3-8ec7-7b69b601d554", "zza1b131-4886-7ca3-8ec7-7b69b601d554"} {
		if _, ok := uuidV7Time(id); ok {
			t.Error(id)
		}
	}
}
