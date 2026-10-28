package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-queue/internal/admission"
	"agent-queue/internal/model"
	"agent-queue/internal/platform"
	"agent-queue/internal/protocol"
	"agent-queue/internal/scheduler"
	"agent-queue/internal/store"
)

func (e *env) reserveArgs(mem uint64) ReserveArgs {
	return ReserveArgs{
		Name: "stand", Cwd: e.cwd, Memory: mem, CPUs: 1, TTLSeconds: 3600,
		Teardown: "echo bye\n", SessionID: "s1",
	}
}

func (e *env) mustReserve(t *testing.T, a ReserveArgs) ItemView {
	t.Helper()
	var v ItemView
	if err := e.call(t, "reserve", a, &v); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	return v
}

func finishTask(e *env, id model.ItemID) {
	e.ex.Finish(id, platform.ExitInfo{ServiceResult: "success", ExitCode: "exited", ExitStatus: "0", EndedAt: time.Now()})
	e.ex.SetAlive(id, false)
}

func TestReserveGrantedSync(t *testing.T) {
	e := newEnv(t)
	e.start(t)
	v := e.mustReserve(t, e.reserveArgs(256*mib))
	if v.State != model.StateGranted || v.Kind != model.KindReservation {
		t.Fatalf("view: %+v", v)
	}
	if v.ExpiresAt == nil || v.ExpiresAt.Before(time.Now().Add(50*time.Minute)) {
		t.Fatalf("expires_at: %v", v.ExpiresAt)
	}
}

func TestReserveQueuedShowsBlocker(t *testing.T) {
	e := newEnv(t)
	e.start(t)
	task := e.mustSubmit(t, e.submitArgs(gib))
	e.waitState(t, task.ID, model.StateRunning)
	v := e.mustReserve(t, e.reserveArgs(256*mib))
	if v.State != model.StateQueued || v.Position != 1 || v.BlockedBy != admission.BlockPoolMemory {
		t.Fatalf("view: %+v", v)
	}
}

