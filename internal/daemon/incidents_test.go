package daemon

import (
	"agent-queue/internal/platform"
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"agent-queue/internal/platform/fake"
	"agent-queue/internal/store"
)

var errTest = errors.New("test error")

func newIncTest(t *testing.T, pressure func() (bool, float64)) (*incidentSampler, *fake.Host, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "aq.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	host := fake.NewHost()
	return &incidentSampler{host: host, st: st, pressure: pressure}, host, st
}

func TestIncidentsOOMDelta(t *testing.T) {
	s, host, st := newIncTest(t, func() (bool, float64) { return false, 0 })
	for _, v := range []uint64{8, 8, 10} {
		host.SetOOMKills(v)
		s.step(t.Context())
	}
	inc, err := st.Incidents(t.Context(), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(inc) != 1 || inc[0].Kind != "kernel_oom" || inc[0].Value != 2 {
		t.Fatalf("%+v", inc)
	}
}

func TestIncidentsPSI(t *testing.T) {
	closed, avg := false, 0.5
	s, _, st := newIncTest(t, func() (bool, float64) { return closed, avg })
	s.step(t.Context()) // baseline
	closed, avg = true, 7.5
	s.step(t.Context())
	s.step(t.Context()) // no change
	closed, avg = false, 0.8
	s.step(t.Context())
	inc, err := st.Incidents(t.Context(), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(inc) != 2 || inc[0].Kind != "psi_close" || inc[0].Value != 7.5 || inc[1].Kind != "psi_open" || inc[1].Value != 0.8 {
		t.Fatalf("%+v", inc)
	}
}

func TestKernelOOMExcludesSliceKills(t *testing.T) {
	for _, tc := range []struct {
		kernel, slice uint64
		bad           bool
		want          float64
	}{{3, 2, false, 1}, {2, 2, false, 0}, {1, 1, false, 0}, {3, 0, true, 3}} {
		s, h, st := newIncTest(t, func() (bool, float64) { return false, 0 })
		n := uint64(4)
		s.sliceOOMKills = func() (uint64, error) {
			if tc.bad {
				return 0, errTest
			}
			return n, nil
		}
		h.SetOOMKills(10)
		s.step(t.Context())
		n += tc.slice
		h.SetOOMKills(10 + tc.kernel)
		s.step(t.Context())
		incs, e := st.Incidents(t.Context(), time.Time{})
		if e != nil || tc.want == 0 && len(incs) != 0 || tc.want > 0 && (len(incs) != 1 || incs[0].Value != tc.want || tc.bad && incs[0].Detail != "unattributed") {
			t.Fatal(incs, e)
		}
	}
}

type fakeOOMLog struct{ calls int }

func (f *fakeOOMLog) Kills(ctx context.Context) ([]platform.OOMKill, error) {
	f.calls++
	return []platform.OOMKill{{At: time.Now(), Cgroup: "/x.scope"}}, nil
}
func TestIncidentsRecordOOMDKills(t *testing.T) {
	s, _, st := newIncTest(t, func() (bool, float64) { return false, 0 })
	f := &fakeOOMLog{}
	s.oomd = f
	for i := 0; i < 13; i++ {
		s.step(t.Context())
	}
	incs, e := st.Incidents(t.Context(), time.Time{})
	if e != nil || f.calls != 3 || len(incs) != 3 || incs[0].Kind != "oomd_kill" || incs[0].Detail != "/x.scope" || incs[0].Value != 1 {
		t.Fatal(incs, f, e)
	}
}

type flakyOOMHost struct {
	*fake.Host
	fail bool
}

func (h *flakyOOMHost) KernelOOMKills() (uint64, error) {
	if h.fail {
		return 0, errTest
	}
	return h.Host.KernelOOMKills()
}

func TestKernelOOMReadFailureKeepsSliceBaseline(t *testing.T) {
	s, h, st := newIncTest(t, func() (bool, float64) { return false, 0 })
	fh := &flakyOOMHost{Host: h}
	s.host = fh
	n := uint64(4)
	s.sliceOOMKills = func() (uint64, error) { return n, nil }
	h.SetOOMKills(10)
	s.step(t.Context())
	// One task-slice kill lands while the kernel counter is unreadable.
	n, fh.fail = 5, true
	h.SetOOMKills(11)
	s.step(t.Context())
	fh.fail = false
	s.step(t.Context())
	incs, e := st.Incidents(t.Context(), time.Time{})
	if e != nil || len(incs) != 0 {
		t.Fatal(incs, e)
	}
}
