package platform

import (
	"path/filepath"
	"strings"
)

// AgentIndex returns the first configured agent matching a process's comm or
// executable basename in its NUL-delimited command line, or -1 if none match.
func AgentIndex(agents []string, comm, cmdline string) int {
	comm = strings.TrimSpace(comm)
	argv0, _, _ := strings.Cut(cmdline, "\x00")
	executable := filepath.Base(argv0)
	for i, name := range agents {
		if name == comm || name == executable {
			return i
		}
	}
	return -1
}
