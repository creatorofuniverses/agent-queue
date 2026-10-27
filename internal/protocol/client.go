package protocol

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"syscall"
)

// Call performs a request and decodes the result into out (may be nil).
func Call(ctx context.Context, socketPath, op string, args any, out any) error {
	return CallStream(ctx, socketPath, op, args, nil, out)
}

// CallStream is like Call but delivers intermediate stream messages to onStream.
func CallStream(ctx context.Context, socketPath, op string, args any, onStream func(json.RawMessage) error, out any) error {
	var rawArgs json.RawMessage
	if args != nil {
		b, err := json.Marshal(args)
		if err != nil {
			return err
		}
		rawArgs = b
	}
	reqLine, err := json.Marshal(Request{V: Version, Op: op, Args: rawArgs})
	if err != nil {
		return err
	}
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", socketPath)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED) {
			return fmt.Errorf("%w: %v", ErrDaemonUnavailable, err)
		}
		return err
	}
	defer c.Close()
	// Closing the conn unblocks reads when ctx ends.
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	ctxErr := func(err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	if _, err := c.Write(append(reqLine, '\n')); err != nil {
		return ctxErr(err)
	}
	br := bufio.NewReaderSize(c, 64*1024)
	for {
		line, err := readLine(br, maxResponseLine)
		if err != nil {
			return ctxErr(fmt.Errorf("read response: %w", err))
		}
		var r Response
		if err := json.Unmarshal(line, &r); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
		if r.Stream != nil {
			if onStream != nil {
				if err := onStream(r.Stream); err != nil {
					return err
				}
			}
			continue
		}
		if !r.OK {
			if r.Error == nil {
				return &Error{Code: CodeInternal, Message: "malformed error response"}
			}
			return r.Error
		}
		if out != nil && r.Result != nil {
			return json.Unmarshal(r.Result, out)
		}
		return nil
	}
}
