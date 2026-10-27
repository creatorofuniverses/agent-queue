package store

import (
	"context"
	"database/sql"
	"errors"
	"resource-dispatcher/internal/model"
	"strings"
	"time"
)

const eventCols = `id,item_id,kind,audience,session_id,client_type,detail,created_at,channel,status,attempts,next_attempt_at,sent_at,acked_at,last_error`

func insertEvents(ctx context.Context, c *sql.Conn, evs []model.Event) error {
	for _, e := range evs {
		if e.Status == "" {
			e.Status = model.EventPending
		}
		if e.Status == model.EventPending && e.Channel.Retried() && e.NextAttemptAt.IsZero() {
			e.NextAttemptAt = e.CreatedAt
		}
		_, err := c.ExecContext(ctx, `INSERT INTO events (`+eventCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, e.ID, string(e.ItemID), string(e.Kind), string(e.Audience), e.SessionID, e.ClientType, e.Detail, fmtTime(e.CreatedAt), string(e.Channel), string(e.Status), e.Attempts, fmtTime(e.NextAttemptAt), fmtTime(e.SentAt), fmtTime(e.AckedAt), e.LastError)
		if err != nil {
			return err
		}
	}
	return nil
}
func scanEvent(r scanner) (model.Event, error) {
	var e model.Event
	var created, next, sent, acked string
	err := r.Scan(&e.ID, &e.ItemID, &e.Kind, &e.Audience, &e.SessionID, &e.ClientType, &e.Detail, &created, &e.Channel, &e.Status, &e.Attempts, &next, &sent, &acked, &e.LastError)
	if err != nil {
		return e, err
	}
	for _, p := range []struct {
		dst *time.Time
		src string
	}{{&e.CreatedAt, created}, {&e.NextAttemptAt, next}, {&e.SentAt, sent}, {&e.AckedAt, acked}} {
		if *p.dst, err = parseTime(p.src); err != nil {
			return e, err
		}
	}
	return e, nil
}

type eventQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func queryEvents(ctx context.Context, q eventQuerier, sqlText string, args ...any) ([]model.Event, error) {
	rows, err := q.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var evs []model.Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		evs = append(evs, e)
	}
	return evs, rows.Err()
}
func (s *Store) AddEvents(ctx context.Context, evs []model.Event) error {
	return s.immediate(ctx, func(c *sql.Conn) error { return insertEvents(ctx, c, evs) })
}
func (s *Store) ClaimDue(ctx context.Context, ch model.Channel, now time.Time) (*model.Event, error) {
	var out *model.Event
	err := s.immediate(ctx, func(c *sql.Conn) error {
		e, err := scanEvent(c.QueryRowContext(ctx, `SELECT `+eventCols+` FROM events WHERE channel=? AND status='pending' AND next_attempt_at<=? ORDER BY created_at,rowid LIMIT 1`, string(ch), fmtTime(now)))
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		r, err := c.ExecContext(ctx, `UPDATE events SET status='delivering' WHERE id=? AND status='pending'`, e.ID)
		if err != nil {
			return err
		}
		n, err := r.RowsAffected()
		if err != nil {
			return err
		}
		if n > 0 {
			e.Status = model.EventDelivering
			out = &e
		}
		return nil
	})
	return out, err
}
func (s *Store) ResetDelivering(ctx context.Context) (int, error) {
	var n int64
	err := s.immediate(ctx, func(c *sql.Conn) error {
		r, e := c.ExecContext(ctx, `UPDATE events SET status='pending' WHERE status='delivering'`)
		if e != nil {
			return e
		}
		n, e = r.RowsAffected()
		return e
	})
	return int(n), err
}
func (s *Store) NextDue(ctx context.Context, ch model.Channel) (time.Time, bool, error) {
	var v sql.NullString
	e := s.db.QueryRowContext(ctx, `SELECT MIN(next_attempt_at) FROM events WHERE channel=? AND status='pending'`, string(ch)).Scan(&v)
	if e != nil || !v.Valid {
		return time.Time{}, false, e
	}
	at, e := parseTime(v.String)
	return at, true, e
}
func (s *Store) PendingChannels(ctx context.Context) ([]model.Channel, error) {
	rows, e := s.db.QueryContext(ctx, `SELECT DISTINCT channel FROM events WHERE status='pending'`)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var out []model.Channel
	for rows.Next() {
		var ch model.Channel
		if e = rows.Scan(&ch); e != nil {
			return nil, e
		}
		if ch.Retried() {
			out = append(out, ch)
		}
	}
	return out, rows.Err()
}
func (s *Store) MarkSent(ctx context.Context, id string, at time.Time) error {
	return s.immediate(ctx, func(c *sql.Conn) error {
		_, e := c.ExecContext(ctx, `UPDATE events SET status='sent',sent_at=?,last_error='' WHERE id=? AND status='delivering'`, fmtTime(at), id)
		return e
	})
}
func (s *Store) MarkRetry(ctx context.Context, id string, attempts int, next time.Time, lastErr string) error {
	return s.immediate(ctx, func(c *sql.Conn) error {
		_, e := c.ExecContext(ctx, `UPDATE events SET status='pending',attempts=?,next_attempt_at=?,last_error=? WHERE id=? AND status='delivering'`, attempts, fmtTime(next), lastErr, id)
		return e
	})
}
func (s *Store) MarkFailed(ctx context.Context, id string, attempts int, lastErr string, evs []model.Event) error {
	return s.immediate(ctx, func(c *sql.Conn) error {
		r, e := c.ExecContext(ctx, `UPDATE events SET status='failed',attempts=?,last_error=? WHERE id=? AND status='delivering'`, attempts, lastErr, id)
		if e != nil {
			return e
		}
		n, e := r.RowsAffected()
		if e != nil || n == 0 {
			return e
		}
		return insertEvents(ctx, c, evs)
	})
}
func kindArgs(kinds []model.EventKind) (string, []any) {
	args := make([]any, len(kinds))
	for i, k := range kinds {
		args[i] = string(k)
	}
	return strings.TrimSuffix(strings.Repeat("?,", len(kinds)), ","), args
}
func (s *Store) MarkClientWaitSent(ctx context.Context, itemID model.ItemID, kinds []model.EventKind, at time.Time) ([]model.Event, error) {
	if len(kinds) == 0 {
		return nil, nil
	}
	var evs []model.Event
	err := s.immediate(ctx, func(c *sql.Conn) error {
		marks, args := kindArgs(kinds)
		args = append([]any{string(itemID)}, args...)
		var e error
		evs, e = queryEvents(ctx, c, `SELECT `+eventCols+` FROM events WHERE item_id=? AND channel='client_wait' AND status='pending' AND kind IN (`+marks+`) ORDER BY created_at,rowid`, args...)
		if e != nil {
			return e
		}
		for i := range evs {
			_, e = c.ExecContext(ctx, `UPDATE events SET status='sent',sent_at=? WHERE id=? AND status='pending'`, fmtTime(at), evs[i].ID)
			if e != nil {
				return e
			}
			evs[i].Status = model.EventSent
			evs[i].SentAt = at
		}
		return nil
	})
	return evs, err
}
func (s *Store) HasPendingClientWait(ctx context.Context, id model.ItemID, kind model.EventKind) (bool, error) {
	var n int
	e := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM events WHERE item_id=? AND kind=? AND channel='client_wait' AND status='pending')`, string(id), string(kind)).Scan(&n)
	return n != 0, e
}
func (s *Store) Ack(ctx context.Context, id string, at time.Time) (model.Event, error) {
	var out model.Event
	err := s.immediate(ctx, func(c *sql.Conn) error {
		_, e := c.ExecContext(ctx, `UPDATE events SET status='acked',acked_at=CASE WHEN acked_at='' THEN ? ELSE acked_at END WHERE id=?`, fmtTime(at), id)
		if e != nil {
			return e
		}
		out, e = scanEvent(c.QueryRowContext(ctx, `SELECT `+eventCols+` FROM events WHERE id=?`, id))
		if errors.Is(e, sql.ErrNoRows) {
			return ErrNotFound
		}
		return e
	})
	return out, err
}
func (s *Store) UnackedSessionEvents(ctx context.Context, session string) ([]model.Event, error) {
	return queryEvents(ctx, s.db, `SELECT `+eventCols+` FROM events WHERE session_id=? AND audience='session' AND status<>'acked' ORDER BY created_at,rowid`, session)
}
func (s *Store) LastDeliveryError(ctx context.Context, ch model.Channel) (string, error) {
	var msg string
	e := s.db.QueryRowContext(ctx, `SELECT last_error FROM events WHERE channel=? AND last_error<>'' AND status IN ('pending','failed') ORDER BY created_at DESC,rowid DESC LIMIT 1`, string(ch)).Scan(&msg)
	if errors.Is(e, sql.ErrNoRows) {
		return "", nil
	}
	return msg, e
}
func (s *Store) CountPending(ctx context.Context) (int, error) {
	var n int
	e := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE status='pending'`).Scan(&n)
	return n, e
}
