-- Memory that agents' own processes run outside aq and docker (a test run, a
-- build), split out of "other"; 0 in samples taken before it was measured.
ALTER TABLE host_samples ADD COLUMN spawned INTEGER NOT NULL DEFAULT 0;
