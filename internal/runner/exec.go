// Package runner holds the in-unit supervisor (aq _exec) and result recorder.
package runner

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

type ExecArgs struct {
	JobDir, TmpDir       string
	Cwd                  string // the command's working directory; empty keeps ours
	MaxStdout, MaxStderr uint64
	Argv                 []string
}

type execMeta struct {
	LogsTruncated bool `json:"logs_truncated"`
}

// ParseExecArgs parses: --job-dir D --tmp-dir T [--cwd C] --max-stdout N --max-stderr N -- argv...
func ParseExecArgs(args []string) (ExecArgs, error) {
	var a ExecArgs
	var haveJob, haveTmp, haveOut, haveErr bool
	i := 0
	for ; i < len(args); i++ {
		if args[i] == "--" {
			i++
			break
		}
		if i+1 >= len(args) {
			return a, fmt.Errorf("flag %s needs a value", args[i])
		}
		v := args[i+1]
		switch args[i] {
		case "--job-dir":
			a.JobDir, haveJob = v, true
		case "--tmp-dir":
			a.TmpDir, haveTmp = v, true
		case "--cwd":
			a.Cwd = v
		case "--max-stdout", "--max-stderr":
			n, err := strconv.ParseUint(v, 10, 64)
			if err != nil {
				return a, fmt.Errorf("%s: %w", args[i], err)
			}
			if args[i] == "--max-stdout" {
				a.MaxStdout, haveOut = n, true
			} else {
				a.MaxStderr, haveErr = n, true
			}
		default:
			return a, fmt.Errorf("unknown flag %s", args[i])
		}
		i++
	}
	if !haveJob || !haveTmp || !haveOut || !haveErr {
		return a, errors.New("missing required flag")
	}
	a.Argv = args[min(i, len(args)):]
	if len(a.Argv) == 0 {
		return a, errors.New("missing command after --")
	}
	return a, nil
}

// Exec runs the command, capturing capped logs, and returns its exit code.
func Exec(a ExecArgs) int {
	if err := os.MkdirAll(a.JobDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "aq: job dir:", err)
		return 127
	}
	if err := os.MkdirAll(a.TmpDir, 0o755); err != nil {
		return launchFail(a, err)
	}
	outF, err := os.Create(filepath.Join(a.JobDir, "stdout.log"))
	if err != nil {
		return launchFail(a, err)
	}
	defer outF.Close()
	errF, err := os.Create(filepath.Join(a.JobDir, "stderr.log"))
	if err != nil {
		return launchFail(a, err)
	}
	defer errF.Close()

	outW := newCapWriter(outF, a.MaxStdout)
	errW := newCapWriter(errF, a.MaxStderr)

	cmd := exec.Command(a.Argv[0], a.Argv[1:]...)
	cmd.Dir = a.Cwd
	cmd.Env = append(os.Environ(), "TMPDIR="+a.TmpDir, "TMP="+a.TmpDir, "TEMP="+a.TmpDir)
	cmd.Stdout = outW
	cmd.Stderr = errW
	// A descendant may keep the pipes open after the command exits.
	cmd.WaitDelay = 2 * time.Second

	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sigs)

	if err := cmd.Start(); err != nil {
		return launchFail(a, err)
	}
	done := make(chan struct{})
	go func() {
		for {
			select {
			case s := <-sigs:
				_ = cmd.Process.Signal(s)
			case <-done:
				return
			}
		}
	}()
	_ = cmd.Wait() // may be exec.ErrWaitDelay; the exit code comes from ProcessState
	close(done)

	meta, _ := json.Marshal(execMeta{LogsTruncated: outW.truncated || errW.truncated})
	_ = os.WriteFile(filepath.Join(a.JobDir, "exec.json"), meta, 0o644)

	ps := cmd.ProcessState
	if ps == nil {
		return 127
	}
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ps.ExitCode()
}

func launchFail(a ExecArgs, err error) int {
	_ = os.WriteFile(filepath.Join(a.JobDir, "launch_error.txt"), []byte(err.Error()+"\n"), 0o644)
	return 127
}
