package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/creatorofuniverses/agent-queue/internal/client"
	"github.com/creatorofuniverses/agent-queue/internal/config"
	"github.com/creatorofuniverses/agent-queue/internal/daemon"
	"github.com/creatorofuniverses/agent-queue/internal/model"
	"github.com/creatorofuniverses/agent-queue/internal/protocol"
	"os"
	"path/filepath"
	"time"
)

type toolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

const wakeDescription = "MCP calls cannot wake an idle session: after submitting, Claude Code runs aq wait <id> as a background command; Codex is pinged by the daemon via codex queue."

func toolDefinitions() []toolDef {
	str := func() map[string]any { return map[string]any{"type": "string"} }
	obj := func(fields []string, required []string) map[string]any {
		p := map[string]any{}
		for _, n := range fields {
			p[n] = str()
		}
		return map[string]any{"type": "object", "properties": p, "required": required, "additionalProperties": false}
	}
	submit := obj([]string{"cwd", "memory", "expected", "timeout", "priority", "profile", "name", "key", "within", "project", "client", "session_id"}, []string{"cwd", "argv", "timeout"})
	reserve := obj([]string{"cwd", "memory", "ttl", "teardown_file", "task_memory", "docker_project", "priority", "name", "key", "project", "client", "session_id"}, []string{"cwd", "memory", "ttl", "teardown_file"})
	for _, s := range []map[string]any{submit, reserve} {
		p := s["properties"].(map[string]any)
		p["cpus"] = map[string]any{"type": "integer"}
		p["env"] = map[string]any{"type": "array", "items": str()}
		p["no_notify"] = map[string]any{"type": "boolean"}
		p["session_id"].(map[string]any)["description"] = "Defaults to CODEX_THREAD_ID / CLAUDE_CODE_SESSION_ID of the aq mcp process if set; pass it explicitly if your client does not propagate it"
	}
	submit["properties"].(map[string]any)["argv"] = map[string]any{"type": "array", "items": str()}
	reserve["properties"].(map[string]any)["task_cpus"] = map[string]any{"type": "integer"}
	release := obj([]string{"id"}, []string{"id"})
	release["properties"].(map[string]any)["force"] = map[string]any{"type": "boolean"}
	list := obj([]string{"session_id"}, []string{})
	for _, f := range []string{"mine", "all"} {
		list["properties"].(map[string]any)[f] = map[string]any{"type": "boolean"}
	}
	logs := obj([]string{"id", "stream"}, []string{"id"})
	logs["properties"].(map[string]any)["stream"] = map[string]any{"type": "string", "enum": []string{"stdout", "stderr"}}
	logs["properties"].(map[string]any)["tail"] = map[string]any{"type": "integer", "minimum": 1, "maximum": 2000, "default": 200}
	estimate := obj([]string{"memory", "expected", "timeout", "priority", "profile", "name", "cwd", "project"}, []string{})
	estimate["properties"].(map[string]any)["cpus"] = map[string]any{"type": "integer"}
	return []toolDef{
		{"queue_estimate", "Forecast admission without submitting. Pass the same cwd you will submit with: a profile derived from name uses cwd's repository (default: the MCP server's directory).", estimate},
		{"queue_submit", "Queue a command with memory and time limits. " + wakeDescription, submit},
		{"queue_reserve", "Reserve a stand and task budget with a teardown script. " + wakeDescription, reserve},
		{"queue_extend", "Extend a granted reservation.", obj([]string{"id", "ttl"}, []string{"id", "ttl"})},
		{"queue_release", "Release a stand after shutting it down; force releases failed teardown accounting.", release},
		{"queue_cancel", "Cancel a task or queued reservation.", obj([]string{"id", "reason"}, []string{"id"})},
		{"queue_status", "Get current item state.", obj([]string{"id"}, []string{"id"})},
		{"queue_result", "Get result and log paths.", obj([]string{"id"}, []string{"id"})},
		{"queue_list", "List items and unacknowledged session events.", list},
		{"queue_ack", "Acknowledge an event after recording its result.", obj([]string{"event_id"}, []string{"event_id"})},
		{"queue_logs", "Get a bounded log tail without following.", logs},
	}
}

