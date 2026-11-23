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
	done   chan struct{}
	report tmpscan.Report
	err    error
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
	for {
		select {
		case <-w.ctx.Done():
			// The scan may have been started by a socket request. Wait for it
			// before the daemon closes the store used by the event emitter.
			w.mu.Lock()
			pass := w.current
			w.mu.Unlock()
			if pass != nil {
				<-pass.done
			}
			return
		case <-t.C:
			if _, err := w.rescan(w.ctx); err != nil && w.ctx.Err() == nil {
				slog.Warn("scan /tmp", "err", err)
			}
			t.Reset(every)
		}
	}
}

func (w *tmpWatch) rescan(ctx context.Context) (tmpscan.Report, error) {
	w.mu.Lock()
	if err := w.ctx.Err(); err != nil {
		w.mu.Unlock()
		return tmpscan.Report{}, err
	}
	pass := w.current
	if pass == nil {
		pass = &tmpPass{done: make(chan struct{})}
		w.current = pass
		go w.scanOnce(pass)
	}
	w.mu.Unlock()

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
	if pass.err == nil {
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
