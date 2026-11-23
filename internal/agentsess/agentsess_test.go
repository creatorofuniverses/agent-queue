package agentsess

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/creatorofuniverses/agent-queue/internal/platform"
)

func TestReadSkipsBrokenAndForeign(t *testing.T) {
	dir, proc := t.TempDir(), t.TempDir()
	files := map[string]string{
		"1.json": `{"pid":1,"sessionId":"one","name":"project-1","cwd":"/project","status":"shell","statusUpdatedAt":1795464773195,"procStart":"123","version":"2.1.330"}`,
		"2.json": "not JSON",
		"3.json": `{"pid":3,"sessionId":"stale","procStart":"122"}`,
		"4.key":  `{"pid":4,"sessionId":"ignored","procStart":"123"}`,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, pid := range []string{"1", "3", "4"} {
		procDir := filepath.Join(proc, pid)
		if err := os.Mkdir(procDir, 0700); err != nil {
			t.Fatal(err)
		}
		stat := pid + " (claude ) session) S 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 123 456\n"
		if err := os.WriteFile(filepath.Join(procDir, "stat"), []byte(stat), 0600); err != nil {
			t.Fatal(err)
		}
	}

	got := (Reader{Dir: dir, ProcRoot: proc}).Read()
	want := Info{
		ProcStart:   123,
		SessionID:   "one",
		Name:        "project-1",
		Cwd:         "/project",
		Status:      "shell",
		StatusSince: time.UnixMilli(1795464773195),
	}
	if len(got) != 1 || got[1] != want {
		t.Fatalf("Read() = %+v, want only pid 1 with %+v", got, want)
	}
}

func TestReadMissingDir(t *testing.T) {
	r := Reader{Dir: filepath.Join(t.TempDir(), "missing")}
	if got := r.Read(); got == nil || len(got) != 0 {
		t.Fatalf("Read() = %#v, want an empty map", got)
	}
}

func TestEnrichIdle(t *testing.T) {
	now := time.Unix(1795464773, 0)
	usage := []platform.SessionUsage{
		{PID: 1, Client: "claude", Start: 123, AgentBytes: 123},
		{PID: 2, Client: "claude", Start: 123, Cwd: "/live"},
		{PID: 3, Client: "claude", Cwd: "/process"},
		{PID: 4, Client: "claude", Start: 123},
	}
	info := map[int]Info{
		1: {ProcStart: 123, SessionID: "one", Name: "project-1", Cwd: "/fallback", Status: "idle", StatusSince: now.Add(-13 * time.Hour)},
		2: {ProcStart: 123, Cwd: "/recorded", Status: "busy"},
		4: {ProcStart: 123, Status: "shell"},
	}

	got := Enrich(usage, info, now)
	if len(got) != 4 {
		t.Fatalf("Enrich() returned %d sessions, want 4", len(got))
	}
	idle := got[0]
	if idle.PID != 1 || idle.AgentBytes != 123 || idle.SessionID != "one" ||
		idle.Name != "project-1" || idle.Status != "idle" || idle.Cwd != "/fallback" ||
		idle.IdleSeconds == nil || *idle.IdleSeconds != 13*3600 {
		t.Errorf("idle session = %+v, want metadata, fallback cwd and 46800 idle seconds", idle)
	}
	busy := got[1]
	if busy.Cwd != "/live" || busy.IdleSeconds == nil || *busy.IdleSeconds != 0 {
		t.Errorf("busy session = %+v, want live cwd and zero idle seconds", busy)
	}
	if got[2].Cwd != "/process" || got[2].IdleSeconds != nil {
		t.Errorf("session without a file = %+v, want process cwd and unknown idle time", got[2])
	}
	if got[3].IdleSeconds != nil {
		t.Errorf("session without a status timestamp = %+v, want unknown idle time", got[3])
	}
}

func TestEnrichResumeID(t *testing.T) {
	usage := []platform.SessionUsage{
		{PID: 1, Client: "claude", Cmdline: []string{"claude", "--resume", "abc"}},
	}
	got := Enrich(usage, nil, time.Time{})
	if len(got) != 1 || got[0].SessionID != "abc" {
		t.Fatalf("Enrich() = %+v, want one session with ID abc", got)
	}
}

func TestEnrichRejectsDifferentOrUnknownSampledStart(t *testing.T) {
	dir, proc := t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(proc, "1"), 0700); err != nil {
		t.Fatal(err)
	}
	stat := "1 (claude) S 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 200 0\n"
	if err := os.WriteFile(filepath.Join(proc, "1", "stat"), []byte(stat), 0600); err != nil {
		t.Fatal(err)
	}
	record := `{"pid":1,"procStart":"200","sessionId":"new","name":"new process","cwd":"/new","status":"busy"}`
	if err := os.WriteFile(filepath.Join(dir, "1.json"), []byte(record), 0600); err != nil {
		t.Fatal(err)
	}
	info := (Reader{Dir: dir, ProcRoot: proc}).Read()
	for _, start := range []uint64{100, 0} {
		usage := platform.SessionUsage{PID: 1, Start: start, Cmdline: []string{"claude", "--resume", "old"}}
		got := Enrich([]platform.SessionUsage{usage}, info, time.Time{})[0]
		if got.SessionID != "old" || got.Name != "" || got.Cwd != "" || got.Status != "" || got.IdleSeconds != nil {
			t.Errorf("sample start %d got another process's metadata: %+v", start, got)
		}
	}
}
