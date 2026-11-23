package daemon

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/creatorofuniverses/agent-queue/internal/model"
	"github.com/creatorofuniverses/agent-queue/internal/platform"
	"github.com/creatorofuniverses/agent-queue/internal/protocol"
	"github.com/creatorofuniverses/agent-queue/internal/tmpscan"
)

// TmpResponse carries the inventory and current usage of its filesystem.
type TmpResponse struct {
	tmpscan.Report
	FS *platform.FSUsage `json:"fs"`
}

func (d *server) tmpReport(ctx context.Context, rescan bool) (TmpResponse, error) {
	if rescan && d.rescan != nil {
		report, err := d.rescan(ctx)
		if err != nil {
			return TmpResponse{}, err
		}
		return TmpResponse{Report: report, FS: d.scanFilesystem(report.Root)}, nil
	}
	if d.lastTmp != nil {
		if report := d.lastTmp.Load(); report != nil {
			return TmpResponse{Report: *report, FS: d.scanFilesystem(report.Root)}, nil
		}
	}
	return TmpResponse{}, &protocol.Error{Code: protocol.CodeUnavailable, Message: "no /tmp scan available yet"}
}

func (d *server) scanFilesystem(root string) *platform.FSUsage {
	if root == "" {
		return nil
	}
	fs, err := d.host.FSUsage(root)
	if err != nil {
		return nil
	}
	return &fs
}

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
			w.mu.Lock()
			pass := w.startScanLocked(true)
			w.mu.Unlock()
			if pass == nil {
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

// startScanLocked shares the current pass with callers. Joining a manual pass
// from the scheduler does not turn it into an alarm observation.
func (w *tmpWatch) startScanLocked(scheduled bool) *tmpPass {
	if w.ctx.Err() != nil {
		return nil
	}
	if w.current == nil {
		pass := &tmpPass{done: make(chan struct{}), started: time.Now()}
		w.current = pass
		go w.scanOnce(pass, scheduled)
	}
	return w.current
}

func (w *tmpWatch) rescan(ctx context.Context) (tmpscan.Report, error) {
	w.mu.Lock()
	if w.current == nil {
		if last := w.last.Load(); last != nil && time.Since(last.At) < time.Minute {
			w.mu.Unlock()
			return *last, nil
		}
	}
	pass := w.startScanLocked(false)
	w.mu.Unlock()
	if pass == nil {
		return tmpscan.Report{}, w.ctx.Err()
	}

	// One caller leaving does not cancel the inventory shared by the others.
	select {
	case <-ctx.Done():
		return tmpscan.Report{}, ctx.Err()
	case <-pass.done:
		return pass.report, pass.err
	}
}

func (w *tmpWatch) scanOnce(pass *tmpPass, scheduled bool) {
	pass.report, pass.err = w.scan(w.ctx)
	if pass.err == nil && w.ctx.Err() == nil {
		w.last.Store(&pass.report)
		if scheduled && w.alerts != nil {
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
