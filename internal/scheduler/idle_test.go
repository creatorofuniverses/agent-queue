package scheduler

import (
	"strings"
	"testing"
	"time"

	"github.com/creatorofuniverses/agent-queue/internal/model"
)

func TestIdleReservationReportedOncePerIdleStretch(t *testing.T) {
	e := newResEnv(t)
	r := e.rreq(gib)
	r.DockerProject, r.TaskMemory, r.TaskCPUs, r.SessionID, r.ClientType = "p", gib, 1, "s1", "claude"
	a := e.reserve(t, r)
	e.meter.Set("p", 0) // granted, but the stand was never brought up
	e.tick(t)
	idle := func() []model.Event { return e.events(t, a.ID, model.EventReservationIdle) }

	e.clk.Advance(9 * time.Minute)
	e.tick(t)
	if n := len(idle()); n != 0 {
		t.Fatalf("%d idle events before idle_after", n)
	}
	e.clk.Advance(time.Minute)
	e.tick(t)
	evs := idle()
	if len(evs) != 2 { // the session and the operator
		t.Fatalf("events %+v", evs)
	}
	if d := evs[0].Detail; !strings.Contains(d, "1.0 GiB stand + 1.0 GiB task budget") || !strings.Contains(d, "for 10m0s") || !strings.Contains(d, "aq release "+string(a.ID)) {
		t.Fatalf("detail %q", d)
	}
	e.clk.Advance(time.Hour)
	e.tick(t)
	if n := len(idle()); n != 2 {
		t.Fatalf("reported again in the same idle stretch: %d", n)
	}

	// Work resumes, then stops: a new idle stretch is reported again.
	n := e.runNested(t, a.ID, 100*mib)
	e.tick(t)
	e.finish(n.ID, success)
	e.tick(t)
	e.clk.Advance(10 * time.Minute)
	e.tick(t)
	if n := len(idle()); n != 4 {
		t.Fatalf("after a new idle stretch: %d events", n)
	}
}

func TestIdleNeedsAMeasuredStand(t *testing.T) {
	e := newResEnv(t)
	a := e.reserve(t, e.rreq(gib)) // no docker project: aq cannot tell an idle stand
	e.tick(t)
	e.clk.Advance(time.Hour)
	e.tick(t)
	if evs := e.events(t, a.ID, model.EventReservationIdle); len(evs) != 0 {
		t.Fatalf("events %+v", evs)
	}

	e = newResEnv(t)
	r := e.rreq(gib)
	r.DockerProject = "p"
	b := e.reserve(t, r)
	e.meter.Set("p", 300*mib) // the stand is up
	e.tick(t)
	e.clk.Advance(time.Hour)
	e.tick(t)
	if evs := e.events(t, b.ID, model.EventReservationIdle); len(evs) != 0 {
		t.Fatalf("a running stand is not idle: %+v", evs)
	}
}
