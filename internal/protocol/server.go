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
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// requestTimeout bounds how long a peer may take to send its request line.
const requestTimeout = 10 * time.Second

// Serve listens on socketPath until ctx is cancelled, then closes all
// connections and removes the socket. Each connection carries exactly one
// request. Handlers must honor their ctx: Serve waits for them on shutdown.
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
	if fi, err := os.Lstat(socketPath); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("%s exists and is not a socket", socketPath)
		}
		if err := os.Remove(socketPath); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
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
	var mu sync.Mutex
	conns := map[net.Conn]struct{}{}
	go func() {
		<-ctx.Done()
		ln.Close() // also unlinks the socket file
		mu.Lock()
		for c := range conns {
			c.Close()
		}
		mu.Unlock()
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
			if retryableAccept(err) {
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(50 * time.Millisecond):
				}
				continue
			}
			return err
		}
		mu.Lock()
		if ctx.Err() != nil {
			mu.Unlock()
			c.Close()
			return nil
		}
		conns[c] = struct{}{}
		mu.Unlock()
		wg.Add(1)
		go func() {
			defer wg.Done()
			serveConn(ctx, c.(*net.UnixConn), uid, h)
			mu.Lock()
			delete(conns, c)
			mu.Unlock()
		}()
	}
}

func retryableAccept(err error) bool {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return errors.Is(err, syscall.EMFILE) || errors.Is(err, syscall.ENFILE) ||
		errors.Is(err, syscall.ECONNABORTED) || errors.Is(err, syscall.ENOBUFS)
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
	c.SetReadDeadline(time.Now().Add(requestTimeout))
	line, err := readLine(br, maxLine)
	c.SetReadDeadline(time.Time{})
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
	res, err := callHandler(hctx, h, req, w)
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

// callHandler runs h, turning a panic into an INTERNAL error.
func callHandler(ctx context.Context, h Handler, req Request, w StreamWriter) (res any, err error) {
	defer func() {
		if r := recover(); r != nil {
			res, err = nil, &Error{Code: CodeInternal, Message: fmt.Sprint("handler panic: ", r)}
		}
	}()
	return h(ctx, req.Op, req.Args, w)
}

var errTooLong = errors.New("line too long")

// readLine reads one newline-terminated line of at most limit bytes.
func readLine(br *bufio.Reader, limit int) ([]byte, error) {
	var buf []byte
	for {
		part, err := br.ReadSlice('\n')
		buf = append(buf, part...)
		if len(buf) > limit {
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
