package runner

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"agent-queue/internal/platform"
)

// RecordResult writes JobDir/result.json from systemd's ExecStopPost env and the unit cgroup.
func RecordResult(jobDir string, getenv func(string) string, cgroupRoot string) error {
	self, _ := os.ReadFile("/proc/self/cgroup")
	return recordResult(jobDir, getenv, cgroupRoot, string(self))
}

// recordResult takes the /proc/self/cgroup content explicitly (testable).
func recordResult(jobDir string, getenv func(string) string, cgroupRoot, selfCgroup string) error {
	info := platform.ExitInfo{
		ServiceResult: getenv("SERVICE_RESULT"),
		ExitCode:      getenv("EXIT_CODE"),
		ExitStatus:    getenv("EXIT_STATUS"),
		EndedAt:       time.Now(),
	}
	if rel := cgroupPath(selfCgroup); rel != "" {
		dir := filepath.Join(cgroupRoot, rel)
		if b, err := os.ReadFile(filepath.Join(dir, "memory.peak")); err == nil {
			info.MemoryPeak, _ = strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
		}
		if f, err := os.Open(filepath.Join(dir, "memory.events")); err == nil {
			sc := bufio.NewScanner(f)
			for sc.Scan() {
				k, v, ok := strings.Cut(sc.Text(), " ")
				if !ok {
					continue
				}
				n, _ := strconv.ParseUint(strings.TrimSpace(v), 10, 64)
				switch k {
				case "oom":
					info.OwnOOMEvents = n
				case "oom_kill":
					info.OwnOOMKills = n
				}
			}
			f.Close()
		}
	}
	if b, err := os.ReadFile(filepath.Join(jobDir, "launch_error.txt")); err == nil {
		info.LaunchError = strings.TrimSpace(string(b))
	}
	if b, err := os.ReadFile(filepath.Join(jobDir, "exec.json")); err == nil {
		var m execMeta
		if json.Unmarshal(b, &m) == nil {
			info.LogsTruncated = m.LogsTruncated
		}
	}

	data, err := json.Marshal(info)
	if err != nil {
		return err
	}
	tmp := filepath.Join(jobDir, "result.json.tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(jobDir, "result.json"))
}

// cgroupPath extracts the cgroup v2 path from /proc/self/cgroup content.
func cgroupPath(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if p, ok := strings.CutPrefix(line, "0::"); ok {
			return strings.TrimPrefix(strings.TrimSpace(p), "/")
		}
	}
	return ""
}

// ReadResultFile returns (nil, nil) when result.json does not exist yet.
func ReadResultFile(jobDir string) (*platform.ExitInfo, error) {
	b, err := os.ReadFile(filepath.Join(jobDir, "result.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var info platform.ExitInfo
	if err := json.Unmarshal(b, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// Main routes the hidden subcommands; args excludes the subcommand name.
func Main(sub string, args []string) int {
	switch sub {
	case "_exec":
		a, err := ParseExecArgs(args)
		if err != nil {
			os.Stderr.WriteString("aq _exec: " + err.Error() + "\n")
			return 64
		}
		return Exec(a)
	case "_record-result":
		if len(args) != 1 {
			os.Stderr.WriteString("usage: aq _record-result JOB_DIR\n")
			return 64
		}
		if err := RecordResult(args[0], os.Getenv, "/sys/fs/cgroup"); err != nil {
			os.Stderr.WriteString("aq _record-result: " + err.Error() + "\n")
			return 1
		}
		return 0
	}
	return 64
}
