// Package units parses and formats memory sizes (binary multiples).
package units

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

const (
	kib = 1 << 10
	mib = 1 << 20
	gib = 1 << 30
	tib = 1 << 40
)

// ParseSize parses "4G", "512M", "1024K", "2T" (binary multiples).
// A bare number is MiB. The result must be positive.
func ParseSize(s string) (uint64, error) {
	orig := s
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("invalid size %q: empty", orig)
	}
	mult := float64(mib)
	switch last := s[len(s)-1]; last {
	case 'K', 'k':
		mult, s = kib, s[:len(s)-1]
	case 'M', 'm':
		mult, s = mib, s[:len(s)-1]
	case 'G', 'g':
		mult, s = gib, s[:len(s)-1]
	case 'T', 't':
		mult, s = tib, s[:len(s)-1]
	}
	n, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
		return 0, fmt.Errorf("invalid size %q", orig)
	}
	if n <= 0 {
		return 0, fmt.Errorf("invalid size %q: must be positive", orig)
	}
	v := n * mult
	if v >= math.MaxUint64 {
		return 0, fmt.Errorf("invalid size %q: too large", orig)
	}
	b := uint64(v)
	if b == 0 {
		return 0, fmt.Errorf("invalid size %q: must be positive", orig)
	}
	return b, nil
}

// FormatSize renders bytes as "4.0 GiB", "512 MiB", "64 KiB" or "100 B".
func FormatSize(b uint64) string {
	switch {
	case b >= tib:
		return fmt.Sprintf("%.1f TiB", float64(b)/tib)
	case b >= gib:
		return fmt.Sprintf("%.1f GiB", float64(b)/gib)
	case b >= mib:
		return fmt.Sprintf("%d MiB", b/mib)
	case b >= kib:
		return fmt.Sprintf("%d KiB", b/kib)
	}
	return fmt.Sprintf("%d B", b)
}

// ParseDuration accepts Go durations with an optional leading whole number of days.
func ParseDuration(s string) (time.Duration, error) {
	bad := func() (time.Duration, error) { return 0, fmt.Errorf("invalid duration %q", s) }
	if s == "" || strings.ContainsAny(s, "+-") {
		return bad()
	}
	var days time.Duration
	rest := s
	if i := strings.IndexByte(s, 'd'); i >= 0 {
		if i == 0 {
			return bad()
		}
		for _, c := range s[:i] {
			if c < '0' || c > '9' {
				return bad()
			}
		}
		n, e := strconv.ParseUint(s[:i], 10, 64)
		if e != nil || n > uint64(math.MaxInt64/int64(24*time.Hour)) {
			return bad()
		}
		days = time.Duration(n) * 24 * time.Hour
		rest = s[i+1:]
		if rest == "" {
			return days, nil
		}
	}
	d, e := time.ParseDuration(rest)
	if e != nil || d < 0 || d > time.Duration(math.MaxInt64)-days {
		return bad()
	}
	return days + d, nil
}
