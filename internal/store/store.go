// Package store persists queue items and incidents in SQLite.
package store

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"resource-dispatcher/internal/model"
)

var (
	ErrNotFound            = errors.New("store: not found")
	ErrIdempotencyConflict = errors.New("store: idempotency key reused with a different request")
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// timeLayout is fixed-width UTC so that text comparison matches time order.
const timeLayout = "2006-01-02T15:04:05.000000000Z07:00"

type Store struct {
	stateDir string
	db       *sql.DB
}

// Open opens (creating if needed) the database at path and applies migrations.
func Open(path string) (*Store, error) {
	if err := makePrivate(path); err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "synchronous(NORMAL)")
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	s := &Store{db: db, stateDir: filepath.Dir(path)}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: migrate: %w", err)
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// makePrivate creates the database file 0600 (SQLite gives -wal/-shm the same mode)
// and removes group/other access from existing database files.
func makePrivate(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	f.Close()
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		st, err := os.Stat(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if st.Mode().Perm()&0o077 != 0 {
			if err := os.Chmod(p, st.Mode().Perm()&0o700); err != nil {
				return err
			}
		}
	}
	return nil
}

// immediate runs fn inside BEGIN IMMEDIATE ... COMMIT on a dedicated connection.
func (s *Store) immediate(ctx context.Context, fn func(c *sql.Conn) error) error {
	c, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	if _, err := c.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	if err := fn(c); err != nil {
		c.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
		return err
	}
	if _, err := c.ExecContext(ctx, "COMMIT"); err != nil {
		c.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
		return err
	}
	return nil
}

func (s *Store) migrate(ctx context.Context) error {
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return err
	}
	type mig struct {
		version int
		sql     string
	}
	var migs []mig
	for _, e := range entries {
		name := e.Name()
		num, _, ok := strings.Cut(name, "_")
		v, err := strconv.Atoi(num)
		if !ok || err != nil {
			return fmt.Errorf("bad migration name %q", name)
		}
		b, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		migs = append(migs, mig{v, string(b)})
	}
	sort.Slice(migs, func(i, j int) bool { return migs[i].version < migs[j].version })

	return s.immediate(ctx, func(c *sql.Conn) error {
		var cur int
		if err := c.QueryRowContext(ctx, "PRAGMA user_version").Scan(&cur); err != nil {
			return err
		}
		for _, m := range migs {
			if m.version <= cur {
				continue
			}
			if _, err := c.ExecContext(ctx, m.sql); err != nil {
				return fmt.Errorf("migration %d: %w", m.version, err)
			}
			if _, err := c.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", m.version)); err != nil {
				return err
			}
		}
		return nil
	})
}

const itemCols = `id, kind, name, state, reason, priority, argv, cwd, env,
mem_limit, mem_expected, cpus, timeout_ns,
session_id, client_type, project, idem_key, request_hash, node_id,
created_at, admitted_at, started_at, ended_at,
exit_code, signal, memory_peak, unit_name, job_dir, git_rev, git_dirty,
cancel_requested, cancel_reason, logs_truncated, diagnostic,
parent_id, task_memory, task_cpus, expires_at, warn_at, docker_project,
teardown_attempt, teardown_launch_error, notify_grant, ttl_held, over_declared_notified, no_notify, finish_notified, profile, est_duration_ns, memory_from_profile, extend_count, logs_pruned, teardown_started_at, expired_at`

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(timeLayout)
}

func parseTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339Nano, s)
}

func itemArgs(it *model.Item) ([]any, error) {
	argv := it.Argv
	if argv == nil {
		argv = []string{}
	}
	argvJSON, err := json.Marshal(argv)
	if err != nil {
		return nil, err
	}
	env := it.Env
	if env == nil {
		env = map[string]string{}
	}
	envJSON, err := json.Marshal(env)
	if err != nil {
		return nil, err
	}
	var exit any
	if it.ExitCode != nil {
		exit = *it.ExitCode
	}
	node := it.NodeID
	if node == "" {
		node = "local"
	}
	return []any{
		string(it.ID), string(it.Kind), it.Name, string(it.State), string(it.Reason), int(it.Priority),
		string(argvJSON), it.Cwd, string(envJSON),
		int64(it.Limit), int64(it.Expected), it.CPUs, int64(it.Timeout),
		it.SessionID, it.ClientType, it.Project, it.IdemKey, it.RequestHash, node,
		fmtTime(it.CreatedAt), fmtTime(it.AdmittedAt), fmtTime(it.StartedAt), fmtTime(it.EndedAt),
		exit, it.Signal, int64(it.MemoryPeak), it.UnitName, it.JobDir, it.GitRev, it.GitDirty,
		it.CancelRequested, it.CancelReason, it.LogsTruncated, it.Diagnostic,
		string(it.ParentID), int64(it.TaskMemory), it.TaskCPUs, fmtTime(it.ExpiresAt), fmtTime(it.WarnAt), it.DockerProject,
		it.TeardownAttempt, it.TeardownLaunchError, it.NotifyGrant, it.TTLHeld, it.OverDeclaredNotified, it.NoNotify, it.FinishNotified,
		it.Profile, int64(it.EstDuration), it.MemoryFromProfile, it.ExtendCount, it.LogsPruned, fmtTime(it.TeardownStartedAt), fmtTime(it.ExpiredAt),
	}, nil
}

