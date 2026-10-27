// Package protocol implements the versioned newline-delimited JSON protocol
// spoken between the aq CLI and the daemon over a Unix socket.
//
// The server treats EOF on a connection as a client disconnect and cancels the
// handler's context. Clients must therefore keep their write side open until
// the final response arrives; half-close is not supported.
package protocol

import (
	"context"
	"encoding/json"
	"errors"
)

const Version = 1

// maxLine bounds a single protocol line so a peer cannot force unbounded reads.
const maxLine = 1 << 20

// maxResponseLine bounds a response line read by clients (results can be large).
const maxResponseLine = 64 << 20

const (
	CodeBadRequest          = "BAD_REQUEST"
	CodeUnsupportedVersion  = "UNSUPPORTED_VERSION"
	CodePermissionDenied    = "PERMISSION_DENIED"
	CodeNotFound            = "NOT_FOUND"
	CodeRejected            = "REJECTED"
	CodeIdempotencyConflict = "IDEMPOTENCY_CONFLICT"
	CodeInternal            = "INTERNAL"
)

// ErrDaemonUnavailable is returned by clients when the socket cannot be reached.
var ErrDaemonUnavailable = errors.New("daemon unavailable")

type Request struct {
	V    int             `json:"v"`
	Op   string          `json:"op"`
	Args json.RawMessage `json:"args,omitempty"`
}

// Response is either a final reply or, when Stream is set, an intermediate
// stream message.
type Response struct {
	V      int             `json:"v"`
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Stream json.RawMessage `json:"stream,omitempty"`
	Error  *Error          `json:"error,omitempty"`
}

type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// StreamWriter sends intermediate messages before the final response.
type StreamWriter interface {
	Send(v any) error
}

// Handler serves one request. A returned *Error is passed to the client as is;
// any other error becomes INTERNAL.
type Handler func(ctx context.Context, op string, args json.RawMessage, w StreamWriter) (any, error)
