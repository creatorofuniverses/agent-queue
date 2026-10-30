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
	e.Limit = p.Margin(e.MaxPeak)
	e.Expected = min(hist.Quantile(p.MemoryPercentile), e.Limit)
	e.Duration = Percentile(durations, p.DurationPercentile)
	return e, true
}

// Margin is peak × LimitMargin rounded up to MiB, within [MinMemory, MaxMemory].
func (p Params) Margin(peak uint64) uint64 {
	const mib = 1 << 20
	scaled := math.Ceil(float64(peak)*p.LimitMargin/mib) * mib
	if scaled >= float64(p.MaxMemory) {
		return p.MaxMemory
	}
	return max(p.MinMemory, uint64(scaled))
}

// Reservation is what the history of a reservation name says it needs: Stand
// from the measured stand peaks, Budget from the most its nested tasks used at
// once, each by the limit rule of task profiles over its last Window usable
// runs; zero with no usable run.
type Reservation struct {
	Stand, Budget         uint64
	StandRuns, BudgetRuns int
}

// ForReservation computes Reservation from earlier reservations, newest first.
// A peak of zero teaches nothing: no nested task ran, or the stand's containers
// were not labeled with its docker project.
func ForReservation(hist []*model.Item, p Params) Reservation {
	var r Reservation
	var stand, budget uint64
	for _, it := range hist {
		if it.DockerProject != "" && it.MemoryPeak > 0 && r.StandRuns < p.Window {
			r.StandRuns++
			stand = max(stand, it.MemoryPeak)
		}
		if it.TaskMemoryPeak != nil && *it.TaskMemoryPeak > 0 && r.BudgetRuns < p.Window {
			r.BudgetRuns++
			budget = max(budget, *it.TaskMemoryPeak)
		}
	}
	if r.StandRuns > 0 {
		r.Stand = p.Margin(stand)
	}
	if r.BudgetRuns > 0 {
		r.Budget = p.Margin(budget)
	}
	return r
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
