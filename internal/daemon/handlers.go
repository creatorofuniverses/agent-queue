package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"resource-dispatcher/internal/model"
	"resource-dispatcher/internal/protocol"
	"resource-dispatcher/internal/scheduler"
	"resource-dispatcher/internal/store"
)

// SubmitArgs is the submit request. Sizes are bytes.
type SubmitArgs struct {
	Name           string            `json:"name"`
	Argv           []string          `json:"argv"`
	Cwd            string            `json:"cwd"`
	Env            map[string]string `json:"env"`
	Limit          uint64            `json:"limit"`
	Expected       uint64            `json:"expected"`
	CPUs           int               `json:"cpus"`
	TimeoutSeconds int64             `json:"timeout_seconds"`
	Priority       string            `json:"priority"`
	SessionID      string            `json:"session_id"`
	ClientType     string            `json:"client_type"`
	Project        string            `json:"project"`
	IdemKey        string            `json:"idem_key"`
	GitRev         string            `json:"git_rev"`
	GitDirty       bool              `json:"git_dirty"`
	Within         model.ItemID      `json:"within"`
}

// ReserveArgs is the reserve request. Sizes are bytes; Teardown is the script's content.
type ReserveArgs struct {
	Name          string            `json:"name"`
	Cwd           string            `json:"cwd"`
	Env           map[string]string `json:"env"`
	Memory        uint64            `json:"memory"`
	CPUs          int               `json:"cpus"`
	TaskMemory    uint64            `json:"task_memory"`
	TaskCPUs      int               `json:"task_cpus"`
	TTLSeconds    int64             `json:"ttl_seconds"`
	Teardown      string            `json:"teardown"`
	DockerProject string            `json:"docker_project"`
	Priority      string            `json:"priority"`
	SessionID     string            `json:"session_id"`
	ClientType    string            `json:"client_type"`
	Project       string            `json:"project"`
	IdemKey       string            `json:"idem_key"`
}

type releaseArgs struct {
	ID    model.ItemID `json:"id"`
	Force bool         `json:"force"`
}

type extendArgs struct {
	ID         model.ItemID `json:"id"`
	TTLSeconds int64        `json:"ttl_seconds"`
}

type idArgs struct {
	ID model.ItemID `json:"id"`
}

type cancelArgs struct {
	ID     model.ItemID `json:"id"`
	Reason string       `json:"reason"`
}

type listArgs struct {
	SessionID string `json:"session_id"`
	All       bool   `json:"all"`
}

// LogsArgs selects a log stream; Tail 0 means from the beginning.
type LogsArgs struct {
	ID     model.ItemID `json:"id"`
	Stream string       `json:"stream"`
	Tail   int          `json:"tail"`
	Follow bool         `json:"follow"`
}

// LogLines is both the logs result and the follow stream message.
type LogLines struct {
	Lines []string `json:"lines"`
}

type pingResult struct {
	Version    string `json:"version"`
	PID        int    `json:"pid"`
	Recovering bool   `json:"recovering"`
}

const followPoll = 200 * time.Millisecond

func badRequest(format string, a ...any) error {
	return &protocol.Error{Code: protocol.CodeBadRequest, Message: fmt.Sprintf(format, a...)}
}

func decode(raw json.RawMessage, v any) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = []byte("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return badRequest("bad args: %v", err)
	}
	return nil
}

// mapErr turns domain errors into protocol errors.
func mapErr(err error) error {
	var pe *protocol.Error
	switch {
	case err == nil, errors.As(err, &pe):
		return err
	case errors.Is(err, store.ErrNotFound):
		return &protocol.Error{Code: protocol.CodeNotFound, Message: "item not found"}
	case errors.Is(err, store.ErrIdempotencyConflict):
		return &protocol.Error{Code: protocol.CodeIdempotencyConflict, Message: err.Error()}
	case errors.Is(err, scheduler.ErrRejected):
		return &protocol.Error{Code: protocol.CodeRejected, Message: err.Error()}
	case errors.Is(err, scheduler.ErrBadState):
		return &protocol.Error{Code: protocol.CodeBadState, Message: err.Error()}
	}
	return err
}

