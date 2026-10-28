// Package daemon wires the store, scheduler and socket server into the aq daemon.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	osexec "os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"resource-dispatcher/internal/config"
	"resource-dispatcher/internal/model"
	"resource-dispatcher/internal/notify"
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
type Options struct{ Usage platform.UsageMeter }

func Run(ctx context.Context, paths config.Paths, cfg config.Config, exec platform.Executor, host platform.HostMetrics, meter platform.StandMeter, opts Options) error {
	if err := os.MkdirAll(paths.RuntimeDir, 0o700); err != nil {
		return err
	}
	if err := privateDir(paths.StateDir); err != nil {
		return err
	}
	lock, err := flock(paths.Lock())
	if err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) {
			return fmt.Errorf("daemon already running (lock %s held)", paths.Lock())
		}
		return err
	}
	defer lock.Close() // closing releases the flock

	mem, err := host.Memory()
	if err != nil {
		return fmt.Errorf("read host memory: %w", err)
	}
	if err := cfg.Validate(mem.Total, host.NumCPU()); err != nil {
		return err
	}
	// A daemon with other state/runtime dirs on the same slice would stop this
	// one's units as orphans, so each slice (tasks and teardown) gets its own lock
	// at a fixed place.
	if paths.SliceLockDir == "" {
		return errors.New("slice lock directory is not set")
	}
	for _, slice := range []string{cfg.SliceName, cfg.TeardownSliceName()} {
		l, err := lockSlice(paths, slice)
		if err != nil {
			return err
		}
		defer l.Close()
	}
	st, err := store.Open(paths.DB())
	if err != nil {
		return err
	}
	defer st.Close()
	if err := exec.EnsureSlice(ctx, cfg.PoolMemory); err != nil {
		return fmt.Errorf("ensure slice: %w", err)
	}
	if err := exec.EnsureTeardownSlice(ctx, teardownSliceMax(cfg)); err != nil {
		return fmt.Errorf("ensure teardown slice: %w", err)
	}

	router := notify.NewRouter(cfg)
	deliverers := map[model.Channel]notify.Deliverer{}
	for _, a := range cfg.Agents {
		if a.Delivery == config.DeliveryPush {
			deliverers[model.ExecChannel(a.Name)] = notify.NewExecDeliverer(a, osexec.LookPath)
		}
	}
	if cfg.Webhook.URL != "" {
		deliverers[model.ChannelWebhook] = notify.NewWebhookDeliverer(cfg.Webhook, nil)
	}
	outbox := notify.NewOutbox(st, cfg, paths, router, deliverers, time.Now)
	sched := scheduler.New(scheduler.Deps{Store: st, Exec: exec, Host: host, Meter: meter, Cfg: cfg, Paths: paths, Route: router.Route, OnEvents: outbox.Kick})
	d := &server{host: host, opts: opts, sched: sched, st: st, paths: paths, router: router, outbox: outbox, cfg: cfg}
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
	loops.Add(1)
	go func() { defer loops.Done(); outbox.Run(runCtx) }()
	if meter != nil {
		loops.Add(1)
		go func() {
			defer loops.Done()
			meter.Run(runCtx)
		}()
	}
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
	host       platform.HostMetrics
	opts       Options
	router     *notify.Router
	outbox     *notify.Outbox
	cfg        config.Config
	sched      *scheduler.Scheduler
	st         *store.Store
	paths      config.Paths
	recovering atomic.Bool
}

func lockSlice(paths config.Paths, slice string) (*os.File, error) {
	f, err := flock(paths.SliceLock(slice))
	if errors.Is(err, unix.EWOULDBLOCK) {
		return nil, fmt.Errorf("another aq daemon already manages slice %s (lock %s held)", slice, paths.SliceLock(slice))
	}
	return f, err
}

// teardownSliceMax is teardown_concurrency × teardown_memory, saturating.
func teardownSliceMax(cfg config.Config) uint64 {
	r := cfg.Reservations
	n := uint64(max(r.TeardownConcurrency, 1))
	if r.TeardownMemory > math.MaxUint64/n {
		return math.MaxUint64
	}
	return n * r.TeardownMemory
}

// flock opens (creating) path and takes an exclusive non-blocking lock on it.
func flock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// privateDir creates dir 0700, or makes an existing one of ours 0700: it holds
// the database and job logs. A directory owned by someone else is refused.
func privateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	st, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if st.Mode().Perm() == 0o700 {
		return nil
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); !ok || int(sys.Uid) != os.Getuid() {
		return fmt.Errorf("state dir %s is not owned by this user", dir)
	}
	return os.Chmod(dir, 0o700)
}
