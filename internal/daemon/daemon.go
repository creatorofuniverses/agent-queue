// Package daemon wires the store, scheduler and socket server into the aq daemon.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"

	"resource-dispatcher/internal/config"
	"resource-dispatcher/internal/platform"
	"resource-dispatcher/internal/protocol"
	"resource-dispatcher/internal/scheduler"
	"resource-dispatcher/internal/store"
)

// Version is reported by ping.
const Version = "0.1.0"

// Run starts the daemon and blocks until ctx ends (returning nil) or startup
// or serving fails. A recovery error is fatal: admission must not open on
// unreconciled state.
func Run(ctx context.Context, paths config.Paths, cfg config.Config, exec platform.Executor, host platform.HostMetrics) error {
	if err := os.MkdirAll(paths.RuntimeDir, 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(paths.StateDir, 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(paths.Lock(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close() // closing releases the flock
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) {
			return fmt.Errorf("daemon already running (lock %s held)", paths.Lock())
		}
		return err
	}

	mem, err := host.Memory()
	if err != nil {
		return fmt.Errorf("read host memory: %w", err)
	}
	if err := cfg.Validate(mem.Total, host.NumCPU()); err != nil {
		return err
	}
	st, err := store.Open(paths.DB())
	if err != nil {
		return err
	}
	defer st.Close()
	if err := exec.EnsureSlice(ctx, cfg.PoolMemory); err != nil {
		return fmt.Errorf("ensure slice: %w", err)
	}

	sched := scheduler.New(scheduler.Deps{Store: st, Exec: exec, Host: host, Cfg: cfg, Paths: paths})
	d := &server{sched: sched, st: st, paths: paths}
	d.recovering.Store(true)
	if err := sched.Recover(ctx); err != nil {
		return fmt.Errorf("recover: %w", err)
	}
	d.recovering.Store(false)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	schedDone := make(chan struct{})
	go func() {
		defer close(schedDone)
		sched.Run(runCtx)
	}()
	var loops sync.WaitGroup
	loops.Add(2)
	go func() {
		defer loops.Done()
		RunGuard(runCtx, exec, host, cfg, sched.SetGuardActive, time.Second)
	}()
	go func() {
		defer loops.Done()
		RunIncidents(runCtx, host, st, sched.Pressure, 10*time.Second)
	}()

	err = protocol.Serve(runCtx, paths.Socket(), d.handle)
	cancel()
	<-schedDone
	loops.Wait()
	if ctx.Err() != nil {
		return nil
	}
	return err
}

type server struct {
	sched      *scheduler.Scheduler
	st         *store.Store
	paths      config.Paths
	recovering atomic.Bool
}
