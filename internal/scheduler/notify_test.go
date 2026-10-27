package scheduler

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"resource-dispatcher/internal/config"
	"resource-dispatcher/internal/model"
	"resource-dispatcher/internal/platform"
	"strings"
	"testing"
	"time"
)

func notifyRoute(c string, a model.Audience) model.Channel {
	if a == model.AudienceOperator {
		return model.ChannelWebhook
	}
	switch c {
	case "codex":
		return model.ExecChannel(c)
	case "claude":
		return model.ChannelClientWait
	}
	return model.ChannelNone
}
func notifyEnv(t *testing.T) *env { e := newEnv(t, nil); e.s.d.Route = notifyRoute; return e }
func eventsOf(t *testing.T, e *env, id model.ItemID, k model.EventKind) []model.Event {
	t.Helper()
	evs, err := e.st.Events(e.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	out := []model.Event{}
	for _, v := range evs {
		if v.Kind == k {
			out = append(out, v)
		}
	}
	return out
}
func TestTaskFinishedEventOnEveryTerminal(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		ei                   platform.ExitInfo
		lost, launch, cancel bool
	}{
		{name: "success", ei: platform.ExitInfo{ServiceResult: "success", ExitStatus: "0"}},
		{name: "failed", ei: platform.ExitInfo{ServiceResult: "exit-code", ExitStatus: "1"}},
		{name: "cancel", cancel: true}, {name: "lost", lost: true}, {name: "launch", launch: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := notifyEnv(t)
			r := e.req(64 * mib)
			r.ClientType = "codex"
			r.SessionID = "t"
			it := e.submit(t, r)
			if tc.cancel {
				e.s.Cancel(e.ctx, it.ID, "")
			} else {
				if tc.launch {
					e.ex.FailLaunch(errors.New("cannot start"))
				}
				e.tick(t)
				if tc.lost {
					e.ex.SetAlive(it.ID, false)
					e.tick(t)
					e.clk.Advance(e.cfg.CancelGrace)
					e.tick(t)
				} else if !tc.launch {
					e.ex.Finish(it.ID, tc.ei)
					e.ex.SetAlive(it.ID, false)
					e.tick(t)
				}
			}
			evs := eventsOf(t, e, it.ID, model.EventTaskFinished)
			if len(evs) != 1 || evs[0].Channel != model.ExecChannel("codex") || evs[0].Status != model.EventPending {
				t.Fatal(evs)
			}
			infra := eventsOf(t, e, it.ID, model.EventTaskFailedInfra)
			if len(infra) != btoi(tc.lost || tc.launch) {
				t.Fatal(infra)
			}
		})
	}
	e := notifyEnv(t)
	r := e.req(0)
	r.SessionID = "t"
	r.ClientType = "codex"
	it, _ := e.s.Submit(e.ctx, r)
	if evs, _ := e.st.Events(e.ctx, it.ID); len(evs) != 0 {
		t.Fatal(evs)
	}
}
func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}
func TestTaskFinishedOnceAcrossRecover(t *testing.T) {
	e := notifyEnv(t)
	r := e.req(64 * mib)
	r.SessionID = "t"
	r.ClientType = "codex"
	it := e.submit(t, r)
	e.tick(t)
	e.ex.Finish(it.ID, platform.ExitInfo{ServiceResult: "success"})
	e.ex.SetAlive(it.ID, false)
	e.tick(t)
	e.rebuild(t, e.ex, true)
	if evs := eventsOf(t, e, it.ID, model.EventTaskFinished); len(evs) != 1 {
		t.Fatal(evs)
	}
}
func TestNoNotifySuppressesSessionEvents(t *testing.T) {
	for _, empty := range []bool{true, false} {
		e := notifyEnv(t)
		r := e.req(64 * mib)
		r.ClientType = "codex"
		r.NoNotify = !empty
		if !empty {
			r.SessionID = "t"
		}
		it := e.submit(t, r)
		e.s.Cancel(e.ctx, it.ID, "")
		if evs := eventsOf(t, e, it.ID, model.EventTaskFinished); len(evs) != 0 {
			t.Fatal(evs)
		}
	}
	e := notifyEnv(t)
	r := e.rreq(64 * mib)
	r.SessionID = "t"
	r.ClientType = "codex"
	r.NoNotify = true
	e.host.Set(platform.MemInfo{Total: 64 * gib, Available: 1})
	it := e.reserve(t, r)
	e.tick(t)
	e.host.Set(platform.MemInfo{Total: 64 * gib, Available: 64 * gib})
	e.tick(t)
	e.clk.Advance(2*time.Hour - e.cfg.Reservations.ExpiryWarning)
	e.tick(t)
	evs := eventsOf(t, e, it.ID, model.EventReservationGranted)
	if len(evs) != 0 {
		t.Fatal(evs)
	}
	evs = eventsOf(t, e, it.ID, model.EventReservationExpiring)
	if len(evs) != 1 || evs[0].Audience != model.AudienceOperator {
		t.Fatal(evs)
	}
}
func TestUnroutedEventSkipped(t *testing.T) {
	e := notifyEnv(t)
	r := e.req(64 * mib)
	r.ClientType = "cli"
	r.SessionID = "s"
	it := e.submit(t, r)
	e.s.Cancel(e.ctx, it.ID, "")
	evs := eventsOf(t, e, it.ID, model.EventTaskFinished)
	if len(evs) != 1 || evs[0].Status != model.EventSkipped || evs[0].Channel != model.ChannelNone {
		t.Fatal(evs)
	}
}
func TestOnEventsCalledAfterWrite(t *testing.T) {
	e := notifyEnv(t)
	n := 0
	e.s.d.OnEvents = func() { n++ }
	r := e.req(64 * mib)
	r.SessionID = "s"
	it := e.submit(t, r)
	e.tick(t)
	if n != 0 {
		t.Fatal(n)
	}
	e.ex.Finish(it.ID, platform.ExitInfo{ServiceResult: "success"})
	e.ex.SetAlive(it.ID, false)
	e.tick(t)
	if n != 1 {
		t.Fatal(n)
	}
}
func TestRequestHashUnchangedByNoNotify(t *testing.T) {
	b, _ := json.Marshal(SubmitRequest{})
	if got := fmt.Sprintf("%x", sha256.Sum256(b)); got != "e67d54ca7961a7f282f81207f6a5a6f2bb1b66642fcd22ed21f0156e70254e34" {
		t.Fatal(got)
	}
}
func TestWaitUntilExpiring(t *testing.T) {
	e := notifyEnv(t)
	it := e.reserve(t, e.rreq(64*mib))
	e.tick(t)
	ctx, cancel := context.WithCancel(e.ctx)
	defer cancel()
	done := make(chan *model.Item, 1)
	go func() { v, _ := e.s.Wait(ctx, it.ID, WaitExpiring); done <- v }()
	e.s.Extend(e.ctx, it.ID, 3*time.Hour)
	select {
	case v := <-done:
		t.Fatal(v)
	case <-time.After(20 * time.Millisecond):
	}
	e.clk.Advance(3*time.Hour - e.cfg.Reservations.ExpiryWarning)
	e.tick(t)
	select {
	case v := <-done:
		if v == nil || v.State != model.StateExpiring {
			t.Fatal(v)
		}
	case <-time.After(time.Second):
		t.Fatal("wait stuck")
	}
	task := e.submit(t, e.req(64*mib))
	_, err := e.s.Wait(e.ctx, task.ID, WaitExpiring)
	if !errors.Is(err, ErrBadRequest) {
		t.Fatal(err)
	}
}
func TestCleaningStuckAlertOnce(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.Notify.CleaningStuckAlert = 65 * time.Second })
	e.s.d.Route = notifyRoute
	it := e.submit(t, e.req(64*mib))
	e.tick(t)
	e.ex.Finish(it.ID, platform.ExitInfo{ServiceResult: "success"})
	e.ex.SetAlive(it.ID, true)
	e.ex.SetBeforeStop(func(id model.ItemID) { e.ex.SetAlive(id, true) })
	e.tick(t)
	// Stop normally marks dead in fake; keep resetting liveness before each tick.
	for i := 1; i <= 75; i++ {
		e.settle()
		e.ex.SetAlive(it.ID, true)
		e.clk.Advance(time.Second)
		e.tick(t)
		n := len(eventsOf(t, e, it.ID, model.EventCleaningStuck))
		want := btoi(i >= 65)
		if n != want {
			t.Fatalf("second %d: %d != %d", i, n, want)
		}
	}
}
func TestForceReleaseOperatorEvent(t *testing.T) {
	e := notifyEnv(t)
	it := e.reserve(t, e.rreq(64*mib))
	it.State = model.StateTeardownFailed
	e.st.Update(e.ctx, it)
	v, err := e.s.Release(e.ctx, it.ID, true)
	if err != nil || v.State != model.StateReleased {
		t.Fatal(v, err)
	}
	e.s.Release(e.ctx, it.ID, true)
	evs := eventsOf(t, e, it.ID, model.EventReservationForceReleased)
	if len(evs) != 1 || evs[0].Channel != model.ChannelWebhook || !strings.Contains(evs[0].Detail, "TEARDOWN_FAILED") {
		t.Fatal(evs)
	}
	it = e.reserve(t, e.rreq(64*mib))
	e.tick(t)
	e.s.Release(e.ctx, it.ID, false)
	if len(eventsOf(t, e, it.ID, model.EventReservationForceReleased)) != 0 {
		t.Fatal("ordinary release event")
	}
}
func TestWaitUntilExpiringOnOverDeclared(t *testing.T) {
	e := notifyEnv(t)
	r := e.rreq(64 * mib)
	r.ClientType = "claude"
	r.SessionID = "c"
	r.DockerProject = "p"
	it := e.reserve(t, r)
	e.tick(t)
	e.meter.Set("p", 128*mib)
	e.tick(t)
	ctx, cancel := context.WithTimeout(e.ctx, time.Second)
	defer cancel()
	v, err := e.s.Wait(ctx, it.ID, WaitExpiring)
	if err != nil || v.State != model.StateGranted {
		t.Fatal(v, err)
	}
}
func TestAdmissionClosedAlertPerEpisode(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.Notify.AdmissionClosedAlert = 3 * time.Second })
	e.s.d.Route = notifyRoute
	e.host.Set(platform.MemInfo{Total: 64 * gib, Available: 1})
	it := e.submit(t, e.req(64*mib))
	e.tick(t)
	e.clk.Advance(2 * time.Second)
	e.tick(t)
	if len(eventsOf(t, e, it.ID, model.EventAdmissionClosed)) != 0 {
		t.Fatal("early")
	}
	e.clk.Advance(time.Second)
	e.tick(t)
	e.tick(t)
	if len(eventsOf(t, e, it.ID, model.EventAdmissionClosed)) != 1 {
		t.Fatal("missing/duplicate")
	}
	e.host.Set(platform.MemInfo{Total: 64 * gib, Available: 64 * gib})
	e.tick(t)
	e.host.Set(platform.MemInfo{Total: 64 * gib, Available: 1})
	it2 := e.submit(t, e.req(64*mib))
	e.tick(t)
	e.clk.Advance(3 * time.Second)
	e.tick(t)
	if len(eventsOf(t, e, it2.ID, model.EventAdmissionClosed)) != 1 {
		t.Fatal("episode not reset")
	}
	// pool_memory is an ordinary capacity wait and must reset an alert episode.
	e2 := notifyEnv(t)
	e2.submit(t, e2.req(gib))
	e2.tick(t)
	q := e2.submit(t, e2.req(64*mib))
	e2.tick(t)
	e2.clk.Advance(time.Hour)
	e2.tick(t)
	if len(eventsOf(t, e2, q.ID, model.EventAdmissionClosed)) != 0 {
		t.Fatal("pool alert")
	}
}
