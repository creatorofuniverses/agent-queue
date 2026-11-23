package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/creatorofuniverses/agent-queue/internal/protocol"
	"github.com/creatorofuniverses/agent-queue/internal/tmpscan"
)

func stubInsight(t *testing.T, h *harness, op, wantArgs string, payload json.RawMessage, replyErr error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	go func() {
		done <- protocol.Serve(ctx, h.paths.Socket(), func(_ context.Context, gotOp string, args json.RawMessage, _ protocol.StreamWriter) (any, error) {
			if gotOp == "ping" {
				return nil, nil
			}
			if gotOp != op || wantArgs != "" && string(args) != wantArgs {
				return nil, fmt.Errorf("unexpected request %s %s", gotOp, args)
			}
			return payload, replyErr
		})
	}()
	ready, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	for protocol.Call(ready, h.paths.Socket(), "ping", nil, nil) != nil {
		if ready.Err() != nil {
			t.Fatal("test socket did not become ready")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestTmpTable(t *testing.T) {
	h := newHarness(t)
	now := time.Now()
	payload := json.RawMessage(fmt.Sprintf(`{
		"root":"/server-only/tmp","fs":{"size":10737418240,"free":6442450944,"inodes":250,"inodes_free":120,"tmpfs":true},"scanned_at":%q,"orphan_bytes":1181116006,"candidate_bytes":429496730,"complete":false,"partial_items":1,
		"items":[
			{"path":"/tmp/small","bytes":1048576,"last_write":%q,"owner":{"kind":"process","pid":42},"type":"venv","state":"in_use","partial":true},
			{"path":"/tmp/big","bytes":1181116006,"last_write":%q,"owner":{"kind":"unknown"},"type":"git","state":"orphan"},
			{"path":"/tmp/scratch","bytes":429496730,"last_write":%q,"owner":{"kind":"claude_project","project":"proj","session":"session-1"},"type":"claude_scratchpad","state":"candidate"}
		]
	}`, now.Add(-3*time.Minute).Format(time.RFC3339Nano), now.Add(-time.Hour).Format(time.RFC3339Nano), now.Add(-25*time.Hour).Format(time.RFC3339Nano), now.Add(-26*time.Hour).Format(time.RFC3339Nano)))
	stubInsight(t, h, "tmp", `{"rescan":true}`, payload, nil)
	code, out, errout := h.run("tmp", "--rescan")
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if code != 0 || errout != "" || len(lines) != 7 ||
		!strings.HasPrefix(lines[0], "tmp: 4.0 of 10.0 GiB;") ||
		!strings.HasSuffix(lines[0], "; orphans 1.1 GiB; candidates 0.4 GiB; scanned 3m ago; partial items: 1; incomplete pass: tmp_orphans paused") ||
		strings.Join(strings.Fields(lines[1]), " ") != "PATH SIZE LAST WRITE OWNER TYPE STATE" ||
		strings.Join(strings.Fields(lines[2]), " ") != "/tmp/big 1.1 GiB 25h ago unknown git orphan" ||
		strings.Join(strings.Fields(lines[3]), " ") != "/tmp/scratch 409 MiB 26h ago proj/session-1 claude_scratchpad candidate" ||
		strings.Join(strings.Fields(lines[4]), " ") != "/tmp/small 1 MiB* 1h ago pid 42 venv in_use" ||
		!strings.HasPrefix(lines[5], "* ") {
		t.Fatalf("want size-sorted inventory with partial marker, got exit %d, stdout %q, stderr %q", code, out, errout)
	}
}

func TestTmpShowsLargestThirtyWithFullStateTotals(t *testing.T) {
	h := newHarness(t)
	report := tmpscan.Report{Root: h.cwd, Complete: true}
	for i := 1; i <= 32; i++ {
		item := tmpscan.Item{Path: fmt.Sprintf("/tmp/item-%02d", i), Bytes: uint64(i) << 10, State: "recent"}
		if i <= 2 {
			item.State = "orphan"
			report.OrphanBytes += item.Bytes
		}
		report.Items = append(report.Items, item)
	}
	payload, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	stubInsight(t, h, "tmp", `{"rescan":false}`, payload, nil)
	totals := "states: aq_task 0 (0 B), in_use 0 (0 B), live 0 (0 B), candidate 0 (0 B), orphan 2 (3 KiB), recent 30 (525 KiB)"
	code, out, errout := h.run("tmp")
	if code != 0 || errout != "" || strings.Count(out, "/tmp/item-") != 30 ||
		!strings.Contains(out, "/tmp/item-32") || !strings.Contains(out, "/tmp/item-03") ||
		strings.Contains(out, "/tmp/item-02") || strings.Contains(out, "tmp_orphans paused") ||
		!strings.HasSuffix(out, totals+"\n… and 2 more (use --all)\n") {
		t.Fatalf("default inventory: exit %d, stdout %q, stderr %q", code, out, errout)
	}
	code, out, errout = h.run("tmp", "--all")
	if code != 0 || errout != "" || strings.Count(out, "/tmp/item-") != 32 ||
		!strings.HasSuffix(out, totals+"\n") {
		t.Fatalf("full inventory: exit %d, stdout %q, stderr %q", code, out, errout)
	}
	code, out, errout = h.run("tmp", "--json")
	if code != 0 || errout != "" || out != string(payload)+"\n" {
		t.Fatalf("JSON inventory: exit %d, stdout %q, stderr %q", code, out, errout)
	}
}

func TestTmpJSON(t *testing.T) {
	h := newHarness(t)
	payload := json.RawMessage(`{"root":"/missing","scanned_at":"2026-11-24T00:00:00Z","items":[],"orphan_bytes":0,"candidate_bytes":0,"complete":true,"fs":{"size":1024,"free":100,"inodes":100,"inodes_free":95,"tmpfs":true},"future_field":42}`)
	stubInsight(t, h, "tmp", `{"rescan":false}`, payload, nil)
	code, out, errout := h.run("tmp", "--json")
	if code != 0 || errout != "" || out != string(payload)+"\n" {
		t.Fatalf("want report verbatim, got exit %d, stdout %q, stderr %q", code, out, errout)
	}
}

func TestTmpUnavailable(t *testing.T) {
	h := newHarness(t)
	stubInsight(t, h, "tmp", `{"rescan":false}`, nil, &protocol.Error{Code: protocol.CodeUnavailable, Message: "no /tmp scan available yet"})
	code, out, errout := h.run("tmp")
	if code != exitUnavailable || out != "" || errout != "aq: no /tmp scan yet; try again in a minute or use --rescan\n" {
		t.Fatalf("want first-scan hint, got exit %d, stdout %q, stderr %q", code, out, errout)
	}
}
