CREATE TABLE items (
    id               TEXT PRIMARY KEY,
    kind             TEXT NOT NULL,
    name             TEXT NOT NULL DEFAULT '',
    state            TEXT NOT NULL,
    reason           TEXT NOT NULL DEFAULT '',
    priority         INTEGER NOT NULL DEFAULT 0,
    argv             TEXT NOT NULL DEFAULT '[]',
    cwd              TEXT NOT NULL DEFAULT '',
    env              TEXT NOT NULL DEFAULT '{}',
    mem_limit        INTEGER NOT NULL DEFAULT 0,
    mem_expected     INTEGER NOT NULL DEFAULT 0,
    cpus             INTEGER NOT NULL DEFAULT 0,
    timeout_ns       INTEGER NOT NULL DEFAULT 0,
    session_id       TEXT NOT NULL DEFAULT '',
    client_type      TEXT NOT NULL DEFAULT '',
    project          TEXT NOT NULL DEFAULT '',
    idem_key         TEXT NOT NULL DEFAULT '',
    request_hash     TEXT NOT NULL DEFAULT '',
    node_id          TEXT NOT NULL DEFAULT 'local',
    created_at       TEXT NOT NULL DEFAULT '',
    admitted_at      TEXT NOT NULL DEFAULT '',
    started_at       TEXT NOT NULL DEFAULT '',
    ended_at         TEXT NOT NULL DEFAULT '',
    exit_code        INTEGER,
    signal           TEXT NOT NULL DEFAULT '',
    memory_peak      INTEGER NOT NULL DEFAULT 0,
    unit_name        TEXT NOT NULL DEFAULT '',
    job_dir          TEXT NOT NULL DEFAULT '',
    git_rev          TEXT NOT NULL DEFAULT '',
    git_dirty        INTEGER NOT NULL DEFAULT 0,
    cancel_requested INTEGER NOT NULL DEFAULT 0,
    cancel_reason    TEXT NOT NULL DEFAULT '',
    logs_truncated   INTEGER NOT NULL DEFAULT 0,
    diagnostic       TEXT NOT NULL DEFAULT ''
);

CREATE UNIQUE INDEX items_idem ON items (client_type, project, idem_key) WHERE idem_key <> '';
CREATE INDEX items_state ON items (state);

CREATE TABLE incidents (
    at     TEXT NOT NULL,
    kind   TEXT NOT NULL,
    value  REAL NOT NULL DEFAULT 0,
    detail TEXT NOT NULL DEFAULT ''
);