type toolArgs struct {
	Profile       string   `json:"profile"`
	Cwd           string   `json:"cwd"`
	Argv          []string `json:"argv"`
	Memory        string   `json:"memory"`
	Expected      string   `json:"expected"`
	Timeout       string   `json:"timeout"`
	CPUs          *int     `json:"cpus"`
	Priority      string   `json:"priority"`
	Name          string   `json:"name"`
	Key           string   `json:"key"`
	Within        string   `json:"within"`
	Project       string   `json:"project"`
	Env           []string `json:"env"`
	NoNotify      bool     `json:"no_notify"`
	SessionID     string   `json:"session_id"`
	Client        string   `json:"client"`
	TTL           string   `json:"ttl"`
	TeardownFile  string   `json:"teardown_file"`
	TaskMemory    string   `json:"task_memory"`
	TaskCPUs      int      `json:"task_cpus"`
	DockerProject string   `json:"docker_project"`
	ID            string   `json:"id"`
	EventID       string   `json:"event_id"`
	Force         bool     `json:"force"`
	Reason        string   `json:"reason"`
	Mine          bool     `json:"mine"`
	All           bool     `json:"all"`
	Stream        string   `json:"stream"`
	Tail          *int     `json:"tail"`
}

func validateArgs(name string, raw json.RawMessage) error {
	var def *toolDef
	for _, d := range toolDefinitions() {
		if d.Name == name {
			def = &d
			break
		}
	}
	if def == nil {
		return fmt.Errorf("unknown tool %s", name)
	}
	var values map[string]json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil || values == nil {
		return fmt.Errorf("arguments must be an object")
	}
	for _, k := range def.InputSchema["required"].([]string) {
		if _, ok := values[k]; !ok {
			return fmt.Errorf("%s is required", k)
		}
	}
	props := def.InputSchema["properties"].(map[string]any)
	for k, v := range values {
		schema, ok := props[k]
		if !ok {
			return fmt.Errorf("unknown argument %s", k)
		}
		var decoded any
		if err := json.Unmarshal(v, &decoded); err != nil {
			return err
		}
		typ := schema.(map[string]any)["type"]
		valid := false
		switch typ {
		case "string":
			_, valid = decoded.(string)
		case "boolean":
			_, valid = decoded.(bool)
		case "integer":
			n, ok := decoded.(float64)
			valid = ok && n == float64(int(n))
		case "array":
			a, ok := decoded.([]any)
			valid = ok
			if ok {
				for _, x := range a {
					if _, ok := x.(string); !ok {
						valid = false
					}
				}
			}
		}
		if !valid {
			return fmt.Errorf("%s must be %s", k, typ)
		}
	}
	return nil
}

type toolResult struct {
	Content           []textContent `json:"content"`
	StructuredContent any           `json:"structuredContent,omitempty"`
	IsError           bool          `json:"isError,omitempty"`
}
type textContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func toolError(err error) toolResult {
	var pe *protocol.Error
	s := "BAD_REQUEST: " + err.Error()
	switch {
	case errors.As(err, &pe):
		s = pe.Code + ": " + pe.Message
	case errors.Is(err, protocol.ErrDaemonUnavailable):
		s = "DAEMON_UNAVAILABLE: " + err.Error()
	}
	return toolResult{Content: []textContent{{"text", s}}, IsError: true}
}

// logsMaxBytes bounds queue_logs: one progress-bar "line" can be megabytes of agent context.
const logsMaxBytes = 256 << 10

const identityUnknown = "session identity unknown: pass client and session_id (CLAUDE_CODE_SESSION_ID / CODEX_THREAD_ID of your session), or no_notify=true"

