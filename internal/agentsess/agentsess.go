// Package agentsess adds client metadata to sampled agent sessions.
package agentsess

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/creatorofuniverses/agent-queue/internal/platform"
)

// Info is what a Claude Code session file says about a live process.
type Info struct {
	SessionID, Name, Cwd, Status string
	ProcStart                    uint64    // start time in kernel clock ticks
	StatusSince                  time.Time // statusUpdatedAt; zero when absent
}

// Session is a SessionUsage with what the client's own files add.
type Session struct {
	platform.SessionUsage
	SessionID   string   `json:"session_id,omitempty"`
	Name        string   `json:"name,omitempty"`
	Status      string   `json:"status,omitempty"`
	IdleSeconds *float64 `json:"idle_seconds"` // nil: unknown
}

// Reader reads ~/.claude/sessions/<pid>.json; ProcRoot checks procStart.
type Reader struct{ Dir, ProcRoot string }

var warnSessionFile sync.Once

// Read returns session metadata by PID, skipping unreadable or stale files.
func (r Reader) Read() map[int]Info {
	info := make(map[int]Info)
	entries, err := os.ReadDir(r.Dir)
	if err != nil {
		return info
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(r.Dir, entry.Name())
		var record struct {
			PID             int    `json:"pid"`
			SessionID       string `json:"sessionId"`
			Name            string `json:"name"`
			Cwd             string `json:"cwd"`
			Status          string `json:"status"`
			StatusUpdatedAt *int64 `json:"statusUpdatedAt"`
			ProcStart       string `json:"procStart"`
		}
		b, err := os.ReadFile(path)
		if err == nil {
			err = json.Unmarshal(b, &record)
		}
		if err != nil {
			warnSessionFile.Do(func() {
				slog.Warn("cannot read Claude Code session file", "path", path, "error", err)
			})
			continue
		}
		pid := strconv.Itoa(record.PID)
		if record.PID <= 0 || entry.Name() != pid+".json" ||
			!matchesProcStart(filepath.Join(r.ProcRoot, pid, "stat"), record.ProcStart) {
			continue
		}
		start, err := strconv.ParseUint(record.ProcStart, 10, 64)
		if err != nil || start == 0 {
			continue
		}
		session := Info{ProcStart: start, SessionID: record.SessionID, Name: record.Name, Cwd: record.Cwd, Status: record.Status}
		if record.StatusUpdatedAt != nil {
			session.StatusSince = time.UnixMilli(*record.StatusUpdatedAt)
		}
		info[record.PID] = session
	}
	return info
}

func matchesProcStart(path, start string) bool {
	if start == "" {
		return false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	stat := string(b)
	end := strings.LastIndexByte(stat, ')')
	if end < 0 {
		return false
	}
	fields := strings.Fields(stat[end+1:]) // Field 3 follows the parenthesized command.
	return len(fields) >= 20 && fields[19] == start
}

// Enrich preserves sample order and fills in the available client metadata.
func Enrich(us []platform.SessionUsage, info map[int]Info, now time.Time) []Session {
	sessions := make([]Session, len(us))
	for i, usage := range us {
		session := &sessions[i]
		session.SessionUsage = usage
		metadata, ok := info[usage.PID]
		if !ok || usage.Start == 0 || metadata.ProcStart != usage.Start {
			session.SessionID = resumeID(usage.Cmdline)
			continue
		}
		session.SessionID = metadata.SessionID
		session.Name = metadata.Name
		session.Status = metadata.Status
		if session.Cwd == "" {
			session.Cwd = metadata.Cwd
		}
		switch {
		case metadata.Status == "busy":
			seconds := 0.0
			session.IdleSeconds = &seconds
		case !metadata.StatusSince.IsZero():
			seconds := now.Sub(metadata.StatusSince).Seconds()
			session.IdleSeconds = &seconds
		}
	}
	return sessions
}

func resumeID(args []string) string {
	for i := 1; i+1 < len(args); i++ {
		if args[i] == "--resume" {
			return args[i+1]
		}
	}
	return ""
}
