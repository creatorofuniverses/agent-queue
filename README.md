English · [Русский](README.ru.md)

# aq — give heavy tasks room to run

![Agents and you send heavy commands to aq; it queues them and runs them on one Linux machine within its memory and CPU](docs/assets/aq-header.png)

Running tests in one terminal, building in another, and letting a couple of coding
agents work alongside you? They all share the same RAM and CPUs. Starting every
heavy command at once can turn a useful workstation into a machine that barely responds.

**aq gives those commands a shared queue and resource limits.** Submit a job with
a memory budget, a CPU allocation, and a timeout. aq starts it when resources are
available, saves its output, and keeps its result for you to inspect.

It is a local Linux utility for people and coding agents working under the same
user account. One binary provides the CLI, background service, and MCP server.

## What you get

- **A shared queue:** tests and builds wait when the pool is full.
- **Limits on each job:** systemd and cgroup v2 enforce memory, CPU quota, and runtime.
- **Less repeated guesswork:** named jobs can learn memory and duration estimates from previous runs.
- **Room for containers:** reservations account for Docker environments and their test jobs together.
- **Results you can come back to:** logs, exit reasons, memory peaks, and queue statistics.
- **Gentler on your SSD:** each job's temp files live in RAM and count toward its memory, and each job records how much it wrote to disk, so a suite that wears the drive shows up in `aq stats`.
- **Agent integration:** bundled Claude Code and Codex skills, optional reminder hooks, and completion delivery.

Tasks can run concurrently when they fit. aq helps contain resource exhaustion;
it does not guarantee that a machine can never run out of memory. Containers
started through Docker live outside a task's limits: use a reservation for them.

## Start here

You need **Linux, systemd 254 or newer, and cgroup v2** with the `memory`, `cpu`,
and `pids` controllers delegated to your user. Building requires **Go 1.26** and
[just](https://github.com/casey/just). There is no macOS or Windows backend.

From a checkout of this repository, prepare your configuration:

```sh
mkdir -p ~/.config/agent-queue
# Copy only if you do not already have a config.
test -e ~/.config/agent-queue/config.toml || \
  cp deploy/config.example.toml ~/.config/agent-queue/config.toml
```

Edit that file before installing. Set `pool_memory` and `pool_cpus` to suit your
machine: the defaults are **16 GiB and 16 CPUs**. Memory must leave at least the
configured host reserve (10% by default), and CPUs cannot exceed the machine's
CPU count. Choose a smaller pool if you want more room for other work.

Optional: delegate the `io` controller to your user as well, and aq records how
much each job writes to disk ([how](docs/guide.md#disk-writes)). Without it,
jobs run normally and the disk figures are simply absent.

```sh
just install
export PATH="$HOME/.local/bin:$PATH"
aq doctor
```

Installation starts a user service and installs the agent skills. It preserves
existing configuration. Hooks are opt-in. For custom paths, upgrades, logout
behavior, or removal, see the [installation guide](docs/guide.md#installation).

Try a small job first:

```sh
id=$(aq submit --memory 64M --cpus 1 --timeout 1m -- /usr/bin/printf 'Hello from aq!\n')
aq wait "$id"
aq logs "$id"
```

`submit` returns an ID immediately. `wait` waits for the result and exits with
code 0 on success. `logs` shows the saved output. Then try your project's build
from its working directory, choosing limits appropriate for it:

```sh
id=$(aq submit --name build --memory 4G --cpus 2 --timeout 30m -- make -j2)
aq status "$id"
aq wait "$id"
aq logs "$id" --tail 50
```

The command runs without an implicit shell. CPU quota limits CPU time; it does
not choose your build's worker count. Set that explicitly, as with `-j2` above.
For shell expansion and `$AQ_CPUS`, see [running tasks](docs/guide.md#running-tasks).

## Everyday commands

| What you want to do | Command |
|---|---|
| See active work and why it is waiting | `aq list` |
| Inspect one job | `aq status ID` |
| Read its output | `aq logs ID --tail 50` |
| Stop a job | `aq cancel ID` |
| See a snapshot of the machine and pool | `aq top` |
| Summarize the last week | `aq stats --since 7d` |
| Find the jobs that write the most to disk | `aq stats --since 1d` ("Disk writes") |
| Learn a command's options | `aq help COMMAND` |

Use `--json` on viewing commands for structured output. If a job fails, start
with `aq status ID` and `aq logs ID --stream stderr`; the guide explains
[exit codes](docs/guide.md#watching-and-waiting) and [memory failures](docs/guide.md#oom-and-other-failures).

## Using aq with coding agents

`just install` installs separate skills for Claude Code and Codex. The skills
explain when to queue a job, when to reserve resources for a container environment,
and how to collect results. Optional hooks remind an agent when it starts heavy
work outside aq; they do not block commands.

Follow the [notification setup](docs/guide.md#notifications) for completion delivery,
and the [MCP](docs/guide.md#mcp) and [hook](docs/guide.md#hook) sections if you use them.
A daemon restart preserves running jobs, and `aq wait` reconnects on its own.

## Why aq cares about your SSD

On my workstation, a dozen coding agents running full test suites
around the clock wrote **1.2 TB to the NVMe drive in five hours**. The drive
reported 16% wear after 28 TB written: at that rate it would have reached its
rated life in about a month.

The data itself was small. What wore the drive were **small forced writes**:
every `fsync` and every file created or deleted makes the filesystem write data
and journal blocks right away, instead of batching them in memory. Test suites
are full of both: test databases that commit with `fsync`, a database reset
before every test, and `pytest` temp directories with tens of thousands of
files per run. One person running tests a few times a day never notices this.
Many agents re-running everything in parallel turn a desktop into a CI farm on
consumer hardware.

So aq now:

- gives each job a private temp directory in RAM (`/tmp`, when it is tmpfs),
  where `fsync` costs nothing, and deletes it when the job ends. The pages count
  toward the job's `--memory`, and a full `/tmp` holds the queue instead of
  failing tests ([details](docs/guide.md#temp-files-live-in-ram-and-count-as-task-memory));
- records the bytes each job wrote to disk and lists the heaviest writers in
  `aq stats` ([details](docs/guide.md#disk-writes)).

If you run many agents on one machine, also: keep `TMPDIR` and pytest
`--basetemp` in tmpfs rather than moving them to disk when `/tmp` fills up
(raise the tmpfs inode limit instead); run test databases with `fsync=off`
(PostgreSQL) or `PRAGMA synchronous=OFF` (SQLite); and check your drive's wear
now and then with `smartctl -A`.

## Results on a real workstation

**To be added — measurements have not been collected yet.**

This section will show the workload, machine specifications, aq configuration,
and measurement period alongside the results. No performance claim is implied.

| Measurement | Result |
|---|---|
| Workload, task count, and concurrent agents | To be added |
| Completed, failed, and cancelled tasks | To be added |
| Queue wait and total completion time | To be added |
| Peak memory and host memory-pressure incidents | To be added |
| Comparable run without aq | To be added |

`aq stats --since 7d` and `aq stats export --since 7d --format jsonl` provide
recorded data for the write-up. A comparison without aq needs a separate run.

## Read more

- [User guide](docs/guide.md): installation, profiles, reservations, notifications, troubleshooting, and retention.
- [Example configuration](deploy/config.example.toml): settings and their defaults.
- [Architecture](docs/architecture.md): scheduling, isolation, recovery, and known limitations.
- [Contributing](CONTRIBUTING.md): development setup, tests, and bug reports.

Contributions and documentation fixes are welcome. Licensed under [MIT](LICENSE).
