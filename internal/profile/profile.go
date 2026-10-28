// Package profile estimates resources from completed task histories.
package profile

import (
	"cmp"
	"fmt"
	"github.com/creatorofuniverses/agent-queue/internal/model"
	"math"
	"slices"
	"time"
	"unicode"
	"unicode/utf8"
)

func Percentile[T cmp.Ordered](vals []T, p int) T {
	if len(vals) == 0 {
		var z T
		return z
	}
	v := slices.Clone(vals)
	slices.Sort(v)
	p = max(1, min(100, p))
	return v[(p*len(v)+99)/100-1]
}

type Run struct {
	ItemID                model.ItemID
	Profile               string
	EndedAt               time.Time
	State                 model.State
	Reason                model.Reason
	CPUs                  int
	Limit, Expected       uint64
	MemoryFromProfile     bool
	EstDuration, Duration time.Duration
	MemoryPeak            uint64
	IOWriteBytes          *uint64 // nil: unknown
	Samples               int
	Hist                  Histogram
}

func (r Run) Usable() bool {
	return (r.State == model.StateSucceeded || r.State == model.StateFailed && r.Reason == model.ReasonCommandFailed) && r.MemoryPeak > 0 && r.Duration > 0
}

type Params struct {
	Window, MemoryPercentile, DurationPercentile int
	LimitMargin                                  float64
	MinMemory, MaxMemory                         uint64
}
type Estimate struct {
	Runs                     int
	Limit, Expected, MaxPeak uint64
	Duration                 time.Duration
}

func Compute(runs []Run, p Params) (Estimate, bool) {
	var e Estimate
	var hist Histogram
	var durations []time.Duration
	for _, r := range runs {
		if !r.Usable() {
			continue
		}
		if e.Runs >= p.Window {
			break
		}
		e.Runs++
		e.MaxPeak = max(e.MaxPeak, r.MemoryPeak)
		hist.Merge(r.Hist)
		durations = append(durations, r.Duration)
	}
	if e.Runs == 0 {
		return e, false
	}
	const mib = 1 << 20
	scaled := math.Ceil(float64(e.MaxPeak)*p.LimitMargin/mib) * mib
	if scaled >= float64(p.MaxMemory) {
		e.Limit = p.MaxMemory
	} else {
		e.Limit = max(p.MinMemory, uint64(scaled))
	}
	e.Expected = min(hist.Quantile(p.MemoryPercentile), e.Limit)
	e.Duration = Percentile(durations, p.DurationPercentile)
	return e, true
}
func ValidName(s string) error {
	if len(s) < 1 || len(s) > 256 || !utf8.ValidString(s) {
		return fmt.Errorf("profile must be 1..256 bytes of valid UTF-8 without control characters")
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return fmt.Errorf("profile must not contain control characters")
		}
	}
	return nil
}
