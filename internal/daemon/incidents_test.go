package daemon

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"resource-dispatcher/internal/platform/fake"
	"resource-dispatcher/internal/store"
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
