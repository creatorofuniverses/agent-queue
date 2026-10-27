package daemon

import (
	"slices"
	"testing"

	"resource-dispatcher/internal/config"
	"resource-dispatcher/internal/platform"
	"resource-dispatcher/internal/platform/fake"
)

func newGuardTest(avail, sliceCur uint64) (*guard, *fake.Executor, *fake.Host, *[]bool) {
	ex, host := fake.NewExecutor(), fake.NewHost()
	host.Set(platform.MemInfo{Total: 30 * gib, Available: avail}) // floor 10% = 3G
	ex.SetSlice(sliceCur, 0)
	var calls []bool
	cfg := config.Default()
	cfg.PoolMemory = 16 * gib
	return &guard{exec: ex, host: host, cfg: cfg, setActive: func(v bool) { calls = append(calls, v) }}, ex, host, &calls
}

func TestGuardLowersAndRaises(t *testing.T) {
	g, ex, host, calls := newGuardTest(20*gib, 2*gib)
	g.step()
	host.Set(platform.MemInfo{Total: 30 * gib, Available: 4 * gib})
	g.step()
	c := ex.Ceilings()
	if last := c[len(c)-1]; last != [2]uint64{3 * gib, 3 * gib} {
		t.Fatalf("ceilings %v", c)
	}
	if got := *calls; got[len(got)-1] != true {
		t.Fatalf("setActive %v", got)
	}
	host.Set(platform.MemInfo{Total: 30 * gib, Available: 20 * gib})
	g.step()
	c = ex.Ceilings()
	if last := c[len(c)-1]; last != [2]uint64{16 * gib, 16 * gib} {
		t.Fatalf("ceilings %v", c)
	}
	if got := *calls; got[len(got)-1] != false {
		t.Fatalf("setActive %v", got)
	}
}

func TestGuardNoFlapping(t *testing.T) {
	g, ex, host, _ := newGuardTest(4*gib, 2*gib)
	for i := range 6 {
		a := 4 * gib
		if i%2 == 1 {
			a += 10 * mib
		}
		host.Set(platform.MemInfo{Total: 30 * gib, Available: a})
		g.step()
	}
	if c := ex.Ceilings(); len(c) != 1 {
		t.Fatalf("applied %d times: %v", len(c), c)
	}
}

type failHost struct {
	*fake.Host
	fail bool
}

func (f *failHost) Memory() (platform.MemInfo, error) {
	if f.fail {
		return platform.MemInfo{}, errTest
	}
	return f.Host.Memory()
}

func TestGuardKeepsCeilingOnError(t *testing.T) {
	g, ex, host, _ := newGuardTest(4*gib, 2*gib)
	fh := &failHost{Host: host}
	g.host = fh
	g.step()
	fh.fail = true
	g.step()
	fh.fail = false
	host.Set(platform.MemInfo{Total: 30 * gib, Available: 4 * gib})
	g.step()
	if c := ex.Ceilings(); !slices.Equal(c, [][2]uint64{{3 * gib, 3 * gib}}) {
		t.Fatalf("%v", c)
	}
}
