package daemon

import (
	"github.com/creatorofuniverses/agent-queue/internal/model"
	"testing"
	"time"
)

func TestListExpectedStart(t *testing.T) {
	e := newEnv(t)
	e.start(t)
	var a, b ItemView
	if err := e.call(t, "submit", e.submitArgs(800*mib), &a); err != nil {
		t.Fatal(err)
	}
	e.waitState(t, a.ID, model.StateRunning)
	if err := e.call(t, "submit", e.submitArgs(800*mib), &b); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		var vs []ItemView
		if err := e.call(t, "list", nil, &vs); err != nil {
			t.Fatal(err)
		}
		for _, v := range vs {
			if v.ID == b.ID && v.ExpectedStart != nil {
				return
			}
			if v.ID == a.ID && v.ExpectedStart != nil {
				t.Fatal(v)
			}
		}
		if time.Now().After(deadline) {
			t.Fatal(vs)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
