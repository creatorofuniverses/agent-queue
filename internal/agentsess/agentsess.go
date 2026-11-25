// Package agentsess adds client metadata to sampled agent sessions.
package agentsess

import (
	"encoding/json"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/creatorofuniverses/agent-queue/internal/platform"
)

// Info is what a Claude Code or OpenClaude session file says about a live process.
type Info struct {
	SessionID, Name, Cwd, Status string
	ProcStart                    uint64    // start time in kernel clock ticks
	StatusSince                  time.Time // statusUpdatedAt, else updatedAt as OpenClaude writes; zero when absent
}

// Session is a SessionUsage with what the client's own files add.
type Session struct {
	platform.SessionUsage
	SessionID   string   `json:"session_id,omitempty"`
	Name        string   `json:"name,omitempty"`
	Status      string   `json:"status,omitempty"`
	IdleSeconds *float64 `json:"idle_seconds"` // nil: unknown
}

// Source is one client's session directory and the adapter identity_env its
// sessions belong to.
type Source struct {
	Dir         string
	IdentityEnv string
}

// Sources lists the session directories of the known clients, most trusted first.
func Sources(home string) []Source {
	if home == "" {
		return nil
	}
	return []Source{
		{Dir: filepath.Join(home, ".claude", "sessions"), IdentityEnv: "CLAUDE_CODE_SESSION_ID"},
		{Dir: filepath.Join(home, ".openclaude", "sessions"), IdentityEnv: "CLAUDE_CODE_SESSION_ID"},
	}
}

// Reader reads the clients' session files; ProcRoot verifies them against live processes.
type Reader struct {
	Sources  []Source
	ProcRoot string
}

func (r Reader) procRoot() string {
	if r.ProcRoot == "" {
		return "/proc"
	}
	return r.ProcRoot
}

var warnSessionFile sync.Once

// Read returns session metadata by PID, skipping unreadable or unverified files.
func (r Reader) Read() map[int]Info {
	info := make(map[int]Info)
	for _, src := range r.Sources {
		entries, err := os.ReadDir(src.Dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			pid, ok := sessionPID(entry.Name(), entry.Type())
			if !ok {
				continue
			}
			if session, ok := r.lookup(src, pid); ok {
				if _, seen := info[pid]; !seen {
					info[pid] = session
				}
			}
		}
	}
	return info
}

// Lookup returns the verified session file of pid from the first source that has one.
func (r Reader) Lookup(pid int) (Source, Info, bool) {
	for _, src := range r.Sources {
		if session, ok := r.lookup(src, pid); ok {
			return src, session, true
		}
	}
	return Source{}, Info{}, false
}

// sessionPID accepts exactly <pid>.json; other names are not session files.
func sessionPID(name string, typ fs.FileMode) (int, bool) {
	if typ&fs.ModeType != 0 || filepath.Ext(name) != ".json" { // regular files only
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSuffix(name, ".json"))
	return pid, err == nil && pid > 0 && name == strconv.Itoa(pid)+".json"
}

// lookup reads and verifies <src.Dir>/<pid>.json against the live process.
func (r Reader) lookup(src Source, pid int) (Info, bool) {
	path := filepath.Join(src.Dir, strconv.Itoa(pid)+".json")
	var record struct {
		PID             int    `json:"pid"`
		SessionID       string `json:"sessionId"`
		Name            string `json:"name"`
		Cwd             string `json:"cwd"`
		Status          string `json:"status"`
		StatusUpdatedAt *int64 `json:"statusUpdatedAt"`
		UpdatedAt       *int64 `json:"updatedAt"`
		ProcStart       string `json:"procStart"`
		StartedAt       *int64 `json:"startedAt"`
	}
	b, err := os.ReadFile(path)
	if err == nil {
		err = json.Unmarshal(b, &record)
	}
	if err != nil {
		warnSessionFile.Do(func() {
			slog.Warn("cannot read agent session file", "path", path, "error", err)
		})
		return Info{}, false
	}
	start, known := procStartTicks(filepath.Join(r.procRoot(), strconv.Itoa(pid), "stat"))
	if record.PID != pid || !known {
		return Info{}, false
	}
	if record.ProcStart != "" {
		// An exact start time names one incarnation of the pid: a mismatch is
		// another process, whatever startedAt says.
		if claimed, err := strconv.ParseUint(record.ProcStart, 10, 64); err != nil || claimed != start {
			return Info{}, false
		}
	} else {
		// OpenClaude writes no procStart. Its file is written once, right after the
		// process starts, so it cannot be much older than the process; the minute
		// covers clock adjustment after boot.
		began := processStart(r.procRoot(), start)
		if record.StartedAt == nil || began.IsZero() ||
			time.UnixMilli(*record.StartedAt).Before(began.Add(-time.Minute)) {
			return Info{}, false
		}
	}
	session := Info{ProcStart: start, SessionID: record.SessionID, Name: record.Name, Cwd: record.Cwd, Status: record.Status}
	switch {
	case record.StatusUpdatedAt != nil:
		session.StatusSince = time.UnixMilli(*record.StatusUpdatedAt)
	case record.UpdatedAt != nil:
		session.StatusSince = time.UnixMilli(*record.UpdatedAt)
	}
	return session, true
}

// procStartTicks is field 22 of /proc/<pid>/stat: the start time in kernel clock ticks.
func procStartTicks(path string) (uint64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	stat := string(b)
	end := strings.LastIndexByte(stat, ')')
	if end < 0 {
		return 0, false
	}
	fields := strings.Fields(stat[end+1:]) // Field 3 follows the parenthesized command.
	if len(fields) < 20 {
		return 0, false
	}
	start, err := strconv.ParseUint(fields[19], 10, 64)
	return start, err == nil
}

// processStart is when a process with these start ticks began: boot time from
// /proc/stat plus ticks, zero without a readable btime.
func processStart(procRoot string, ticks uint64) time.Time {
	b, err := os.ReadFile(filepath.Join(procRoot, "stat"))
	if err != nil {
		return time.Time{}
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 || f[0] != "btime" {
			continue
		}
		sec, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil {
			return time.Time{}
		}
		// /proc reports starttime in Linux user clock ticks (USER_HZ).
		return time.Unix(sec, 0).Add(time.Duration(ticks) * (time.Second / 100))
	}
	return time.Time{}
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
