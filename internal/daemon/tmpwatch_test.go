package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/creatorofuniverses/agent-queue/internal/config"
	"github.com/creatorofuniverses/agent-queue/internal/model"
	"github.com/creatorofuniverses/agent-queue/internal/protocol"
	"github.com/creatorofuniverses/agent-queue/internal/tmpscan"
)

func TestTmpWatchShutdownBoundsStuckScan(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		entered, release := make(chan struct{}), make(chan struct{})
		defer close(release) // Release the context-insensitive scan only after the assertion.
		watch := newTmpWatch(ctx, &atomic.Pointer[tmpscan.Report]{}, nil, func(context.Context) (tmpscan.Report, error) {
			close(entered)
			<-release
			return tmpscan.Report{}, nil
		}, nil)
		done := make(chan struct{})
		go func() {
			watch.run(time.Minute)
			close(done)
		}()

		<-entered
		cancel()
		time.Sleep(5 * time.Second)
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("scan loop still running 5s after cancellation")
		}
	})
}

func TestTmpWatchDiscardsLateReport(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		last := &atomic.Pointer[tmpscan.Report]{}
		alerts := newAlertSet(config.Default())
		entered, release := make(chan struct{}), make(chan struct{})
		var events []model.Event
		first := tmpscan.Report{At: time.Now(), OrphanBytes: 3 * gib, Complete: true}
		calls := 0
		watch := newTmpWatch(ctx, last, alerts, func(context.Context) (tmpscan.Report, error) {
			calls++
			if calls == 1 {
				return first, nil
			}
			close(entered)
			<-release
			late := first
			late.At = time.Now()
			return late, nil
		}, func(_ context.Context, ev model.Event) { events = append(events, ev) })

		if _, err := watch.rescan(ctx); err != nil {
			t.Fatal(err)
		}
		go watch.run(time.Minute)
		<-entered
		cancel()
		time.Sleep(6 * time.Second)
		close(release)
		synctest.Wait()
		if got := last.Load(); got == nil || !got.At.Equal(first.At) {
			t.Errorf("late scan replaced cached report: %+v", got)
		}
		if active := alerts.Active(); len(active) != 0 || len(events) != 0 {
			t.Errorf("late scan fed orphan alarm: active %+v, events %+v", active, events)
		}
	})
}

func TestTmpWatchRunInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		scans := make(chan time.Time, 2)
		watch := newTmpWatch(ctx, &atomic.Pointer[tmpscan.Report]{}, nil, func(context.Context) (tmpscan.Report, error) {
			scans <- time.Now()
			return tmpscan.Report{}, nil
		}, nil)
		start := time.Now()
		go watch.run(time.Minute)
		synctest.Wait()
		if len(scans) != 1 {
			t.Fatalf("got %d initial scans, want one", len(scans))
		}
		if got := <-scans; !got.Equal(start) {
			t.Fatalf("initial scan at %v, want %v", got, start)
		}
		time.Sleep(time.Minute - time.Second)
		synctest.Wait()
		if len(scans) != 0 {
			t.Fatal("extra scan before one interval elapsed")
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if len(scans) != 1 {
			t.Fatalf("got %d scans after one interval, want one", len(scans))
		}
		if got, want := <-scans, start.Add(time.Minute); !got.Equal(want) {
			t.Fatalf("second scan at %v, want %v", got, want)
		}
	})
}

type tmpWatchLogLines chan string

func (lines tmpWatchLogLines) Write(p []byte) (int, error) {
	select {
	case lines <- string(p):
	default:
	}
	return len(p), nil
}

func TestTmpWatchWarnsOncePerStuckPass(t *testing.T) {
	logs := make(tmpWatchLogLines, 8)
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(previous)

	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		release := make(chan struct{})
		defer close(release)
		calls := 0
		watch := newTmpWatch(ctx, &atomic.Pointer[tmpscan.Report]{}, nil, func(context.Context) (tmpscan.Report, error) {
			calls++
			if calls > 1 {
				<-release
			}
			return tmpscan.Report{}, nil
		}, nil)
		go watch.run(time.Minute)
		time.Sleep(30 * time.Second)
		go func() { _, _ = watch.rescan(ctx) }()

		time.Sleep(2*time.Minute - time.Second)
		synctest.Wait()
		if len(logs) != 0 {
			t.Fatalf("warning before two intervals elapsed: %s", <-logs)
		}
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if len(logs) != 1 {
			t.Fatalf("got %d warnings after two intervals, want one", len(logs))
		}
		if line := <-logs; !strings.Contains(line, "level=WARN") {
			t.Fatalf("want WARN level, got %s", line)
		}
		time.Sleep(3 * time.Minute)
		synctest.Wait()
		if len(logs) != 0 {
			t.Fatalf("repeated warning for the same pass: %s", <-logs)
		}
	})
}

