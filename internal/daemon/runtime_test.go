package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/creatorofuniverses/agent-queue/internal/model"
	"github.com/creatorofuniverses/agent-queue/internal/store"
)

// stopNow shuts the daemon down cleanly and waits for it.
func (e *env) stopNow(t *testing.T) {
	t.Helper()
	e.stop()
	select {
	case err := <-e.done:
		e.done <- err // the start cleanup waits for it too
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not stop")
	}
}

func restartEvents(t *testing.T, e *env) []model.Event {
	t.Helper()
	st, err := store.Open(e.paths.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	evs, err := st.EventsSince(t.Context(), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	var out []model.Event
	for _, ev := range evs {
		if ev.Kind == model.EventDaemonRestarted {
			out = append(out, ev)
		}
	}
	return out
}

func TestRestartEvent(t *testing.T) {
	e := newEnv(t)
	e.start(t)
	e.mustSubmit(t, e.submitArgs(64<<20))
	e.stopNow(t)
	if evs := restartEvents(t, e); len(evs) != 0 {
		t.Fatal("first start is not a restart", evs)
	}
	e.start(t)
	e.stopNow(t)
	evs := restartEvents(t, e)
	if len(evs) != 1 || evs[0].Audience != model.AudienceOperator || evs[0].ItemID != "" ||
		!strings.Contains(evs[0].Detail, "after a clean stop") || !strings.Contains(evs[0].Detail, "1 queued") {
		t.Fatalf("%+v", evs)
	}
	// Without a webhook the operator event is recorded as skipped.
	if evs[0].Status != model.EventSkipped {
		t.Fatal(evs[0].Status)
	}
}

func TestRestartAfterCrashSaysSo(t *testing.T) {
	e := newEnv(t)
	e.start(t)
	e.stopNow(t)
	// Simulate a crash: the lifecycle record says the daemon never stopped.
	st, err := store.Open(e.paths.DB())
	if err != nil {
		t.Fatal(err)
	}
	st.SaveRuntime(t.Context(), runtimeLifecycle, []byte(`{"version":"old","started_at":"2026-01-01T00:00:00Z"}`), time.Now())
	st.Close()
	e.start(t)
	e.stopNow(t)
	evs := restartEvents(t, e)
	if len(evs) != 1 || !strings.Contains(evs[0].Detail, "unclean stop") || !strings.Contains(evs[0].Detail, "old → ") {
		t.Fatalf("%+v", evs)
	}
}
