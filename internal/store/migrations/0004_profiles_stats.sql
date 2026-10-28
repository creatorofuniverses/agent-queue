ALTER TABLE items ADD COLUMN profile TEXT NOT NULL DEFAULT '';
ALTER TABLE items ADD COLUMN est_duration_ns INTEGER NOT NULL DEFAULT 0;
ALTER TABLE items ADD COLUMN memory_from_profile INTEGER NOT NULL DEFAULT 0;
ALTER TABLE items ADD COLUMN extend_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE items ADD COLUMN logs_pruned INTEGER NOT NULL DEFAULT 0;
ALTER TABLE items ADD COLUMN teardown_started_at TEXT NOT NULL DEFAULT '';
ALTER TABLE items ADD COLUMN expired_at TEXT NOT NULL DEFAULT '';
CREATE INDEX items_ended ON items(ended_at) WHERE ended_at <> '';
CREATE INDEX items_created ON items(created_at);
CREATE TABLE profile_runs (item_id TEXT PRIMARY KEY, profile TEXT NOT NULL, ended_at TEXT NOT NULL,
  state TEXT NOT NULL, reason TEXT NOT NULL DEFAULT '', cpus INTEGER NOT NULL, mem_limit INTEGER NOT NULL,
  mem_expected INTEGER NOT NULL, memory_from_profile INTEGER NOT NULL, est_duration_ns INTEGER NOT NULL,
  duration_ns INTEGER NOT NULL, memory_peak INTEGER NOT NULL, samples INTEGER NOT NULL, hist TEXT NOT NULL DEFAULT '[]');
CREATE INDEX profile_runs_profile ON profile_runs(profile, ended_at);
CREATE TABLE host_samples (at TEXT NOT NULL, span_s INTEGER NOT NULL,
  mem_total INTEGER NOT NULL, mem_available INTEGER NOT NULL, swap_total INTEGER NOT NULL, swap_free INTEGER NOT NULL,
  psi_full_avg10 REAL NOT NULL, pressure_closed INTEGER NOT NULL,
  agents INTEGER NOT NULL, docker INTEGER NOT NULL, tasks INTEGER NOT NULL, other INTEGER NOT NULL,
  tasks_expected INTEGER NOT NULL, tasks_unconsumed INTEGER NOT NULL, stands INTEGER NOT NULL,
  stands_unconsumed INTEGER NOT NULL, budgets INTEGER NOT NULL, budgets_unconsumed INTEGER NOT NULL,
  committed_memory INTEGER NOT NULL, committed_cpus INTEGER NOT NULL,
  queued INTEGER NOT NULL, running INTEGER NOT NULL, head_blocker TEXT NOT NULL DEFAULT '');
CREATE INDEX host_samples_at ON host_samples(at);
CREATE INDEX incidents_at ON incidents(at);
