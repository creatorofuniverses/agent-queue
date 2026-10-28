package store

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/creatorofuniverses/agent-queue/internal/hook"
	"github.com/creatorofuniverses/agent-queue/internal/model"
	"github.com/creatorofuniverses/agent-queue/internal/profile"
	"strings"
	"time"
)

func (s *Store) ItemsSince(ctx context.Context, since time.Time, name string) ([]*model.Item, error) {
	q := `SELECT ` + itemCols + ` FROM items WHERE created_at>=?`
	args := []any{fmtTime(since)}
	if name != "" {
		q += ` AND profile=?`
		args = append(args, name)
	}
	return s.queryItems(ctx, q+` ORDER BY created_at,rowid`, args...)
}
func (s *Store) RunsSince(ctx context.Context, since time.Time, name string) ([]profile.Run, error) {
	q := `SELECT ` + runCols + ` FROM profile_runs WHERE ended_at>=?`
	args := []any{fmtTime(since)}
	if name != "" {
		q += ` AND profile=?`
		args = append(args, name)
	}
	return s.queryRuns(ctx, q+` ORDER BY ended_at,rowid`, args...)
}
func (s *Store) EventsSince(ctx context.Context, since time.Time) ([]model.Event, error) {
	return queryEvents(ctx, s.db, `SELECT `+eventCols+` FROM events WHERE created_at>=? ORDER BY created_at,rowid`, fmtTime(since))
}

var ExportTables = []string{"items", "runs", "host", "incidents", "events", "hook"}
var itemExportColumns = strings.Split("id,kind,name,profile,project,client_type,priority,state,reason,created_at,admitted_at,started_at,ended_at,memory_limit,memory_expected,cpus,timeout_seconds,estimated_seconds,memory_from_profile,memory_peak,exit_code,within,task_memory,task_cpus,docker_project,extend_count,teardown_attempt,expired_at,logs_pruned", ",")
var runExportColumns = strings.Split("item_id,profile,ended_at,state,reason,usable,cpus,memory_limit,memory_expected,memory_from_profile,estimated_seconds,duration_seconds,memory_peak,samples,sample_p90", ",")

// Export closes each read cursor before emitting a batch, so slow clients hold no SQL transaction.
func (s *Store) Export(ctx context.Context, table string, since time.Time, emit func([]string, [][]any) error) error {
	if table == "hook" {
		hits, e := hook.ReadHits(s.stateDir, since)
		if e != nil {
			return e
		}
		cols := []string{"at", "pattern", "client"}
		if len(hits) == 0 {
			return emit(cols, [][]any{})
		}
		for len(hits) > 0 {
			n := min(500, len(hits))
			rows := make([][]any, n)
			for i, h := range hits[:n] {
				rows[i] = []any{fmtTime(h.At), h.Pattern, h.Client}
			}
			if e = emit(cols, rows); e != nil {
				return e
			}
			hits = hits[n:]
		}
		return nil
	}
	var cols []string
	var selectSQL, from, at string
	switch table {
	case "items":
		cols = itemExportColumns
		from = "items"
		at = "created_at"
		selectSQL = `id,kind,name,profile,project,client_type,CASE priority WHEN 100 THEN 'high' WHEN -10 THEN 'low' ELSE 'normal' END,state,reason,created_at,admitted_at,started_at,ended_at,mem_limit,mem_expected,cpus,timeout_ns/1000000000.0,est_duration_ns/1000000000.0,memory_from_profile,memory_peak,exit_code,parent_id,task_memory,task_cpus,docker_project,extend_count,teardown_attempt,expired_at,logs_pruned`
	case "runs":
		cols = runExportColumns
		from = "profile_runs"
		at = "ended_at"
		selectSQL = `item_id,profile,ended_at,state,reason,CASE WHEN ` + usableRunSQL + ` THEN 1 ELSE 0 END,cpus,mem_limit,mem_expected,memory_from_profile,est_duration_ns/1000000000.0,duration_ns/1000000000.0,memory_peak,samples,hist`
	case "host":
		cols = strings.Split(strings.ReplaceAll(hostCols, " ", ""), ",")
		from = "host_samples"
		at = "at"
		selectSQL = hostCols
	case "incidents":
		cols = []string{"at", "kind", "value", "detail"}
		from = "incidents"
		at = "at"
		selectSQL = strings.Join(cols, ",")
	case "events":
		cols = []string{"id", "item_id", "kind", "audience", "client_type", "channel", "status", "attempts", "created_at", "sent_at", "acked_at"}
		from = "events"
		at = "created_at"
		selectSQL = strings.Join(cols, ",")
	default:
		return fmt.Errorf("unknown export table %q", table)
	}
	var cursor int64
	for {
		rows, e := s.db.QueryContext(ctx, `SELECT rowid,`+selectSQL+` FROM `+from+` WHERE `+at+`>=? AND rowid>? ORDER BY rowid LIMIT 500`, fmtTime(since), cursor)
		if e != nil {
			return e
		}
		var batch [][]any
		for rows.Next() {
			values := make([]any, len(cols)+1)
			ptrs := make([]any, len(values))
			for i := range values {
				ptrs[i] = &values[i]
			}
			if e = rows.Scan(ptrs...); e != nil {
				break
			}
			cursor = values[0].(int64)
			v := values[1:]
			if table == "runs" {
				var h profile.Histogram
				var b []byte
				switch x := v[14].(type) {
				case string:
					b = []byte(x)
				case []byte:
					b = x
				}
				if e = json.Unmarshal(b, &h); e != nil {
					break
				}
				v[14] = h.Quantile(90)
			}
			batch = append(batch, v)
		}
		if e == nil {
			e = rows.Err()
		}
		rows.Close()
		if e != nil {
			return e
		}
		if len(batch) == 0 {
			if cursor == 0 {
				return emit(cols, [][]any{})
			}
			return nil
		}
		if e = emit(cols, batch); e != nil {
			return e
		}
		if len(batch) < 500 {
			return nil
		}
	}
}
