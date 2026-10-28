package client

import (
	"agent-queue/internal/config"
	"errors"
	"testing"
)

func TestDetectIdentity(t *testing.T) {
	for _, tc := range []struct {
		env  map[string]string
		c, s string
		want Identity
		bad  bool
	}{
		{map[string]string{"CODEX_THREAD_ID": "t"}, "", "", Identity{"codex", "t"}, false},
		{map[string]string{"CODEX_THREAD_ID": "t", "CLAUDE_CODE_SESSION_ID": "c"}, "", "", Identity{}, true},
		{map[string]string{"CODEX_THREAD_ID": "t", "CLAUDE_CODE_SESSION_ID": "c"}, "", "s", Identity{}, true},
		{map[string]string{"CODEX_THREAD_ID": "t", "CLAUDE_CODE_SESSION_ID": "c"}, "codex", "", Identity{"codex", "t"}, false},
		{map[string]string{"CLAUDE_CODE_SESSION_ID": "c"}, "", "", Identity{"claude", "c"}, false},
		{nil, "", "", Identity{"cli", ""}, false}, {nil, "", "s", Identity{"cli", "s"}, false},
		{map[string]string{"CODEX_THREAD_ID": "t"}, "claude", "s", Identity{"claude", "s"}, false},
		{nil, "codex", "", Identity{}, true}, {nil, "zzz", "", Identity{}, true},
		{map[string]string{"CODEX_THREAD_ID": "t", "CLAUDE_CODE_SESSION_ID": "c"}, "claude", "", Identity{"claude", "c"}, false},
	} {
		got, e := DetectIdentity(config.Default().Agents, func(k string) string { return tc.env[k] }, tc.c, tc.s)
		if (e != nil) != tc.bad || (!tc.bad && got != tc.want) {
			t.Fatal(tc, got, e)
		}
	}
	_, e := DetectIdentity(config.Default().Agents, func(k string) string { return "x" }, "", "")
	var ue *UsageError
	if !errors.Is(e, ErrAmbiguousIdentity) || !errors.As(e, &ue) {
		t.Fatal(e)
	}
}
