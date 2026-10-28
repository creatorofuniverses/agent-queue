package linux

import (
	"agent-queue/internal/platform"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type OOMDJournal struct {
	CursorFile string
	Run        func(context.Context, ...string) ([]byte, error)
	// baselined: the -n 0 call ran but journalctl wrote no cursor because the
	// unit had no entries yet; later calls must read from the start, not re-baseline.
	baselined bool
}

func NewOOMDJournal(cursorFile string) *OOMDJournal { return &OOMDJournal{CursorFile: cursorFile} }
func (j *OOMDJournal) Kills(ctx context.Context) ([]platform.OOMKill, error) {
	_, e := os.Stat(j.CursorFile)
	missing := errors.Is(e, os.ErrNotExist)
	if e != nil && !missing {
		return nil, e
	}
	first := missing && !j.baselined
	args := []string{"-u", "systemd-oomd", "--cursor-file=" + j.CursorFile, "-o", "json", "--no-pager", "-q"}
	if first {
		args = append(args, "-n", "0")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	run := j.Run
	if run == nil {
		run = func(ctx context.Context, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, "journalctl", args...).Output()
		}
	}
	b, e := run(ctx, args...)
	if e != nil {
		if !missing && badCursor(e) {
			// A corrupt cursor fails every call; drop it and re-baseline.
			os.Remove(j.CursorFile)
			j.baselined = false
		}
		return nil, e
	}
	if first {
		j.baselined = true
		return nil, nil
	}
	var out []platform.OOMKill
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 4096), 1<<20)
	for sc.Scan() {
		var v struct {
			Message json.RawMessage `json:"MESSAGE"`
			At      string          `json:"__REALTIME_TIMESTAMP"`
		}
		if json.Unmarshal(sc.Bytes(), &v) != nil {
			continue
		}
		var msg string
		if json.Unmarshal(v.Message, &msg) != nil {
			continue
		}
		rest, ok := strings.CutPrefix(msg, "Killed ")
		if !ok {
			continue
		}
		cg, _, ok := strings.Cut(rest, " ")
		if !ok || !strings.HasPrefix(cg, "/") {
			continue
		}
		us, e := strconv.ParseInt(v.At, 10, 64)
		if e != nil {
			continue
		}
		out = append(out, platform.OOMKill{At: time.UnixMicro(us).UTC(), Cgroup: cg})
	}
	return out, sc.Err()
}

func badCursor(e error) bool {
	var x *exec.ExitError
	return errors.As(e, &x) && bytes.Contains(x.Stderr, []byte("cursor"))
}
