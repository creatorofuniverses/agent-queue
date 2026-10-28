package hook

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReadHits(t *testing.T) {
	d := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)
	for _, n := range []string{"hook-hits.jsonl", "hook-hits-20260101.jsonl", "hook-hits-20260102.jsonl", "hook-hits.jsonl.1"} {
		os.WriteFile(filepath.Join(d, n), []byte("bad\n{\"at\":\""+now.Format(time.RFC3339)+"\",\"pattern\":1,\"client\":\"codex\"}\n{\"at\":\"2000-01-01T00:00:00Z\",\"pattern\":0}\n"), 0600)
	}
	hits, e := ReadHits(d, now)
	if e != nil || len(hits) != 4 {
		t.Fatal(hits, e)
	}
	hs, e := ReadHits(t.TempDir(), now)
	if e != nil || len(hs) != 0 {
		t.Fatal(hs, e)
	}
}
func TestAppendHitRotationKeepsHistory(t *testing.T) {
	d := t.TempDir()
	old := hitRotationBytes
	hitRotationBytes = 1
	defer func() { hitRotationBytes = old }()
	at := time.Now()
	for i := 0; i < 4; i++ {
		if e := AppendHit(d, at, i, "codex"); e != nil {
			t.Fatal(e)
		}
	}
	files, e := RotatedHitFiles(d)
	hits, err := ReadHits(d, time.Time{})
	if e != nil || err != nil || len(files) != 3 || len(hits) != 4 {
		t.Fatal(files, hits, e, err)
	}
}