// Final review F6: when the client's ctx ends while waiting for the admission pass, the
// stored reservation is still the answer, so a retrying client or a log has its id.
func TestReserveAnswersAfterContextEnds(t *testing.T) {
	e := newEnv(t)
	if err := os.MkdirAll(e.paths.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	st := e.openStore(t)
	sched := scheduler.New(scheduler.Deps{Store: st, Exec: e.ex, Host: e.host, Cfg: e.cfg, Paths: e.paths})
	d := &server{sched: sched, st: st, paths: e.paths} // never Run: no admission pass finishes
	raw, err := json.Marshal(e.reserveArgs(256 * mib))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	out, err := d.reserve(ctx, raw)
	if err != nil {
		t.Fatalf("reserve: %v, want the stored reservation", err)
	}
	v, ok := out.(ItemView)
	if !ok || v.ID == "" || v.State != model.StateQueued || v.Position != 1 {
		t.Fatalf("reply %+v, want a QUEUED view with an id and position 1", out)
	}
	if _, err := st.Get(context.Background(), v.ID); err != nil {
		t.Fatalf("stored reservation %s: %v", v.ID, err)
	}
}

func TestWaitReservationGrant(t *testing.T) {
	e := newEnv(t)
	e.start(t)
	task := e.mustSubmit(t, e.submitArgs(gib))
	e.waitState(t, task.ID, model.StateRunning)
	v := e.mustReserve(t, e.reserveArgs(256*mib))
	if v.State != model.StateQueued {
		t.Fatalf("state %s", v.State)
	}
	got := make(chan ItemView, 1)
	go func() {
		var w ItemView
		if err := e.call(t, "wait", idArgs{ID: v.ID}, &w); err != nil {
			t.Error(err)
		}
		got <- w
	}()
	time.Sleep(50 * time.Millisecond)
	finishTask(e, task.ID)
	select {
	case w := <-got:
		if w.State != model.StateGranted {
			t.Fatalf("waited state %s", w.State)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("wait did not return")
	}
}

func TestReleaseExtendRetryOverSocket(t *testing.T) {
	e := newEnv(t)
	e.start(t)
	v := e.mustReserve(t, e.reserveArgs(256*mib))

	var ext ItemView
	if err := e.call(t, "extend", extendArgs{ID: v.ID, TTLSeconds: 7200}, &ext); err != nil {
		t.Fatalf("extend: %v", err)
	}
	if ext.ExpiresAt == nil || !ext.ExpiresAt.After(v.ExpiresAt.Add(30*time.Minute)) {
		t.Fatalf("expires_at %v -> %v", v.ExpiresAt, ext.ExpiresAt)
	}
	if err := e.call(t, "teardown_retry", idArgs{ID: v.ID}, nil); code(err) != protocol.CodeBadState {
		t.Fatalf("retry on GRANTED: %v", err)
	}
	var rel ItemView
	if err := e.call(t, "release", releaseArgs{ID: v.ID}, &rel); err != nil || rel.State != model.StateReleased {
		t.Fatalf("release: %v %+v", err, rel)
	}
	task := e.mustSubmit(t, e.submitArgs(128*mib))
	if err := e.call(t, "release", releaseArgs{ID: task.ID}, nil); code(err) != protocol.CodeBadState {
		t.Fatalf("release of a task: %v", err)
	}
}

func TestSubmitWithinOverSocket(t *testing.T) {
	e := newEnv(t)
	e.start(t)
	a := e.reserveArgs(256 * mib)
	a.TaskMemory, a.TaskCPUs = 256*mib, 2
	r := e.mustReserve(t, a)
	if r.State != model.StateGranted {
		t.Fatalf("reservation %s", r.State)
	}
	s := e.submitArgs(128 * mib)
	s.Within = r.ID
	v := e.mustSubmit(t, s)
	if v.Within != r.ID {
		t.Fatalf("within %q", v.Within)
	}
	e.waitState(t, v.ID, model.StateRunning)

	s.Limit = 256 * mib
	err := e.call(t, "submit", s, nil)
	var pe *protocol.Error
	if code(err) != protocol.CodeRejected || !asPE(err, &pe) || !strings.Contains(pe.Message, "task budget") {
		t.Fatalf("over budget: %v", err)
	}
}

func asPE(err error, pe **protocol.Error) bool {
	p, ok := err.(*protocol.Error)
	if ok {
		*pe = p
	}
	return ok
}

func TestReservationViewHidesSecrets(t *testing.T) {
	e := newEnv(t)
	e.start(t)
	a := e.reserveArgs(256 * mib)
	a.Env = map[string]string{"SECRET": "x"}
	a.Teardown = "echo topsecret\n"
	var raw json.RawMessage
	if err := e.call(t, "reserve", a, &raw); err != nil {
		t.Fatal(err)
	}
	var v ItemView
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	outs := []string{string(raw)}
	for _, op := range []string{"status", "result"} {
		var r json.RawMessage
		if err := e.call(t, op, idArgs{ID: v.ID}, &r); err != nil {
			t.Fatal(err)
		}
		outs = append(outs, string(r))
	}
	var l json.RawMessage
	if err := e.call(t, "list", map[string]bool{"all": true}, &l); err != nil {
		t.Fatal(err)
	}
	outs = append(outs, string(l))
	for _, o := range outs {
		if strings.Contains(o, `"x"`) || strings.Contains(o, "topsecret") || strings.Contains(o, "SECRET") {
			t.Fatalf("secret leaked: %s", o)
		}
	}
}

func (e *env) openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(e.paths.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func (e *env) insertReservation(t *testing.T, st *store.Store, state model.State, attempt int) *model.Item {
	t.Helper()
	it := &model.Item{
		ID: model.NewID(), Kind: model.KindReservation, Name: "r", State: state, Priority: model.PriorityNormal,
		Cwd: e.cwd, Limit: 64 * mib, Expected: 64 * mib, CPUs: 1, Timeout: time.Hour,
		RequestHash: string(model.NewID()), NodeID: "local", CreatedAt: time.Now(),
		ExpiresAt: time.Now().Add(time.Hour), TeardownAttempt: attempt,
	}
	if _, _, err := st.InsertItem(context.Background(), it); err != nil {
		t.Fatal(err)
	}
	return it
}

func (e *env) logLines(t *testing.T, id model.ItemID, stream string) []string {
	t.Helper()
	var l LogLines
	if err := e.call(t, "logs", LogsArgs{ID: id, Stream: stream}, &l); err != nil {
		t.Fatal(err)
	}
	return l.Lines
}

func TestReservationTeardownLogs(t *testing.T) {
	e := newEnv(t)
	e.start(t)
	st := e.openStore(t)

	failed := e.insertReservation(t, st, model.StateTeardownFailed, 1)
	dir := e.paths.TeardownDir(failed.ID, 1)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stderr.log"), []byte("a\nb\nc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var res ResultView
	if err := e.call(t, "result", idArgs{ID: failed.ID}, &res); err != nil {
		t.Fatal(err)
	}
	if res.StderrPath != filepath.Join(dir, "stderr.log") || res.StdoutPath != filepath.Join(dir, "stdout.log") {
		t.Fatalf("paths %q %q", res.StdoutPath, res.StderrPath)
	}
	if got := e.logLines(t, failed.ID, "stderr"); len(got) != 3 {
		t.Fatalf("stderr lines %v", got)
	}

	// follow on a settled reservation returns at once
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := protocol.CallStream(ctx, e.paths.Socket(), "logs",
		LogsArgs{ID: failed.ID, Stream: "stderr", Follow: true}, func(json.RawMessage) error { return nil }, nil); err != nil {
		t.Fatalf("follow on TEARDOWN_FAILED: %v", err)
	}

	// attempt 0: no paths, no logs
	fresh := e.insertReservation(t, st, model.StateTeardownFailed, 0)
	var r0 ResultView
	if err := e.call(t, "result", idArgs{ID: fresh.ID}, &r0); err != nil {
		t.Fatal(err)
	}
	if r0.StdoutPath != "" || r0.StderrPath != "" {
		t.Fatalf("attempt 0 paths %q %q", r0.StdoutPath, r0.StderrPath)
	}
	if got := e.logLines(t, fresh.ID, "stderr"); len(got) != 0 {
		t.Fatalf("attempt 0 lines %v", got)
	}

	// follow on TEARING_DOWN streams appended lines and ends at TORN_DOWN
	run := e.insertReservation(t, st, model.StateTearingDown, 1)
	e.ex.SetAlive(model.TeardownUnitID(run.ID), true)
	rdir := e.paths.TeardownDir(run.ID, 1)
	if err := os.MkdirAll(rdir, 0o700); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(rdir, "stderr.log")
	if err := os.WriteFile(logPath, []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lines := make(chan string, 8)
	done := make(chan error, 1)
	go func() {
		done <- protocol.CallStream(context.Background(), e.paths.Socket(), "logs",
			LogsArgs{ID: run.ID, Stream: "stderr", Follow: true}, func(m json.RawMessage) error {
				var l LogLines
				if err := json.Unmarshal(m, &l); err != nil {
					return err
				}
				for _, s := range l.Lines {
					lines <- s
				}
				return nil
			}, nil)
	}()
	expect := func(want string) {
		t.Helper()
		select {
		case got := <-lines:
			if got != want {
				t.Fatalf("line %q, want %q", got, want)
			}
		case err := <-done:
			t.Fatalf("follow ended: %v", err)
		case <-time.After(3 * time.Second):
			t.Fatalf("no line %q", want)
		}
	}
	expect("one")
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("two\n")
	f.Close()
	expect("two")
	cur, err := st.Get(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	cur.State = model.StateTornDown
	cur.EndedAt = time.Now()
	if err := st.Update(context.Background(), cur); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("follow did not end")
	}
}

func TestStandMeasuredInView(t *testing.T) {
	e := newEnv(t)
	e.start(t)
	e.meter.Set("p", gib)
	a := e.reserveArgs(512 * mib)
	a.DockerProject = "p"
	v := e.mustReserve(t, a)
	if v.State != model.StateGranted {
		t.Fatalf("state %s", v.State)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		var s ItemView
		if err := e.call(t, "status", idArgs{ID: v.ID}, &s); err != nil {
			t.Fatal(err)
		}
		if s.StandMeasured != nil && *s.StandMeasured == 1073741824 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("stand_measured %v", s.StandMeasured)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