func (d *server) handle(ctx context.Context, op string, args json.RawMessage, w protocol.StreamWriter) (any, error) {
	res, err := d.dispatch(ctx, op, args, w)
	return res, mapErr(err)
}

func (d *server) dispatch(ctx context.Context, op string, args json.RawMessage, w protocol.StreamWriter) (any, error) {
	switch op {
	case "ping":
		return pingResult{Version: Version, PID: os.Getpid(), Recovering: d.recovering.Load()}, nil
	case "submit":
		return d.submit(ctx, args)
	case "reserve":
		return d.reserve(ctx, args)
	case "release":
		var a releaseArgs
		if err := decode(args, &a); err != nil {
			return nil, err
		}
		it, err := d.sched.Release(ctx, a.ID, a.Force)
		if err != nil {
			return nil, err
		}
		return d.view(it, time.Now()), nil
	case "extend":
		var a extendArgs
		if err := decode(args, &a); err != nil {
			return nil, err
		}
		it, err := d.sched.Extend(ctx, a.ID, time.Duration(a.TTLSeconds)*time.Second)
		if err != nil {
			return nil, err
		}
		return d.view(it, time.Now()), nil
	case "teardown_retry":
		var a idArgs
		if err := decode(args, &a); err != nil {
			return nil, err
		}
		it, err := d.sched.RetryTeardown(ctx, a.ID)
		if err != nil {
			return nil, err
		}
		return d.view(it, time.Now()), nil
	case "status", "result":
		var a idArgs
		if err := decode(args, &a); err != nil {
			return nil, err
		}
		it, err := d.st.Get(ctx, a.ID)
		if err != nil {
			return nil, err
		}
		return d.itemResult(op == "result", it), nil
	case "list":
		return d.list(ctx, args)
	case "cancel":
		var a cancelArgs
		if err := decode(args, &a); err != nil {
			return nil, err
		}
		it, err := d.sched.Cancel(ctx, a.ID, a.Reason)
		if err != nil {
			return nil, err
		}
		return d.view(it, time.Now()), nil
	case "wait":
		var a idArgs
		if err := decode(args, &a); err != nil {
			return nil, err
		}
		it, err := d.sched.Wait(ctx, a.ID)
		if err != nil {
			return nil, err
		}
		return d.view(it, time.Now()), nil
	case "logs":
		return d.logs(ctx, args, w)
	}
	return nil, badRequest("unknown op %q", op)
}

func (d *server) itemResult(full bool, it *model.Item) any {
	v := d.view(it, time.Now())
	if !full {
		return v
	}
	rv := ResultView{ItemView: v, LogsTruncated: it.LogsTruncated}
	if dir := d.logDir(it); dir != "" {
		rv.StdoutPath, rv.StderrPath = filepath.Join(dir, "stdout.log"), filepath.Join(dir, "stderr.log")
	}
	return rv
}

// logDir is where the item's logs live: the job directory of a task, the latest
// teardown attempt's directory of a reservation ("" before the first attempt).
func (d *server) logDir(it *model.Item) string {
	if !it.IsReservation() {
		return d.paths.JobDir(it.ID)
	}
	if it.TeardownAttempt == 0 {
		return ""
	}
	return d.paths.TeardownDir(it.ID, it.TeardownAttempt)
}

// view is viewOf plus the stand measurement of a reservation.
func (d *server) view(it *model.Item, now time.Time) ItemView {
	v := viewOf(it, now)
	if it.IsReservation() {
		if used, ok := d.sched.StandUsage(it.ID); ok {
			v.StandMeasured = &used
		}
	}
	return v
}

