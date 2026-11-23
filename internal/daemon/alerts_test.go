package daemon

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/creatorofuniverses/agent-queue/internal/agentsess"
	"github.com/creatorofuniverses/agent-queue/internal/config"
	"github.com/creatorofuniverses/agent-queue/internal/levelalarm"
	"github.com/creatorofuniverses/agent-queue/internal/model"
	"github.com/creatorofuniverses/agent-queue/internal/platform"
	"github.com/creatorofuniverses/agent-queue/internal/platform/fake"
)

func TestSessionsHeavyText(t *testing.T) {
	longest, shorter, skew := (44 * time.Hour).Seconds(), (20 * time.Hour).Seconds(), -1.0
	sessions := []agentsess.Session{
		{SessionUsage: platform.SessionUsage{Cwd: "/home/u/other", AgentBytes: 390 << 20, RunBytes: 10 << 20}, Name: "shorter", IdleSeconds: &shorter},
		{SessionUsage: platform.SessionUsage{Cwd: "/home/u/shop", AgentBytes: 500 << 20, RunBytes: 9 << 20}, Name: "shop", IdleSeconds: &longest},
		{SessionUsage: platform.SessionUsage{AgentBytes: 450 << 20}, Name: "unknown-idle"},
		{SessionUsage: platform.SessionUsage{AgentBytes: 1 << 20}, Name: "too-small", IdleSeconds: &skew},
	}
	for i := range 3 {
		sessions = append(sessions, agentsess.Session{
			SessionUsage: platform.SessionUsage{AgentBytes: (400 - uint64(i)) << 20},
			Name:         fmt.Sprintf("worker-%d", i),
		})
	}
	cfg := config.Default()
	cfg.Sessions.AlertMemory = 2550 << 20
	cfg.Sessions.AlertIdleCount = 2
	cfg.Alerts.Window = time.Minute
	events := map[model.EventKind]string{}
	d := SamplerDeps{Host: fake.NewHost(), Home: "/home/u", Alerts: newAlertSet(cfg),
		Emit: func(_ context.Context, event model.Event) { events[event.Kind] = event.Detail }}
	at := time.Unix(1795464773, 0)
	d.sampleAlerts(t.Context(), at, platform.MemInfo{}, sessions, true)
	d.sampleAlerts(t.Context(), at.Add(time.Minute), platform.MemInfo{}, sessions, true)

	heavy := events[model.EventSessionsHeavy]
	if !strings.Contains(heavy, "shop (project ~/shop, 509M, idle 44h)") ||
		strings.Count(heavy, "(project ") != 5 || strings.Contains(heavy, "too-small") ||
		strings.Index(heavy, "shop (") > strings.Index(heavy, "unknown-idle (") {
		t.Fatalf("heavy alert should list the five largest sessions: %q", heavy)
	}
	idle := events[model.EventSessionsIdle]
	first, second := strings.Index(idle, "shop ("), strings.Index(idle, "shorter (")
	if !strings.Contains(idle, "idle sessions at 2") || first < 0 || second <= first ||
		strings.Contains(idle, "unknown-idle") || strings.Contains(idle, "too-small") {
		t.Fatalf("idle alert should list only qualifying sessions, longest first: %q", idle)
	}
}

func TestAlertSetActive(t *testing.T) {
	cfg := config.Default()
	cfg.Alerts.Window = time.Minute
	a := newAlertSet(cfg)
	at := time.Unix(1795464773, 0)
	for i := range 2 {
		sampleAt := at.Add(time.Duration(i) * time.Minute)
		a.observeLevel(model.EventTmpFull, sampleAt, 80, true)
		a.observeLevel(model.EventSwapFull, sampleAt, 90, true)
	}
	since := at.Add(time.Minute)
	want := []AlertView{
		{Name: "swap_full", Since: since, Value: 90, Threshold: 75},
		{Name: "tmp_full", Since: since, Value: 80, Threshold: 70},
	}
	if got := a.Active(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Active() = %+v, want %+v", got, want)
	}

	ev, fired := a.observeLevel(model.EventTmpFull, at.Add(2*time.Minute), 10, true)
	if !fired || ev.High || !ev.Since.Equal(since) {
		t.Fatalf("want an all-clear for the open episode, got %+v (%t)", ev, fired)
	}
	if got := a.Active(); !reflect.DeepEqual(got, want[:1]) {
		t.Fatalf("Active() after clearing = %+v, want %+v", got, want[:1])
	}
}

func TestAlertAllClear(t *testing.T) {
	at := time.Unix(1795464773, 0)
	for _, tc := range []struct {
		raise  model.EventKind
		normal model.EventKind
		value  float64
		detail string
	}{
		{model.EventTmpFull, "tmp_full_normal", 20, "/tmp usage back to 20% after 10m."},
		{model.EventSwapFull, "swap_full_normal", 10, "swap usage back to 10% after 10m."},
		{model.EventSessionsHeavy, "sessions_heavy_normal", float64(gib), "agent session memory back to 1.0 GiB after 10m."},
		{model.EventSessionsIdle, "sessions_idle_normal", 1, "idle sessions back to 1 after 10m."},
		{model.EventTmpOrphans, "tmp_orphans_normal", 512 << 20, "orphan and candidate /tmp items back to 512 MiB after 10m."},
	} {
		t.Run(string(tc.raise), func(t *testing.T) {
			var got model.Event
			emitAlert(t.Context(), func(_ context.Context, event model.Event) { got = event },
				tc.raise, at, levelalarm.Event{Since: at.Add(-10 * time.Minute), Value: tc.value}, "Current top items.")
			if got.Kind != tc.normal || got.Detail != tc.detail || got.Audience != model.AudienceOperator {
				t.Errorf("got %s / %s / %q, want %s / operator / %q", got.Kind, got.Audience, got.Detail, tc.normal, tc.detail)
			}
		})
	}
}
