// Package model defines the shared types of the aq queue.
package model

import (
	"crypto/rand"
	"fmt"
	"strings"
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
	// KindReservation is a stand reservation: it holds pool resources until
	// released or torn down.
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

	// Reservation states.
	StateGranted         State = "GRANTED"
	StateExpiring        State = "EXPIRING"
	StateTearingDown     State = "TEARING_DOWN"
	StateTornDown        State = "TORN_DOWN"
	StateTeardownFailed  State = "TEARDOWN_FAILED"
	StateTeardownUnknown State = "TEARDOWN_UNKNOWN"
	StateReleased        State = "RELEASED"
)

// TerminalStates is the single list of states that free the pool; the store
// builds its SQL from it.
func TerminalStates() []State {
	return []State{StateSucceeded, StateFailed, StateCancelled, StateRejected, StateTornDown, StateReleased}
}

func (s State) Terminal() bool {
	for _, t := range TerminalStates() {
		if s == t {
			return true
		}
	}
	return false
}

// Held reports whether a reservation in this state counts against the pool.
// TEARDOWN_FAILED and TEARDOWN_UNKNOWN are non-terminal and keep their accounting.
func (s State) Held() bool {
	switch s {
	case StateGranted, StateExpiring, StateTearingDown, StateTeardownFailed, StateTeardownUnknown:
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

func (r Reason) Infra() bool {
	switch r {
	case ReasonLaunchFailed, ReasonLost, ReasonStorageFull, ReasonOOMHostGuard:
		return true
	}
	return false
}

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

	NoNotify, FinishNotified bool
	CancelRequested          bool
	CancelReason             string
	LogsTruncated            bool
	Diagnostic               string

	// ParentID is the reservation a nested task runs within.
	ParentID ItemID
	// Reservation fields. For a reservation Limit = Expected = stand memory,
	// CPUs = stand CPUs, Timeout = TTL, Cwd = original cwd, Argv = nil.
	TaskMemory uint64
	TaskCPUs   int
	ExpiresAt  time.Time
	WarnAt     time.Time
	// DockerProject is the compose project measured for the stand ("" if none).
	DockerProject string
	// TeardownAttempt counts launched teardown attempts (first is 1).
	TeardownAttempt int
	// TeardownLaunchError is the Launch error of the current attempt; reset on every attempt+1.
	TeardownLaunchError  string
	NotifyGrant          bool
	TTLHeld              bool
	OverDeclaredNotified bool
}

func (it *Item) IsReservation() bool { return it.Kind == KindReservation }

// Committed is the resources this item holds against the pool, regardless of
// state (the scheduler decides which states count). A nested task is paid for
// by its reservation's task budget; a reservation holds stand plus budget.
func (it *Item) Committed() Resources {
	switch {
	case it.ParentID != "":
		return Resources{}
	case it.IsReservation():
		return Resources{MemoryBytes: it.Expected + it.TaskMemory, CPUs: it.CPUs + it.TaskCPUs}
	}
	return Resources{MemoryBytes: it.Expected, CPUs: it.CPUs}
}

// TeardownUnitID is the executor id of a reservation's teardown unit.
func TeardownUnitID(id ItemID) ItemID { return teardownPrefix + id }

const teardownPrefix = "teardown-"

// IsTeardownUnitID reports whether id names a teardown unit rather than a task.
func IsTeardownUnitID(id ItemID) bool { return strings.HasPrefix(string(id), teardownPrefix) }

type EventKind string

const (
	EventTaskFinished             EventKind = "task_finished"
	EventTaskFailedInfra          EventKind = "task_failed_infra"
	EventCleaningStuck            EventKind = "cleaning_stuck"
	EventAdmissionClosed          EventKind = "admission_closed"
	EventDeliveryFailed           EventKind = "delivery_failed"
	EventReservationForceReleased EventKind = "reservation_force_released"
	EventReservationGranted       EventKind = "reservation_granted"
	EventReservationExpiring      EventKind = "reservation_expiring"
	EventReservationHeldByTask    EventKind = "reservation_held_by_tasks"
	EventTeardownSucceeded        EventKind = "teardown_succeeded"
	EventTeardownFailed           EventKind = "teardown_failed"
	EventTeardownUnknown          EventKind = "teardown_unknown"
	EventStandOverDeclared        EventKind = "stand_over_declared"
)

type Audience string

const (
	AudienceSession  Audience = "session"
	AudienceOperator Audience = "operator"
)

// Event is a notification recorded in the same transaction as the item change
// that caused it; delivery is a separate concern.
type EventStatus string

const (
	EventPending    EventStatus = "pending"
	EventDelivering EventStatus = "delivering"
	EventSent       EventStatus = "sent"
	EventAcked      EventStatus = "acked"
	EventSkipped    EventStatus = "skipped"
	EventFailed     EventStatus = "failed"
)

type Channel string

const (
	ChannelNone       Channel = "none"
	ChannelWebhook    Channel = "webhook"
	ChannelClientWait Channel = "client_wait"
)

func ExecChannel(adapter string) Channel { return Channel("exec:" + adapter) }
func (c Channel) ExecAdapter() (string, bool) {
	a, ok := strings.CutPrefix(string(c), "exec:")
	if !ok || a == "" {
		return "", false
	}
	return a, true
}
func (c Channel) Retried() bool { _, ok := c.ExecAdapter(); return c == ChannelWebhook || ok }

type Event struct {
	Channel                        Channel
	Status                         EventStatus
	Attempts                       int
	NextAttemptAt, SentAt, AckedAt time.Time
	LastError                      string
	ID                             string
	ItemID                         ItemID
	Kind                           EventKind
	Audience                       Audience
	SessionID                      string
	ClientType, Detail             string
	CreatedAt                      time.Time
}

type Incident struct {
	At     time.Time
	Kind   string // "kernel_oom", "psi_close", "psi_open"
	Value  float64
	Detail string
}