func TestTmpWatchOrphansAlert(t *testing.T) {
	last := &atomic.Pointer[tmpscan.Report]{}
	alerts := newAlertSet(config.Default())
	at := time.Unix(1795464773, 0)
	high := tmpscan.Report{At: at, OrphanBytes: 3 * gib / 2, CandidateBytes: 3 * gib / 2, Complete: true, Items: []tmpscan.Item{
		{Path: "/tmp/live", State: "in_use", Bytes: 4 * gib},
		{Path: "/tmp/large", State: "orphan", Bytes: gib},
		{Path: "/tmp/next", State: "candidate", Bytes: gib / 2},
		{Path: "/tmp/third", State: "orphan", Bytes: gib / 4},
		{Path: "/tmp/fourth", State: "candidate", Bytes: gib / 8},
		{Path: "/tmp/fifth", State: "orphan", Bytes: gib / 16},
		{Path: "/tmp/tiny", State: "orphan", Bytes: 1},
	}}
	partial := tmpscan.Report{At: at.Add(time.Minute)}
	highAgain := high
	highAgain.At = at.Add(2 * time.Minute)
	low := tmpscan.Report{At: at.Add(3 * time.Minute), Complete: true}
	reports := []tmpscan.Report{high, partial, highAgain, low, partial, low}
	var events []model.Event
	watch := newTmpWatch(t.Context(), last, alerts, func(context.Context) (tmpscan.Report, error) {
		r := reports[0]
		reports = reports[1:]
		return r, nil
	}, func(_ context.Context, ev model.Event) { events = append(events, ev) })

	for i, want := range []int{0, 0, 1, 1, 1, 2} {
		got, err := watch.rescan(t.Context())
		if err != nil || last.Load() == nil || !last.Load().At.Equal(got.At) || len(events) != want {
			t.Fatalf("pass %d: report %+v, cached %+v, events %+v, error %v", i, got, last.Load(), events, err)
		}
		if i == 2 {
			active := alerts.Active()
			if len(active) != 1 || active[0].Name != "tmp_orphans" || !active[0].Since.Equal(highAgain.At) || active[0].Value != float64(3*gib) {
				t.Fatalf("active after second complete high pass: %+v", active)
			}
		}
	}
	ev := events[0]
	if ev.Kind != model.EventTmpOrphans || ev.Audience != model.AudienceOperator {
		t.Fatalf("unexpected orphan alarm event: %+v", ev)
	}
	for _, want := range []string{"3.0 GiB", "/tmp/large", "/tmp/next", "/tmp/third", "/tmp/fourth", "/tmp/fifth", "aq tmp"} {
		if !strings.Contains(ev.Detail, want) {
			t.Errorf("orphan detail missing %q: %s", want, ev.Detail)
		}
	}
	if strings.Contains(ev.Detail, "/tmp/live") || strings.Contains(ev.Detail, "/tmp/tiny") ||
		strings.Index(ev.Detail, "/tmp/large") > strings.Index(ev.Detail, "/tmp/next") {
		t.Errorf("orphan detail should list five largest orphan/candidate items: %s", ev.Detail)
	}
	if events[1].Kind != model.EventTmpOrphansNormal || len(alerts.Active()) != 0 {
		t.Fatalf("no all-clear after two complete low passes: %+v", events)
	}
}

func TestTmpOpRescan(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d := server{lastTmp: &atomic.Pointer[tmpscan.Report]{}}
		root := t.TempDir()
		entered, release := make(chan struct{}, 2), make(chan struct{})
		want := tmpscan.Report{At: time.Now(), Root: root, Complete: true}
		calls := 0
		watch := newTmpWatch(t.Context(), d.lastTmp, nil, func(ctx context.Context) (tmpscan.Report, error) {
			calls++
			select {
			case entered <- struct{}{}:
			case <-ctx.Done():
				return tmpscan.Report{}, ctx.Err()
			}
			select {
			case <-release:
				return want, nil
			case <-ctx.Done():
				return tmpscan.Report{}, ctx.Err()
			}
		}, nil)
		d.rescan = watch.rescan

		if _, err := d.handle(t.Context(), "tmp", nil, nil); err == nil {
			t.Fatal("tmp succeeded before first scan")
		} else if pe := new(protocol.Error); !errors.As(err, &pe) || pe.Code != protocol.CodeUnavailable {
			t.Fatalf("tmp before first scan: %v", err)
		}

		type result struct {
			value any
			err   error
		}
		rescan := func(ctx context.Context) <-chan result {
			done := make(chan result, 1)
			go func() {
				value, err := d.handle(ctx, "tmp", json.RawMessage(`{"rescan":true}`), nil)
				done <- result{value, err}
			}()
			return done
		}
		first := rescan(t.Context())
		<-entered
		second := rescan(t.Context())
		synctest.Wait()
		close(release)
		check := func(value any, err error) {
			t.Helper()
			got, ok := value.(tmpscan.Report)
			if err != nil || !ok || !got.At.Equal(want.At) || got.Root != want.Root || !got.Complete {
				t.Fatalf("tmp returned %+v, %v, want %+v", value, err, want)
			}
		}
		for _, done := range []<-chan result{first, second} {
			r := <-done
			check(r.value, r.err)
		}
		check(d.handle(t.Context(), "tmp", nil, nil))
		if calls != 1 {
			t.Fatalf("concurrent rescans started %d scans, want one", calls)
		}
	})
}
