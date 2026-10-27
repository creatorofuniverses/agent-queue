ALTER TABLE items ADD COLUMN parent_id TEXT NOT NULL DEFAULT '';
ALTER TABLE items ADD COLUMN task_memory INTEGER NOT NULL DEFAULT 0;
ALTER TABLE items ADD COLUMN task_cpus INTEGER NOT NULL DEFAULT 0;
ALTER TABLE items ADD COLUMN expires_at TEXT NOT NULL DEFAULT '';
ALTER TABLE items ADD COLUMN warn_at TEXT NOT NULL DEFAULT '';
ALTER TABLE items ADD COLUMN docker_project TEXT NOT NULL DEFAULT '';
ALTER TABLE items ADD COLUMN teardown_attempt INTEGER NOT NULL DEFAULT 0;
ALTER TABLE items ADD COLUMN teardown_launch_error TEXT NOT NULL DEFAULT '';
ALTER TABLE items ADD COLUMN notify_grant INTEGER NOT NULL DEFAULT 0;
ALTER TABLE items ADD COLUMN ttl_held INTEGER NOT NULL DEFAULT 0;
ALTER TABLE items ADD COLUMN over_declared_notified INTEGER NOT NULL DEFAULT 0;

CREATE INDEX items_parent ON items (parent_id) WHERE parent_id <> '';

CREATE TABLE events (
    id          TEXT PRIMARY KEY,
    item_id     TEXT NOT NULL,
    kind        TEXT NOT NULL,
    audience    TEXT NOT NULL,
    session_id  TEXT NOT NULL DEFAULT '',
    client_type TEXT NOT NULL DEFAULT '',
    detail      TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL,
    status      TEXT NOT NULL DEFAULT 'pending'
);

CREATE INDEX events_item ON events (item_id);
