package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"github.com/creatorofuniverses/agent-queue/internal/model"
	"github.com/creatorofuniverses/agent-queue/internal/profile"
	"time"
)

const runCols = `item_id, profile, ended_at, state, reason, cpus, mem_limit, mem_expected, memory_from_profile, est_duration_ns, duration_ns, memory_peak, samples, hist, io_write_bytes`
const usableRunSQL = `(state='SUCCEEDED' OR (state='FAILED' AND reason='COMMAND_FAILED')) AND memory_peak>0 AND duration_ns>0`

func (s *Store) UpdateFinal(ctx context.Context, it *model.Item, evs []model.Event, run *profile.Run) error {
	if run == nil {
		return s.UpdateWithEvents(ctx, it, evs)
	}
	args, e := itemArgs(it)
	if e != nil {
		return e
	}
	hist, e := json.Marshal(run.Hist)
	if e != nil {
		return e
	}
	return s.immediate(ctx, func(c *sql.Conn) error {
		res, e := c.ExecContext(ctx, updateSQL, append(args[1:], args[0])...)
		if e != nil {
			return e
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		if e = insertEvents(ctx, c, evs); e != nil {
			return e
		}
		_, e = c.ExecContext(ctx, `INSERT OR IGNORE INTO profile_runs (`+runCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, string(run.ItemID), run.Profile, fmtTime(run.EndedAt), string(run.State), string(run.Reason), run.CPUs, int64(run.Limit), int64(run.Expected), run.MemoryFromProfile, int64(run.EstDuration), int64(run.Duration), int64(run.MemoryPeak), run.Samples, string(hist), nullU64(run.IOWriteBytes))
		return e
	})
}
func scanRun(r scanner) (profile.Run, error) {
	var v profile.Run
	var at, hist string
	var duration, estimate int64
	var ioWrite sql.NullInt64
	e := r.Scan(&v.ItemID, &v.Profile, &at, &v.State, &v.Reason, &v.CPUs, &v.Limit, &v.Expected, &v.MemoryFromProfile, &estimate, &duration, &v.MemoryPeak, &v.Samples, &hist, &ioWrite)
	if e != nil {
		return v, e
	}
	v.IOWriteBytes = u64Ptr(ioWrite)
	v.Duration = time.Duration(duration)
	v.EstDuration = time.Duration(estimate)
	v.EndedAt, e = parseTime(at)
	if e != nil {
		return v, e
	}
	e = json.Unmarshal([]byte(hist), &v.Hist)
	return v, e
}
func (s *Store) queryRuns(ctx context.Context, q string, args ...any) ([]profile.Run, error) {
	rows, e := s.db.QueryContext(ctx, q, args...)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var out []profile.Run
	for rows.Next() {
		r, e := scanRun(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
func (s *Store) UsableRuns(ctx context.Context, name string, limit int) ([]profile.Run, error) {
	return s.queryRuns(ctx, `SELECT `+runCols+` FROM profile_runs WHERE profile=? AND `+usableRunSQL+` ORDER BY ended_at DESC,rowid DESC LIMIT ?`, name, limit)
}
func (s *Store) ProfileRuns(ctx context.Context, name string, limit int) ([]profile.Run, error) {
	return s.queryRuns(ctx, `SELECT `+runCols+` FROM profile_runs WHERE profile=? ORDER BY ended_at DESC,rowid DESC LIMIT ?`, name, limit)
}

type ProfileInfo struct {
	Name         string
	Runs, Usable int
	LastEnded    time.Time
}

func (s *Store) Profiles(ctx context.Context) ([]ProfileInfo, error) {
	rows, e := s.db.QueryContext(ctx, `SELECT profile,COUNT(*),SUM(CASE WHEN `+usableRunSQL+` THEN 1 ELSE 0 END),MAX(ended_at) FROM profile_runs GROUP BY profile ORDER BY profile`)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var out []ProfileInfo
	for rows.Next() {
		var p ProfileInfo
		var at string
		if e = rows.Scan(&p.Name, &p.Runs, &p.Usable, &at); e != nil {
			return nil, e
		}
		if p.LastEnded, e = parseTime(at); e != nil {
			return nil, e
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
