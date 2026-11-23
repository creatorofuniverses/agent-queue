package daemon

import (
	"context"
	"encoding/json"
	"errors"
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
