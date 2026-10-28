package hook

import (
	"bytes"
	"encoding/json"
	"github.com/creatorofuniverses/agent-queue/internal/config"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHookMatches(t *testing.T) {
	p, e := Compile(config.Default().Hook.Patterns)
	if e != nil {
		t.Fatal(e)
	}
	for _, tc := range []struct {
		cmd string
		idx int
	}{{"pytest -n 8 tests", 0}, {"uv run pytest -n auto", 0}, {"pytest tests -q", -1}, {"docker compose -p x up -d", 1}, {"docker-compose up", 2}, {"docker run --rm alpine", 3}, {"aq submit --memory 2G --timeout 10m -- pytest -n 4", -1}, {"/home/u/.local/bin/aq submit ... -- docker run x", -1}, {"echo docker running", -1},
		// aq in any segment of a compound command, with quoted separators inside its argv
		{"cd /r && aq submit --memory 1G --timeout 1m -- pytest -n 4", -1}, {"FOO=1 aq submit --memory 1G --timeout 1m -- docker run x", -1},
		{`cd /r && aq submit --memory 4G --timeout 30m -- bash -lc 'cd x && pytest -n "$AQ_CPUS" tests'`, -1},
		{"cd /r && pytest -n 4", 0}, {"pytest -n 4; aq list", 0}, {`echo "a;b" | docker run x`, 3}} {
		var b bytes.Buffer
		raw, _ := json.Marshal(map[string]any{"tool_input": map[string]string{"command": tc.cmd}})
		hit := -1
		PreToolUse(bytes.NewReader(raw), &b, p, func(i int) { hit = i })
		if hit != tc.idx {
			t.Errorf("%s: %d != %d", tc.cmd, hit, tc.idx)
		}
	}
}
func TestHookOutputShape(t *testing.T) {
	p, _ := Compile(config.Default().Hook.Patterns)
	var b bytes.Buffer
	n := -1
	PreToolUse(strings.NewReader(`{"tool_input":{"command":"pytest -n 4"}}`), &b, p, func(i int) { n = i })
	var v struct {
		Output struct {
			Event   string `json:"hookEventName"`
			Context string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if e := json.Unmarshal(b.Bytes(), &v); e != nil || n != 0 || v.Output.Event != "PreToolUse" || !strings.Contains(v.Output.Context, "matched pattern 1") {
		t.Fatal(b.String(), n, e)
	}
}
func TestHookCommandArray(t *testing.T) {
	p, _ := Compile(config.Default().Hook.Patterns)
	for _, raw := range []string{`{"tool_input":{"command":["bash","-lc","pytest -n 4"]}}`, `{"tool_input":{"cmd":"docker run x"}}`} {
		var b bytes.Buffer
		PreToolUse(strings.NewReader(raw), &b, p, nil)
		if b.Len() == 0 {
			t.Fatal(raw)
		}
	}
}
func TestHookNeverFails(t *testing.T) {
	p, _ := Compile(config.Default().Hook.Patterns)
	for _, raw := range []string{"", "{", `{"tool_input":{}}`, "\xff\x00"} {
		var b bytes.Buffer
		PreToolUse(strings.NewReader(raw), &b, p, nil)
		if b.Len() != 0 {
			t.Fatal(b.String())
		}
	}
}
func TestAppendHitRotatesAndOmitsCommand(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hook-hits.jsonl")
	os.WriteFile(path, bytes.Repeat([]byte("x"), (1<<20)+1), 0600)
	if e := AppendHit(dir, time.Now(), 0, "codex"); e != nil {
		t.Fatal(e)
	}
	if files, e := RotatedHitFiles(dir); e != nil || len(files) != 1 {
		t.Fatal(files, e)
	}
	b, e := os.ReadFile(path)
	if e != nil || strings.Count(string(b), "\n") != 1 || strings.Contains(string(b), "command") {
		t.Fatal(string(b), e)
	}
	var v map[string]any
	json.Unmarshal(b, &v)
	if len(v) != 3 || v["client"] != "codex" || v["pattern"] != float64(0) {
		t.Fatal(v)
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0600 {
		t.Fatal(st.Mode())
	}
}
func TestDefaultPatternsCompile(t *testing.T) {
	p, e := Compile(config.Default().Hook.Patterns)
	if e != nil || len(p) != 4 {
		t.Fatal(p, e)
	}
}
