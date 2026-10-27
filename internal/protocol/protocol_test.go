package protocol

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func startServer(t *testing.T, h Handler) string {
	t.Helper()
	// Short path: unix socket paths are limited to ~108 bytes.
	dir, err := os.MkdirTemp("", "aqp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "run", "aq.sock")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, sock, h) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	for i := 0; i < 200; i++ {
		if c, err := net.Dial("unix", sock); err == nil {
			c.Close()
			return sock
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("server did not start")
	return ""
}

func echo(ctx context.Context, op string, args json.RawMessage, w StreamWriter) (any, error) {
	var v any
	if len(args) > 0 {
		if err := json.Unmarshal(args, &v); err != nil {
			return nil, &Error{Code: CodeBadRequest, Message: err.Error()}
		}
	}
	return map[string]any{"op": op, "args": v}, nil
}

func TestRoundTrip(t *testing.T) {
	sock := startServer(t, echo)
	var out struct {
		Op   string
		Args map[string]int
	}
	if err := Call(context.Background(), sock, "ping", map[string]int{"a": 1}, &out); err != nil {
		t.Fatal(err)
	}
	if out.Op != "ping" || out.Args["a"] != 1 {
		t.Fatalf("got %+v", out)
	}
}

func TestErrorPassthrough(t *testing.T) {
	sock := startServer(t, func(ctx context.Context, op string, args json.RawMessage, w StreamWriter) (any, error) {
		if op == "plain" {
			return nil, errors.New("boom")
		}
		return nil, &Error{Code: CodeNotFound, Message: "no such job"}
	})
	err := Call(context.Background(), sock, "get", nil, nil)
	var pe *Error
	if !errors.As(err, &pe) || pe.Code != "NOT_FOUND" || pe.Message != "no such job" {
		t.Fatalf("got %v", err)
	}
	err = Call(context.Background(), sock, "plain", nil, nil)
	if !errors.As(err, &pe) || pe.Code != "INTERNAL" {
		t.Fatalf("got %v", err)
	}
}

func TestUnsupportedVersion(t *testing.T) {
	sock := startServer(t, echo)
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte(`{"v":2,"op":"x"}` + "\n"))
	line, err := bufio.NewReader(c).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var r Response
	if err := json.Unmarshal(line, &r); err != nil {
		t.Fatal(err)
	}
	if r.OK || r.Error == nil || r.Error.Code != "UNSUPPORTED_VERSION" {
		t.Fatalf("got %s", line)
	}
}

func TestBadRequest(t *testing.T) {
	sock := startServer(t, echo)
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte("not json\n"))
	line, _ := bufio.NewReader(c).ReadBytes('\n')
	var r Response
	json.Unmarshal(line, &r)
	if r.Error == nil || r.Error.Code != "BAD_REQUEST" {
		t.Fatalf("got %s", line)
	}
}

func TestStream(t *testing.T) {
	sock := startServer(t, func(ctx context.Context, op string, args json.RawMessage, w StreamWriter) (any, error) {
		for i := 0; i < 3; i++ {
			if err := w.Send(map[string]int{"n": i}); err != nil {
				return nil, err
			}
		}
		return "done", nil
	})
	var got []int
	var out string
	err := CallStream(context.Background(), sock, "logs", nil, func(m json.RawMessage) error {
		var v struct{ N int }
		if err := json.Unmarshal(m, &v); err != nil {
			return err
		}
		got = append(got, v.N)
		return nil
	}, &out)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0] != 0 || got[2] != 2 || out != "done" {
		t.Fatalf("got %v %q", got, out)
	}
}

func TestDaemonUnavailable(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "nope.sock")
	err := Call(context.Background(), sock, "ping", nil, nil)
	if !errors.Is(err, ErrDaemonUnavailable) {
		t.Fatalf("got %v", err)
	}
}

func TestSocketPermissions(t *testing.T) {
	sock := startServer(t, echo)
	fi, err := os.Stat(sock)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode %o", fi.Mode().Perm())
	}
	di, err := os.Stat(filepath.Dir(sock))
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %o", di.Mode().Perm())
	}
}

func TestStaleSocketAndCleanup(t *testing.T) {
	dir, _ := os.MkdirTemp("", "aqp")
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "aq.sock")
	// Leave a stale socket file behind.
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	l.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, sock, echo) }()
	var cerr error
	for i := 0; i < 200; i++ {
		if cerr = Call(ctx, sock, "ping", nil, nil); cerr == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if cerr != nil {
		t.Fatal(cerr)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Fatalf("socket not removed: %v", err)
	}
}

func TestCallHonorsContext(t *testing.T) {
	sock := startServer(t, func(ctx context.Context, op string, args json.RawMessage, w StreamWriter) (any, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := Call(ctx, sock, "wait", nil, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("did not return promptly")
	}
}
