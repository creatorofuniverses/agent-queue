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
	if _, err := c.Write([]byte(`{"v":2,"op":"x"}` + "\n")); err != nil {
		t.Fatal(err)
	}
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
	if _, err := c.Write([]byte("not json\n")); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(c).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var r Response
	if err := json.Unmarshal(line, &r); err != nil {
		t.Fatal(err)
	}
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
	dir, err := os.MkdirTemp("", "aqp")
	if err != nil {
		t.Fatal(err)
	}
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

func rawRequest(t *testing.T, sock, line string) net.Conn {
	t.Helper()
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write([]byte(line + "\n")); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestDisconnectCancelsHandler(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	sock := startServer(t, func(ctx context.Context, op string, args json.RawMessage, w StreamWriter) (any, error) {
		close(started)
		<-ctx.Done()
		close(cancelled)
		return nil, ctx.Err()
	})
	c := rawRequest(t, sock, `{"v":1,"op":"wait"}`)
	<-started
	c.Close()
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("handler ctx not cancelled on disconnect")
	}
}

func TestIdleConnDoesNotBlockShutdown(t *testing.T) {
	dir, _ := os.MkdirTemp("", "aqp")
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "aq.sock")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, sock, echo) }()
	var c net.Conn
	var err error
	for i := 0; i < 200; i++ {
		if c, err = net.Dial("unix", sock); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	time.Sleep(50 * time.Millisecond) // let the server accept it
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return with an idle connection")
	}
}

func TestServeRefusesNonSocket(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "aq.sock")
	if err := os.WriteFile(path, []byte("precious"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Serve(context.Background(), path, echo); err == nil {
		t.Fatal("expected error")
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != "precious" {
		t.Fatalf("file damaged: %q %v", b, err)
	}
}

func TestHandlerPanic(t *testing.T) {
	sock := startServer(t, func(ctx context.Context, op string, args json.RawMessage, w StreamWriter) (any, error) {
		if op == "panic" {
			panic("kaboom")
		}
		return "ok", nil
	})
	var pe *Error
	err := Call(context.Background(), sock, "panic", nil, nil)
	if !errors.As(err, &pe) || pe.Code != "INTERNAL" {
		t.Fatalf("got %v", err)
	}
	if err := Call(context.Background(), sock, "fine", nil, nil); err != nil {
		t.Fatalf("server did not survive: %v", err)
	}
}

func TestPermissionDenied(t *testing.T) {
	dir, _ := os.MkdirTemp("", "aqp")
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		serveConn(context.Background(), c.(*net.UnixConn), uint32(os.Getuid())+1, echo)
	}()
	err = Call(context.Background(), sock, "ping", nil, nil)
	var pe *Error
	if !errors.As(err, &pe) || pe.Code != "PERMISSION_DENIED" {
		t.Fatalf("got %v", err)
	}
}
