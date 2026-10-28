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

	"github.com/creatorofuniverses/agent-queue/internal/platform"
	"github.com/creatorofuniverses/agent-queue/internal/tmpdir"
)

// RecordResult writes JobDir/result.json from systemd's ExecStopPost env and the unit cgroup.
func RecordResult(jobDir string, getenv func(string) string, cgroupRoot string) error {
	self, _ := os.ReadFile("/proc/self/cgroup")
	return recordResult(jobDir, getenv, cgroupRoot, "/sys/dev/block", string(self))
}

// recordResult takes the /proc/self/cgroup content and the /sys/dev/block
// root explicitly (testable).
func recordResult(jobDir string, getenv func(string) string, cgroupRoot, devRoot, selfCgroup string) error {
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
		info.IOReadBytes, info.IOWriteBytes = diskIO(filepath.Join(dir, "io.stat"), devRoot)
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

// diskIO sums io.stat over physical disks: devices with a /sys/dev/block/M:m/device
// link. Device-mapper, md, loop and zram have none; they either pass the same
// bytes on to a physical disk (counted there) or never reach one (swap on zram).
// No io.stat means no io controller: nil, nil.
func diskIO(path, devRoot string) (read, write *uint64) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, nil
	}
	var r, w uint64
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if _, err := os.Stat(filepath.Join(devRoot, f[0], "device")); err != nil {
			continue
		}
		for _, kv := range f[1:] {
			k, v, _ := strings.Cut(kv, "=")
			n, _ := strconv.ParseUint(v, 10, 64)
			switch k {
			case "rbytes":
				r += n
			case "wbytes":
				w += n
			}
		}
	}
	return &r, &w
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
		// TMP_DIR is optional: units launched by an older daemon pass JOB_DIR alone.
		if len(args) < 1 || len(args) > 2 || len(args) == 2 && !filepath.IsAbs(args[1]) {
			os.Stderr.WriteString("usage: aq _record-result JOB_DIR [TMP_DIR]\n")
			return 64
		}
		code := 0
		if err := RecordResult(args[0], os.Getenv, "/sys/fs/cgroup"); err != nil {
			os.Stderr.WriteString("aq _record-result: " + err.Error() + "\n")
			code = 1
		}
		// After recording, so memory.peak still includes the temp files; on tmpfs
		// their pages would otherwise outlive the task outside any accounting.
		if len(args) == 2 {
			if err := tmpdir.RemoveIn(filepath.Dir(args[1]), filepath.Base(args[1]), os.Getuid()); err != nil {
				os.Stderr.WriteString("aq _record-result: remove temp dir: " + err.Error() + "\n")
			}
		}
		return code
	}
	return 64
}
