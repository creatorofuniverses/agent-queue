-- The task budget a reservation is charged against the pool, learned from the
-- history of its profile; 0 charges the declared task_memory (every
-- reservation before it, and one without history).
ALTER TABLE items ADD COLUMN task_expected INTEGER NOT NULL DEFAULT 0;
CREATE INDEX items_reservation_profile ON items(profile, ended_at) WHERE kind = 'reservation';
