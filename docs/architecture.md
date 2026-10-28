# aq architecture

How `aq` works and why it is built this way, for developers who want to
understand the design or contribute. For installation and usage, see the
[README](../README.md).

## 1. Problem and goals

Several coding agents (and people) share one Linux machine, and each starts
heavy work: `pytest -n 16`, large builds, docker compose stands. Each checks
free memory on its own, sees enough, and starts; three of them count on the
same free gigabytes at once, and the machine stalls in reclaim or the OOM
killer takes down something unrelated. Undeclared load (a stand left running,
containers started by tests) makes it worse.

The core of `aq` is one place where "does it fit?" and "take it" happen
atomically. Heavy work is either run by `aq` or declared to it. Cgroup
limits, the queue, notifications, estimates and statistics are conveniences
and safety nets around that point.

Goals: a queue of commands that `aq` runs under hard cgroup limits and cleans
up itself; reservations for load the caller starts itself, with a mandatory
teardown script; priorities and backfill; per-profile history used to fill in
requests and forecast starts; waking the submitting agent session when its
task ends, without polling; operator alerts; new agent clients through
configuration rather than scheduler changes; visibility into where host
memory goes. The daemon itself must stay light (no process per waiting task,
no frequent subprocess polling), and no task may ever run without its limits.

### Non-goals

- More than one machine (items carry a `node_id`, always `local`).
- Preemption, checkpointing, automatic retries, automatic limit increases
  after an OOM.
- Managing containers `aq` does not own, beyond running a teardown script, or
  tracking containers a task starts itself: they live in dockerd's cgroup, and
  the supported pattern is a reservation plus `submit --within` (section 5).
- Protection against an agent that deliberately bypasses `aq` with the same
  Unix permissions. The threat model is one user and cooperative clients.
- Workspace fingerprinting: only the git revision and a dirty flag are
  recorded, never enforced.
- Changing a queued item's priority, aging of `low` items, a web UI, a metrics
  endpoint, other operating systems.

## 2. Components and process model

One Go binary, `aq`, with several modes:

- `aq daemon` holds all state and logic. It runs as the systemd user service
  `aq.service` (`Restart=on-failure`), outside the task slice, so only tasks
  are charged to the pool.
- `aq <command>` is a thin CLI client: it parses flags, detects the caller's
  identity, captures a few environment variables and sends one request.
- `aq mcp` is a stdio MCP server (newline-delimited JSON-RPC) exposing the
  same API as tools: `queue_submit`, `queue_reserve`, `queue_extend`,
  `queue_release`, `queue_cancel`, `queue_status`, `queue_result`,
  `queue_list`, `queue_ack`, `queue_logs`, `queue_estimate`.
- `aq hook pretooluse` is a PreToolUse hook that never talks to the daemon.
- `aq _exec` and `aq _record-result` are hidden modes run inside a task's unit.

```
  agent / person                        systemd user manager
  +----------------+                    +--------------------------------------+
  | aq <command>   |---+                | aq.service: aq daemon                |
  | aq mcp (stdio) |---+-- Unix socket ->|  scheduler | store (SQLite) | outbox |
  +----------------+      JSON, one     |  host guard | samplers | retention   |
  | aq hook        |      request per   |        | systemd-run / systemctl     |
  | (no daemon I/O)|      connection    |        v                             |
  +----------------+                    | aq.slice   (MemoryMax = pool_memory) |
                                        |   aq-<id>.service: aq _exec -- argv  |
                                        | aqtd.slice (teardown units)          |
                                        |   aq-teardown-<id>.service           |
                                        +--------------------------------------+
```

The protocol is newline-delimited JSON over
`$XDG_RUNTIME_DIR/agent-queue/aq.sock` (directory `0700`, socket `0600`); the
server rejects peers of another Unix user (`SO_PEERCRED`). A connection
carries one request `{v, op, args}`, optional stream messages (`logs
--follow`, `stats export`) and one final response `{v, ok, result | error}`.
The version is `1`. A client keeps its write side open until the final
response: EOF means the client left and cancels the handler, which is what
keeps blocked `wait` handlers from leaking.

The daemon holds an exclusive `flock` on `daemon.lock` in its runtime
directory and one lock per slice under `$XDG_RUNTIME_DIR` (independent of
`AQ_RUNTIME_DIR`), so two daemons with different directories cannot manage
the same slice and stop each other's units as orphans. State lives in
`~/.local/state/agent-queue/`, the config in
`~/.config/agent-queue/config.toml` (overridable with `AQ_STATE_DIR`,
`AQ_RUNTIME_DIR`, `AQ_CONFIG`). The config is TOML over built-in defaults;
unknown keys are an error, and values are validated against `MemTotal` and
the CPU count at startup.

