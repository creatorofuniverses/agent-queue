package daemon

import "testing"

func TestCPUHint(t *testing.T) {
	cases := []struct {
		argv []string
		cpus int
		want string
	}{
		{[]string{"pytest", "-n", "8", "tests"}, 1,
			"the command starts 8 workers (pytest -n 8) but gets 1 cpu: they share it and run slower, or time out; pass --cpus 8, or -n \"$AQ_CPUS\""},
		{[]string{"bash", "-lc", "cd api && uv run pytest --numprocesses=4 -q"}, 2,
			"the command starts 4 workers (pytest --numprocesses=4) but gets 2 cpus: they share them and run slower, or time out; pass --cpus 4, or -n \"$AQ_CPUS\""},
		{[]string{"pytest", "-n", "auto"}, 4,
			"the command starts one worker per host CPU (pytest -n auto) but gets 4 cpus: they share them and run slower, or time out; pass -n \"$AQ_CPUS\""},
		{[]string{"make", "-j8", "all"}, 2,
			"the command starts 8 workers (make -j8) but gets 2 cpus: they share them and run slower, or time out; pass --cpus 8, or -j \"$AQ_CPUS\""},
		{[]string{"make", "-j", "test"}, 4,
			"the command starts unlimited workers (make -j) but gets 4 cpus: they share them and run slower, or time out; pass -j \"$AQ_CPUS\""},
		{[]string{"pytest", "-n", "8"}, 8, ""},
		{[]string{"pytest", "-n", "auto"}, 16, ""}, // as many as the host has
		{[]string{"bash", "-lc", `pytest -n "$AQ_CPUS" tests`}, 1, ""},
		{[]string{"make", "-j2"}, 4, ""},
		{[]string{"go", "test", "./..."}, 1, ""},
		{[]string{"ls", "-n", "8"}, 1, ""}, // -n of another command
	}
	for _, c := range cases {
		if got := cpuHint(c.argv, c.cpus, 16); got != c.want {
			t.Errorf("cpuHint(%q, %d, 16) =\n %q\nwant\n %q", c.argv, c.cpus, got, c.want)
		}
	}
}
