// Package model defines the shared types of the aq queue.
package model

import (
	"crypto/rand"
	"fmt"
	"time"
)

type ItemID string

// NewID returns a random UUIDv4 in canonical form.
func NewID() ItemID {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return ItemID(fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]))
}

type Kind string

const (
	KindTask Kind = "task"
	// KindReservation is reserved for plan 2.
	KindReservation Kind = "reservation"
)

type State string

const (
	StateQueued     State = "QUEUED"
	StateStarting   State = "STARTING"
	StateRunning    State = "RUNNING"
	StateCleaning   State = "CLEANING"
	StateRecovering State = "RECOVERING"
	StateSucceeded  State = "SUCCEEDED"
	StateFailed     State = "FAILED"
	StateCancelled  State = "CANCELLED"
	StateRejected   State = "REJECTED"
)

func (s State) Terminal() bool {
	switch s {
	case StateSucceeded, StateFailed, StateCancelled, StateRejected:
		return true
	}
	return false
}

type Reason string

const (
	ReasonNone          Reason = ""
	ReasonCommandFailed Reason = "COMMAND_FAILED"
	ReasonOOM           Reason = "OOM"
	ReasonOOMGroup      Reason = "OOM_GROUP"
	ReasonOOMHostGuard  Reason = "OOM_HOST_GUARD"
	ReasonTimeout       Reason = "TIMEOUT"
	ReasonCancelled     Reason = "CANCELLED"
	ReasonLaunchFailed  Reason = "LAUNCH_FAILED"
	ReasonLost          Reason = "LOST"
	ReasonStorageFull   Reason = "STORAGE_FULL"
)

type Priority int

const (
	PriorityLow    Priority = -10
	PriorityNormal Priority = 0
	PriorityHigh   Priority = 100
)

// ParsePriority maps "low", "normal", "high" (or "" for normal) to a Priority.
func ParsePriority(s string) (Priority, error) {
	switch s {
	case "low":
		return PriorityLow, nil
	case "", "normal":
		return PriorityNormal, nil
	case "high":
		return PriorityHigh, nil
	}
	return 0, fmt.Errorf("invalid priority %q (want low, normal or high)", s)
}

func (p Priority) String() string {
	switch p {
	case PriorityLow:
		return "low"
	case PriorityNormal:
		return "normal"
	case PriorityHigh:
		return "high"
	}
	return fmt.Sprintf("%d", int(p))
}

type Resources struct {
	MemoryBytes uint64
	CPUs        int
}

type Item struct {
	ID       ItemID
	Kind     Kind
	Name     string
	State    State
	Reason   Reason
	Priority Priority

	Argv []string
	Cwd  string
	Env  map[string]string

	Limit, Expected uint64
	CPUs            int
	Timeout         time.Duration

	SessionID, ClientType, Project, IdemKey, RequestHash, NodeID string

	CreatedAt, AdmittedAt, StartedAt, EndedAt time.Time

	ExitCode   *int
	Signal     string
	MemoryPeak uint64

	UnitName, JobDir string
	GitRev           string
	GitDirty         bool

	CancelRequested bool
	CancelReason    string
	LogsTruncated   bool
	Diagnostic      string
}

// Committed is the resources this item holds against the pool.
func (it *Item) Committed() Resources {
	return Resources{MemoryBytes: it.Expected, CPUs: it.CPUs}
}

type Incident struct {
	At     time.Time
	Kind   string // "kernel_oom", "psi_close", "psi_open"
	Value  float64
	Detail string
}
