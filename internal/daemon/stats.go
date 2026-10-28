package daemon

import (
	"context"
	"encoding/json"
	"resource-dispatcher/internal/hook"
	"resource-dispatcher/internal/protocol"
	"resource-dispatcher/internal/stats"
	"resource-dispatcher/internal/store"
	"slices"
	"time"
)

type statsArgs struct {
	Since   string `json:"since"`
	Profile string `json:"profile"`
}
type ExportChunk struct {
	Table   string   `json:"table"`
	Columns []string `json:"columns"`
	Rows    [][]any  `json:"rows"`
}

func statsSince(s string) (time.Time, error) {
	if s == "" {
		return time.Now().Add(-7 * 24 * time.Hour), nil
	}
	t, e := time.Parse(time.RFC3339Nano, s)
	if e != nil {
		return t, badRequest("invalid since: %v", e)
	}
	return t, nil
}
func (d *server) stats(ctx context.Context, raw json.RawMessage) (any, error) {
	var a statsArgs
	if e := decode(raw, &a); e != nil {
		return nil, e
	}
	since, e := statsSince(a.Since)
	if e != nil {
		return nil, e
	}
	in := stats.Input{Since: since, Until: time.Now().UTC(), Profile: a.Profile, Patterns: d.cfg.Hook.Patterns, SliceName: d.cfg.SliceName, TeardownSliceName: d.cfg.TeardownSliceName()}
	if in.Items, e = d.st.ItemsSince(ctx, since, a.Profile); e != nil {
		return nil, e
	}
	if in.Runs, e = d.st.RunsSince(ctx, since, a.Profile); e != nil {
		return nil, e
	}
	if a.Profile == "" {
		if in.Events, e = d.st.EventsSince(ctx, since); e != nil {
			return nil, e
		}
		if in.Samples, e = d.st.HostSamples(ctx, since); e != nil {
			return nil, e
		}
		if in.Incidents, e = d.st.Incidents(ctx, since); e != nil {
			return nil, e
		}
		if in.PSIClosedAtSince, e = d.st.PSIClosedAt(ctx, since); e != nil {
			return nil, e
		}
		if in.Hits, e = hook.ReadHits(d.paths.StateDir, since); e != nil {
			return nil, e
		}
	}
	return stats.Summarize(in), nil
}
func (d *server) statsExport(ctx context.Context, raw json.RawMessage, w protocol.StreamWriter) (any, error) {
	var a struct {
		Since string `json:"since"`
		Table string `json:"table"`
	}
	if e := decode(raw, &a); e != nil {
		return nil, e
	}
	since, e := statsSince(a.Since)
	if e != nil {
		return nil, e
	}
	tables := store.ExportTables
	if a.Table != "" {
		if !slices.Contains(tables, a.Table) {
			return nil, badRequest("unknown export table %q", a.Table)
		}
		tables = []string{a.Table}
	}
	for _, table := range tables {
		if e = d.st.Export(ctx, table, since, func(cols []string, rows [][]any) error {
			return w.Send(ExportChunk{Table: table, Columns: cols, Rows: rows})
		}); e != nil {
			return nil, e
		}
	}
	return nil, nil
}
