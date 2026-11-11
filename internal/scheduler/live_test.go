package scheduler

import (
	"testing"

	"github.com/creatorofuniverses/agent-queue/internal/config"
	"github.com/creatorofuniverses/agent-queue/internal/model"
)

// The last pass keeps what each running task and held reservation uses now, for panels.
func TestMemoryCurrentOfLastPass(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.PoolMemory = 16 * gib })
	a := e.submit(t, e.req(4*gib))
	big := e.submit(t, e.req(15*gib))
	e.tick(t)
	e.wantState(t, a.ID, model.StateRunning, model.ReasonNone)
	e.wantState(t, big.ID, model.StateQueued, model.ReasonNone)
	e.ex.SetCurrent(a.ID, 3*gib)
	e.tick(t)
	if cur, ok := e.s.MemoryCurrent(a.ID); !ok || cur != 3*gib {
		t.Fatalf("running: %d %v", cur, ok)
	}
	if _, ok := e.s.MemoryCurrent(big.ID); ok {
		t.Fatal("a queued task has no current memory")
	}
	e.finish(a.ID, success)
	e.tick(t)
	e.settle()
	e.tick(t)
	e.wantState(t, a.ID, model.StateSucceeded, model.ReasonNone)
	if _, ok := e.s.MemoryCurrent(a.ID); ok {
		t.Fatal("a finished task has no current memory")
	}
}

// A held reservation's current memory is its stand plus its nested tasks.
func TestMemoryCurrentOfReservation(t *testing.T) {
	e := newResEnv(t)
	r := e.rreq(gib)
	r.DockerProject, r.TaskMemory, r.TaskCPUs = "p", 3*gib, 2
	res := e.reserve(t, r)
	e.meter.Set("p", 300*mib)
	e.tick(t)
	e.wantState(t, res.ID, model.StateGranted, model.ReasonNone)
	nested := e.runNested(t, res.ID, 500*mib)
	e.tick(t)
	if cur, ok := e.s.MemoryCurrent(res.ID); !ok || cur != 800*mib {
		t.Fatalf("reservation: %d MiB %v", cur/mib, ok)
	}
	if cur, ok := e.s.MemoryCurrent(nested.ID); !ok || cur != 500*mib {
		t.Fatalf("nested: %d MiB %v", cur/mib, ok)
	}
}
