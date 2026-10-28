package daemon

import (
	"time"

	"github.com/creatorofuniverses/agent-queue/internal/admission"
	"github.com/creatorofuniverses/agent-queue/internal/model"
)

// ItemView is the client-facing projection of an item. It deliberately has no
// env field: env values may hold secrets and never leave the daemon.
type ItemView struct {
	LogsPruned        bool         `json:"logs_pruned,omitempty"`
	ExpectedStart     *time.Time   `json:"expected_start,omitempty"`
	Profile           string       `json:"profile,omitempty"`
	MemoryFromProfile bool         `json:"memory_from_profile,omitempty"`
	EstimatedSeconds  float64      `json:"estimated_seconds,omitempty"`
	ExtendCount       int          `json:"extend_count,omitempty"`
	Events            []EventView  `json:"events,omitempty"`
	ClientType        string       `json:"client_type"`
	ID                model.ItemID `json:"id"`
	Name              string       `json:"name"`
	State             model.State  `json:"state"`
	Reason            model.Reason `json:"reason"`
	Priority          string       `json:"priority"`
	MemoryLimit       uint64       `json:"memory_limit"`
	MemoryExpected    uint64       `json:"memory_expected"`
	CPUs              int          `json:"cpus"`
	TimeoutSeconds    int64        `json:"timeout_seconds"`
	SessionID         string       `json:"session_id"`
	Project           string       `json:"project"`
	CreatedAt         time.Time    `json:"created_at"`
	StartedAt         *time.Time   `json:"started_at,omitempty"`
	EndedAt           *time.Time   `json:"ended_at,omitempty"`
	WaitSeconds       float64      `json:"wait_seconds"`
	RunSeconds        float64      `json:"run_seconds"`
	ExitCode          *int         `json:"exit_code,omitempty"`
	Signal            string       `json:"signal,omitempty"`
	MemoryPeak        uint64       `json:"memory_peak"`
	GitRev            string       `json:"git_rev,omitempty"`
	GitDirty          bool         `json:"git_dirty"`
	Diagnostic        string       `json:"diagnostic,omitempty"`
	NodeID            string       `json:"node_id"`

	// Reservation fields. Neither env nor the teardown script is ever part of a view.
	Kind            model.Kind   `json:"kind"`
	Within          model.ItemID `json:"within,omitempty"`
	TaskMemory      uint64       `json:"task_memory"`
	TaskCPUs        int          `json:"task_cpus"`
	ExpiresAt       *time.Time   `json:"expires_at,omitempty"`
	DockerProject   string       `json:"docker_project,omitempty"`
	StandMeasured   *uint64      `json:"stand_measured,omitempty"`
	TeardownAttempt int          `json:"teardown_attempt,omitempty"`
	TTLHeld         bool         `json:"ttl_held"`

	// Set by list for QUEUED items only.
	Position      int               `json:"position,omitempty"`
	BlockedBy     admission.Blocker `json:"blocked_by,omitempty"`
	BlockedDetail string            `json:"blocked_detail,omitempty"`
}

// ResultView is ItemView plus where the logs live.
type ResultView struct {
	ItemView
	StdoutPath    string `json:"stdout_path"`
	StderrPath    string `json:"stderr_path"`
	LogsTruncated bool   `json:"logs_truncated"`
}

func optTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func viewOf(it *model.Item, now time.Time) ItemView {
	v := ItemView{
		LogsPruned: it.LogsPruned,
		Profile:    it.Profile, MemoryFromProfile: it.MemoryFromProfile, EstimatedSeconds: it.EstDuration.Seconds(), ExtendCount: it.ExtendCount,
		ClientType: it.ClientType, ID: it.ID, Name: it.Name, State: it.State, Reason: it.Reason, Priority: it.Priority.String(),
		MemoryLimit: it.Limit, MemoryExpected: it.Expected, CPUs: it.CPUs,
		TimeoutSeconds: int64(it.Timeout / time.Second), SessionID: it.SessionID, Project: it.Project,
		CreatedAt: it.CreatedAt, StartedAt: optTime(it.StartedAt), EndedAt: optTime(it.EndedAt),
		ExitCode: it.ExitCode, Signal: it.Signal, MemoryPeak: it.MemoryPeak,
		GitRev: it.GitRev, GitDirty: it.GitDirty, Diagnostic: it.Diagnostic, NodeID: it.NodeID,
		Kind: it.Kind, Within: it.ParentID, TaskMemory: it.TaskMemory, TaskCPUs: it.TaskCPUs,
		ExpiresAt: optTime(it.ExpiresAt), DockerProject: it.DockerProject,
		TeardownAttempt: it.TeardownAttempt, TTLHeld: it.TTLHeld,
	}
	// Wait runs from creation to start (or to the end/now if it never started).
	waitEnd := it.StartedAt
	if waitEnd.IsZero() {
		waitEnd = it.EndedAt
	}
	if waitEnd.IsZero() {
		waitEnd = now
	}
	v.WaitSeconds = nonNeg(waitEnd.Sub(it.CreatedAt))
	if !it.StartedAt.IsZero() {
		runEnd := it.EndedAt
		if runEnd.IsZero() {
			runEnd = now
		}
		v.RunSeconds = nonNeg(runEnd.Sub(it.StartedAt))
	}
	return v
}

func nonNeg(d time.Duration) float64 {
	if d < 0 {
		return 0
	}
	return d.Seconds()
}

type EventView struct {
	ID        string            `json:"id"`
	Kind      model.EventKind   `json:"kind"`
	Status    model.EventStatus `json:"status"`
	CreatedAt time.Time         `json:"created_at"`
	SentAt    *time.Time        `json:"sent_at,omitempty"`
	Detail    string            `json:"detail"`
}

func eventView(e model.Event) EventView {
	return EventView{e.ID, e.Kind, e.Status, e.CreatedAt, optTime(e.SentAt), e.Detail}
}

type AdapterStatus struct {
	Name     string `json:"name"`
	Delivery string `json:"delivery"`
	OK       bool   `json:"ok"`
	Detail   string `json:"detail"`
}
type WebhookStatus struct {
	Configured bool   `json:"configured"`
	LastError  string `json:"last_error"`
}
