package daemon

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/creatorofuniverses/agent-queue/internal/model"
	"github.com/creatorofuniverses/agent-queue/internal/tmpscan"
)

type tmpWatch struct {
	ctx    context.Context
	last   *atomic.Pointer[tmpscan.Report]
	alerts *alertSet
	scan   func(context.Context) (tmpscan.Report, error)
	emit   func(context.Context, model.Event)

	mu      sync.Mutex
	current *tmpPass
}

type tmpPass struct {
	done    chan struct{}
	started time.Time
	report  tmpscan.Report
	err     error
}

func newTmpWatch(ctx context.Context, last *atomic.Pointer[tmpscan.Report], alerts *alertSet,
	scan func(context.Context) (tmpscan.Report, error), emit func(context.Context, model.Event),
) *tmpWatch {
	return &tmpWatch{ctx: ctx, last: last, alerts: alerts, scan: scan, emit: emit}
}

// run takes an initial inventory and allows an interval between automatic scans.
func (w *tmpWatch) run(every time.Duration) {
	t := time.NewTimer(0)
	defer t.Stop()
	defer w.waitForShutdown()
	for {
		select {
		case <-w.ctx.Done():
			return
		case <-t.C:
			pass, err := w.startScan()
			if err != nil {
				return
			}
			w.waitScheduled(pass, every)
			t.Reset(every)
		}
	}
}

func (w *tmpWatch) waitScheduled(pass *tmpPass, every time.Duration) {
	t := time.NewTimer(time.Until(pass.started.Add(every).Add(every)))
	defer t.Stop()
	warn := t.C
	for {
		select {
		case <-w.ctx.Done():
			return
		case <-pass.done:
			if pass.err != nil && w.ctx.Err() == nil {
				slog.Warn("scan /tmp", "err", pass.err)
			}
			return
		case <-warn:
			warn = nil
			select {
			case <-w.ctx.Done():
			case <-pass.done:
			default:
				slog.Warn("scan /tmp still running", "elapsed", time.Since(pass.started))
			}
		}
	}
}

func (w *tmpWatch) waitForShutdown() {
	w.mu.Lock()
	pass := w.current // The pass may have been started by a socket request.
	w.mu.Unlock()
	if pass == nil {
		return
	}
	// A /proc read can block in the kernel even after cancellation.
	t := time.NewTimer(5 * time.Second)
	defer t.Stop()
	select {
	case <-pass.done:
	case <-t.C:
	}
}

func (w *tmpWatch) startScan() (*tmpPass, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.ctx.Err(); err != nil {
		return nil, err
	}
	if w.current == nil {
		pass := &tmpPass{done: make(chan struct{}), started: time.Now()}
		w.current = pass
		go w.scanOnce(pass)
	}
	return w.current, nil
}

func (w *tmpWatch) rescan(ctx context.Context) (tmpscan.Report, error) {
	pass, err := w.startScan()
	if err != nil {
		return tmpscan.Report{}, err
	}

	// One caller leaving does not cancel the inventory shared by the others.
	select {
	case <-ctx.Done():
		return tmpscan.Report{}, ctx.Err()
	case <-pass.done:
		return pass.report, pass.err
	}
}

func (w *tmpWatch) scanOnce(pass *tmpPass) {
	pass.report, pass.err = w.scan(w.ctx)
	if pass.err == nil && w.ctx.Err() == nil {
		w.last.Store(&pass.report)
		if w.alerts != nil {
			if ev, fired := w.alerts.observeOrphans(pass.report); fired && w.emit != nil {
				emitAlert(w.ctx, w.emit, model.EventTmpOrphans, pass.report.At, ev, tmpAlertDetails(&pass.report))
			}
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.current = nil
	close(pass.done)
}
