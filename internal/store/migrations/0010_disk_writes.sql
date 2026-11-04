-- Bytes written to disk per hour by source: the host counter ("host"), level-1
-- sources, and agents (source agent:<name>:<project>, within = the source key
-- the agent lived in). Level-1 rows have within = ''. Deltas may be negative.
CREATE TABLE disk_writes (
  hour TEXT NOT NULL,
  source TEXT NOT NULL,
  within TEXT NOT NULL DEFAULT '',
  bytes INTEGER NOT NULL,
  PRIMARY KEY (hour, source, within)
);
