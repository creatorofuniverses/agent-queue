---
name: aq
description: Use before running heavy work on this host — full or parallel test suites (pytest -n, go test ./..., e2e), docker compose stands and testcontainers, big builds, an experimental stand you will keep up — to size it, queue it with a memory budget, and get woken when it ends instead of hanging the machine.
---

# Running heavy work through aq

This host runs several agents at once. A heavy command started directly competes
for memory with everyone else and can freeze the whole machine. `aq` queues it
with a memory limit and starts it when the memory is really free. You lose a few
seconds of queueing at worst; an OOM-frozen host loses everyone's work.

## 1. Is it heavy?

Run through aq anything likely to take **more than ~1 GiB or more than a few
minutes**, or anything that starts containers:

| Through aq | Directly |
|---|---|
| the full test suite, `pytest -n`, `go test ./...` of a big repo, e2e | one test file, a single focused test |
| `docker compose up`, testcontainers, a stand for manual checks | `docker ps`, `docker logs`, compose `config` |
| big builds: webpack/vite production, cargo/go of large trees, image builds | lint, format, `git`, small scripts |

The PreToolUse hook hints "aq: this command looks heavy" — take it seriously,
but it is a hint: ignore it for light commands (it also fires on quoted text).

## 2. Size it before you run it

1. **Known workload?** Give repeated work a stable `--name` (`tests`, `e2e`,
   `build`). Check history: `aq profile show tests`. With usable history you may
   omit `--memory`; aq takes the limit from past peaks.
2. **New workload?** Pick `--memory` from what you know: about 300–800M per
   pytest worker, the sum of `docker stats` for a stand, 2× the last observed
   peak if you have one. Too low → OOM (exit 2) and you rerun with more; too high
   → you wait longer. Never blindly retry an OOM with the same limit.
3. **Timeout:** about 2× the expected duration. It is a kill switch, not a
   forecast.
4. **Will it start soon?** `aq estimate --name tests --cpus 4 --timeout 30m`
   (or `--memory 6G`). If the queue is long, do other work first, or ask the
   user whether to wait. `aq top` shows who uses host memory right now.

## 3. Pick the shape

**One command** (tests, a build): `aq submit`.

```sh
aq submit --name tests --memory 4G --cpus 4 --timeout 30m -- \
  bash -lc 'pytest -n "$AQ_CPUS" tests'
```

Pass argv after `--`; quote a shell (`bash -lc '…'`) when you need `$AQ_CPUS`,
pipes or `&&`. The task runs in the submit cwd.

**Tests that need containers or a stand** (testcontainers, compose, e2e): the
containers live outside the task's cgroup, so reserve the stand memory *and* a
budget for the tests, then run the tests inside the reservation:

```sh
aq reserve --name e2e-stand --memory 3G --task-memory 4G --task-cpus 4 --ttl 1h \
  --docker-project myapp-e2e --teardown /abs/path/stand-down.sh
# aq wait RES_ID in background → GRANTED; only then bring the stand up
docker compose -p myapp-e2e up -d
aq submit --within RES_ID --name e2e --memory 4G --cpus 4 --timeout 30m -- \
  bash -lc 'pytest -n "$AQ_CPUS" tests/e2e'
```

**An experimental stand you keep up** (manual checks, a demo, a long debugging
session): `aq reserve` with a realistic `--ttl`, no task budget unless you will
run tests in it. `aq extend RES_ID --ttl 1h` while you still need it;
`aq release RES_ID` after you shut it down.

The teardown script must be idempotent and self-contained (it may run after the
worktree is gone): embed the compose definition instead of relying on cwd. Use
`--docker-project` so aq measures the stand and frees the unused part of it.

## 4. Get woken, do not poll

After `submit`/`reserve`, start `aq wait ID` with **`run_in_background`**. Its
completion wakes this session. Write a receipt into your notes or todo: item ID,
what it runs, why, and what to do with the result. Continue independent work; if
nothing else is left, end the turn — do not sleep or poll `aq status`.

For reservations also start `aq wait RES_ID --until expiring` in background. It
returns before the TTL runs out (extend or release then) or early with a
`stand_over_declared` warning: handle it, `aq ack EVENT_ID`, and **start the
expiry waiter again**. After every `aq extend`, start it again too.

## 5. Read the result

`aq wait` prints one line and exits with the outcome:
0 ok / granted / expiry warning · 1 the command failed · 2 OOM · 3 timeout ·
4 cancelled · 5 infrastructure or rejected · 6 reservation no longer held ·
64 usage · 69 daemon down · 130 interrupted.

Then `aq result ID` and `aq logs ID --tail 100` (`--stream stderr` for errors).
On exit 2 resubmit with a higher `--memory` (say 1.5×) and tell the user; on 3
check whether it hung before raising `--timeout`; on 5 read the diagnostic
(rejected: the request can never fit the pool). Ack the item's event with
`aq ack EVENT_ID` once the outcome is recorded.

## 6. Clean up before you finish

`aq list --mine` shows your items and unacknowledged events. Shut your stands
down and `aq release` their reservations; cancel queued work you no longer need
(`aq cancel ID`). An abandoned reservation holds memory from everyone until its
TTL runs out.

## Recipes

- **End of a branch: full verification.** Run the full suite (and e2e) as named
  aq tasks, in background, then do the review or write-up while it runs. With
  history, `aq submit --name tests --timeout 40m -- …` needs no `--memory`.
- **Several suites.** Submit them as separate named tasks; aq starts each when
  it fits and may backfill short ones ahead of a big one.
- **Repeat after a fix.** Use the same `--name` so the profile keeps learning.
- **Retrying a submit after an error.** Add `--key some-stable-key`: the same
  request returns the same task instead of a duplicate.

## When aq itself is in the way

- `aq: daemon is not running` (69): tell the user; do not silently run the heavy
  command directly.
- A sandbox cannot reach the socket: ask for approval to run the `aq` command on
  the host.
- `session identity unknown` / exit 64 about clients: you were started from
  another agent's shell; pass `--client claude --session "$CLAUDE_CODE_SESSION_ID"`.
- MCP tools (`queue_submit`, `queue_estimate`, …) do the same as the CLI, but
  cannot wake you: still run `aq wait ID` in background.

Details (sizes, TTL rules, MCP fields, events, operator recovery, profiles,
stats): [the reference](references/aq-reference.md). `aq help CMD` lists every
flag with examples.
