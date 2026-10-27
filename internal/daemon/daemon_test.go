package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"resource-dispatcher/internal/config"
	"resource-dispatcher/internal/model"
	"resource-dispatcher/internal/platform"
	"resource-dispatcher/internal/platform/fake"
	"resource-dispatcher/internal/protocol"
)

const (
	mib = uint64(1) << 20
	gib = uint64(1) << 30
)

type env struct {
	paths config.Paths
	cfg   config.Config
	ex    *fake.Executor
	host  *fake.Host
	cwd   string
	done  chan error
	stop  context.CancelFunc
}

func newEnv(t *testing.T) *env {
	t.Helper()
	// Short root: unix socket paths are limited to ~108 bytes.
	root, err := os.MkdirTemp("", "aq")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	e := &env{
		paths: config.Paths{StateDir: filepath.Join(root, "s"), RuntimeDir: filepath.Join(root, "r"), SliceLockDir: root},
		cfg:   config.Default(),
		ex:    fake.NewExecutor(),
		host:  fake.NewHost(),
		cwd:   t.TempDir(),
	}
	e.cfg.PoolMemory = gib
	e.host.Set(platform.MemInfo{Total: 64 * gib, Available: 64 * gib})
	e.host.SetDisk(100 * gib)
	e.host.SetNumCPU(16)
	return e
}

