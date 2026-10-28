package notify

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"resource-dispatcher/internal/config"
	"strings"
	"sync"
	"syscall"
	"time"
)

type execDeliverer struct {
	agent    config.AgentConfig
	lookPath func(string) (string, error)
}

func NewExecDeliverer(a config.AgentConfig, lookPath func(string) (string, error)) Deliverer {
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	return &execDeliverer{a, lookPath}
}

// limitedOutput drains output while keeping only the first 4 KiB.
type limitedOutput struct {
	mu sync.Mutex
	b  []byte
}

func (b *limitedOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	if len(b.b) < 4096 {
		b.b = append(b.b, p[:min(n, 4096-len(b.b))]...)
	}
	return n, nil
}
func (d *execDeliverer) Deliver(ctx context.Context, m Message) error {
	if len(d.agent.Command) == 0 {
		return fmt.Errorf("empty exec command")
	}
	session := m.Event.SessionID
	if m.Item != nil {
		session = m.Item.SessionID
	}
	repl := strings.NewReplacer("{session}", session, "{text}", Text(m), "{event_id}", m.Event.ID, "{item_id}", string(m.Event.ItemID), "{kind}", string(m.Event.Kind))
	argv := make([]string, len(d.agent.Command))
	for i, a := range d.agent.Command {
		argv[i] = repl.Replace(a)
	}
	path, err := d.lookPath(argv[0])
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, path, argv[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = time.Second
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	var out limitedOutput
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err = cmd.Run(); err != nil {
		// Exit 0 with a lingering helper still holding the output pipe is a delivery.
		if errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.Success() {
			return nil
		}
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		if len(out.b) > 0 {
			return fmt.Errorf("%s", out.b)
		}
		return err
	}
	return nil
}
