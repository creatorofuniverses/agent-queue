package protocol

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/sys/unix"
)

// Serve listens on socketPath until ctx is cancelled, then removes the socket.
// Each connection carries exactly one request.
func Serve(ctx context.Context, socketPath string, h Handler) error {
	dir := filepath.Dir(socketPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	// A stale socket may remain from a crashed daemon; the daemon lock
	// guarantees no live instance owns it.
	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return err
	}
	if err := os.Chmod(socketPath, 0o600); err != nil {
		ln.Close()
		return err
	}
	var wg sync.WaitGroup
	go func() {
		<-ctx.Done()
		ln.Close() // also unlinks the socket file
	}()
	defer func() {
		wg.Wait()
		os.Remove(socketPath)
	}()
	uid := uint32(os.Getuid())
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			serveConn(ctx, c.(*net.UnixConn), uid, h)
		}()
	}
}

func peerUID(c *net.UnixConn) (uint32, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return 0, err
	}
	var cred *unix.Ucred
	var serr error
	if err := raw.Control(func(fd uintptr) {
		cred, serr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if serr != nil {
		return 0, serr
	}
	return cred.Uid, nil
}

type connWriter struct {
	c  net.Conn
	mu sync.Mutex
}

func (w *connWriter) write(r Response) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	_, err = w.c.Write(append(b, '\n'))
	return err
}

func (w *connWriter) Send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return w.write(Response{V: Version, OK: true, Stream: b})
}

func fail(code, msg string) Response {
	return Response{V: Version, Error: &Error{Code: code, Message: msg}}
}

func serveConn(ctx context.Context, c *net.UnixConn, uid uint32, h Handler) {
	defer c.Close()
	w := &connWriter{c: c}
	if got, err := peerUID(c); err != nil || got != uid {
		w.write(fail(CodePermissionDenied, "peer uid mismatch"))
		return
	}
	br := bufio.NewReaderSize(c, 64*1024)
	line, err := readLine(br)
	if err != nil {
		if errors.Is(err, errTooLong) {
			w.write(fail(CodeBadRequest, "request too large"))
		}
		return
	}
	var req Request
	if err := json.Unmarshal(line, &req); err != nil {
		w.write(fail(CodeBadRequest, "malformed request: "+err.Error()))
		return
	}
	if req.V != Version {
		w.write(fail(CodeUnsupportedVersion, fmt.Sprintf("unsupported protocol version %d (want %d)", req.V, Version)))
		return
	}
	if req.Op == "" {
		w.write(fail(CodeBadRequest, "missing op"))
		return
	}
	// Cancel the handler when the client goes away (EOF after the request).
	hctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		io.Copy(io.Discard, br)
		cancel()
	}()
	res, err := h(hctx, req.Op, req.Args, w)
	if err != nil {
		var pe *Error
		if errors.As(err, &pe) {
			w.write(Response{V: Version, Error: pe})
		} else {
			w.write(fail(CodeInternal, err.Error()))
		}
		return
	}
	b, err := json.Marshal(res)
	if err != nil {
		w.write(fail(CodeInternal, "encode result: "+err.Error()))
		return
	}
	w.write(Response{V: Version, OK: true, Result: b})
}

var errTooLong = errors.New("line too long")

// readLine reads one newline-terminated line of at most maxLine bytes.
func readLine(br *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		part, err := br.ReadSlice('\n')
		buf = append(buf, part...)
		if len(buf) > maxLine {
			return nil, errTooLong
		}
		if err == nil {
			return buf, nil
		}
		if err != bufio.ErrBufferFull {
			return nil, err
		}
	}
}
