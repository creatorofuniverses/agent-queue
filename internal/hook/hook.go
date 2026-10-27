// Package hook implements fail-open heavy-command reminders, without daemon I/O.
package hook

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

func Compile(patterns []string) ([]*regexp.Regexp, error) {
	out := make([]*regexp.Regexp, 0, len(patterns))
	for _, p := range patterns {
		r, e := regexp.Compile(p)
		if e != nil {
			return nil, e
		}
		out = append(out, r)
	}
	return out, nil
}
func command(v json.RawMessage) string {
	var s string
	if json.Unmarshal(v, &s) == nil {
		return s
	}
	var a []string
	if json.Unmarshal(v, &a) == nil {
		return strings.Join(a, " ")
	}
	return ""
}
func PreToolUse(in io.Reader, out io.Writer, patterns []*regexp.Regexp, record func(int)) {
	b, e := io.ReadAll(io.LimitReader(in, (1<<20)+1))
	if e != nil || len(b) > 1<<20 {
		return
	}
	var v struct {
		ToolInput struct {
			Command json.RawMessage `json:"command"`
			Cmd     json.RawMessage `json:"cmd"`
		} `json:"tool_input"`
	}
	if json.Unmarshal(b, &v) != nil {
		return
	}
	cmd := command(v.ToolInput.Command)
	if cmd == "" {
		cmd = command(v.ToolInput.Cmd)
	}
	words := strings.Fields(cmd)
	if len(words) == 0 {
		return
	}
	if words[0] == "aq" || strings.HasSuffix(words[0], "/aq") {
		return
	}
	for i, p := range patterns {
		if p.MatchString(cmd) {
			text := fmt.Sprintf("aq: this command looks heavy (matched pattern %d). On this host run heavy commands through aq so they wait for memory instead of hanging the machine: aq submit --memory <M> --timeout <T> -- <command>; for containers use aq reserve and aq submit --within. Ignore this if the command is light. See the aq skill.", i+1)
			result := map[string]any{"hookSpecificOutput": map[string]string{"hookEventName": "PreToolUse", "additionalContext": text}}
			if json.NewEncoder(out).Encode(result) == nil && record != nil {
				record(i)
			}
			return
		}
	}
}
func AppendHit(stateDir string, at time.Time, idx int, client string) error {
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return err
	}
	p := filepath.Join(stateDir, "hook-hits.jsonl")
	if st, err := os.Stat(p); err == nil && st.Size() > 1<<20 {
		if err = os.Rename(p, p+".1"); err != nil {
			return err
		}
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err = f.Chmod(0600); err != nil {
		return err
	}
	b, err := json.Marshal(struct {
		At      string `json:"at"`
		Pattern int    `json:"pattern"`
		Client  string `json:"client"`
	}{at.Format(time.RFC3339), idx, client})
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	return err
}
