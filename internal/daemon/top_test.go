package daemon

import (
	"errors"
	"resource-dispatcher/internal/platform"
	"resource-dispatcher/internal/platform/fake"
	"testing"
)

func TestTopView(t *testing.T) {
	e := newEnv(t)
	m := &fake.UsageMeter{}
	m.Set(platform.Usage{Agents: []platform.AgentUsage{{Name: "claude", Bytes: 5 * gib, Procs: 2}}, Docker: 7 * gib, Tasks: 2 * gib})
	e.opts = Options{Usage: m}
	e.host.Set(platform.MemInfo{Total: 32 * gib, Available: 12 * gib})
	e.start(t)
	var v TopView
	if err := e.call(t, "top", nil, &v); err != nil || v.Physical.Used != 20*gib || v.Physical.Other != 6*gib {
		t.Fatal(v, err)
	}
	m.Fail(errors.New("unavailable"))
	if err := e.call(t, "top", nil, &v); err != nil || v.Physical.Error == "" || v.Physical.Used != 0 || v.Host.MemTotal != 32*gib {
		t.Fatal(v, err)
	}
}