func (e *env) start(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	e.stop = cancel
	e.done = make(chan error, 1)
	go func() { e.done <- Run(ctx, e.paths, e.cfg, e.ex, e.host) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-e.done:
		case <-time.After(5 * time.Second):
			t.Error("daemon did not stop")
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := protocol.Call(context.Background(), e.paths.Socket(), "ping", nil, nil); err == nil {
			return
		}
		select {
		case err := <-e.done:
			t.Fatalf("daemon exited: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("daemon did not come up")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (e *env) call(t *testing.T, op string, args, out any) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return protocol.Call(ctx, e.paths.Socket(), op, args, out)
}

func (e *env) submitArgs(mem uint64) SubmitArgs {
	return SubmitArgs{Argv: []string{"true"}, Cwd: e.cwd, Limit: mem, CPUs: 1, TimeoutSeconds: 3600}
}

func (e *env) mustSubmit(t *testing.T, a SubmitArgs) ItemView {
	t.Helper()
	var v ItemView
	if err := e.call(t, "submit", a, &v); err != nil {
		t.Fatalf("submit: %v", err)
	}
	return v
}

func (e *env) waitState(t *testing.T, id model.ItemID, want model.State) ItemView {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var v ItemView
		if err := e.call(t, "status", idArgs{ID: id}, &v); err != nil {
			t.Fatal(err)
		}
		if v.State == want {
			return v
		}
		if time.Now().After(deadline) {
			t.Fatalf("state %s, want %s", v.State, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func code(err error) string {
	var pe *protocol.Error
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

func TestSecondDaemonRefused(t *testing.T) {
	e := newEnv(t)
	e.start(t)
	err := Run(context.Background(), e.paths, e.cfg, fake.NewExecutor(), e.host)
	if err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("second Run: %v", err)
	}
	if err := e.call(t, "ping", nil, nil); err != nil {
		t.Fatalf("first daemon not alive: %v", err)
	}
}

// A daemon with its own state and runtime dirs but the same slice would treat the
// first daemon's units as orphans and stop them: it must not start.
func TestSecondDaemonOnSameSliceRefused(t *testing.T) {
	e := newEnv(t)
	e.start(t)
	other := newEnv(t)
	other.paths.SliceLockDir = e.paths.SliceLockDir
	ex := fake.NewExecutor()
	err := Run(context.Background(), other.paths, other.cfg, ex, other.host)
	if err == nil || !strings.Contains(err.Error(), e.cfg.SliceName) {
		t.Fatalf("second Run: %v", err)
	}
	if err := e.call(t, "ping", nil, nil); err != nil {
		t.Fatalf("first daemon not alive: %v", err)
	}

	// another slice is fine
	other.cfg.SliceName = "aqother.slice"
	other.start(t)
}

// The teardown slice is locked too: a daemon whose task slice is another daemon's
// teardown slice would stop that daemon's teardown units as orphans.
func TestTeardownSliceLocked(t *testing.T) {
	e := newEnv(t)
	e.cfg.SliceName = "aq.slice"
	e.start(t)
	if pool, td := e.ex.Ensured(); pool != e.cfg.PoolMemory ||
		td != uint64(e.cfg.Reservations.TeardownConcurrency)*e.cfg.Reservations.TeardownMemory {
		t.Fatalf("A ensured pool %d teardown %d", pool, td)
	}
	other := newEnv(t)
	other.paths.SliceLockDir = e.paths.SliceLockDir
	other.cfg.SliceName = "aqtd.slice"
	ex := fake.NewExecutor()
	err := Run(context.Background(), other.paths, other.cfg, ex, other.host)
	if err == nil || !strings.Contains(err.Error(), "already manages slice aqtd.slice") {
		t.Fatalf("second Run: %v", err)
	}
	if pool, td := ex.Ensured(); pool != 0 || td != 0 {
		t.Fatalf("B ensured pool %d teardown %d before failing", pool, td)
	}
	if err := e.call(t, "ping", nil, nil); err != nil {
		t.Fatalf("first daemon not alive: %v", err)
	}
}

func TestStateDirMadePrivate(t *testing.T) {
	e := newEnv(t)
	if err := os.MkdirAll(e.paths.StateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	e.start(t)
	if st, err := os.Stat(e.paths.StateDir); err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("state dir %v %v, want 0700", st.Mode().Perm(), err)
	}
	if st, err := os.Stat(e.paths.DB()); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("db %v %v, want 0600", st.Mode().Perm(), err)
	}
}

func TestSubmitStatusWaitFlow(t *testing.T) {
	e := newEnv(t)
	e.start(t)
	v := e.mustSubmit(t, e.submitArgs(128*mib))
	if v.Priority != "normal" || v.MemoryLimit != 128*mib {
		t.Fatalf("view: %+v", v)
	}
	e.waitState(t, v.ID, model.StateRunning)

	waited := make(chan ItemView, 1)
	go func() {
		var w ItemView
		if err := e.call(t, "wait", idArgs{ID: v.ID}, &w); err != nil {
			t.Error(err)
		}
		waited <- w
	}()
	time.Sleep(50 * time.Millisecond)
	e.ex.Finish(v.ID, platform.ExitInfo{ServiceResult: "success", ExitCode: "exited", ExitStatus: "0", EndedAt: time.Now()})
	e.ex.SetAlive(v.ID, false)
	select {
	case w := <-waited:
		if w.State != model.StateSucceeded {
			t.Fatalf("wait: %+v", w)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("wait did not return")
	}

	var r ResultView
	if err := e.call(t, "result", idArgs{ID: v.ID}, &r); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(r.StdoutPath, "stdout.log") || !strings.HasSuffix(r.StderrPath, "stderr.log") {
		t.Fatalf("result: %+v", r)
	}
	if err := e.call(t, "status", idArgs{ID: "nope"}, nil); code(err) != protocol.CodeNotFound {
		t.Fatalf("missing id: %v", err)
	}
}

func TestSubmitIdempotencyOverSocket(t *testing.T) {
	e := newEnv(t)
	e.start(t)
	a := e.submitArgs(128 * mib)
	a.IdemKey = "k1"
	v1 := e.mustSubmit(t, a)
	v2 := e.mustSubmit(t, a)
	if v1.ID != v2.ID {
		t.Fatalf("ids differ: %s %s", v1.ID, v2.ID)
	}
	a.Argv = []string{"false"}
	if err := e.call(t, "submit", a, nil); code(err) != protocol.CodeIdempotencyConflict {
		t.Fatalf("want IDEMPOTENCY_CONFLICT, got %v", err)
	}
}

func TestSubmitErrors(t *testing.T) {
	e := newEnv(t)
	e.start(t)
	a := e.submitArgs(128 * mib)
	a.Priority = "urgent"
	if err := e.call(t, "submit", a, nil); code(err) != protocol.CodeBadRequest {
		t.Fatalf("bad priority: %v", err)
	}
	if err := e.call(t, "submit", map[string]any{"bogus": 1}, nil); code(err) != protocol.CodeBadRequest {
		t.Fatalf("unknown field: %v", err)
	}
	a = e.submitArgs(128 * mib)
	a.Cwd = "relative"
	err := e.call(t, "submit", a, nil)
	if code(err) != protocol.CodeRejected || !strings.Contains(err.Error(), "cwd") {
		t.Fatalf("rejected: %v", err)
	}
}

func TestListShowsBlocker(t *testing.T) {
	e := newEnv(t)
	e.start(t)
	first := e.mustSubmit(t, e.submitArgs(768*mib))
	e.waitState(t, first.ID, model.StateRunning)
	head := e.mustSubmit(t, e.submitArgs(512*mib)) // does not fit next to first

	deadline := time.Now().Add(5 * time.Second)
	for {
		var vs []ItemView
		if err := e.call(t, "list", listArgs{}, &vs); err != nil {
			t.Fatal(err)
		}
		for _, v := range vs {
			if v.ID == head.ID && v.BlockedBy == "pool_memory" && v.Position == 1 && v.BlockedDetail != "" {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no blocker in list: %+v", vs)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestEnvNotExposed(t *testing.T) {
	e := newEnv(t)
	e.start(t)
	a := e.submitArgs(128 * mib)
	a.Env = map[string]string{"SECRET": "hunter2-value"}
	v := e.mustSubmit(t, a)
	for _, c := range []struct {
		op   string
		args any
	}{
		{"status", idArgs{ID: v.ID}}, {"result", idArgs{ID: v.ID}},
		{"list", listArgs{All: true}}, {"submit", a},
	} {
		var raw json.RawMessage
		if err := e.call(t, c.op, c.args, &raw); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "hunter2") || strings.Contains(string(raw), "SECRET") {
			t.Fatalf("%s leaks env: %s", c.op, raw)
		}
	}
}

func TestCancelOverSocket(t *testing.T) {
	e := newEnv(t)
	e.start(t)
	first := e.mustSubmit(t, e.submitArgs(768*mib))
	e.waitState(t, first.ID, model.StateRunning)
	q := e.mustSubmit(t, e.submitArgs(512*mib))
	var v ItemView
	if err := e.call(t, "cancel", cancelArgs{ID: q.ID, Reason: "test"}, &v); err != nil {
		t.Fatal(err)
	}
	if v.State != model.StateCancelled || v.Reason != model.ReasonCancelled {
		t.Fatalf("cancel: %+v", v)
	}
}

func TestLogsTailAndFollow(t *testing.T) {
	e := newEnv(t)
	e.start(t)
	v := e.mustSubmit(t, e.submitArgs(128*mib))
	e.waitState(t, v.ID, model.StateRunning)

	dir := e.paths.JobDir(v.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "stdout.log")
	var sb strings.Builder
	for i := 1; i <= 200; i++ {
		fmt.Fprintf(&sb, "line %d\n", i)
	}
	if err := os.WriteFile(logPath, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	var ll LogLines
	if err := e.call(t, "logs", LogsArgs{ID: v.ID, Stream: "stdout", Tail: 100}, &ll); err != nil {
		t.Fatal(err)
	}
	if len(ll.Lines) != 100 || ll.Lines[0] != "line 101" || ll.Lines[99] != "line 200" {
		t.Fatalf("tail: %d lines, first %q", len(ll.Lines), ll.Lines[0])
	}
	if err := e.call(t, "logs", LogsArgs{ID: v.ID, Stream: "stderr"}, &ll); err != nil || len(ll.Lines) != 0 {
		t.Fatalf("missing stderr: %v %v", err, ll)
	}

	got := make(chan string, 16)
	followDone := make(chan error, 1)
	go func() {
		followDone <- protocol.CallStream(context.Background(), e.paths.Socket(), "logs",
			LogsArgs{ID: v.ID, Stream: "stdout", Tail: 2, Follow: true},
			func(raw json.RawMessage) error {
				var m LogLines
				if err := json.Unmarshal(raw, &m); err != nil {
					return err
				}
				for _, l := range m.Lines {
					got <- l
				}
				return nil
			}, nil)
	}()
	for _, want := range []string{"line 199", "line 200"} {
		select {
		case l := <-got:
			if l != want {
				t.Fatalf("got %q want %q", l, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timeout on initial tail")
		}
	}
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("late line\n")
	f.Close()
	select {
	case l := <-got:
		if l != "late line" {
			t.Fatalf("got %q", l)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("appended line not streamed")
	}
	select {
	case err := <-followDone:
		t.Fatalf("follow ended early: %v", err)
	default:
	}
	e.ex.Finish(v.ID, platform.ExitInfo{ServiceResult: "success", ExitCode: "exited", ExitStatus: "0", EndedAt: time.Now()})
	e.ex.SetAlive(v.ID, false)
	select {
	case err := <-followDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("follow did not end at terminal state")
	}
}

func TestTailStart(t *testing.T) {
	dir := t.TempDir()
	for _, c := range []struct {
		content string
		n       int
		want    string
	}{
		{"a\nb\nc\n", 2, "b\nc\n"},
		{"a\nb\nc", 2, "b\nc"},
		{"a\n", 5, "a\n"},
		{"", 3, ""},
	} {
		p := filepath.Join(dir, "f")
		os.WriteFile(p, []byte(c.content), 0o644)
		f, _ := os.Open(p)
		off, err := tailStart(f, int64(len(c.content)), c.n)
		f.Close()
		if err != nil || c.content[off:] != c.want {
			t.Errorf("%q n=%d: got %q err %v", c.content, c.n, c.content[off:], err)
		}
	}
}
