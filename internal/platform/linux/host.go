// Package linux implements platform interfaces on Linux via procfs.
package linux

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"resource-dispatcher/internal/platform"
)

// Host reads host metrics from procfs rooted at procRoot.
type Host struct {
	procRoot string
}

var _ platform.HostMetrics = (*Host)(nil)

func NewHost(procRoot string) *Host { return &Host{procRoot: procRoot} }

func (h *Host) readFile(rel string, parse func(io.Reader) error) error {
	f, err := os.Open(filepath.Join(h.procRoot, rel))
	if err != nil {
		return err
	}
	defer f.Close()
	return parse(f)
}

func (h *Host) Memory() (m platform.MemInfo, err error) {
	err = h.readFile("meminfo", func(r io.Reader) (e error) { m, e = parseMeminfo(r); return })
	return
}

func (h *Host) PressureFullAvg10() (v float64, err error) {
	err = h.readFile("pressure/memory", func(r io.Reader) (e error) { v, e = parsePSIFullAvg10(r); return })
	return
}

func (h *Host) KernelOOMKills() (v uint64, err error) {
	err = h.readFile("vmstat", func(r io.Reader) (e error) { v, e = parseVMStatOOMKill(r); return })
	return
}

func (h *Host) DiskFree(path string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize), nil
}

func (h *Host) NumCPU() int { return runtime.NumCPU() }

func parseMeminfo(r io.Reader) (platform.MemInfo, error) {
	var m platform.MemInfo
	var haveTotal bool
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		key, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		var dst *uint64
		switch key {
		case "MemTotal":
			dst, haveTotal = &m.Total, true
		case "MemAvailable":
			dst = &m.Available
		case "SwapTotal":
			dst = &m.SwapTotal
		case "SwapFree":
			dst = &m.SwapFree
		default:
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			return m, fmt.Errorf("meminfo: empty value for %s", key)
		}
		kb, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			return m, fmt.Errorf("meminfo %s: %w", key, err)
		}
		*dst = kb * 1024
	}
	if err := sc.Err(); err != nil {
		return m, err
	}
	if !haveTotal {
		return m, fmt.Errorf("meminfo: MemTotal not found")
	}
	return m, nil
}

func parsePSIFullAvg10(r io.Reader) (float64, error) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 0 || fields[0] != "full" {
			continue
		}
		for _, f := range fields[1:] {
			if v, ok := strings.CutPrefix(f, "avg10="); ok {
				return strconv.ParseFloat(v, 64)
			}
		}
	}
	if err := sc.Err(); err != nil {
		return 0, err
	}
	return 0, fmt.Errorf("psi: full avg10 not found")
}

// parseVMStatOOMKill returns 0 when the counter is absent (old kernels).
func parseVMStatOOMKill(r io.Reader) (uint64, error) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "oom_kill "); ok {
			return strconv.ParseUint(strings.TrimSpace(v), 10, 64)
		}
	}
	return 0, sc.Err()
}