func (d *server) reserve(ctx context.Context, raw json.RawMessage) (any, error) {
	var a ReserveArgs
	if err := decode(raw, &a); err != nil {
		return nil, err
	}
	prio, err := model.ParsePriority(a.Priority)
	if err != nil {
		return nil, badRequest("%v", err)
	}
	it, err := d.sched.Reserve(ctx, scheduler.ReserveRequest{
		Name: a.Name, Cwd: a.Cwd, Env: a.Env, Memory: a.Memory, CPUs: a.CPUs,
		TaskMemory: a.TaskMemory, TaskCPUs: a.TaskCPUs, TTL: time.Duration(a.TTLSeconds) * time.Second,
		Teardown: []byte(a.Teardown), DockerProject: a.DockerProject, Priority: prio,
		SessionID: a.SessionID, ClientType: a.ClientType, Project: a.Project, IdemKey: a.IdemKey,
	})
	if err != nil {
		return nil, err
	}
	// Answer after the nearest finished admission pass: granted, or queued with its blocker.
	// The reservation is stored either way, so if ctx ends first it is still the answer.
	if err := d.sched.AfterNextPass(ctx); err != nil {
		ctx = context.WithoutCancel(ctx)
	}
	if it, err = d.st.Get(ctx, it.ID); err != nil {
		return nil, err
	}
	v := d.view(it, time.Now())
	if it.State == model.StateQueued {
		pos, err := d.queuePositions(ctx)
		if err != nil {
			return nil, err
		}
		d.markQueued(&v, it, pos)
	}
	return v, nil
}

// queuePositions numbers the queued items in admission order, over all sessions.
// Nested tasks start outside the queue and take no position.
func (d *server) queuePositions(ctx context.Context) (map[model.ItemID]int, error) {
	active, err := d.st.ListActive(ctx)
	if err != nil {
		return nil, err
	}
	pos := map[model.ItemID]int{}
	for _, it := range active {
		if it.State == model.StateQueued && it.ParentID == "" {
			pos[it.ID] = len(pos) + 1
		}
	}
	return pos, nil
}

func (d *server) markQueued(v *ItemView, it *model.Item, pos map[model.ItemID]int) {
	v.Position = pos[it.ID]
	if headID, dec := d.sched.HeadDecision(); it.ID == headID && !dec.Admit {
		v.BlockedBy, v.BlockedDetail = dec.Blocker, dec.Detail
	}
}

func (d *server) submit(ctx context.Context, raw json.RawMessage) (any, error) {
	var a SubmitArgs
	if err := decode(raw, &a); err != nil {
		return nil, err
	}
	prio, err := model.ParsePriority(a.Priority)
	if err != nil {
		return nil, badRequest("%v", err)
	}
	it, err := d.sched.Submit(ctx, scheduler.SubmitRequest{
		Name: a.Name, Argv: a.Argv, Cwd: a.Cwd, Env: a.Env, Limit: a.Limit, Expected: a.Expected, CPUs: a.CPUs,
		Timeout:  time.Duration(a.TimeoutSeconds) * time.Second,
		Priority: prio, SessionID: a.SessionID, ClientType: a.ClientType, Project: a.Project,
		IdemKey: a.IdemKey, GitRev: a.GitRev, GitDirty: a.GitDirty, ParentID: a.Within,
	})
	if err != nil {
		return nil, err
	}
	return d.view(it, time.Now()), nil
}

