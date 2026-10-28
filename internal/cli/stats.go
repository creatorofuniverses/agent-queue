package cli

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"github.com/creatorofuniverses/agent-queue/internal/daemon"
	"github.com/creatorofuniverses/agent-queue/internal/stats"
	"github.com/creatorofuniverses/agent-queue/internal/units"
	"time"
)

func cmdStats(c *cmdCtx, args []string) error {
	export := len(args) > 0 && args[0] == "export"
	if export {
		args = args[1:]
	}
	fs := c.newFlagSet("stats")
	since := fs.String("since", "7d", "history window `DURATION`, e.g. 1d or 12h")
	asJSON := fs.Bool("json", false, "JSON output")
	name := fs.String("profile", "", "only this profile `KEY` or task name")
	format := fs.String("format", "jsonl", "export `FORMAT`: jsonl or csv")
	table := fs.String("table", "", "export `TABLE`: items, runs, host, incidents, events or hook (csv needs one)")
	pos, e := parseFlags(fs, args)
	if e != nil {
		return e
	}
	if len(pos) > 0 {
		return usagef("stats: unexpected arguments")
	}
	duration, e := units.ParseDuration(*since)
	if e != nil {
		return usagef("stats: invalid --since: %v", e)
	}
	at := time.Now().Add(-duration).UTC().Format(time.RFC3339Nano)
	if export {
		if *format != "jsonl" && *format != "csv" {
			return usagef("stats export: --format must be jsonl or csv")
		}
		if *format == "csv" && *table == "" {
			return usagef("stats export: csv requires --table")
		}
		if *name != "" {
			return usagef("stats export: --profile is unsupported")
		}
		paths, e := c.paths()
		if e != nil {
			return e
		}
		enc := json.NewEncoder(c.stdout)
		csvw := csv.NewWriter(c.stdout)
		header := false
		e = callStreamOn(c.ctx, paths, "stats_export", map[string]string{"since": at, "table": *table}, func(raw json.RawMessage) error {
			// UseNumber keeps integers out of %g notation in CSV cells.
			var chunk daemon.ExportChunk
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.UseNumber()
			if e := dec.Decode(&chunk); e != nil {
				return e
			}
			if *format == "csv" && !header {
				if e := csvw.Write(chunk.Columns); e != nil {
					return e
				}
				header = true
			}
			for _, row := range chunk.Rows {
				if *format == "jsonl" {
					obj := map[string]any{"table": chunk.Table}
					for i, k := range chunk.Columns {
						obj[k] = row[i]
					}
					if e := enc.Encode(obj); e != nil {
						return e
					}
				} else {
					cells := make([]string, len(row))
					for i, v := range row {
						if v != nil {
							cells[i] = fmt.Sprint(v)
						}
					}
					if e := csvw.Write(cells); e != nil {
						return e
					}
				}
			}
			return nil
		})
		csvw.Flush()
		if e != nil {
			return e
		}
		return csvw.Error()
	}
	var v stats.Summary
	if e = c.call(c.ctx, "stats", map[string]string{"since": at, "profile": c.profileKey(*name)}, &v); e != nil {
		return e
	}
	if *asJSON {
		return printJSON(c, v)
	}
	c.printStats(v)
	return nil
}
