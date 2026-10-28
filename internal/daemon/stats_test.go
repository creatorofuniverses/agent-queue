package daemon

import (
	"context"
	"encoding/json"
	"github.com/creatorofuniverses/agent-queue/internal/protocol"
	"github.com/creatorofuniverses/agent-queue/internal/stats"
	"testing"
)

func TestStatsOps(t *testing.T) {
	e := newEnv(t)
	e.start(t)
	var v ItemView
	if err := e.call(t, "submit", e.submitArgs(100*mib), &v); err != nil {
		t.Fatal(err)
	}
	var s stats.Summary
	if err := e.call(t, "stats", nil, &s); err != nil || len(s.Items) != 1 {
		t.Fatal(s, err)
	}
	chunks := 0
	err := protocol.CallStream(context.Background(), e.paths.Socket(), "stats_export", map[string]string{"table": "items"}, func(raw json.RawMessage) error {
		var c ExportChunk
		if e := json.Unmarshal(raw, &c); e != nil {
			return e
		}
		if c.Table != "items" || len(c.Rows) != 1 {
			t.Fatal(c)
		}
		chunks++
		return nil
	}, nil)
	if err != nil || chunks != 1 {
		t.Fatal(chunks, err)
	}
	if err = e.call(t, "stats_export", map[string]string{"table": "bad"}, nil); code(err) != protocol.CodeBadRequest {
		t.Fatal(err)
	}
}
