# aq details

CLI and MCP use the same daemon over a local Unix socket. Sizes are binary:
`4G`, `512M`; bare numbers mean MiB. Task memory minimum is 64M. `--expected`
changes admission accounting while `--memory` remains the hard limit. CPUs are
integers. `--key` makes a repeated identical submission return the same item.
`--project` overrides the git-root/cwd project. Commands run from the submit cwd.

`aq reserve --memory STAND --task-memory TASKS --task-cpus N --ttl D --teardown FILE`
queues a stand plus task budget. Stand CPUs default to 0; task budget fields must
be supplied together. `aq submit --within RES_ID` uses that budget immediately;
requests beyond its remaining memory or CPU budget are rejected. Containers live
outside task cgroups: budget them in the stand, even if started by test code.
TTL starts at grant. While nested tasks run, teardown is held and new nested
submissions are rejected; after the last task ends an expired held stand is
removed immediately. `extend --ttl` sets a minimum remaining lifetime without
accumulating repeats.

Notification identity: `--client NAME --session S` overrides discovery. Otherwise
the one configured adapter with a nonempty identity_env is used (Codex
CODEX_THREAD_ID, Claude CLAUDE_CODE_SESSION_ID); when several are set the
environment is ambiguous and submit/reserve need an explicit client.
Claude Code passes CLAUDE_CODE_SESSION_ID to `aq mcp` itself.
`--no-notify` suppresses session events, while operator alerts remain enabled.
Codex MCP 0.164 does not propagate session identity by default: pass `client` and
`session_id` from the invoking session, or intentionally set `no_notify`.
MCP `cwd` and `teardown_file` must be absolute; it captures the MCP process's
environment, not a fresh shell's. `queue_logs` allows 1..2000 tail lines (default
200), without follow, and at most 256 KiB of the newest output. MCP has eleven queue tools, including `queue_estimate`.

`aq wait ID` blocks on the socket until a task is terminal or a reservation
leaves QUEUED. `aq wait RES_ID --until expiring` returns when it leaves
QUEUED/GRANTED or a client-wait stand-over-declaration warning arrives. An early
GRANTED warning consumes the waiter: handle, ack, then re-arm it. Client-wait
marks only returned matching events sent; ack remains explicit. Exit 0 for
EXPIRING and early GRANTED, 4/5 for cancelled/rejected, 6 after the stand is no
longer held. `--until expiring` on tasks is a usage error (64).

`aq list --mine` lists session items, including finished ones with unacked
session events, plus event IDs/kinds/statuses. `aq ack EVENT_ID` accepts any
status, is idempotent, and stops delivery if acknowledged before the outbox
claim. Once claimed, an in-flight ping can still arrive; deduplicate event_id.
A daemon restart can retry an in-flight delivery with the same event_id.

Operator recovery: inspect `aq result RES_ID` and `aq logs RES_ID --tail 100`.
`TEARDOWN_FAILED` / `TEARDOWN_UNKNOWN` retain resources. After diagnosing and
restoring teardown safety, an operator can use `aq teardown --retry RES_ID`;
after manually removing the stand, `aq release RES_ID --force`. No automatic
retry is performed. Normal `release` assumes you already shut the stand down.

`aq doctor` checks limits, runtime, adapter executables and webhook failures.
Only PATH/HOME/LANG/LC_*/VIRTUAL_ENV and explicit --env pairs are captured;
reserve also captures DOCKER_HOST/DOCKER_CONTEXT/DOCKER_CONFIG. Notifications
never embed env values, teardown scripts or log contents. Session death is not
detected in v1. The soft hook reminds about heavy commands; it does not block.


Profiles: `--profile P` is explicit; otherwise `--name N` derives a profile from
`--project` or the main repository root (shared across worktrees), then `:N`.
`aq profile list [--json]` and `aq profile show P [--json]` expose histories and
estimates. An initial run needs `--memory`; later runs can use history. Limits
use max usable peak × margin, expected uses pooled memory samples, duration
uses the configured percentile capped by timeout. OOM does not increase limits.

`aq estimate [--memory S] [--expected S] [--name N|--profile P] [--project P]
[--cpus N] [--priority P] [--timeout D] [--json]` forecasts without saving an item.
It reports starts_now/backfill, queue position, expected_start, blocked_by,
confidence, and the sources of memory/duration estimates. MCP `queue_estimate`
accepts memory/expected/timeout/priority/profile/name/cwd/project strings and
integer cpus, with no session identity requirement. Supply memory or a profile
with usable history; timeout is optional. Backfill uses the same admission pass.

`aq top [--json]` is one memory/pressure/queue snapshot. Physical categories
agents/docker/tasks/other and scheduler commitments are separate views.
`aq stats [--since 7d] [--profile P] [--json]` summarizes history;
`aq stats export --since 7d --format jsonl|csv [--table T]` exports raw rows
(items/runs/host/incidents/events/hook). CSV requires a table. No commands,
env, cwd, session IDs or delivery error text are exported. Logs of terminal
items expire after 7d or oldest first above 5G; active logs are preserved.
A pruned item has logs_pruned=true; logs exits 1 and result reports removal.
History lasts 90d, host samples 30d (10-minute rows after 2d).