## 3. Execution model

### Transient units

An admitted task becomes a transient user unit, started with `systemd-run
--user --quiet --collect --expand-environment=no` (systemd 254 or newer) as
`<unit_prefix><id>.service` in `slice_name` (defaults `"aq-"`, `"aq.slice"`).
IDs are random UUIDv4.

| Property | Value |
|---|---|
| `MemoryMax` | the task's limit (`--memory`, or from the profile) |
| `MemoryHigh` | limit × `memory_high_ratio` (default `1.0`, equal to `MemoryMax`) |
| `MemorySwapMax` | `0` |
| `CPUQuota` | `--cpus` × 100 % |
| `KillMode`, `OOMPolicy` | `control-group`, `kill` |
| `OOMScoreAdjust` | `900` (teardown units: `0`) |
| `RuntimeMaxSec` | `--timeout`, rounded up to seconds |
| `TimeoutStopSec`, `TimeoutStopFailureMode` | `cancel_grace` (default `"10s"`), `kill` |
| `ExecStopPost` | `aq _record-result <job dir>` |

The command is an argv array: no implicit shell, and systemd does not expand
`$` in it. The environment is explicit: the client captures only `PATH`,
`HOME`, `LANG`, `LC_*`, `VIRTUAL_ENV` (reservations also `DOCKER_HOST`,
`DOCKER_CONTEXT`, `DOCKER_CONFIG`) plus `--env K=V`, and the daemon adds
`AQ_JOB_ID` and `AQ_CPUS`. Values are redacted from launch errors and
dropped from the database once an item is terminal.

Because some distributions ship a `service.d` drop-in that turns a stop
timeout into `SIGABRT` with a core dump, and drop-ins beat transient
properties, the daemon also writes a runtime drop-in for its unit prefix
(`<unit_prefix>.service.d/99-aq-kill.conf`) with `TimeoutStopFailureMode=kill`.

`systemd-run` rejects invalid properties before anything starts. After it
returns, the daemon compares the unit's `memory.max` with the requested
limit; if the limit is not in place, the unit is stopped and the task ends as
`LAUNCH_FAILED`.

### Runner and result recording

The unit's main process, `aq _exec`, creates the task's temp directory
(`TMPDIR`, `TMP`, `TEMP`), changes into the task's cwd (not set on the unit, so
`ExecStopPost` still runs if the task deletes its directory), forwards
`SIGTERM`/`SIGINT`, and writes stdout and stderr to `jobs/<id>/stdout.log` and
`stderr.log`. Each stream is capped by `[logs] max_stdout` / `max_stderr`
(`"64M"`); the excess is read and discarded, so a task never blocks on a full
pipe, and the truncation is recorded.

The result is written by the unit, not the daemon. `ExecStopPost` runs
`aq _record-result`, which takes `SERVICE_RESULT`, `EXIT_CODE` and
`EXIT_STATUS` from systemd and `memory.peak` and `memory.events` from the
unit's cgroup, and atomically writes `jobs/<id>/result.json`. A collected unit
or a daemon that is down does not lose the outcome. The recorder runs under
the unit's `MemoryMax`, hence the 64 MiB minimum task memory.

### Lifecycle

Task states: `QUEUED -> STARTING -> RUNNING -> CLEANING -> SUCCEEDED | FAILED |
CANCELLED`, and `REJECTED` for invalid requests. Each pass reads whether the
unit's cgroup tree is populated (`cgroup.events`), then whether
`result.json` exists. A result moves the task to `CLEANING`; it is finalized
only when its cgroup is empty, and its resources stay counted until then.
Processes lingering `2 × cancel_grace` after the result get another stop, and
after `cleaning_stuck_alert` (`"5m"`) the operator is alerted. No processes
and no result, continuously for `cancel_grace`, is `FAILED/LOST`.