func (d *server) list(ctx context.Context, raw json.RawMessage) (any, error) {
	var a listArgs
	if err := decode(raw, &a); err != nil {
		return nil, err
	}
	items, err := d.st.List(ctx, store.ListFilter{SessionID: a.SessionID, IncludeTerminal: a.All})
	if err != nil {
		return nil, err
	}
	pos, err := d.queuePositions(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	views := make([]ItemView, 0, len(items))
	for _, it := range items {
		v := d.view(it, now)
		if it.State == model.StateQueued {
			d.markQueued(&v, it, pos)
		}
		views = append(views, v)
	}
	return views, nil
}

func (d *server) logs(ctx context.Context, raw json.RawMessage, w protocol.StreamWriter) (any, error) {
	var a LogsArgs
	if err := decode(raw, &a); err != nil {
		return nil, err
	}
	var name string
	switch a.Stream {
	case "", "stdout":
		name = "stdout.log"
	case "stderr":
		name = "stderr.log"
	default:
		return nil, badRequest("stream must be stdout or stderr")
	}
	if a.Tail < 0 {
		return nil, badRequest("tail must not be negative")
	}
	it, err := d.st.Get(ctx, a.ID)
	if err != nil {
		return nil, err
	}
	path := ""
	if dir := d.logDir(it); dir != "" {
		path = filepath.Join(dir, name)
	}

	f, err := os.Open(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	var size, off int64
	if f != nil {
		defer f.Close()
		if fi, err := f.Stat(); err == nil {
			size = fi.Size()
		}
		if a.Tail > 0 {
			off, err = tailStart(f, size, a.Tail)
			if err != nil {
				return nil, err
			}
		}
	}
	if !a.Follow {
		if f == nil {
			return LogLines{Lines: []string{}}, nil
		}
		buf := make([]byte, size-off)
		if _, err := f.ReadAt(buf, off); err != nil && err != io.EOF {
			return nil, err
		}
		return LogLines{Lines: splitLines(string(buf))}, nil
	}
	return LogLines{}, d.follow(ctx, a.ID, path, off, followDone(it), w)
}

// followDone says when a follow of it's logs is over (once the file is drained):
// a task at a terminal state, a reservation once it is not tearing down or its
// attempt changed.
func followDone(it *model.Item) func(*model.Item) bool {
	if !it.IsReservation() {
		return func(x *model.Item) bool { return x.State.Terminal() }
	}
	attempt := it.TeardownAttempt
	return func(x *model.Item) bool {
		return x.State != model.StateTearingDown || x.TeardownAttempt != attempt
	}
}

// follow streams complete lines appended from off until done(item) and the file is drained.
func (d *server) follow(ctx context.Context, id model.ItemID, path string, off int64, done func(*model.Item) bool, w protocol.StreamWriter) error {
	var pending []byte
	for {
		// Sample the state first: whatever was written before it went terminal is read below.
		it, err := d.st.Get(ctx, id)
		if err != nil {
			return err
		}
		terminal := done(it)
		chunk, err := readFrom(path, off)
		if err != nil {
			return err
		}
		off += int64(len(chunk))
		pending = append(pending, chunk...)
		var lines []string
		if i := bytes.LastIndexByte(pending, '\n'); i >= 0 {
			lines = splitLines(string(pending[:i+1]))
			pending = append([]byte(nil), pending[i+1:]...)
		}
		if terminal && len(chunk) == 0 && len(pending) > 0 {
			lines = append(lines, string(pending))
			pending = nil
		}
		if len(lines) > 0 {
			if err := w.Send(LogLines{Lines: lines}); err != nil {
				return err
			}
		}
		if terminal && len(chunk) == 0 {
			return nil
		}
		if len(chunk) > 0 {
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(followPoll):
		}
	}
}

func readFrom(path string, off int64) ([]byte, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if fi.Size() <= off {
		return nil, nil
	}
	buf := make([]byte, min(fi.Size()-off, 1<<20))
	n, err := f.ReadAt(buf, off)
	if err != nil && err != io.EOF {
		return nil, err
	}
	return buf[:n], nil
}

func splitLines(s string) []string {
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return []string{}
	}
	return strings.Split(s, "\n")
}

// tailStart returns the offset where the last n lines of f (size bytes) begin.
func tailStart(f *os.File, size int64, n int) (int64, error) {
	const chunk = 64 << 10
	end := size
	buf := make([]byte, chunk)
	first := true
	count := 0
	for end > 0 {
		start := max(end-chunk, 0)
		b := buf[:end-start]
		if _, err := f.ReadAt(b, start); err != nil && err != io.EOF {
			return 0, err
		}
		for i := len(b) - 1; i >= 0; i-- {
			if b[i] != '\n' {
				first = false
				continue
			}
			// A newline ending the file terminates the last line rather than starting a new one.
			if first && start+int64(i) == size-1 {
				first = false
				continue
			}
			first = false
			if count++; count == n {
				return start + int64(i) + 1, nil
			}
		}
		end = start
	}
	return 0, nil
}
