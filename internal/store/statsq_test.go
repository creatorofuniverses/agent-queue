package store

import (
	"context"
	"encoding/json"
	"github.com/creatorofuniverses/agent-queue/internal/model"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestExportColumnsNoSecrets(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	now := time.Now().UTC()
	it := newItem(0, now)
	it.Argv = []string{"secret-argv"}
	it.Env = map[string]string{"K": "secret-env"}
	it.Cwd = "secret-cwd"
	it.SessionID = "secret-session"
	it.Diagnostic = "secret-diagnostic"
	s.InsertItem(ctx, it)
	s.AddEvents(ctx, []model.Event{{ID: "event", ItemID: it.ID, Kind: model.EventTaskFinished, Audience: model.AudienceSession, Detail: "secret-detail", LastError: "secret-error", CreatedAt: now}})
	var all []byte
	for _, table := range []string{"items", "runs", "host", "incidents", "events", "hook"} {
		if e := s.Export(ctx, table, time.Time{}, func(cols []string, rows [][]any) error {
			if table == "items" {
				want := strings.Split("id,kind,name,profile,project,client_type,priority,state,reason,created_at,admitted_at,started_at,ended_at,memory_limit,memory_expected,cpus,timeout_seconds,estimated_seconds,memory_from_profile,memory_peak,exit_code,within,task_memory,task_cpus,docker_project,extend_count,teardown_attempt,expired_at,logs_pruned,io_write_bytes,io_read_bytes", ",")
				if !reflect.DeepEqual(cols, want) {
					t.Fatal(cols)
				}
			}
			b, e := json.Marshal(rows)
			all = append(all, b...)
			return e
		}); e != nil {
			t.Fatal(e)
		}
	}
	for _, v := range []string{"secret-argv", "secret-env", "secret-cwd", "secret-session", "secret-diagnostic", "secret-detail", "secret-error"} {
		if strings.Contains(string(all), v) {
			t.Fatal(v)
		}
	}
	if e := s.Export(ctx, "bad", now, func([]string, [][]any) error { return nil }); e == nil {
		t.Fatal("unknown")
	}
}
func TestExportBatchesAndSince(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	now := time.Now().UTC()
	for i := 0; i < 501; i++ {
		if e := s.AddIncident(ctx, model.Incident{At: now, Kind: "kernel_oom"}); e != nil {
			t.Fatal(e)
		}
	}
	sizes := []int{}
	if e := s.Export(ctx, "incidents", now, func(c []string, r [][]any) error { sizes = append(sizes, len(r)); return nil }); e != nil || !reflect.DeepEqual(sizes, []int{500, 1}) {
		t.Fatal(sizes, e)
	}
}
func TestPSIClosedAt(t *testing.T) {
	s := openTemp(t)
	at := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	if v, e := s.PSIClosedAt(t.Context(), at); e != nil || v {
		t.Fatal(v, e)
	}
	for _, inc := range []model.Incident{{At: at.Add(-20 * time.Minute), Kind: "psi_close"}, {At: at.Add(-15 * time.Minute), Kind: "psi_open"}, {At: at.Add(-10 * time.Minute), Kind: "psi_close"}, {At: at.Add(-5 * time.Minute), Kind: "kernel_oom", Value: 1}, {At: at.Add(5 * time.Minute), Kind: "psi_open"}} {
		if e := s.AddIncident(t.Context(), inc); e != nil {
			t.Fatal(e)
		}
	}
	if v, e := s.PSIClosedAt(t.Context(), at); e != nil || !v {
		t.Fatal(v, e)
	}
	if v, e := s.PSIClosedAt(t.Context(), at.Add(-12*time.Minute)); e != nil || v {
		t.Fatal(v, e)
	}
}
