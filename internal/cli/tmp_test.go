package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/creatorofuniverses/agent-queue/internal/protocol"
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
		"root":%q,"scanned_at":%q,"orphan_bytes":1181116006,"candidate_bytes":429496730,"complete":false,
		"items":[
			{"path":"/tmp/small","bytes":1048576,"last_write":%q,"owner":{"kind":"process","pid":42},"type":"venv","state":"in_use","partial":true},
			{"path":"/tmp/big","bytes":1181116006,"last_write":%q,"owner":{"kind":"unknown"},"type":"git","state":"orphan"},
			{"path":"/tmp/scratch","bytes":429496730,"last_write":%q,"owner":{"kind":"claude_project","project":"proj","session":"session-1"},"type":"claude_scratchpad","state":"candidate"}
		]
	}`, h.cwd, now.Add(-3*time.Minute).Format(time.RFC3339Nano), now.Add(-time.Hour).Format(time.RFC3339Nano), now.Add(-25*time.Hour).Format(time.RFC3339Nano), now.Add(-26*time.Hour).Format(time.RFC3339Nano)))
	stubInsight(t, h, "tmp", `{"rescan":true}`, payload, nil)
	code, out, errout := h.run("tmp", "--rescan")
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if code != 0 || errout != "" || len(lines) != 6 ||
		!strings.HasPrefix(lines[0], "tmp: ") || !strings.Contains(lines[0], " of ") ||
		!strings.HasSuffix(lines[0], "; orphans 1.1 GiB; candidates 0.4 GiB; scanned 3m ago") ||
		strings.Join(strings.Fields(lines[1]), " ") != "PATH SIZE LAST WRITE OWNER TYPE STATE" ||
		strings.Join(strings.Fields(lines[2]), " ") != "/tmp/big 1.1 GiB 25h ago unknown git orphan" ||
		strings.Join(strings.Fields(lines[3]), " ") != "/tmp/scratch 409 MiB 26h ago proj/session-1 claude_scratchpad candidate" ||
		strings.Join(strings.Fields(lines[4]), " ") != "/tmp/small 1 MiB* 1h ago pid 42 venv in_use" ||
		!strings.HasPrefix(lines[5], "* ") {
		t.Fatalf("want size-sorted inventory with partial marker, got exit %d, stdout %q, stderr %q", code, out, errout)
	}
}

func TestTmpJSON(t *testing.T) {
	h := newHarness(t)
	payload := json.RawMessage(`{"root":"/missing","scanned_at":"2026-11-24T00:00:00Z","items":[],"orphan_bytes":0,"candidate_bytes":0,"complete":true,"future_field":42}`)
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
