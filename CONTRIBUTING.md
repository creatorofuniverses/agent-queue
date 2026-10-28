# Contributing to aq

Thanks for your interest. aq is small and opinionated: it does one job (keep a
shared Linux host responsive while many agents run heavy work on it) and leans
on systemd and cgroup v2 instead of reimplementing them. Read
[docs/architecture.md](docs/architecture.md) before a non-trivial change; it
explains the admission model and the decisions behind it.

## Setup

- Linux with systemd (user manager) and the unified cgroup v2 hierarchy, with
  the memory and cpu controllers delegated to the user (`aq doctor` checks this).
- Go 1.26 and [just](https://github.com/casey/just).
- Docker, only for the integration tests that exercise reservations.

```sh
just build              # bin/aq
just test               # unit tests: go test -race ./...
just test-install       # install/uninstall recipes into a temporary tree
just test-integration   # integration and e2e tests against the real systemd user manager
```

Unit tests use in-memory fakes of systemd, the host and the clock
(`internal/platform/fake`) and must not touch the machine. Integration and e2e
tests (build tag `integration`) start real transient units under a temporary
state directory and slice; they clean up after themselves. The e2e harness
removes agent session variables from the daemon's environment, so a test never
notifies the session that runs it. `TestE2ERealCodexQueue` runs only when
`AQ_E2E_CODEX_THREAD` names a throwaway Codex thread.

Never run `just install`, `just uninstall` or `just purge` to test a change on a
machine whose aq is in use: they restart or stop the live daemon. Use
`just test-install`, or point every destination at a temporary directory
(see the variables at the top of the [justfile](justfile)).

## Change guidelines

- **Tests first.** A behaviour change comes with a test that fails without it.
  Scheduler and admission logic is pure where possible (`admission.PlanPass`,
  `admission.Schedule`); test it there rather than through the daemon.
- **Before sending:** `gofmt`, `go vet ./...` and `just test` are clean; run
  `just test-integration` when you touch the executor, the daemon lifecycle or
  the store.
- **Dependencies.** The module has three direct dependencies (TOML, x/sys and a
  pure-Go SQLite). Adding one needs a strong reason: aq is a single static binary
  that must build without cgo.
- **Agent-facing output is a contract.** Agents read the plain text of
  `aq wait`, `aq result`, `aq list` and `aq submit` (non-TTY stdout) and the
  `--json` output. Do not change those formats casually; human-only styling goes
  in the TTY renderers (`internal/cli/pretty.go`). The agent skills in
  `deploy/skills/` describe the workflow; update them with any change an agent
  would notice.
- **Database.** Schema changes are new numbered migrations in
  `internal/store/migrations/`; never edit an applied one.
- **Comments and naming** follow the surrounding code: short doc comments that
  explain why, not what.

## Reporting bugs

Include `aq version`, `aq doctor --json`, the command you ran, and for a task
`aq result ID --json` plus the relevant part of `aq logs ID`. For scheduling
questions `aq top --json` taken at the time helps. Do not paste task
environments or logs that may contain secrets.

## License

By contributing you agree that your contributions are licensed under the
[MIT License](LICENSE).
