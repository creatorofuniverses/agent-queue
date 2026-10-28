-- Physical-disk bytes per task from the unit's io.stat; NULL when the io
-- controller was not available (unknown, not zero).
ALTER TABLE items ADD COLUMN io_write_bytes INTEGER;
ALTER TABLE items ADD COLUMN io_read_bytes INTEGER;
ALTER TABLE profile_runs ADD COLUMN io_write_bytes INTEGER;
