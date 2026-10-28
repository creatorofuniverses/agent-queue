package scheduler

import (
	"testing"
	"time"

	"resource-dispatcher/internal/model"
	"resource-dispatcher/internal/platform"
)

func TestMapOutcome(t *testing.T) {
	oom := platform.ExitInfo{ServiceResult: "oom-kill", ExitCode: "killed", ExitStatus: "KILL", OwnOOMKills: 1}
	ownOOM := oom
	ownOOM.OwnOOMEvents = 1
	oomByEventsOnly := platform.ExitInfo{ServiceResult: "exit-code", ExitCode: "exited", ExitStatus: "137", OwnOOMKills: 1, OwnOOMEvents: 1}
	cases := []struct {
		name          string
		ei            platform.ExitInfo
		cancel, guard bool
		state         model.State
		reason        model.Reason
	}{
		{"launch error", platform.ExitInfo{ServiceResult: "exit-code", ExitStatus: "127", LaunchError: "exec: not found"}, true, true, model.StateFailed, model.ReasonLaunchFailed},
		{"cancel wins over oom", ownOOM, true, false, model.StateCancelled, model.ReasonCancelled},
		{"cancel after success", platform.ExitInfo{ServiceResult: "success", ExitStatus: "0"}, true, false, model.StateCancelled, model.ReasonCancelled},
		{"own oom", ownOOM, false, true, model.StateFailed, model.ReasonOOM},
		{"oom kills counted, own max events", oomByEventsOnly, false, false, model.StateFailed, model.ReasonOOM},
		{"oom under guard", oom, false, true, model.StateFailed, model.ReasonOOMHostGuard},
		{"oom group", oom, false, false, model.StateFailed, model.ReasonOOMGroup},
		{"oom-kill without counters", platform.ExitInfo{ServiceResult: "oom-kill"}, false, false, model.StateFailed, model.ReasonOOMGroup},
		{"timeout", platform.ExitInfo{ServiceResult: "timeout", ExitCode: "killed", ExitStatus: "TERM"}, false, false, model.StateFailed, model.ReasonTimeout},
		{"success", platform.ExitInfo{ServiceResult: "success", ExitCode: "exited", ExitStatus: "0"}, false, false, model.StateSucceeded, model.ReasonNone},
		{"exit-code 0", platform.ExitInfo{ServiceResult: "exit-code", ExitCode: "exited", ExitStatus: "0"}, false, false, model.StateSucceeded, model.ReasonNone},
		{"exit-code 3", platform.ExitInfo{ServiceResult: "exit-code", ExitCode: "exited", ExitStatus: "3"}, false, false, model.StateFailed, model.ReasonCommandFailed},
		{"signal", platform.ExitInfo{ServiceResult: "signal", ExitCode: "killed", ExitStatus: "SEGV"}, false, false, model.StateFailed, model.ReasonCommandFailed},
		{"empty", platform.ExitInfo{}, false, false, model.StateFailed, model.ReasonCommandFailed},
	}
	for _, c := range cases {
		st, r := MapOutcome(c.ei, c.cancel, c.guard)
		if st != c.state || r != c.reason {
			t.Errorf("%s: got %s/%s, want %s/%s", c.name, st, r, c.state, c.reason)
		}
	}
}

func TestExitDetails(t *testing.T) {
	cases := []struct {
		status string
		code   int // -1: nil
		signal string
	}{
		{"3", 3, ""},
		{"0", 0, ""},
		{"KILL", -1, "SIGKILL"},
		{"SIGTERM", -1, "SIGTERM"},
		{"", -1, ""},
	}
	for _, c := range cases {
		code, sig := exitDetails(platform.ExitInfo{ExitStatus: c.status})
		if (c.code < 0) != (code == nil) || code != nil && *code != c.code || sig != c.signal {
			t.Errorf("%q: got %v %q, want %d %q", c.status, code, sig, c.code, c.signal)
		}
	}
}

func TestGuardAttributionWindow(t *testing.T) {
	e := newEnv(t, nil)
	t0 := e.clk.Now()
	e.s.SetGuardActive(true)
	e.clk.Advance(time.Second)
	e.s.SetGuardActive(true)
	e.clk.Advance(19 * time.Second)
	e.s.SetGuardActive(false)
	e.clk.Advance(time.Second)
	check := func(at time.Time, want model.Reason) {
		t.Helper()
		it := e.submit(t, e.req(100*mib))
		e.tick(t)
		e.ex.Finish(it.ID, platform.ExitInfo{ServiceResult: "oom-kill", OwnOOMKills: 1, EndedAt: at})
		e.ex.SetAlive(it.ID, false)
		e.tick(t)
		e.wantState(t, it.ID, model.StateFailed, want)
	}
	check(t0.Add(19*time.Second), model.ReasonOOMHostGuard)
	check(t0.Add(24*time.Second), model.ReasonOOMHostGuard)
	check(t0.Add(30*time.Second), model.ReasonOOMGroup)
	check(t0.Add(-10*time.Second), model.ReasonOOMGroup)
	e.clk.Advance(t0.Add(40 * time.Second).Sub(e.clk.Now()))
	e.s.SetGuardActive(true)
	e.clk.Advance(time.Second)
	e.s.SetGuardActive(false)
	e.clk.Advance(time.Second)
	check(t0.Add(19*time.Second), model.ReasonOOMHostGuard)
	e.clk.Advance(t0.Add(60 * time.Second).Sub(e.clk.Now()))
	e.s.SetGuardActive(true)
	check(t0.Add(30*time.Second), model.ReasonOOMGroup)
}
func TestExpiredAtRecordedOnce(t *testing.T) {
	for _, nested := range []bool{false, true} {
		e := newResEnv(t)
		r := e.rreq(100 * mib)
		r.TTL = time.Minute
		if nested {
			r.TaskMemory = 256 * mib
			r.TaskCPUs = 1
		}
		it := e.reserve(t, r)
		e.tick(t)
		if nested {
			nr := e.req(100 * mib)
			nr.ParentID = it.ID
			e.submit(t, nr)
			e.tick(t)
		}
		e.clk.Advance(61 * time.Second)
		at := e.clk.Now()
		e.tick(t)
		got := e.get(t, it.ID)
		if !got.ExpiredAt.Equal(at) {
			t.Fatal(got)
		}
		e.clk.Advance(time.Second)
		e.tick(t)
		if !e.get(t, it.ID).ExpiredAt.Equal(at) {
			t.Fatal("changed")
		}
	}
	e := newResEnv(t)
	it := e.reserve(t, e.rreq(100*mib))
	e.tick(t)
	e.s.Release(e.ctx, it.ID, false)
	if !e.get(t, it.ID).ExpiredAt.IsZero() {
		t.Fatal("explicit release expired")
	}
}
