package daemon

import (
	"testing"
)

func TestEstimateOp(t *testing.T) {
	e := newEnv(t)
	e.start(t)
	var v EstimateView
	if err := e.call(t, "estimate", EstimateArgs{Limit: 100 * mib, CPUs: 1, TimeoutSeconds: 60}, &v); err != nil || !v.StartsNow || v.Confidence != "now" || v.DurationSource != "timeout" {
		t.Fatal(v, err)
	}
	if err := e.call(t, "estimate", EstimateArgs{Limit: 2 * gib, CPUs: 1}, &v); err == nil {
		t.Fatal("oversize")
	}
}
