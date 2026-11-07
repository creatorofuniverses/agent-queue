package client

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/creatorofuniverses/agent-queue/internal/config"
)

// A Codex multi-agent v2 sub-agent's shell carries its own CODEX_THREAD_ID, but Codex
// refuses `codex queue` into that thread ("direct app-server input is not allowed for
// multi-agent v2 sub-agents"), so an event addressed there is never delivered. aq records
// the thread at the top of the sub-agent tree instead: the session that spawned the work
// and can still be woken. Codex exposes the tree only in the session_meta line that opens
// each rollout under $CODEX_HOME/sessions; when the thread's own rollout cannot be read the
// id is kept as given.

const codexIdentityEnv = "CODEX_THREAD_ID"

var codexThreadID = regexp.MustCompile(`^[0-9A-Za-z-]{1,64}$`)

const (
	codexMaxHops   = 8
	codexMetaLimit = 16 << 20 // the first line embeds the base instructions
)

type codexSessionMeta struct {
	Type    string `json:"type"`
	Payload struct {
		ID                string `json:"id"`
		SessionID         string `json:"session_id"`
		ParentThreadID    string `json:"parent_thread_id"`
		ThreadSource      string `json:"thread_source"`
		MultiAgentVersion string `json:"multi_agent_version"`
	} `json:"payload"`
}

func isCodex(a config.AgentConfig) bool { return a.IdentityEnv == codexIdentityEnv }

// codexRootThread returns the nearest thread above id that accepts queued messages. A v2
// sub-agent never does, so the thread it names wins even when that thread's own rollout
// cannot be read; id is kept only when nothing marks it as such a sub-agent.
func codexRootThread(env func(string) string, id string) string {
	if !codexThreadID.MatchString(id) {
		return id
	}
	home := env("CODEX_HOME")
	if home == "" {
		if h := env("HOME"); h != "" {
			home = filepath.Join(h, ".codex")
		} else {
			return id
		}
	}
	cur := id
	for range codexMaxHops {
		m, ok := readCodexMeta(home, cur)
		if !ok {
			return cur
		}
		if m.Payload.ThreadSource != "subagent" || m.Payload.MultiAgentVersion != "v2" {
			return cur
		}
		next := m.Payload.SessionID
		if next == "" || next == cur {
			next = m.Payload.ParentThreadID
		}
		if next == cur || !codexThreadID.MatchString(next) {
			return cur
		}
		cur = next
	}
	return cur
}

func readCodexMeta(home, id string) (codexSessionMeta, bool) {
	var m codexSessionMeta
	paths := codexRollouts(home, id)
	if len(paths) == 0 {
		return m, false
	}
	f, err := os.Open(paths[0])
	if err != nil {
		return m, false
	}
	defer f.Close()
	if json.NewDecoder(io.LimitReader(f, codexMetaLimit)).Decode(&m) != nil || m.Type != "session_meta" || m.Payload.ID != id {
		return m, false
	}
	return m, true
}

// codexRollouts finds the rollout of thread id. Codex files it under the local date it was
// created, which a UUIDv7 thread id carries, so that day (and its neighbours, for a time
// zone change) is tried before walking every day of the history.
func codexRollouts(home, id string) []string {
	name := "rollout-*-" + id + ".jsonl"
	if at, ok := uuidV7Time(id); ok {
		for _, d := range []int{0, -1, 1} {
			day := at.AddDate(0, 0, d).Local().Format("2006/01/02")
			if p, _ := filepath.Glob(filepath.Join(home, "sessions", filepath.FromSlash(day), name)); len(p) > 0 {
				return p
			}
		}
	}
	p, _ := filepath.Glob(filepath.Join(home, "sessions", "*", "*", "*", name))
	return p
}

// uuidV7Time returns the creation time a version 7 UUID starts with.
func uuidV7Time(id string) (time.Time, bool) {
	h := strings.ReplaceAll(id, "-", "")
	if len(h) != 32 || h[12] != '7' {
		return time.Time{}, false
	}
	ms, err := strconv.ParseInt(h[:12], 16, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.UnixMilli(ms), true
}

// ListSession maps an explicit `list --session` id to the session aq recorded for it, so a
// Codex sub-agent listing its own thread finds the items filed under the root thread.
func ListSession(agents []config.AgentConfig, env func(string) string, session string) string {
	if session == "" {
		return ""
	}
	for _, a := range agents {
		if isCodex(a) {
			return codexRootThread(env, session)
		}
	}
	return session
}
