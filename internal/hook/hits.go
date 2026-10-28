package hook

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"
)

type Hit struct {
	At      time.Time `json:"at"`
	Pattern int       `json:"pattern"`
	Client  string    `json:"client"`
}

var hitRotationBytes int64 = 1 << 20

func RotatedHitFiles(stateDir string) ([]string, error) {
	files, e := filepath.Glob(filepath.Join(stateDir, "hook-hits-*.jsonl"))
	if e != nil {
		return nil, e
	}
	legacy := filepath.Join(stateDir, "hook-hits.jsonl.1")
	if _, e = os.Stat(legacy); e == nil {
		files = append(files, legacy)
	} else if !os.IsNotExist(e) {
		return nil, e
	}
	sort.Strings(files)
	return files, nil
}
func ReadHits(stateDir string, since time.Time) ([]Hit, error) {
	files, e := RotatedHitFiles(stateDir)
	if e != nil {
		return nil, e
	}
	files = append(files, filepath.Join(stateDir, "hook-hits.jsonl"))
	var out []Hit
	for _, p := range files {
		f, e := os.Open(p)
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			return nil, e
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 4096), 1<<20)
		for sc.Scan() {
			var h Hit
			if json.Unmarshal(sc.Bytes(), &h) == nil && !h.At.IsZero() && !h.At.Before(since) {
				out = append(out, h)
			}
		}
		e = sc.Err()
		f.Close()
		if e != nil {
			return nil, e
		}
	}
	return out, nil
}

// Link/unlink gives rename semantics without replacing a prior rotation.
func rotateHits(path string) error {
	now := time.Now().UTC()
	for {
		dst := filepath.Join(filepath.Dir(path), "hook-hits-"+now.Format("20060102T150405.000000000Z")+".jsonl")
		e := os.Link(path, dst)
		if os.IsExist(e) {
			now = now.Add(time.Nanosecond)
			continue
		}
		if e != nil {
			return e
		}
		return os.Remove(path)
	}
}
