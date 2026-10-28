package store

import (
	"context"
	"database/sql"
	"resource-dispatcher/internal/model"
	"time"
)

type PruneCandidate struct {
	ID      model.ItemID
	EndedAt time.Time
}

func (s *Store) PruneCandidates(ctx context.Context) ([]PruneCandidate, error) {
	rows, e := s.db.QueryContext(ctx, `SELECT id,ended_at FROM items WHERE state IN `+terminalStates+` AND logs_pruned=0 AND ended_at<>'' ORDER BY ended_at,rowid`)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var out []PruneCandidate
	for rows.Next() {
		var v PruneCandidate
		var at string
		if e = rows.Scan(&v.ID, &at); e != nil {
			return nil, e
		}
		v.EndedAt, e = parseTime(at)
		if e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
func (s *Store) MarkLogsPruned(ctx context.Context, ids []model.ItemID) error {
	return s.immediate(ctx, func(c *sql.Conn) error {
		for _, id := range ids {
			if _, e := c.ExecContext(ctx, `UPDATE items SET logs_pruned=1 WHERE id=? AND state IN `+terminalStates, string(id)); e != nil {
				return e
			}
		}
		return nil
	})
}
func (s *Store) ActiveIDs(ctx context.Context) (map[model.ItemID]bool, error) {
	rows, e := s.db.QueryContext(ctx, `SELECT id FROM items WHERE state NOT IN `+terminalStates)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := map[model.ItemID]bool{}
	for rows.Next() {
		var id model.ItemID
		if e = rows.Scan(&id); e != nil {
			return nil, e
		}
		out[id] = true
	}
	return out, rows.Err()
}
func (s *Store) HasItem(ctx context.Context, id model.ItemID) (bool, error) {
	var n int
	e := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM items WHERE id=?`, string(id)).Scan(&n)
	return n > 0, e
}
func (s *Store) DeleteHistory(ctx context.Context, before time.Time) (int, error) {
	n := 0
	err := s.immediate(ctx, func(c *sql.Conn) error {
		old := `SELECT id FROM items WHERE state IN ` + terminalStates + ` AND ended_at<>'' AND ended_at<?`
		for _, table := range []string{"events", "profile_runs"} {
			key := "item_id"
			if _, e := c.ExecContext(ctx, `DELETE FROM `+table+` WHERE `+key+` IN (`+old+`)`, fmtTime(before)); e != nil {
				return e
			}
		}
		res, e := c.ExecContext(ctx, `DELETE FROM items WHERE id IN (`+old+`)`, fmtTime(before))
		if e != nil {
			return e
		}
		count, e := res.RowsAffected()
		if e != nil {
			return e
		}
		n = int(count)
		if _, e = c.ExecContext(ctx, `DELETE FROM profile_runs WHERE ended_at<?`, fmtTime(before)); e != nil {
			return e
		}
		_, e = c.ExecContext(ctx, `DELETE FROM incidents WHERE at<?`, fmtTime(before))
		return e
	})
	return n, err
}
func (s *Store) ThinHostSamples(ctx context.Context, before time.Time) error {
	return s.immediate(ctx, func(c *sql.Conn) error {
		_, e := c.ExecContext(ctx, `DELETE FROM host_samples WHERE at<? AND rowid NOT IN (SELECT rowid FROM (SELECT rowid,ROW_NUMBER() OVER(PARTITION BY substr(at,1,15) ORDER BY at,rowid) AS rank FROM host_samples WHERE at<?) WHERE rank=1)`, fmtTime(before), fmtTime(before))
		if e != nil {
			return e
		}
		_, e = c.ExecContext(ctx, `UPDATE host_samples SET span_s=600 WHERE at<? AND span_s<>600`, fmtTime(before))
		return e
	})
}
func (s *Store) DeleteHostSamples(ctx context.Context, before time.Time) error {
	_, e := s.db.ExecContext(ctx, `DELETE FROM host_samples WHERE at<?`, fmtTime(before))
	return e
}
