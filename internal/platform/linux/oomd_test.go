package linux

import (
	"context"
	"os"
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
