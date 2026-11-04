package store

import (
	"context"
	"database/sql"
	"time"
)

// DiskWrite is the bytes written in one hour by one source. Bytes is signed:
// a correction delta may be negative.
type DiskWrite struct {
	Hour   time.Time
	Source string
	Within string
	Bytes  int64
}

func diskHour(t time.Time) string { return fmtTime(t.UTC().Truncate(time.Hour)) }

// AddDiskWrites adds each row's bytes to its (hour, source, within) bucket in
// one transaction.
func (s *Store) AddDiskWrites(ctx context.Context, rows []DiskWrite) error {
	if len(rows) == 0 {
		return nil
	}
	return s.immediate(ctx, func(c *sql.Conn) error {
		for _, r := range rows {
			if _, e := c.ExecContext(ctx, `INSERT INTO disk_writes (hour,source,within,bytes) VALUES (?,?,?,?) ON CONFLICT(hour,source,within) DO UPDATE SET bytes = bytes + excluded.bytes`, diskHour(r.Hour), r.Source, r.Within, r.Bytes); e != nil {
				return e
			}
		}
		return nil
	})
}

// DiskWritesSince returns the buckets from the start of since's hour on.
func (s *Store) DiskWritesSince(ctx context.Context, since time.Time) ([]DiskWrite, error) {
	rows, e := s.db.QueryContext(ctx, `SELECT hour,source,within,bytes FROM disk_writes WHERE hour>=? ORDER BY hour,source,within`, diskHour(since))
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var out []DiskWrite
	for rows.Next() {
		var v DiskWrite
		var h string
		if e = rows.Scan(&h, &v.Source, &v.Within, &v.Bytes); e != nil {
			return nil, e
		}
		if v.Hour, e = parseTime(h); e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// DeleteDiskWrites drops the buckets of hours before the hour of before.
func (s *Store) DeleteDiskWrites(ctx context.Context, before time.Time) error {
	_, e := s.db.ExecContext(ctx, `DELETE FROM disk_writes WHERE hour<?`, diskHour(before))
	return e
}