type scanner interface{ Scan(dest ...any) error }

func scanItem(r scanner) (*model.Item, error) {
	var (
		it                                model.Item
		id, kind, state, reason           string
		prio                              int
		argv, env                         string
		limit, expected, timeout, peak    int64
		created, admitted, started, ended string
		exit                              sql.NullInt64
		parent                            string
		taskMem                           int64
		expires, warn                     string
		teardownStarted, expired          string
		estDuration                       int64
	)
	err := r.Scan(&id, &kind, &it.Name, &state, &reason, &prio, &argv, &it.Cwd, &env,
		&limit, &expected, &it.CPUs, &timeout,
		&it.SessionID, &it.ClientType, &it.Project, &it.IdemKey, &it.RequestHash, &it.NodeID,
		&created, &admitted, &started, &ended,
		&exit, &it.Signal, &peak, &it.UnitName, &it.JobDir, &it.GitRev, &it.GitDirty,
		&it.CancelRequested, &it.CancelReason, &it.LogsTruncated, &it.Diagnostic,
		&parent, &taskMem, &it.TaskCPUs, &expires, &warn, &it.DockerProject,
		&it.TeardownAttempt, &it.TeardownLaunchError, &it.NotifyGrant, &it.TTLHeld, &it.OverDeclaredNotified, &it.NoNotify, &it.FinishNotified, &it.Profile, &estDuration, &it.MemoryFromProfile, &it.ExtendCount, &it.LogsPruned, &teardownStarted, &expired)
	if err != nil {
		return nil, err
	}
	it.ID, it.Kind, it.State, it.Reason = model.ItemID(id), model.Kind(kind), model.State(state), model.Reason(reason)
	it.Priority = model.Priority(prio)
	it.EstDuration = time.Duration(estDuration)
	it.ParentID, it.TaskMemory = model.ItemID(parent), uint64(taskMem)
	it.Limit, it.Expected, it.Timeout, it.MemoryPeak = uint64(limit), uint64(expected), time.Duration(timeout), uint64(peak)
	if err := json.Unmarshal([]byte(argv), &it.Argv); err != nil {
		return nil, err
	}
	if len(it.Argv) == 0 {
		it.Argv = nil
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(env), &m); err != nil {
		return nil, err
	}
	if len(m) > 0 {
		it.Env = m
	}
	for _, p := range []struct {
		dst *time.Time
		src string
	}{{&it.CreatedAt, created}, {&it.AdmittedAt, admitted}, {&it.StartedAt, started}, {&it.EndedAt, ended},
		{&it.ExpiresAt, expires}, {&it.WarnAt, warn}, {&it.TeardownStartedAt, teardownStarted}, {&it.ExpiredAt, expired}} {
		if *p.dst, err = parseTime(p.src); err != nil {
			return nil, err
		}
	}
	if exit.Valid {
		v := int(exit.Int64)
		it.ExitCode = &v
	}
	return &it, nil
}

var insertSQL = `INSERT INTO items (` + itemCols + `) VALUES (?` + strings.Repeat(",?", strings.Count(itemCols, ",")) + `)`

var updateSQL = func() string {
	var sets []string
	for _, c := range strings.Split(itemCols, ",")[1:] {
		sets = append(sets, strings.TrimSpace(c)+"=?")
	}
	return `UPDATE items SET ` + strings.Join(sets, ", ") + ` WHERE id=?`
}()