Outcome mapping: a launch error is `LAUNCH_FAILED`; a requested cancel is
`CANCELLED`; an OOM kill with `oom > 0` in the task's own `memory.events` is
`OOM` (its own limit); another OOM kill while the host guard was active
(within 5 s of the task's end) is `OOM_HOST_GUARD`, otherwise `OOM_GROUP` (the
slice's pool limit); `SERVICE_RESULT=timeout` is `TIMEOUT`; a nonzero exit is
`COMMAND_FAILED`. Exit code 137 is never used to infer an OOM. The reason
`STORAGE_FULL` and the state `RECOVERING` exist in the model, but no code path
currently produces them.

`aq cancel` is idempotent. A queued item is cancelled at once; a starting or
running one is marked and stopped in the background (`systemctl --user stop`:
`SIGTERM` to the whole cgroup, `SIGKILL` after `cancel_grace`), including
descendants that called `setsid` or daemonized. A cancelled unit that is
still alive is stopped again every `cancel_grace`.

### Keeping an OOM inside the task

- The task's `MemoryMax` makes the memory-cgroup OOM killer act inside that
  cgroup only; `OOMPolicy=kill` ends the whole unit.
- `MemoryHigh` equals `MemoryMax` by default: with no swap, a lower
  `MemoryHigh` throttles an over-limit task indefinitely instead of letting it
  reach the OOM, and it would crawl to its timeout.
- The slice has `MemoryMax = pool_memory` and `MemorySwapMax=0`, set by the
  daemon at startup (`systemctl --user set-property --runtime`), and the host
  guard lowers its ceiling when the host runs short (section 4).
- `OOMScoreAdjust=900` makes task processes the first choice in a global OOM.
- Teardown units run in a separate slice, `aqtd.slice` (the task slice name
  plus `td`), with `MemoryMax = teardown_concurrency × teardown_memory`,
  outside the pool and the guard, at `OOMScoreAdjust=0`: the script that frees
  a stand must not be what gets killed.

## 4. Admission

A task has a `limit` (its `MemoryMax`) and an `expected` value (`--expected`,
by default the limit, or from the profile). Admission uses `expected`; the
limit protects the host from one runaway task. The sum of running limits can
therefore exceed what the host can give: in the worst case one task is killed,
not the host, and the host guard enforces that order.

### The check

Every pass runs under one scheduler mutex, so concurrent submitters cannot
take the same memory. For a candidate `X`, the first failing check is its
blocker:

1. `recovering`: reconciliation after a daemon start is not finished.
2. `pressure`: the PSI gate is closed. It closes when memory PSI
   `full avg10 >= close_full_avg10` (`5.0`) for `close_after` (`"10s"`), and
   opens when it stays below `open_full_avg10` (`1.0`) for `open_after`
   (`"30s"`).
3. `disk`: free space on the state directory is below `min_disk_free`
   (`"2G"`).
4. `pool_memory`: committed memory + `expected(X)` > `pool_memory` (`"16G"`).
5. `pool_cpu`: committed CPUs + `cpus(X)` > `pool_cpus` (`16`).
6. `live_memory`: `MemAvailable - unconsumed < reserve_floor + expected(X)`,
   where `reserve_floor` is `reserve_floor_percent` (`10`) of `MemTotal`.

Committed resources are `expected` and CPUs of non-nested tasks from
`STARTING` until finalized, and stand plus task budget of held reservations.
`unconsumed` is memory promised but not yet visible in `MemAvailable`: for a
running task `max(0, expected - memory.current)`, for reservations the parts in
section 5, for a running teardown `max(0, teardown_memory -
memory.current)`. Time alone never counts as consumption. The pool check
prevents the race; the live check catches background load that has grown.

Requests that can never fit (zero memory, limit or CPUs above the pool, limit
below 64 MiB, `expected > limit`, a cwd that does not exist) are `REJECTED` at
submit. Gates (`recovering`, `pressure`, `disk`) delay admission but never
kill running tasks.

### Order and backfill

The queue is ordered by priority (`high`, `normal`, `low`), then creation
time; tasks and reservations share it. A pass runs on submit and release and
at least once a second. It admits from the head while items fit; the first
that does not is the blocked head, whose blocker is shown in `aq list`,
`aq top` and `aq estimate`. If `recovering`, `pressure`, `disk` or
`live_memory` keeps the head waiting for `admission_closed_alert` (`"10m"`),
the operator is alerted.

With `[scheduler] backfill = true` and a head blocked by `pool_memory`,
`pool_cpu` or `live_memory` (never by a gate), later items may overtake it
EASY-style, without delaying it:

- Each holder has an estimated end: a task at start + profile duration, or
  start + timeout + `cancel_grace` without history (at least now, if
  overrun); a granted reservation at `max(expiry, nested deadlines) +
  teardown_timeout`; a `TEARING_DOWN` one at teardown start +
  `teardown_timeout`; failed teardowns never.
- The shadow time is the first release at which the head fits; the capacity
  left at that moment after placing the head is the extra capacity. If no
  release makes the head fit, there is no backfill.
- A later item is admitted if it fits now and either ends, by its estimate, by
  the shadow time, or fits into the remaining extra capacity, which it then
  consumes. A candidate that does not qualify is skipped, not a stop.
- Reservations backfill only through the extra capacity, never by time: their
  end depends on `aq extend`, on nested tasks and on shared teardown slots. A
  reservation is also skipped if the head waits on a teardown before the
  shadow time and the newcomer would compete for a slot
  (`teardown_concurrency`).

A backfilled admission does not reset the head's `admission_closed` timer.
Estimates can be wrong, and then the head waits longer; that is accepted.

### The host guard

Admission cannot stop running tasks from growing toward their limits while
background load grows too. Once a second the guard computes

```
ceiling = min(pool_memory, slice memory.current + MemAvailable - reserve_floor)
```

(never below 256 MiB) and writes it to the task slice's `memory.max` and
`memory.high` in cgroupfs. Decreases apply at once, increases only in steps of
at least 64 MiB, so the value does not oscillate. The write uses a blocking
file descriptor: on a non-blocking one the kernel skips the reclaim/OOM step
of a `memory.max` write, and lowering it would kill nothing. The guard
rewrites the value after systemd resets it (units starting or stopping in the
slice do), and while the ceiling is below the pool it is marked active, which
is what labels kills `OOM_HOST_GUARD`. It never touches the teardown slice.

## 5. Reservations

A reservation holds memory for load `aq` does not run (a compose stand, or
containers started by tests). It has two parts that never flow into each
other: the stand (`--memory`, `--cpus`, CPUs default 0) and a budget for
nested tasks (`--task-memory` and `--task-cpus`, set together). Both count
against the pool. A reservation waits in the task queue; `aq reserve` answers
after the next pass with `GRANTED` or `QUEUED` plus position and blocker. The
TTL (`--ttl`, at most `max_ttl`, `"24h"`) starts at the grant.

States: `QUEUED -> GRANTED -> EXPIRING -> TEARING_DOWN -> TORN_DOWN |
TEARDOWN_FAILED | TEARDOWN_UNKNOWN`, plus `GRANTED/EXPIRING -> RELEASED` and
`QUEUED -> CANCELLED`. The two failure states are not terminal: the
reservation stays in the accounting.

Unconsumed memory of a reservation:

- task budget: always `max(0, task_memory - Σ memory.current of its running
  nested tasks)`;
- stand with `--docker-project P`: every 5 s the daemon asks the Docker
  Engine API (`/var/run/docker.sock`) for running containers labelled
  `com.docker.compose.project=P`, sums their `memory.current - inactive_file`,
  and counts `max(0, memory - measured)`. A measurement older than 30 s is
  ignored. A project belongs to at most one non-terminal reservation; a stand
  above its declaration triggers `stand_over_declared` once;
- stand without a fresh measurement: the full `memory`, for the reservation's
  whole life. A running stand is then counted twice (in `MemAvailable` and
  here), but a granted stand can always grow to its declaration. There is no
  "stand is up" signal: being up says nothing about how much more it will
  use.

`aq extend ID --ttl D` sets the expiry to `max(expiry, now + D)`, so a repeat
is harmless. `expiry_warning` (`"15m"`) before expiry the reservation becomes
`EXPIRING` and `reservation_expiring` goes to the session and the operator;
at expiry it is due for teardown. `aq release ID` ends a granted reservation
without the script (the caller shut the stand down) and is refused while
nested tasks are unfinished.

### Nested tasks: `submit --within`

If a reservation and its tasks were admitted independently, two agents could
each hold a reservation while neither task fits the pool. `aq submit --within
RES_ID` therefore runs the task inside the task budget, with no second pool
admission and no queue. The parent must be `GRANTED` or `EXPIRING` and not
past its expiry; the limit must fit `task_memory`, and `expected` and CPUs of
unfinished nested tasks plus the new one must fit the budget, or the submit is
rejected at once. Nested tasks start on the next pass regardless of pressure,
disk or live memory (only recovery holds them back), and keep their own
cgroup limits.

At expiry, a reservation with unfinished nested tasks is held (`EXPIRING`,
`ttl held`) until the latest nested deadline (`started_at + timeout + 2 ×
cancel_grace`); the operator is told, new `--within` tasks are refused, and it
is torn down as soon as the last nested task ends.

### Teardown

The script is mandatory (`--teardown FILE`, at most 64 KiB of UTF-8). The
client sends its content and the daemon keeps a copy in
`jobs/<id>/teardown.sh`, so a deleted worktree cannot make a stand
unremovable. Scripts must be self-contained and idempotent (`docker compose
-p P down`).

Due reservations get teardown slots by expiry, at most `teardown_concurrency`
(`2`) at a time; the rest wait in `EXPIRING`. The reservation is persisted as
`TEARING_DOWN` with a new attempt number before the launch. The script runs
as `bash <copy>` in `aq-teardown-<id>.service` with `teardown_memory`
(`"512M"`), one CPU, `teardown_timeout` (`"5m"`), the captured environment
plus `AQ_ORIGINAL_CWD` and `AQ_RESERVATION_ID`, in the original cwd if it
still exists or else an empty directory under `jobs/<id>/teardown-<n>/`. The
unit records its result as a task does. Exit 0 is `TORN_DOWN`; a nonzero exit,
timeout or OOM is `TEARDOWN_FAILED`; a unit gone without a result is
`TEARDOWN_UNKNOWN`. Failures alert the operator, keep the memory counted, and
are never retried automatically: the operator runs `aq teardown --retry ID`
(safe because scripts are idempotent) or, after cleaning up by hand,
`aq release --force ID`. Both are refused while the previous teardown unit
still has processes.

## 6. Profiles and estimates

A profile is a stable name for a recurring task. With `--name N` and no
`--profile`, the client derives `<main repository root>:<N>` from `git
rev-parse --git-common-dir`, so all worktrees share history; `--project`
replaces the root, `--profile P` sets the whole name. No name, no history.

While a profiled task runs, its `memory.current` goes into a histogram at most
once a second (16 linear sub-buckets per power of two). The finished run is
stored with its `memory.peak`, duration, outcome and histogram. A run is
usable if it `SUCCEEDED` or failed with `COMMAND_FAILED`, with nonzero peak
and duration; OOMs, timeouts and cancellations never feed the estimate, so a
failure never raises a limit automatically. From the last `window` (`10`)
usable runs:

- `limit = max(peak) × limit_margin` (`1.2`), rounded up to a MiB, clamped to
  64 MiB .. `pool_memory`;
- `expected` = `memory_percentile` (`90`) of the merged histograms, at most
  the limit: the level the task spends its time at, not one maximum;
- duration = `duration_percentile` (`90`) of run durations, capped by
  `--timeout`.

An explicit `--memory` always wins. Omitting memory for a profile without
usable runs is rejected.

`aq estimate` replays the admission logic on the latest pass's snapshot with
a hypothetical item inserted after everything of equal or higher priority,
storing nothing. It reports whether the item would start now (and whether by
backfill), or its position, blocker and an expected start from a forward
simulation of holder releases, with a confidence of `now`, `history` (every
holder ahead has a profile estimate), `partial`, `timeout` (none has) or
`unknown`. The same simulation fills the `START` column of `aq list`; while a
gate is closed there is no forecast.

## 7. Notifications

Events are rows in `events`, written in the same SQLite transaction as the
state change that causes them, with a stable `event_id`.

| Event | Audience |
|---|---|
| `task_finished`; `reservation_granted` (only if it waited) | session |
| `reservation_expiring`, `stand_over_declared` | session and operator |
| `task_failed_infra` (`LAUNCH_FAILED`, `LOST`, `OOM_HOST_GUARD`), `reservation_held_by_tasks`, `teardown_succeeded`, `teardown_failed`, `teardown_unknown`, `reservation_force_released`, `cleaning_stuck`, `admission_closed`, `delivery_failed` | operator |

Session events are skipped without a session or with `--no-notify`; operator
events are stored as `skipped` without a webhook. Intermediate states never
wake a session.

The outbox runs one lane per channel. A lane claims the oldest due event
(`pending -> delivering`), delivers it within `delivery_timeout` (`"30s"`)
and marks it `sent`. Failures retry with exponential backoff from `retry_min`
(`"1s"`) to `retry_max` (`"5m"`), ±20 % jitter, until `give_up_after`
(`"24h"`) after creation, when the event becomes `failed` and, for a session
event, a `delivery_failed` operator event is created. Delivery is at least
once: a crash between delivering and recording can repeat an event, with the
same `event_id`, after restart. A `sent` event is never delivered again,
because every push wakes a model and costs tokens.

`acked` is set only by `aq ack EVENT_ID`, after the agent has recorded the
result. Ack is bookkeeping: `aq list --mine` / `--session` show unacknowledged
events, and the skills reconcile them on the next contact. Acking a still
`pending` event stops its delivery. Message text is at most 8 KiB (name, ID,
state, reason, project, how to fetch the result, log path) and never includes
environment values, teardown scripts or log contents.

### Channels

Adapters are `[[agents]]` entries (`name`, `identity_env`, `delivery`,
`command`); a non-empty list replaces the defaults
(`codex`/`CODEX_THREAD_ID`/`push`, `claude`/`CLAUDE_CODE_SESSION_ID`/`client-wait`).

- `push`: the daemon runs `command`, an argv template with `{session}`,
  `{text}`, `{event_id}`, `{item_id}`, `{kind}`, without a shell, in its own
  process group, killed on timeout; exit 0 is a delivery. The Codex default is
  `codex queue --thread {session} --message {text}`. If the executable is not
  in the daemon's `PATH`, the submit fails before anything is stored instead
  of losing notifications.
- `client-wait`: the agent (Claude Code) starts `aq wait ID` as a background
  command and ends its turn; the harness wakes the session when the process
  exits. `aq wait` blocks on the socket without polling and returns when a task
  is terminal or a reservation has left the queue (with `--until expiring`,
  when it is past `GRANTED` or its stand went over its declaration). It marks
  the matching pending events `sent` and prints their IDs; its exit code
  encodes the outcome (see the README).
- `webhook` (operator): `POST` of a JSON body to `[webhook] url` with
  `Idempotency-Key: <event_id>` and, with `token_file`, `Authorization: Bearer
  <token>` (read on every attempt). Only 2xx counts; redirects are not
  followed.

MCP does not replace waking: a tool call cannot wake an idle session, so a
client-wait agent still runs `aq wait` after `queue_submit`.

Identity is detected by the client: the adapter whose `identity_env` is set
gives the client type and session. If several are set (an agent started from
another agent's shell inherits its variable), `aq` does not guess: the CLI
exits with 64, MCP requires explicit `client` and `session_id`. With none set
the client is `cli` and gets no session events. `--client` / `--session` (MCP:
`client`, `session_id`, `no_notify`) set it explicitly.

## 8. Observability

`aq top` is one snapshot with two views that are deliberately not added up:

- physical use, in non-overlapping categories summing to `MemTotal -
  MemAvailable`: tasks (working set, `memory.current - inactive_file`, of the
  task and teardown slices), docker (container cgroups under
  `system.slice/docker-*.scope` or `docker/*`), agents (PSS of processes whose
  name or `argv[0]` is in `[stats] agent_processes`, default `["claude",
  "codex"]`, outside task and docker cgroups), and other. The `/proc` scan has
  a 5 s timeout and never runs twice at once, so a process stuck in an
  uninterruptible read cannot hang the daemon;
- scheduler commitments from the latest pass: task `expected`, stands, task
  budgets, the unconsumed part of each, totals against the pool, the queue and
  the head's blocker.

Every `sample_interval` (`"30s"`) a host sample stores memory, swap, PSI, the
gate, both views, committed memory and CPUs, queue counts and the head
blocker. Incidents are sampled every 10 s, after a baseline reading:
`kernel_oom` is the growth of `oom_kill` in `/proc/vmstat` minus OOM kills in
the task and teardown slices (contained task OOMs are not host incidents);
`oomd_kill` comes from `Killed ...` entries of the `systemd-oomd` journal,
read about once a minute with a cursor file; `psi_close`/`psi_open` are gate
transitions.

The hook reads the client's hook JSON, splits the shell command on `;`, `&`,
`|` and newlines outside quotes, skips segments that run `aq`, and matches the
rest against `[hook] patterns` (parallel pytest, `docker compose up/run`,
`docker-compose up/run`, `docker run`). A match returns `additionalContext`
with a reminder. It never blocks, never contacts the daemon, and is silent on
errors. Hits go to `hook-hits.jsonl` in the state directory (time, pattern
index, client, never the command), rotated at 1 MiB.

`aq stats [--since D] [--profile P]` summarizes outcomes and waiting, profile
accuracy, queue and host, reservations by client type, delivery, hook hits
and incidents. `aq stats export --format jsonl|csv` streams the tables
`items`, `runs`, `host`, `incidents`, `events`, `hook`. Commands,
environment, cwd, session IDs, diagnostics and delivery error texts are never
exported. `aq doctor` checks the host prerequisites, runs a probe unit that
proves `MemoryMax` is applied, and checks the daemon, adapters and webhook.

Retention runs at startup and every 10 minutes. Logs of finished items go
after `logs_max_age` (`"7d"`) or, oldest first, while `jobs/` exceeds
`logs_max_total` (`"5G"`); active items, failed teardowns included, are kept.
History (items, events, runs) and incidents go after `history_max_age`
(`"90d"`), never before their logs. Host samples are kept `samples_max_age`
(`"30d"`); beyond `samples_full_res` (`"2d"`) one sample per 10 minutes
remains. Orphaned task and temp directories are removed after an hour.

## 9. State and recovery

State is one SQLite database, `aq.db` (WAL, `synchronous=NORMAL`, mode
`0600`), migrated through `PRAGMA user_version`:

- `items`: tasks and reservations (request, resources, state, timestamps,
  outcome, profile, parent, task budget, expiry, teardown attempt, docker
  project), with an idempotency key and request hash: a repeated `--key` with
  the same request returns the same item, a different one is
  `IDEMPOTENCY_CONFLICT`;
- `events`: the outbox and ack state;
- `profile_runs`: finished profiled runs with their histograms;
- `host_samples`, `incidents`.

State changes use `BEGIN IMMEDIATE` transactions, and none is held while a
unit is launched or stopped. Logs, result files and teardown copies live in
`jobs/<id>/`.

At startup the daemon validates the config, takes its locks, opens the
database, sets up both slices and the drop-in, and runs recovery with
admission closed:

- each non-terminal task: liveness first, then the result file. Result and no
  processes: finalized from the file. Result and processes: `CLEANING`.
  Processes and no result: `RUNNING`. Unreadable result: `LOST`. `STARTING`
  with neither: back to `QUEUED` (a unit always records a result, so none
  ran). Anything else: `LOST`;
- a `TEARING_DOWN` reservation with a live unit is adopted, one with a result
  is settled from it, one with neither becomes `TEARDOWN_UNKNOWN`; expiry
  missed during downtime is handled by the next pass;
- prefixed units in either slice that no active item owns are stopped.

A recovery error makes the daemon exit (admission must not open on
unreconciled state) and systemd restarts it. The loops start and the socket
is served only after recovery.

Running tasks and teardowns survive a restart (they are independent systemd
units), as do their results, queued items, reservations with their TTLs, and
undelivered events. Client connections do not: a background `aq wait`,
`aq logs --follow` or an MCP call in flight fails, and the agent must start
`aq wait` again. In-memory state is lost too: the guard's activity windows,
the PSI gate, and memory samples of runs in progress.

## 10. Key design decisions

**systemd transient units, not an own supervisor.** A supervisor in the
daemon would need its own cgroup management, tree kill, OOM attribution and a
way to outlive daemon restarts. A transient unit gets all of this from the
user manager: limits set when its cgroup is created, `KillMode=control-group`,
`OOMPolicy`, `RuntimeMaxSec`, and a process tree independent of the daemon.
The daemon calls the `systemd-run` and `systemctl` commands instead of the
D-Bus API, which keeps dependencies small at the cost of a process spawn per
launch or stop.

**Results recorded by the unit.** The daemon may be down when a task ends and
systemd may collect the unit before it returns; `ExecStopPost` writing
`result.json` makes the outcome independent of both.

**No external queue or HPC scheduler.** On one machine a batch system such as
HyperQueue covers a small part of the problem while adding a server and
workers to run and a second source of truth to reconcile. The admission core
is a few hundred lines of pure Go.

**cgroup v2 only.** The design relies on delegation to the user manager,
`memory.peak`, the `oom`/`oom_kill` split in `memory.events`, `cgroup.events`
`populated`, and PSI.

**Pure-Go SQLite and few dependencies.** `modernc.org/sqlite` keeps the
binary free of CGO and gives atomic state changes together with their events,
with no separate service. The only other direct dependencies are
`github.com/BurntSushi/toml` and `golang.org/x/sys` (flock, `SO_PEERCRED`,
`statfs`, terminal detection). The Docker API, webhook, MCP and terminal
output use the standard library, without SDKs or UI libraries.

**Expected versus limit, guarded.** Admitting by the hard limit would hold
memory most runs never touch; admitting by an estimate alone would let tasks
overrun the host. Splitting the two, as Kubernetes requests and limits do,
plus a guard that makes the slice hit its ceiling first, trades an occasional
killed task for better use of the machine.

**Time is never proof of consumption.** An unmeasured stand stays fully
counted, and unused task budget is always counted. This wastes some memory;
the alternative lets two reservations be granted the same free gigabytes.

**Nested tasks bypass the queue.** Admitting a reservation's tasks separately
can deadlock agents that each hold a reservation; charging them to the budget
removes the cycle.

**Fail closed.** Recovery errors keep admission closed, and failed teardowns
keep their memory counted until a person decides.

**A soft hook; waking left to real wake-up paths.** The hook only reminds:
false positives and misses are acceptable for a guard against forgetfulness,
and it must never break agent work, even with the daemon down. Waking uses
mechanisms that can wake an idle session (`codex queue`, a background
`aq wait`), not MCP.

**Plain text for agents, pretty output for people.** Skills parse the plain
output, so its format is stable. Colors, tables, memory bars and short IDs
appear only when stdout is a terminal (`AQ_UI=pretty|plain` forces a mode,
`NO_COLOR` drops colors); `--json` works everywhere. Commands accept a unique
ID prefix of at least 4 characters.

**A pure core behind platform interfaces.** `internal/admission` has no I/O
and no clock. The scheduler sees `platform.Executor`, `HostMetrics`,
`StandMeter`, `UsageMeter` and `OOMKillLog`, implemented for Linux and as an
in-memory fake for tests. Executors declare capabilities (hard memory limit,
tree kill, OOM attribution, survives daemon restart), so a weaker platform
can be added later without claiming the same guarantees.

## 11. Known limitations and future work

- Linux only: systemd user manager 254+, cgroup v2 with `memory`, `cpu`,
  `pids` delegated, one machine. Executor capabilities are declared but not
  yet reported by `aq doctor` or used by policy.
- Containers a task starts itself are outside its cgroup and must be covered
  by a reservation's stand. Only compose projects are measured, and meter
  failures (for example, no access to the Docker socket) are silent: the
  stand is counted in full with no log line or doctor check.
- The death of a reservation owner's session is not detected.
- Estimates do not guarantee start times. Task CPU use is not sampled;
  profiles cover memory and duration only.
- A restart cuts client connections and forgets in-memory state: a guard kill
  finalized after a restart is reported as `OOM_GROUP`, and a run spanning a
  restart loses its earlier samples. The socket is served only after
  recovery, so clients see "daemon unavailable" (exit 69) meanwhile.
- Same-user threat model: environment values are briefly visible in the
  `systemd-run` command line during launch, and cleared values may linger in
  SQLite free pages or the WAL.
- `admission_closed` is not raised when a pass ends early on a host read
  error.
- `aq doctor` can keep reporting `WEBHOOK_FAILING` from an old failure after
  the webhook recovers, and does not check the teardown slice.
- The hook is a regex heuristic that also matches inside quoted strings;
  `hook-hits.jsonl` rotation is not locked across processes (statistics
  only). `queue_logs` returns the log text twice (text and structured
  content). A flag value of literally `--` ends the flags.
- Extensions the design leaves room for: other platforms behind the existing
  interfaces, several nodes (`node_id`), automatic tracking of containers
  started by tasks, and session liveness for reservation owners.

## 12. Code map

| Path | Contents |
|---|---|
| `cmd/aq` | entry point: `_exec`/`_record-result` to the runner, everything else to the CLI |
| `internal/admission` | pure admission: blocker order, EASY backfill and forecasts, PSI gate, guard ceiling |
| `internal/scheduler` | mutex-guarded queue: submit, passes, launch, reaping, cancel, reservations, teardown, recovery, estimates, profile sampling |
| `internal/store` | SQLite store and migrations: items, outbox, profile runs, samples, retention and export queries |
| `internal/platform` | OS-facing interfaces (`Executor`, `HostMetrics`, meters, OOM log) |
| `internal/platform/linux` | systemd/cgroup executor, procfs host metrics, Docker stand meter, usage meter, systemd-oomd journal reader |
| `internal/platform/fake` | goroutine-safe fake platform for tests |
| `internal/runner` | in-unit supervisor (`aq _exec`) and result recorder (`aq _record-result`) |
| `internal/daemon` | wiring and locks, request handlers, host guard, samplers, incidents, retention, `top`/`stats` views |
| `internal/protocol` | versioned newline-delimited JSON over the Unix socket, client and server |
| `internal/notify` | routing, outbox lanes with retries, push (exec) and webhook delivery, message text |
| `internal/client` | request building shared by CLI and MCP, environment capture, identity, profile and project detection |
| `internal/cli` | commands, help, plain and pretty output, exit codes, `aq doctor` |
| `internal/mcp` | stdio MCP server and tools |
| `internal/hook` | PreToolUse reminder and hit counter |
| `internal/profile` | usable runs, histogram, percentile estimates, name validation |
| `internal/stats` | pure summaries for `aq stats` |
| `internal/model` | items, states, reasons, priorities, events |
| `internal/config` | loading, defaults, validation, paths |
| `internal/units` | sizes and durations |
| `deploy/` | systemd units, example config, hook fragments, agent skills |
| `test/e2e` | end-to-end tests against the real user manager in separate slices |
