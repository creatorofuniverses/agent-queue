package store

import (
	"context"
	"time"
)

type HostSample struct {
	At                                                                                                    time.Time
	Span                                                                                                  time.Duration
	MemTotal, MemAvailable, SwapTotal, SwapFree                                                           uint64
	PSIFullAvg10                                                                                          float64
	PressureClosed                                                                                        bool
	Agents, Docker, Tasks, Other                                                                          uint64
	TasksExpected, TasksUnconsumed, Stands, StandsUnconsumed, Budgets, BudgetsUnconsumed, CommittedMemory uint64
	CommittedCPUs, Queued, Running                                                                        int
	HeadBlocker                                                                                           string
}

const hostCols = `at, span_s, mem_total, mem_available, swap_total, swap_free, psi_full_avg10, pressure_closed, agents, docker, tasks, other, tasks_expected, tasks_unconsumed, stands, stands_unconsumed, budgets, budgets_unconsumed, committed_memory, committed_cpus, queued, running, head_blocker`

func (s *Store) AddHostSample(ctx context.Context, v HostSample) error {
	_, e := s.db.ExecContext(ctx, `INSERT INTO host_samples (`+hostCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, fmtTime(v.At), int64(v.Span/time.Second), v.MemTotal, v.MemAvailable, v.SwapTotal, v.SwapFree, v.PSIFullAvg10, v.PressureClosed, v.Agents, v.Docker, v.Tasks, v.Other, v.TasksExpected, v.TasksUnconsumed, v.Stands, v.StandsUnconsumed, v.Budgets, v.BudgetsUnconsumed, v.CommittedMemory, v.CommittedCPUs, v.Queued, v.Running, v.HeadBlocker)
	return e
}
func (s *Store) HostSamples(ctx context.Context, since time.Time) ([]HostSample, error) {
	rows, e := s.db.QueryContext(ctx, `SELECT `+hostCols+` FROM host_samples WHERE at>=? ORDER BY at,rowid`, fmtTime(since))
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var out []HostSample
	for rows.Next() {
		var v HostSample
		var at string
		var span int64
		if e = rows.Scan(&at, &span, &v.MemTotal, &v.MemAvailable, &v.SwapTotal, &v.SwapFree, &v.PSIFullAvg10, &v.PressureClosed, &v.Agents, &v.Docker, &v.Tasks, &v.Other, &v.TasksExpected, &v.TasksUnconsumed, &v.Stands, &v.StandsUnconsumed, &v.Budgets, &v.BudgetsUnconsumed, &v.CommittedMemory, &v.CommittedCPUs, &v.Queued, &v.Running, &v.HeadBlocker); e != nil {
			return nil, e
		}
		v.At, e = parseTime(at)
		if e != nil {
			return nil, e
		}
		v.Span = time.Duration(span) * time.Second
		out = append(out, v)
	}
	return out, rows.Err()
}