// InsertItem stores it. With a non-empty IdemKey the lookup and insert are one
// BEGIN IMMEDIATE transaction; an existing row with the same hash is returned
// with created=false, a different hash yields ErrIdempotencyConflict.
func (s *Store) InsertItem(ctx context.Context, it *model.Item) (*model.Item, bool, error) {
	args, err := itemArgs(it)
	if err != nil {
		return nil, false, err
	}
	if it.IdemKey == "" {
		if _, err := s.db.ExecContext(ctx, insertSQL, args...); err != nil {
			return nil, false, err
		}
		got, err := s.Get(ctx, it.ID)
		return got, err == nil, err
	}
	var (
		stored  *model.Item
		created bool
	)
	err = s.immediate(ctx, func(c *sql.Conn) error {
		row := c.QueryRowContext(ctx, `SELECT `+itemCols+` FROM items WHERE client_type=? AND project=? AND idem_key=?`,
			it.ClientType, it.Project, it.IdemKey)
		ex, err := scanItem(row)
		switch {
		case err == nil:
			if ex.RequestHash != it.RequestHash {
				return ErrIdempotencyConflict
			}
			stored = ex
			return nil
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		if _, err := c.ExecContext(ctx, insertSQL, args...); err != nil {
			return err
		}
		created = true
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	if created {
		stored, err = s.Get(ctx, it.ID)
		if err != nil {
			return nil, false, err
		}
	}
	return stored, created, nil
}

func (s *Store) Get(ctx context.Context, id model.ItemID) (*model.Item, error) {
	it, err := scanItem(s.db.QueryRowContext(ctx, `SELECT `+itemCols+` FROM items WHERE id=?`, string(id)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return it, err
}

// Update overwrites the whole row for it.ID.
func (s *Store) Update(ctx context.Context, it *model.Item) error {
	args, err := itemArgs(it)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, updateSQL, append(args[1:], args[0])...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateWithEvents overwrites the row for it.ID and inserts evs in one
// BEGIN IMMEDIATE transaction; with no events it is Update.
func (s *Store) UpdateWithEvents(ctx context.Context, it *model.Item, evs []model.Event) error {
	if len(evs) == 0 {
		return s.Update(ctx, it)
	}
	args, err := itemArgs(it)
	if err != nil {
		return err
	}
	return s.immediate(ctx, func(c *sql.Conn) error {
		res, err := c.ExecContext(ctx, updateSQL, append(args[1:], args[0])...)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return insertEvents(ctx, c, evs)
	})
}

// Events returns all event fields, oldest first.
func (s *Store) Events(ctx context.Context, itemID model.ItemID) ([]model.Event, error) {
	return queryEvents(ctx, s.db, `SELECT `+eventCols+` FROM events WHERE item_id=? ORDER BY created_at,rowid`, string(itemID))
}

func (s *Store) queryItems(ctx context.Context, q string, args ...any) ([]*model.Item, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Item
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// terminalStates is the SQL list built from model.TerminalStates().
var terminalStates = func() string {
	q := make([]string, 0, len(model.TerminalStates()))
	for _, s := range model.TerminalStates() {
		q = append(q, "'"+string(s)+"'")
	}
	return "(" + strings.Join(q, ",") + ")"
}()

// ListActive returns non-terminal items in queue order.
func (s *Store) ListActive(ctx context.Context) ([]*model.Item, error) {
	return s.queryItems(ctx, `SELECT `+itemCols+` FROM items WHERE state NOT IN `+terminalStates+
		` ORDER BY priority DESC, created_at ASC, rowid ASC`)
}

type ListFilter struct {
	WithUnacked     bool
	SessionID       string
	IncludeTerminal bool
	Limit           int
}

// List returns items newest first; Limit 0 means 100.
func (s *Store) List(ctx context.Context, f ListFilter) ([]*model.Item, error) {
	q := `SELECT ` + itemCols + ` FROM items WHERE 1=1`
	var args []any
	if f.SessionID != "" {
		q += ` AND session_id=?`
		args = append(args, f.SessionID)
	}
	if !f.IncludeTerminal {
		if f.WithUnacked && f.SessionID != "" {
			q += ` AND (state NOT IN ` + terminalStates + ` OR EXISTS (SELECT 1 FROM events WHERE events.item_id=items.id AND events.session_id=items.session_id AND audience='session' AND channel<>'none' AND status<>'acked'))`
		} else {
			q += ` AND state NOT IN ` + terminalStates
		}
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	q += ` ORDER BY created_at DESC, rowid DESC LIMIT ?`
	return s.queryItems(ctx, q, append(args, limit)...)
}

func (s *Store) AddIncident(ctx context.Context, inc model.Incident) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO incidents (at, kind, value, detail) VALUES (?,?,?,?)`,
		fmtTime(inc.At), inc.Kind, inc.Value, inc.Detail)
	return err
}

// Incidents returns incidents at or after since, oldest first.
// PSIClosedAt reports whether the latest pressure transition before at closed admission.
func (s *Store) PSIClosedAt(ctx context.Context, at time.Time) (bool, error) {
	var kind string
	err := s.db.QueryRowContext(ctx, `SELECT kind FROM incidents WHERE kind IN ('psi_close','psi_open') AND at < ? ORDER BY at DESC, rowid DESC LIMIT 1`, fmtTime(at)).Scan(&kind)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return kind == "psi_close", err
}

func (s *Store) Incidents(ctx context.Context, since time.Time) ([]model.Incident, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT at, kind, value, detail FROM incidents WHERE at >= ? ORDER BY at ASC, rowid ASC`,
		fmtTime(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Incident
	for rows.Next() {
		var inc model.Incident
		var at string
		if err := rows.Scan(&at, &inc.Kind, &inc.Value, &inc.Detail); err != nil {
			return nil, err
		}
		if inc.At, err = parseTime(at); err != nil {
			return nil, err
		}
		out = append(out, inc)
	}
	return out, rows.Err()
}
