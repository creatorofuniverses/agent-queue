-- Daemon state that otherwise lives only in memory, saved on a clean shutdown
-- and read back on start (run samples, guard history, the last version).
CREATE TABLE runtime_state (
    key      TEXT PRIMARY KEY,
    value    TEXT NOT NULL,
    saved_at TEXT NOT NULL
);
