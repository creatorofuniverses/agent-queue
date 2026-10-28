package linux

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOOMDJournalBaselineAndParse(t *testing.T) {
	f := filepath.Join(t.TempDir(), "cursor")
	j := NewOOMDJournal(f)
	calls := 0
	j.Run = func(ctx context.Context, args ...string) ([]byte, error) {
		s := strings.Join(args, " ")
		if !strings.Contains(s, "--cursor-file="+f) {
			t.Fatal(s)
		}
		calls++
		if calls == 1 {
			if !strings.Contains(s, "-n 0") {
				t.Fatal(s)
			}
			os.WriteFile(f, []byte("cursor"), 0600)
			return []byte(`{"MESSAGE":"Killed /old.scope x","__REALTIME_TIMESTAMP":"1000000"}`), nil
		}
		if strings.Contains(s, "-n 0") {
			t.Fatal(s)
		}
		return []byte("{\"MESSAGE\":\"Killed /user.slice/user-1000.slice/user@1000.service/app.slice/x.scope due to memory pressure for ...\",\"__REALTIME_TIMESTAMP\":\"2000000\"}\n{\"MESSAGE\":\"other\"}\n{\"MESSAGE\":[75,105],\"__REALTIME_TIMESTAMP\":\"2000000\"}\n"), nil
	}
	v, e := j.Kills(context.Background())
	if e != nil || len(v) != 0 {
		t.Fatal(v, e)
	}
	v, e = j.Kills(context.Background())
	if e != nil || len(v) != 1 || !strings.HasSuffix(v[0].Cgroup, "/x.scope") || !v[0].At.Equal(time.Unix(2, 0)) {
		t.Fatal(v, e)
	}
}

func TestOOMDJournalNoEntriesAtBaseline(t *testing.T) {
	f := filepath.Join(t.TempDir(), "cursor")
	j := NewOOMDJournal(f)
	var calls []string
	j.Run = func(ctx context.Context, args ...string) ([]byte, error) {
		s := strings.Join(args, " ")
		calls = append(calls, s)
		if len(calls) < 3 {
			return nil, nil // unit has no entries yet: journalctl writes no cursor
		}
		os.WriteFile(f, []byte("cursor"), 0600)
		return []byte(`{"MESSAGE":"Killed /a.scope due to memory pressure","__REALTIME_TIMESTAMP":"1000000"}`), nil
	}
	for i := 0; i < 2; i++ {
		if v, e := j.Kills(context.Background()); e != nil || len(v) != 0 {
			t.Fatal(v, e)
		}
	}
	v, e := j.Kills(context.Background())
	if e != nil || len(v) != 1 || strings.Contains(calls[1], "-n 0") || strings.Contains(calls[2], "-n 0") {
		t.Fatal(v, e, calls)
	}
}

func TestOOMDJournalDropsCorruptCursor(t *testing.T) {
	f := filepath.Join(t.TempDir(), "cursor")
	os.WriteFile(f, []byte("garbage"), 0600)
	j := NewOOMDJournal(f)
	j.Run = func(ctx context.Context, args ...string) ([]byte, error) {
		return nil, &exec.ExitError{Stderr: []byte("Failed to seek to cursor: Invalid argument")}
	}
	if _, e := j.Kills(context.Background()); e == nil {
		t.Fatal("want error")
	}
	if _, e := os.Stat(f); !os.IsNotExist(e) {
		t.Fatal("corrupt cursor kept", e)
	}
	var first string
	j.Run = func(ctx context.Context, args ...string) ([]byte, error) {
		first = strings.Join(args, " ")
		return nil, nil
	}
	j.Kills(context.Background())
	if !strings.Contains(first, "-n 0") {
		t.Fatal(first)
	}
}

func TestOOMDJournalKeepsCursorOnTimeout(t *testing.T) {
	f := filepath.Join(t.TempDir(), "cursor")
	os.WriteFile(f, []byte("cursor"), 0600)
	j := NewOOMDJournal(f)
	j.Run = func(ctx context.Context, args ...string) ([]byte, error) { return nil, context.DeadlineExceeded }
	j.Kills(context.Background())
	if _, e := os.Stat(f); e != nil {
		t.Fatal(e)
	}
}
