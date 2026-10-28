[← README](../README.md)

# User guide

- [Requirements](#requirements)
- [Installation](#installation)
- [Help and output](#help-and-output)
- [Running tasks](#running-tasks)
- [Profiles and estimates](#profiles-and-estimates)
- [Watching and waiting](#watching-and-waiting)
- [Retention](#retention)
- [Diagnostics: `aq doctor`](#diagnostics-aq-doctor)
- [Changing the pool](#changing-the-pool)
- [OOM and other failures](#oom-and-other-failures)
- [Cancellation](#cancellation)
- [Reservations](#reservations)
- [Notifications](#notifications)
- [MCP](#mcp)
- [Hook](#hook)
- [Recovery](#recovery)
- [Limitations of the current version](#limitations-of-the-current-version)
- [Development](#development)
- [Further reading](#further-reading)
- [License](#license)

## Requirements

- Linux with cgroup v2 and the `memory`, `cpu`, `pids` controllers delegated to the user;
- systemd ≥ 254 (user manager);
- Go 1.26 and [just](https://github.com/casey/just) to build.

## Installation

Choose your pool before installing: follow the [first-run setup](../README.md#start-here). In particular, the default `pool_cpus = 16` must not exceed the machine’s CPU count.

```sh
just install
```

`just install` builds `bin/aq` and:

- puts the binary in `~/.local/bin/aq`;
- puts `aq.service` and `aq.slice` in `~/.config/systemd/user/`;
- copies `deploy/config.example.toml` to `~/.config/agent-queue/config.toml`
  only if there is no config yet (an existing one is not touched);
- runs `systemctl --user daemon-reload`, enables `aq.service` and
  (re)starts it, so running `just install` again updates the running daemon;
- copies the `aq` skill separately for each agent: `deploy/skills/claude/aq` to
  `~/.claude/skills/aq`, `deploy/skills/codex/aq` to `~/.codex/skills/aq` (the
  paths can be changed with `CLAUDE_SKILLS`/`CODEX_SKILLS`; an empty value means
  do not install); hooks are set up separately;
- if linger is off, prints a hint: without `loginctl enable-linger $USER`
  the daemon and all tasks stop when the last session ends.

Before installing, check `pool_memory` in the example config: the default
`16G` must be no more than `MemTotal` minus the 10 % reserve, otherwise the
daemon will not start (`CONFIG_INVALID`).

Directories can be overridden with environment variables: `BINDIR`, `UNITDIR`,
`CONFDIR`, `STATEDIR`, `CLAUDE_SKILLS`, `CODEX_SKILLS`, and `SYSTEMCTL` (default
`systemctl --user`). For example, `SYSTEMCTL=true just install` only lays out
the files.

If the installed `aq` responds and the queue has tasks or reservations,
`install`, `uninstall` and `purge` first list them. A restart is safe for them:
tasks keep running, `aq wait` reconnects, and the daemon saves the in-memory run
samples and guard history on a clean stop and restores them on start.

Check after installation:

```sh
aq doctor
systemctl --user status aq.service
```

All `aq doctor` lines should be `ok`. If you run `aq doctor` before the daemon
has started for the first time, it will correctly report `SLICE_MISSING`,
`KILL_DROPIN_MISSING` and `DAEMON_UNAVAILABLE`: `aq.slice` and the runtime
drop-in `$XDG_RUNTIME_DIR/systemd/user/aq-.service.d/99-aq-kill.conf` are
created by the daemon at startup. After `systemctl --user start aq.service`
these checks pass.

Removal:

```sh
just uninstall   # stops and disables the service, removes the binary and units
just purge       # same, plus removes ~/.local/state/agent-queue (DB and logs)
```

`just uninstall` keeps the DB, logs and config. `just purge` removes the state
but also keeps the config.

## Help and output

`aq help` lists commands by group; `aq help CMD` (or `aq CMD --help`) shows the
description, flags and examples for a command. A flag error is printed once,
with a hint on where to find the help.

The output depends on who reads it:

- **a person at a terminal**: colored states, tables, memory bars in `aq top`
  and `aq status`, short IDs and durations like `3m05s`;
- **an agent or script** (stdout is not a terminal): the same plain text as
  before, with a format that does not change because skills parse it;
- `--json`: machine-readable output in any mode.

`AQ_UI=pretty` or `AQ_UI=plain` selects the mode explicitly (for example, for
`aq list | less -R`); `NO_COLOR=1` turns colors off. Wherever a command takes a
task or reservation ID, a unique prefix of at least 4 characters is enough
(`aq status 5a0e71f2`), as shown by `aq list`.

## Running tasks

```sh
aq submit --memory 4G --cpus 2 --timeout 30m -- \
  bash -lc '/path/.venv/bin/python -m pytest -n "$AQ_CPUS"'
```

- `--memory` (required when there is no usable history): the hard memory limit
  of the task: `4G`, `512M`, `1024K`, `2T` (binary units); a bare number is MiB;
  the minimum is `64M`.
- `--timeout` (required): the maximum run time, for example `30m`.
- `--cpus`: how many pool CPUs to take (default 1); sets `CPUQuota`.
- `--priority low|normal|high`, `--name`, `--key` (idempotency: a repeated
  `submit` with the same key returns the same task), `--session`, `--env K=V`,
  `--expected` (expected consumption, default = `--memory`), `--json`.

The command is passed as argv, with no implicit shell, and systemd does not
expand variables in it. So `$AQ_CPUS` has to be expanded inside the task: running
through `bash -lc '…'` in single quotes leaves `$AQ_CPUS` untouched by your
shell, and `bash` inside the unit expands it, where the service has set
`AQ_CPUS=<--cpus>`. Without `bash -lc`, your shell would expand the variable
before `aq` starts (and get an empty string). Besides `AQ_CPUS`, the task gets
`AQ_JOB_ID`, its own `TMPDIR`/`TMP`/`TEMP`, and from the calling environment
`PATH`, `HOME`, `LANG`, `LC_*`, `VIRTUAL_ENV`.

`aq submit` prints the task id and returns immediately.

## Profiles and estimates

Give recurring runs a stable `--name`, for example `tests-unit`. Without an
explicit `--profile`, the profile is `<main repository root>:<name>`; it is
shared by all worktrees. `--project` replaces the root, and `--profile P` sets
the whole name. With no name and no profile, no history is collected.

Name the workload, not the run: the same suite keeps the same name across
releases. A version, date, commit or ticket number in the name (`tests-b15`)
starts a new, empty history every time it changes; keep that context outside
aq. A different name is right when the memory shape differs (another suite,
another worker count, a matrix entry such as `tests-pg16`). Leave `--project`
unset inside a worktree, or that worktree gets its own history.

The first run requires `--memory`. After at least one usable run, memory can be
omitted:

```sh
aq submit --name tests-unit --memory 2G --cpus 2 --timeout 15m -- pytest tests/unit
aq profile list
aq profile show tests-unit          # short name = <repository root>:tests-unit
aq submit --name tests-unit --cpus 2 --timeout 15m -- pytest tests/unit
```

Successful runs and `COMMAND_FAILED` runs with a nonzero peak and duration are
usable. OOMs, timeouts and cancellations do not raise the limit automatically.
Based on the last 10 usable runs, the limit is the maximum peak × 1.2, rounded
up to MiB (from 64 MiB up to the pool size); expected is the p90 of the
`memory.current` samples, no higher than the limit. An explicit `--memory`
always wins, and `--expected` can be overridden. The duration estimate is the
history p90, capped by `--timeout`.

Before a heavy run, find out whether you will have to wait:

```sh
aq estimate --name tests-unit --cpus 2 --timeout 15m
# queued: position 2, expected start in 12m0s
# memory: 1.2 GiB limit, 900 MiB expected
# duration: 3m0s (profile)
# blocked by: pool_memory ...
# confidence: partial
```

`aq estimate` does not queue anything. Without history you can pass `--memory`,
and without a duration estimate, `--timeout`. Confidence is one of `now`,
`history`, `partial`, `timeout` or `unknown`. `aq list` shows a `START` column,
and `aq status` shows the expected start of a waiting item.

Backfill lets a later task overtake a blocked head if it fits now and, by
estimate, will free its resources before the head starts, or fits into the free
remainder left after the head. Reservations overtake only through this
remainder: shared teardown slots can delay the release. Pressure, disk and
recovery close the overtaking; so does an unreachable head. Estimates can be
wrong, so the actual start is sometimes later than predicted. To disable it, set
`[scheduler] backfill = false`.

## Watching and waiting

```sh
aq list              # active tasks: state, memory, wait, BLOCKED_BY
aq list --all        # including finished ones
aq status <id>       # details: state, reason, memory peak, exit code
aq result <id>       # same plus paths to stdout.log/stderr.log
aq logs <id> [--stream stderr] [--tail 100] [--follow]
aq wait <id>         # waits for completion, prints the outcome and exits with a code:
```

| `aq wait` code | Meaning |
|---|---|
| 0 | `SUCCEEDED` |
| 1 | `COMMAND_FAILED` (the command exited with a nonzero code; it is in `aq status`) |
| 2 | `OOM`, `OOM_GROUP`, `OOM_HOST_GUARD` |
| 3 | `TIMEOUT` |
| 4 | `CANCELLED` |
| 5 | infrastructure: `LAUNCH_FAILED`, `LOST`, `STORAGE_FULL`, `REJECTED` |
| 6 | the reservation is no longer held (see "Reservations") |
| 64 | usage error |
| 69 | daemon unavailable |

A typical agent flow:

```sh
id=$(aq submit --memory 4G --cpus 2 --timeout 30m -- make test)
aq wait "$id"; code=$?
aq logs "$id" --tail 50
```

All viewing commands have `--json` for parsing by programs.

`aq top` (or `--json`) shows a single snapshot: memory/PSI/swap, the
non-overlapping physical categories agents/docker/tasks/other, and separately
the commitments of tasks/stands/budgets and the reasons the head is delayed.
These two views do not add up to each other. If "everything is slow", start with
this snapshot.

```sh
aq stats --since 7d
aq stats --since 30d --profile /path/to/repo:tests-unit --json
aq stats export --since 7d --format jsonl
aq stats export --since 7d --format csv --table host
```

The summaries cover task outcomes and waiting, profile accuracy, queue and host,
reservations, delivery, hook hits and OOM/PSI incidents. A profile filter
returns only the items and runs of that profile. Export tables are `items`,
`runs`, `host`, `incidents`, `events`, `hook`; CSV requires `--table`. Commands,
environment, cwd, session identifiers and delivery error texts are not included
in statistics or exports.

## Retention

Every 10 minutes and after recovery, the daemon deletes the logs of finished
items older than 7 days, including reservation teardown logs. The total limit
for `jobs/` is 5 GiB: when it is exceeded, the oldest finished items are
deleted. Active items, including `TEARDOWN_FAILED/UNKNOWN`, are kept. After
deletion `logs_pruned = true`, `aq logs` answers `removed by retention`, and
`aq result` does not show paths. Orphans with no DB row and inactive temporary
directories are removed after an hour.

Item history, their events/runs, incidents and rotated hook files are kept for
90 days. The host is measured every 30 seconds and kept for 30 days; for data
older than 2 days, the first row of each 10-minute bucket with a span of 600 s
remains.

New keys (a full example is in `deploy/config.example.toml`):

```toml
[profiles]
window = 10
memory_percentile = 90
duration_percentile = 90
limit_margin = 1.2
[scheduler]
backfill = true
[retention]
logs_max_age = "7d"
logs_max_total = "5G"
history_max_age = "90d"
samples_max_age = "30d"
samples_full_res = "2d"
[stats]
sample_interval = "30s"
agent_processes = ["claude", "codex"]
```

Config durations support days: `7d`, `1d12h`. History takes 1–100 runs,
percentiles are 50–100, limit margin is 1–4. Retention values must be positive,
`samples_full_res < samples_max_age`, the total log limit is at least 256 MiB,
the host interval is from 5 s to 5 min; process names are non-empty and contain
no `/`.

## Diagnostics: `aq doctor`

`aq doctor` checks cgroup v2, controller delegation, the presence of the slice,
that `MemoryMax` is really applied (a probe unit at 64M), the config, free
space, daemon availability, the systemd version and the runtime drop-in for
stopping tasks. Each error is marked with a code (`CGROUP_V2_MISSING`,
`CONTROLLER_MISSING`, `SLICE_MISSING`, `PROBE_FAILED`, `CONFIG_INVALID`,
`DISK_LOW`, `DAEMON_UNAVAILABLE`, `SYSTEMD_TOO_OLD`, `KILL_DROPIN_MISSING`) and
a hint.

## Changing the pool

Edit `~/.config/agent-queue/config.toml` (`pool_memory`, `pool_cpus`,
`reserve_floor_percent`, etc.; all keys are described in
`deploy/config.example.toml`) and restart the daemon:

```sh
systemctl --user restart aq.service
```

Running tasks are not interrupted when the daemon restarts: they are separate
systemd units, and the new daemon picks up their state and results. A waiting
`aq wait` reconnects for up to 10 minutes (it prints
`aq: lost the daemon (restart?); reconnecting` to stderr) and then reports the
result as usual. On a clean stop the daemon saves the run samples of running
tasks and the guard history, and restores them on start; after a crash they are
lost. Each restart is an operator event `daemon_restarted` (version, clean or
not, what was picked up), sent to the webhook if one is configured.

## OOM and other failures

If a task exceeds `--memory`, the kernel kills it inside its cgroup, and
`aq wait` exits with code 2. Details:

```sh
aq status <id>                    # OOM reason, memory peak
journalctl --user -u aq-<id>      # the task unit's journal (systemd: oom-kill, etc.)
aq logs <id> --stream stderr
```

`OOM_GROUP` means the task was killed by the pool limit (`aq.slice`);
`OOM_HOST_GUARD` means the host guard, which temporarily lowered the slice
ceiling because the machine was short of memory. Increase `--memory` or reduce
the command's parallelism.

## Cancellation

```sh
aq cancel <id> [--reason "..."]
```

A queued task is removed immediately. For a running task, the whole unit is
stopped: systemd sends SIGTERM, and after `cancel_grace` (default 10 s) SIGKILL
to all processes in the cgroup, including those that went into `setsid` and
daemonized descendants.

## Reservations

A reservation is needed when the memory is taken not by the command itself but
by something that outlives it or lives outside its cgroup: a compose stand
(database, broker, application services) or containers that tests start through
testcontainers. Containers run in the dockerd cgroup, the task's limits do not
apply to them, and without a reservation the pool does not know about them. A
reservation takes the stand's memory (and CPU, if needed) in the pool in advance
for a given time, along with a budget for the tasks that will work with the
stand.

```sh
cat > teardown.sh <<'EOF'
docker compose -p myapp-e2e down -v --remove-orphans
EOF

out=$(aq reserve --memory 6G --cpus 2 --task-memory 4G --task-cpus 4 \
  --ttl 2h --teardown teardown.sh --docker-project myapp-e2e)
# out: "<id> GRANTED" or "<id> QUEUED position=… blocked_by=…"
id=${out%% *}
aq wait "$id"                      # waits for the grant; code 0 = GRANTED/EXPIRING
docker compose -p myapp-e2e up -d  # bring the stand up only after the grant
```

- `--memory` (required): the stand's memory; `--cpus`: the stand's CPU
  (default 0).
- `--task-memory` and `--task-cpus`: the budget for nested tasks
  (`submit --within`); they are set only together: minimum `64M` and 1 CPU.
- `--ttl` (required): the reservation lifetime, counted from the grant; no more
  than `max_ttl` (24 h).
- `--teardown` (required): the teardown script file (up to 64 KiB).
- `--docker-project`: the stand's compose project. Every 5 s the daemon
  measures, through `/var/run/docker.sock`, the memory of containers labeled
  `com.docker.compose.project=<project>` and counts as not yet used only the
  difference between the declared and the measured amount. Without
  `--docker-project`, the stand's whole `--memory` counts as unused for the
  entire life of the reservation: this is safe, but memory is underused. One
  project can belong to only one unfinished reservation (until it is
  `TORN_DOWN`, `RELEASED` or `CANCELLED`). If the stand exceeded the declared
  amount, a notification is recorded.
- `--name`, `--key`, `--session`, `--priority`, `--env K=V`, `--json`: as for
  `submit`.

The stand and the task budget together occupy the pool until the reservation is
released. A reservation is queued the same way as a task, and `aq reserve`
answers after the nearest admission pass: `GRANTED` right away if it fit,
otherwise `QUEUED` with the position and the reason for waiting.

### The teardown script

On `reserve`, the client sends the contents of the script, and the daemon keeps
a copy in `jobs/<id>/teardown.sh`. When the lifetime ends, the daemon runs
`bash <copy>` as a separate unit `aq-teardown-<id>.service` in the slice
`aqtd.slice`: outside the pool, with the limits `teardown_memory`, 1 CPU and
`teardown_timeout` (default 5 min). The script must be:

- **self-contained**: it must not depend on files in the working directory,
  because the worktree may be deleted by the time of teardown. The script's cwd
  is the reservation's original directory if it still exists, otherwise the
  empty service directory `jobs/<id>/teardown-<n>/cwd`; the original path is
  passed in `AQ_ORIGINAL_CWD`, and the reservation id in `AQ_RESERVATION_ID`;
- **idempotent**: running it again is safe: `docker compose -p <project>
  down`, not "delete what I created".

The script's environment is captured at `reserve`: the same as for `submit`,
plus `DOCKER_HOST`, `DOCKER_CONTEXT`, `DOCKER_CONFIG` and `--env`.

### Tasks on a stand: `submit --within`

pytest against a stand, like tests that start containers themselves through
testcontainers, run inside a reservation:

```sh
aq submit --within "$id" --memory 3G --cpus 4 --timeout 30m -- \
  bash -lc 'pytest -n "$AQ_CPUS" tests/e2e'
```

A nested task does not go through admission to the pool again and does not wait
in the queue: it starts immediately, within the reservation's budget. If the sum
of `expected` of the unfinished nested tasks together with the new one exceeds
`--task-memory`, or the sum of their CPUs exceeds `--task-cpus`, `submit`
refuses at once (`REJECTED`, "task budget", code 5). This way two agents that
each got a reservation do not block each other. Containers started by the task
itself live in the dockerd cgroup, not the task's: include their memory in the
reservation's `--memory` (the `--docker-project` measurement sees only the
containers of the compose project), and have the teardown script remove them
too.

### Extending and releasing

```sh
aq extend "$id" --ttl 1h   # expiry will be no earlier than 1 h from now
aq release "$id"           # the agent shut the stand down itself: release without the script
```

`extend` does not add to the lifetime; it sets "no earlier than now + D", so
repeating it is safe; you can extend in `GRANTED` and `EXPIRING`. `release`
releases a granted reservation if it has no unfinished nested tasks, and cancels
a queued reservation (`CANCELLED`). `aq cancel` on a reservation works only
while it is queued.

### Expiry and failed teardown

- `expiry_warning` (15 min) before the end of the lifetime, the reservation
  moves to `EXPIRING`, and a warning is recorded: "extend or it will be torn
  down".
- When the lifetime ends, the reservation moves to `TEARING_DOWN` and the
  teardown script runs (no more than `teardown_concurrency` at a time; the rest
  wait for a slot in `EXPIRING`). Exit code 0 → `TORN_DOWN`, the memory returns
  to the pool.
- If nested tasks are still running in the reservation, it is not torn down: the
  lifetime is pushed to their deadline (`started_at + timeout + 2 ×
  cancel_grace`), `aq status` shows `ttl held: yes`, and new `--within`
  submissions are rejected. As soon as the last nested task has finished, the
  reservation is torn down immediately, without waiting for the pushed lifetime.
- A nonzero exit code, timeout or OOM of the script → `TEARDOWN_FAILED`; the
  teardown unit disappeared without a result → `TEARDOWN_UNKNOWN`. In both cases
  the reservation **stays accounted in the pool**, and there are no automatic
  retries. Sort out the stand (`aq logs <id> --stream stderr` shows the logs of
  the last attempt; all attempts are in `jobs/<id>/teardown-<n>/`) and choose:

```sh
aq teardown --retry "$id"  # run the teardown script again
aq release --force "$id"   # the stand was removed by hand: release without the script
```

A crash or restart of the daemon does not interrupt teardown: the unit runs on
its own, and the new daemon picks up its result.

`aq wait` codes for a reservation (returned as soon as the reservation leaves
the queue):

| Code | Meaning |
|---|---|
| 0 | `GRANTED`, `EXPIRING`: the reservation is granted |
| 4 | `CANCELLED` |
| 5 | `REJECTED` |
| 6 | the reservation is no longer held: `RELEASED`, `TEARING_DOWN`, `TORN_DOWN`, `TEARDOWN_FAILED`, `TEARDOWN_UNKNOWN` |

Notifications about a grant from the queue, expiry and exceeding the declared
amount go to the owner; expiry, holds by tasks, the outcome and forced teardown
go to the operator (see below).

## Notifications

The client detects the session through a configured adapter with a non-empty
`identity_env`: Codex (`CODEX_THREAD_ID`, push via `codex queue`) or Claude
(`CLAUDE_CODE_SESSION_ID`, client-wait via a background `aq wait`). If the
variables of several adapters are non-empty (the session was started from
another agent's shell and inherited its variable), aq does not guess:
submit/reserve exit with code 64, and MCP requires explicit `client` and
`session_id`. `--client NAME --session S` set them explicitly; `--no-notify` on
submit/reserve turns off only session notifications. Without a detected session
the CLI works as `cli`. A push adapter that is named but has no session is an
error; a delivery command missing from the daemon's PATH rejects the request
before it is saved. The service PATH includes
`%h/.local/bin:/usr/local/bin:/usr/bin:/bin`.

A new push client is added without changing the core:

```toml
[[agents]]
name = "pi"
identity_env = "PI_SESSION"
delivery = "push"
command = ["pi", "send", "{session}", "{text}"]
```

If there is at least one `[[agents]]`, the list **replaces** the defaults
entirely. The command is argv with no shell; `{event_id}`, `{item_id}`, `{kind}`
are also supported. `delivery = "client-wait"` requires an empty command.

After submit/reserve, Claude starts `aq wait ID` with `run_in_background` and
ends its turn; Codex gets a ping automatically. `aq wait RES_ID --until
expiring` waits for the warning about expiry or about the stand exceeding its
declared amount. If it returned with `GRANTED`/`stand_over_declared`, handle and
acknowledge the event, then start this waiter again in the background for the
future expiry.

```sh
aq list --mine             # own active items + finished ones with unacknowledged events
aq list --session SESSION  # explicit filter
aq result ITEM_ID
aq ack EVENT_ID            # after the result is written to a checkpoint
```

An event is saved atomically together with the state change. Push/webhook are
retried with backoff and jitter until delivery succeeds (sent); sent without an
ack does not wake the session again. A daemon crash can produce a duplicate with
the same event_id: check the receipt and skip events that were already handled.
An ack before the event is picked up cancels the delivery; after pickup, a ping
may still arrive. `aq wait` shows the delivered event IDs; an interrupted waiter
can be started again.

Operator alerts are enabled through the config:

```toml
[webhook]
url = "https://hooks.example.org/aq"
token_file = "/absolute/path/to/token"
timeout = "10s"
```

A POST with `Content-Type: application/json`, `Idempotency-Key: EVENT_ID`, and
with a token, `Authorization: Bearer TOKEN`. The token is re-read on every
attempt. The body contains `event_id, kind, audience, item_id, item_kind, item_name, state,
reason, project, session_id, client_type, detail, text, created_at` (RFC3339).
Notifications contain no env values, teardown script or logs; the text is
limited to 8 KiB. Without a URL, operator events are saved as skipped. After
`give_up_after`, a failed delivery becomes failed; a session delivery failure
creates `delivery_failed` for the operator, an operator delivery failure is only
logged. Thresholds and timeouts are in `[notify]` of the example config.
`aq doctor` shows `ADAPTER_UNAVAILABLE`, `WEBHOOK_FAILING`, and also a disabled
webhook.

To get the alerts in Telegram, run [Gong](https://github.com/creatorofuniverses/gong)
with its own bot and set `format = "gong"`: aq then sends Gong's `/notify`
request instead of its own JSON — an HTML message (kind, item, state, detail and
`aq result ID`) led by a status mark (✅ done, ❌ failed, ⚠️ needs a look,
⏸️ admission closed, ⏳ expiring or held, ❓ unknown, 🔄 daemon restarted),
`level` by severity (`error` for failed teardowns and
infrastructure failures, `warning` for closed admission, forced releases and
oversized stands) and the event kind as `category`. Put Gong's `api_token` in
`token_file`.

```toml
[webhook]
url = "http://127.0.0.1:8082/notify"
token_file = "/home/USER/.config/agent-queue/gong-token"
format = "gong"
```

## MCP

The same binary works as a stdio MCP server; the daemon must be started
separately. Connection examples (run them yourself):

```sh
claude mcp add --transport stdio aq -- /home/USER/.local/bin/aq mcp
```

In the Codex config:

```toml
[mcp_servers.aq]
command = "/home/USER/.local/bin/aq"
args = ["mcp"]
env_vars = ["XDG_RUNTIME_DIR"]
```

Or set `[mcp_servers.aq.env] AQ_RUNTIME_DIR = "/run/user/1000/agent-queue"`
(for your own UID); with non-standard paths, also AQ_STATE_DIR and AQ_CONFIG.
Codex 0.164 filters the MCP environment: the session is not passed automatically.
For `queue_submit`/`queue_reserve`, pass the **client and session_id** of the
current session explicitly, or deliberately `no_notify: true`. An unknown
identity is an error, not a silent disabling of notifications. If the launcher
passed the correct session variable in the MCP env explicitly, aq will detect
it.

Tools: queue_submit, queue_reserve, queue_extend, queue_release,
queue_cancel, queue_status, queue_result, queue_list, queue_ack, queue_logs, queue_estimate.
`cwd` and `teardown_file` are absolute; queue_logs: tail 1..2000 (default 200),
no follow. MCP does not wake an idle session: Claude still starts
`aq wait ID` in the background after submitting through MCP; Codex gets the
daemon push.

## Hook

`aq hook pretooluse` gently reminds about a heavy command run outside aq:
pytest with `-n`, compose up/run, docker run. It does not contact the daemon,
does not block, and stays silent on error. Patterns are in `[hook].patterns`.
The hook-hits.jsonl counter records only the time, the pattern index and the
client, without the command text; it rotates after 1 MiB.

Installation does not change hook settings. Merge the fragment you need,
[Claude](../deploy/hooks/claude-settings.json) or [Codex](../deploy/hooks/codex-hooks.json),
into the client's settings by hand (do not overwrite existing hooks). The
matcher is `Bash` in both; Codex requires you to trust the new definition
through `/hooks`.
The aq skill ([Claude](../deploy/skills/claude/aq/SKILL.md), [Codex](../deploy/skills/codex/aq/SKILL.md))
describes how an agent should work with aq: what counts as heavy, how to
estimate memory, when to `submit` and when to `reserve` with a stand, how to get
the result and clean up after itself.

## Recovery

- If the daemon crashed or was restarted, tasks keep running on their own; at
  startup the daemon reconciles the DB with systemd and collects the results of
  tasks that finished (the task unit's `ExecStopPost` writes them to
  `jobs/<id>/result.json`). A task whose unit disappeared without a result gets
  `LOST`.
- `systemd` restarts a crashed daemon by itself (`Restart=on-failure`).
- `aq wait` survives a restart: it reconnects and reports the result. Operator
  alerts get a `daemon_restarted` event saying whether the stop was clean.
- While the daemon is recovering, new tasks do not start.

## Limitations of the current version

- The death of a reservation's owner session is not detected; there is no
  automatic teardown on that basis.
- Estimates do not guarantee a start time; per-second CPU samples of tasks are
  not collected yet.
- Linux with systemd and cgroup v2 only, user mode only.

## Development

```sh
just build              # bin/aq
just test               # go test -race ./...
just test-integration   # go test -tags integration -p 1 ./... (real systemd)
just test-install       # install/uninstall recipes into a temporary directory
```

The integration and e2e tests (`test/e2e/`) start real units in separate slices
`aqtest.slice` and `aqe2e.slice` with a 1 GiB pool (reservation teardowns go in
`aqtesttd.slice` and `aqe2etd.slice`) and stop them afterwards; the installed
`aq.service` is not needed for them.

## Further reading

- [docs/architecture.md](architecture.md) — how aq works and why: execution
  model, admission and backfill, reservations, profiles, notifications,
  recovery, design decisions and known limitations.
- [CONTRIBUTING.md](../CONTRIBUTING.md) — development setup and change guidelines.

## License

[MIT](../LICENSE)
