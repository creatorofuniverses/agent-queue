package agentsess

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/creatorofuniverses/agent-queue/internal/platform"
)

func TestSources(t *testing.T) {
	if got := Sources(""); got != nil {
		t.Fatalf("Sources(\"\") = %+v, want nil", got)
	}
	want := []Source{
		{Dir: "/home/o/.claude/sessions", IdentityEnv: "CLAUDE_CODE_SESSION_ID"},
		{Dir: "/home/o/.openclaude/sessions", IdentityEnv: "CLAUDE_CODE_SESSION_ID"},
	}
	if got := Sources("/home/o"); !reflect.DeepEqual(got, want) {
		t.Fatalf("Sources() = %+v, want %+v", got, want)
	}
}

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

	got := (Reader{Sources: []Source{{Dir: dir, IdentityEnv: "CLAUDE_CODE_SESSION_ID"}}, ProcRoot: proc}).Read()
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
	r := Reader{Sources: []Source{{Dir: filepath.Join(t.TempDir(), "missing"), IdentityEnv: "CLAUDE_CODE_SESSION_ID"}}}
	if got := r.Read(); got == nil || len(got) != 0 {
		t.Fatalf("Read() = %#v, want an empty map", got)
	}
}

// procDir writes <root>/<pid>/stat with the given start ticks and, with btime, the
// boot time 1000000 in <root>/stat: 50 ticks after it is Unix 1000000.5, or 1000000500 ms.
func procDir(t *testing.T, pid string, ticks uint64, btime bool) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, pid), 0700); err != nil {
		t.Fatal(err)
	}
	stat := pid + " (openclaude) S 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 " + strconv.FormatUint(ticks, 10) + " 0\n"
	if err := os.WriteFile(filepath.Join(root, pid, "stat"), []byte(stat), 0600); err != nil {
		t.Fatal(err)
	}
	if btime {
		if err := os.WriteFile(filepath.Join(root, "stat"), []byte("btime 1000000\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestReadVerifiesStartedAt(t *testing.T) {
	// OpenClaude writes no procStart: its startedAt must not precede the process by
	// more than a minute, and an exact procStart decides alone.
	const busy = `{"pid":5,"sessionId":"oc","status":"busy","updatedAt":1000000500,"startedAt":%d}`
	verified := Info{ProcStart: 50, SessionID: "oc", Status: "busy", StatusSince: time.UnixMilli(1000000500)}
	cases := []struct {
		name, file, content string
		btime               bool
		want                *Info // nil: the file is not verified
	}{
		{"3 s after the process began", "5.json", fmt.Sprintf(busy, 1000000503), true, &verified},
		{"exactly 60 s before the process began", "5.json", fmt.Sprintf(busy, 999940500), true, &verified},
		{"60 s and 1 ms before the process began", "5.json", fmt.Sprintf(busy, 999940499), true, nil},
		{"procStart of another process", "5.json", `{"pid":5,"sessionId":"oc","procStart":"49","startedAt":1000000503}`, true, nil},
		{"neither procStart nor startedAt", "5.json", `{"pid":5,"sessionId":"oc"}`, true, nil},
		{"no boot time", "5.json", fmt.Sprintf(busy, 1000000503), false, nil},
		{"exact procStart without boot time", "5.json", `{"pid":5,"sessionId":"oc","procStart":"50"}`, false, &Info{ProcStart: 50, SessionID: "oc"}},
		{"no sessionId", "5.json", `{"pid":5,"sessionId":"","status":"busy","startedAt":1000000503}`, true, &Info{ProcStart: 50, Status: "busy"}},
		{"pid differs from the file name", "5.json", `{"pid":4,"sessionId":"oc","startedAt":1000000503}`, true, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, proc := t.TempDir(), procDir(t, "5", 50, tc.btime)
			if err := os.WriteFile(filepath.Join(dir, tc.file), []byte(tc.content), 0600); err != nil {
				t.Fatal(err)
			}
			got := (Reader{Sources: []Source{{Dir: dir, IdentityEnv: "CLAUDE_CODE_SESSION_ID"}}, ProcRoot: proc}).Read()
			if tc.want == nil {
				if len(got) != 0 {
					t.Fatalf("Read() = %+v, want the file not verified", got)
				}
				return
			}
			if len(got) != 1 || got[5] != *tc.want {
				t.Fatalf("Read() = %+v, want only pid 5 with %+v", got, *tc.want)
			}
		})
	}
}

func TestReadFirstSourceWins(t *testing.T) {
	first, second, proc := t.TempDir(), t.TempDir(), procDir(t, "7", 10, true)
	r := Reader{Sources: []Source{
		{Dir: first, IdentityEnv: "CLAUDE_CODE_SESSION_ID"},
		{Dir: second, IdentityEnv: "CLAUDE_CODE_SESSION_ID"},
	}, ProcRoot: proc}
	files := map[string]string{
		filepath.Join(first, "7.json"):  `{"pid":7,"sessionId":"claude","startedAt":1000000103}`,
		filepath.Join(second, "7.json"): `{"pid":7,"sessionId":"oc","startedAt":1000000103}`,
	}
	for path, content := range files {
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	want := Info{ProcStart: 10, SessionID: "claude"}
	if got := r.Read(); len(got) != 1 || got[7] != want {
		t.Fatalf("Read() = %+v, want only pid 7 with %+v from the first source", got, want)
	}
	if src, info, ok := r.Lookup(7); !ok || info != want || src != r.Sources[0] {
		t.Fatalf("Lookup(7) = %+v, %+v, %v; want the first source with %+v", src, info, ok, want)
	}
	// An unverified file of the first source does not shadow the second one.
	stale := `{"pid":7,"sessionId":"stale","startedAt":999900000}`
	if err := os.WriteFile(filepath.Join(first, "7.json"), []byte(stale), 0600); err != nil {
		t.Fatal(err)
	}
	if got := r.Read(); len(got) != 1 || got[7].SessionID != "oc" {
		t.Fatalf("Read() = %+v, want the second source's record for pid 7", got)
	}
}

func TestLookupUnverified(t *testing.T) {
	dir, proc := t.TempDir(), procDir(t, "6", 50, true)
	// pid 5 has no file; pid 6 has one that started long before the process.
	if err := os.WriteFile(filepath.Join(dir, "6.json"), []byte(`{"pid":6,"sessionId":"oc","startedAt":999900000}`), 0600); err != nil {
		t.Fatal(err)
	}
	r := Reader{Sources: []Source{{Dir: dir, IdentityEnv: "CLAUDE_CODE_SESSION_ID"}}, ProcRoot: proc}
	for _, pid := range []int{5, 6} {
		if src, info, ok := r.Lookup(pid); ok {
			t.Errorf("Lookup(%d) = %+v, %+v; want no verified file", pid, src, info)
		}
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
	info := (Reader{Sources: []Source{{Dir: dir, IdentityEnv: "CLAUDE_CODE_SESSION_ID"}}, ProcRoot: proc}).Read()
	for _, start := range []uint64{100, 0} {
		usage := platform.SessionUsage{PID: 1, Start: start, Cmdline: []string{"claude", "--resume", "old"}}
		got := Enrich([]platform.SessionUsage{usage}, info, time.Time{})[0]
		if got.SessionID != "old" || got.Name != "" || got.Cwd != "" || got.Status != "" || got.IdleSeconds != nil {
			t.Errorf("sample start %d got another process's metadata: %+v", start, got)
		}
	}
}
