package daemon

import (
	"context"
	"encoding/json"
	"github.com/creatorofuniverses/agent-queue/internal/config"
	"github.com/creatorofuniverses/agent-queue/internal/model"
	"github.com/creatorofuniverses/agent-queue/internal/notify"
	"github.com/creatorofuniverses/agent-queue/internal/platform"
	"github.com/creatorofuniverses/agent-queue/internal/protocol"
	"github.com/creatorofuniverses/agent-queue/internal/scheduler"
	"github.com/creatorofuniverses/agent-queue/internal/store"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSubmitRejectedWhenPushCommandMissing(t *testing.T) {
	e := newEnv(t)
	e.cfg.Agents[0].Command[0] = "aq-no-such-binary"
	e.start(t)
	a := e.submitArgs(64 * mib)
	a.ClientType = "codex"
	a.SessionID = "t1"
	err := e.call(t, "submit", a, nil)
	if code(err) != protocol.CodeRejected || !strings.Contains(err.Error(), "not found in daemon PATH") {
		t.Fatal(err)
	}
	var items []ItemView
	e.call(t, "list", listArgs{All: true}, &items)
	if len(items) != 0 {
		t.Fatal(items)
	}
	a.NoNotify = true
	e.mustSubmit(t, a)
}
func TestSubmitUnknownClientRejected(t *testing.T) {
	e := newEnv(t)
	e.start(t)
	a := e.submitArgs(64 * mib)
	a.ClientType = "zzz"
	if err := e.call(t, "submit", a, nil); code(err) != protocol.CodeRejected {
		t.Fatal(err)
	}
}
func TestWaitMarksClientWaitSentOnce(t *testing.T) {
	e := newEnv(t)
	e.start(t)
	a := e.submitArgs(64 * mib)
	a.ClientType = "claude"
	a.SessionID = "c"
	it := e.mustSubmit(t, a)
	e.waitState(t, it.ID, model.StateRunning)
	e.ex.Finish(it.ID, platform.ExitInfo{ServiceResult: "success"})
	e.ex.SetAlive(it.ID, false)
	var v ItemView
	if err := e.call(t, "wait", idArgs{it.ID}, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Events) != 1 || v.Events[0].Status != model.EventSent {
		t.Fatal(v)
	}
	id := v.Events[0].ID
	v = ItemView{}
	e.call(t, "wait", idArgs{it.ID}, &v)
	if len(v.Events) != 0 {
		t.Fatal(v)
	}
	var items []ItemView
	e.call(t, "list", listArgs{SessionID: "c"}, &items)
	if len(items) != 1 || len(items[0].Events) != 1 {
		t.Fatal(items)
	}
	if err := e.call(t, "ack", map[string]string{"event_id": id}, nil); err != nil {
		t.Fatal(err)
	}
	e.call(t, "list", listArgs{SessionID: "c"}, &items)
	if len(items) != 0 {
		t.Fatal(items)
	}
}
func TestAckOp(t *testing.T) {
	e := newEnv(t)
	e.start(t)
	if err := e.call(t, "ack", map[string]string{"event_id": "absent"}, nil); code(err) != protocol.CodeNotFound {
		t.Fatal(err)
	}
}
func TestPingReportsAdapters(t *testing.T) {
	e := newEnv(t)
	e.cfg.Agents[0].Command[0] = "aq-no-such-binary"
	e.start(t)
	var p pingResult
	e.call(t, "ping", nil, &p)
	if len(p.Adapters) != 2 || p.Adapters[0].OK || !p.Adapters[1].OK || p.Webhook.Configured {
		t.Fatal(p)
	}
}
func TestTwoSessionsOnlyOwnEvents(t *testing.T) {
	e := newEnv(t)
	e.start(t)
	for _, sid := range []string{"A", "B"} {
		a := e.submitArgs(64 * mib)
		a.ClientType = "claude"
		a.SessionID = sid
		v := e.mustSubmit(t, a)
		e.waitState(t, v.ID, model.StateRunning)
		e.ex.Finish(v.ID, platform.ExitInfo{ServiceResult: "success"})
		e.ex.SetAlive(v.ID, false)
		if err := e.call(t, "wait", idArgs{v.ID}, nil); err != nil {
			t.Fatal(err)
		}
	}
	var items []ItemView
	e.call(t, "list", listArgs{SessionID: "A"}, &items)
	if len(items) != 1 || items[0].SessionID != "A" {
		t.Fatal(items)
	}
	for _, v := range items {
		if len(v.Events) != 1 {
			t.Fatal(v)
		}
	}
}

// Direct handler with a manual clock verifies over-declaration consumption and rearm.
func TestWaitUntilExpiringOp(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	st, err := store.Open(filepath.Join(t.TempDir(), "aq.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now()
	cfg := config.Default()
	route := notify.NewRouter(cfg)
	sc := scheduler.New(scheduler.Deps{Store: st, Exec: e.ex, Host: e.host, Cfg: cfg, Paths: e.paths, Now: func() time.Time { return now }, Route: route.Route})
	sc.Recover(ctx)
	d := &server{sched: sc, st: st, paths: e.paths, router: route, cfg: cfg}
	task, _ := sc.Submit(ctx, scheduler.SubmitRequest{Cwd: e.cwd, Argv: []string{"true"}, Limit: 64 * mib, CPUs: 1, Timeout: time.Hour})
	raw, _ := json.Marshal(map[string]any{"id": task.ID, "until": "expiring"})
	if _, err = d.handle(ctx, "wait", raw, nil); code(err) != protocol.CodeBadRequest {
		t.Fatal(err)
	}
	res, _ := sc.Reserve(ctx, scheduler.ReserveRequest{Cwd: e.cwd, Memory: 64 * mib, TTL: 2 * time.Hour, Teardown: []byte("true"), SessionID: "c", ClientType: "claude"})
	sc.Tick(ctx)
	res, _ = st.Get(ctx, res.ID)
	st.AddEvents(ctx, []model.Event{{ID: "over", ItemID: res.ID, Kind: model.EventStandOverDeclared, Channel: model.ChannelClientWait, Status: model.EventPending, CreatedAt: now, Audience: model.AudienceSession, SessionID: "c"}})
	raw, _ = json.Marshal(map[string]any{"id": res.ID, "until": "expiring"})
	out, err := d.handle(ctx, "wait", raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	v := out.(ItemView)
	if v.State != model.StateGranted || len(v.Events) != 1 || v.Events[0].Kind != model.EventStandOverDeclared {
		t.Fatal(v)
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	if _, err = d.handle(cctx, "wait", raw, nil); err == nil {
		t.Fatal("rearmed wait did not block")
	}
	now = res.WarnAt
	sc.Tick(ctx)
	out, err = d.handle(ctx, "wait", raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	v = out.(ItemView)
	if v.State != model.StateExpiring || len(v.Events) != 1 || v.Events[0].Kind != model.EventReservationExpiring || v.Events[0].Status != model.EventSent {
		t.Fatal(v)
	}
}
