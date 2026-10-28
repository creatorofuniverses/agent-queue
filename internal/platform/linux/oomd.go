package linux

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"resource-dispatcher/internal/platform"
	"strconv"
	"strings"
	"time"
)

type OOMDJournal struct {
	CursorFile string
	Run        func(context.Context, ...string) ([]byte, error)
}

func NewOOMDJournal(cursorFile string) *OOMDJournal { return &OOMDJournal{CursorFile: cursorFile} }
func (j *OOMDJournal) Kills(ctx context.Context) ([]platform.OOMKill, error) {
	_, e := os.Stat(j.CursorFile)
	first := errors.Is(e, os.ErrNotExist)
	if e != nil && !first {
		return nil, e
	}
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
	if e != nil || first {
		return nil, e
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
