package client

import (
	"context"
	"testing"
)

func TestBuildSubmitMatchesOldValidation(t *testing.T) {
	for _, tc := range []struct {
		mut  func(*SubmitOptions)
		want string
	}{
		{func(o *SubmitOptions) { o.Timeout = "" }, "submit: --timeout is required"},
		{func(o *SubmitOptions) { o.Memory = "bad" }, "submit: --memory:"},
		{func(o *SubmitOptions) { o.CPUs = 0 }, "submit: --cpus must be at least 1"},
		{func(o *SubmitOptions) { o.Env = []string{"SECRET"} }, "submit: --env must be K=V"},
	} {
		o := SubmitOptions{Cwd: t.TempDir(), Argv: []string{"true"}, Memory: "64M", Timeout: "1m", CPUs: 1}
		tc.mut(&o)
		_, e := BuildSubmit(context.Background(), o, func(string) string { return "" })
		if e == nil || len(e.Error()) < len(tc.want) || e.Error()[:len(tc.want)] != tc.want {
			t.Fatal(e, tc.want)
		}
	}
}
