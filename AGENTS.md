# AGENTS.md

Instructions for coding agents (Codex, Claude Code and others) working on this
repository. Humans: see [CONTRIBUTING.md](CONTRIBUTING.md); the rules are the same.

## What this is

`aq` is a local resource dispatcher for Linux: one Go binary that is both a
daemon (a systemd user service) and its CLI. It queues heavy commands from
several agents against a shared memory/CPU pool and runs each in its own
systemd transient unit with cgroup v2 limits. Read
[docs/architecture.md](docs/architecture.md) before changing admission,
scheduling, reservations or recovery; [docs/guide.md](docs/guide.md) is the
user-facing behaviour you must keep true.

## Layout

- `cmd/aq` — entry point; `internal/cli` — commands, help (`help.go`), TTY
  rendering (`pretty.go`, `style.go`).
- `internal/daemon` — socket handlers, background loops (samples, incidents,
  retention, guard); `internal/scheduler` — the state machine and admission
  passes; `internal/admission` — pure admission, forecast and backfill logic.
- `internal/store` — SQLite (pure Go) and numbered migrations in
  `internal/store/migrations/`.
- `internal/platform` — interfaces; `platform/linux` — systemd/cgroup/proc;
  `platform/fake` — in-memory fakes for unit tests.
- `internal/notify`, `internal/mcp`, `internal/hook`, `internal/profile`,
  `internal/stats`, `internal/client`, `internal/protocol`, `internal/runner`.
- `deploy/` — systemd units, example config, opt-in hooks, and the agent
  skills `deploy/skills/claude/aq` and `deploy/skills/codex/aq`.
- `test/e2e` — end-to-end tests against the real systemd user manager.

## Commands

```sh
just build              # bin/aq
just test               # go test -race ./...  (unit tests, fakes only)
just test-install       # install/uninstall recipes into a temporary tree
just test-integration   # integration + e2e: real systemd user manager, docker
gofmt -l . ; go vet ./...
```

Before you finish a change: `gofmt` clean, `go vet ./...` clean, `just test`
green. Run `just test-integration` when you touch the executor
(`platform/linux`), the daemon lifecycle, recovery or the store.

## Hard rules

- **Never touch a live installation.** Do not run `just install`,
  `just uninstall`, `just purge`, `systemctl --user … aq.service`, and do not
  edit `~/.config/agent-queue/`, `~/.local/state/agent-queue/`,
  `~/.config/systemd/user/aq.*` or `~/.local/bin/aq`. Other agents on the
  machine may depend on the running daemon; a restart cuts their `aq wait`
  waiters. Verify install changes with `just test-install`.
- **Do not run `TestE2ERealCodexQueue`** (leave `AQ_E2E_CODEX_THREAD` unset): it
  sends a real message into a Codex thread.
- **No new dependencies** without an explicit request: the module has three
  direct ones and must build with `CGO_ENABLED=0`.
- **Scratch files** go under the ignored `bin/scratch/`, never as `.go` files
  (they break `go vet ./...`).
- **Local records stay local.** `docs/codex/`, `docs/superpowers/`,
  `docs/notes/` and `.private-history/` are ignored on purpose; never commit or
  link them from public files.
- Do not push, tag or open pull requests unless asked.

## Contracts to keep

- **Agent-facing output.** The plain (non-TTY) text of `aq submit`, `aq wait`,
  `aq result`, `aq list`, `aq reserve` and all `--json` output are read by
  agents and skills. Change them only deliberately, together with the skills and
  docs. Human styling belongs in the TTY renderers, behind `ui.pretty`.
- **Exit codes** of `aq wait` (`internal/cli/exitcodes.go`) are documented in the
  skills, `aq help wait` and the guide.
- **Socket protocol and MCP tool schemas** are used by other processes; keep them
  backward compatible.
- **Database.** Schema changes are new migrations; never edit an applied one.
  Old databases must open.

## How to work here

- Tests first: a behaviour change comes with a test that fails without it.
  Prefer testing pure logic (`admission.PlanPass`, `admission.Schedule`,
  `profile.Compute`, `stats.Summarize`) over the daemon; daemon and CLI tests use
  the fakes in `internal/platform/fake`.
- Match the surrounding code: short doc comments that explain why, existing
  naming and error style, no speculative abstractions.
- When behaviour changes, update in the same change: `docs/guide.md` and
  `docs/guide.ru.md`, `README.md`/`README.ru.md` if the summary changes,
  `docs/architecture.md` for design changes, `aq help` text in
  `internal/cli/help.go`, and both agent skills in `deploy/skills/`.
- Commit messages: imperative subject with a type prefix (`feat:`, `fix:`,
  `docs:`, `refactor:`, `build:`), body explaining why.

## Running heavy work on this machine

If `aq` is installed here, other agents share this host's memory through it.
Use it for your own heavy commands outside this repository's test suite — follow
the `aq` skill. The repository's own `just test-integration` creates its own
test slices and must run directly (it needs the user manager's environment).