func callTool(ctx context.Context, call Caller, agents []config.AgentConfig, env func(string) string, name string, raw json.RawMessage) toolResult {
	if len(raw) == 0 {
		raw = json.RawMessage("{}")
	}
	if err := validateArgs(name, raw); err != nil {
		return toolError(err)
	}
	var a toolArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return toolError(err)
	}
	var args any
	op := ""
	switch name {
	case "queue_estimate":
		cpus := 1
		if a.CPUs != nil {
			cpus = *a.CPUs
		}
		cwd := a.Cwd
		if cwd == "" {
			cwd, _ = os.Getwd()
		}
		v, e := client.BuildEstimate(ctx, client.EstimateOptions{Cwd: cwd, Memory: a.Memory, Expected: a.Expected, Timeout: a.Timeout, Priority: a.Priority, Profile: a.Profile, Name: a.Name, Project: a.Project, CPUs: cpus})
		if e != nil {
			return toolError(e)
		}
		args = v
		op = "estimate"
	case "queue_submit", "queue_reserve":
		id, err := client.DetectIdentity(agents, env, a.Client, a.SessionID)
		if errors.Is(err, client.ErrAmbiguousIdentity) {
			id, err = client.Identity{ClientType: "cli", SessionID: a.SessionID}, nil
		}
		if err != nil {
			return toolError(err)
		}
		// §9.5: the identity actually submitted must reach a session; no silent cli in MCP.
		if !a.NoNotify && (id.SessionID == "" || id.ClientType == "cli") {
			return toolError(errors.New(identityUnknown))
		}
		cpus := 0
		if name == "queue_submit" {
			cpus = 1
		}
		if a.CPUs != nil {
			cpus = *a.CPUs
		}
		if name == "queue_submit" {
			op = "submit"
			args, err = client.BuildSubmit(ctx, client.SubmitOptions{Profile: a.Profile, Cwd: a.Cwd, Argv: a.Argv, Memory: a.Memory, Expected: a.Expected, Timeout: a.Timeout, CPUs: cpus, Priority: a.Priority, Name: a.Name, Key: a.Key, Within: a.Within, Project: a.Project, Env: a.Env, NoNotify: a.NoNotify, Identity: id}, env)
		} else {
			op = "reserve"
			if !filepath.IsAbs(a.TeardownFile) {
				return toolError(errors.New("teardown_file must be absolute"))
			}
			args, err = client.BuildReserve(ctx, client.ReserveOptions{Cwd: a.Cwd, Memory: a.Memory, TTL: a.TTL, CPUs: cpus, TaskMemory: a.TaskMemory, TaskCPUs: a.TaskCPUs, TeardownFile: a.TeardownFile, DockerProject: a.DockerProject, Priority: a.Priority, Name: a.Name, Key: a.Key, Project: a.Project, Env: a.Env, NoNotify: a.NoNotify, Identity: id}, env)
		}
		if err != nil {
			return toolError(err)
		}
	case "queue_extend":
		op = "extend"
		d, err := time.ParseDuration(a.TTL)
		if err != nil || d < time.Second {
			return toolError(errors.New("extend: --ttl is required and must be a duration of at least 1s"))
		}
		args = map[string]any{"id": a.ID, "ttl_seconds": int64(d / time.Second)}
	case "queue_release":
		op = "release"
		args = map[string]any{"id": a.ID, "force": a.Force}
	case "queue_cancel":
		op = "cancel"
		args = map[string]string{"id": a.ID, "reason": a.Reason}
	case "queue_status":
		op = "status"
		args = map[string]string{"id": a.ID}
	case "queue_result":
		op = "result"
		args = map[string]string{"id": a.ID}
	case "queue_ack":
		op = "ack"
		args = map[string]string{"event_id": a.EventID}
	case "queue_list":
		op = "list"
		if a.Mine && a.SessionID == "" {
			id, err := client.DetectIdentity(agents, env, "", "")
			if err != nil {
				return toolError(err)
			}
			if id.SessionID == "" {
				return toolError(errors.New("list: --mine needs a session (CODEX_THREAD_ID, CLAUDE_CODE_SESSION_ID or --session)"))
			}
			a.SessionID = id.SessionID
		}
		args = map[string]any{"session_id": a.SessionID, "all": a.All}
	case "queue_logs":
		op = "logs"
		n := 200
		if a.Tail != nil {
			n = *a.Tail
		}
		if n < 1 || n > 2000 {
			return toolError(errors.New("tail must be between 1 and 2000"))
		}
		if a.Stream != "" && a.Stream != "stdout" && a.Stream != "stderr" {
			return toolError(errors.New("stream must be stdout or stderr"))
		}
		args = daemon.LogsArgs{ID: model.ItemID(a.ID), Stream: a.Stream, Tail: n, Follow: false, MaxBytes: logsMaxBytes}
	}
	var result any
	if err := call(ctx, op, args, &result); err != nil {
		return toolError(err)
	}
	if list, ok := result.([]any); ok {
		result = map[string]any{"items": list}
	}
	if result == nil {
		result = map[string]any{}
	}
	b, err := json.Marshal(result)
	if err != nil {
		return toolError(err)
	}
	// Normalize to the JSON object used in both content forms.
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var structured any
	if err = dec.Decode(&structured); err != nil {
		return toolError(err)
	}
	return toolResult{Content: []textContent{{"text", string(b)}}, StructuredContent: structured}
}
