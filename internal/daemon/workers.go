package daemon

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Parallel runners whose worker count the submit can see. The flag must follow
// the runner within one shell segment, so "ls -n 8" is not pytest's -n.
var workerFlags = []struct {
	runner *regexp.Regexp
	letter string
}{
	{regexp.MustCompile(`\b(pytest|py\.test)\b[^;&|]*?\s((?:-n|--numprocesses)(?:=|\s+)(\S+))`), "-n"},
	{regexp.MustCompile(`\b(make)\b[^;&|]*?\s((?:-j|--jobs)(?:=|\s+)?(\d+|\S*AQ_CPUS\S*)?)`), "-j"},
}

// cpuHint warns when argv starts more workers than the task gets CPUs: the
// unit's CPUQuota makes them share, a suite then runs several times slower
// and can hit its timeout. "" when they fit or the count is $AQ_CPUS.
func cpuHint(argv []string, cpus, hostCPUs int) string {
	cmd := strings.Join(argv, " ")
	for _, f := range workerFlags {
		m := f.runner.FindStringSubmatch(cmd)
		if m == nil {
			continue
		}
		value := strings.Trim(m[3], `"'`)
		if strings.Contains(value, "AQ_CPUS") {
			return ""
		}
		var workers, fix string
		switch n, err := strconv.Atoi(value); {
		case err == nil:
			if n <= cpus {
				return ""
			}
			workers, fix = fmt.Sprintf("%d workers", n), fmt.Sprintf("--cpus %d, or ", n)
		case value == "auto" || value == "logical":
			if cpus >= hostCPUs {
				return ""
			}
			workers = "one worker per host CPU"
		case value == "":
			workers = "unlimited workers"
		default:
			return ""
		}
		share := fmt.Sprintf("%d cpus: they share them", cpus)
		if cpus == 1 {
			share = "1 cpu: they share it"
		}
		return fmt.Sprintf("the command starts %s (%s %s) but gets %s and run slower, or time out; pass %s%s \"$AQ_CPUS\"",
			workers, m[1], strings.TrimSpace(m[2]), share, fix, f.letter)
	}
	return ""
}
