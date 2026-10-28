set shell := ["bash", "-euo", "pipefail", "-c"]

# Destinations are overridable through the environment, e.g.
# `BINDIR=/tmp/x SYSTEMCTL=true just install` only lays the files out.
go := env("GO", "go")
bindir := env("BINDIR", home_directory() / ".local/bin")
unitdir := env("UNITDIR", home_directory() / ".config/systemd/user")
confdir := env("CONFDIR", home_directory() / ".config/agent-queue")
statedir := env("STATEDIR", home_directory() / ".local/state/agent-queue")
systemctl := env("SYSTEMCTL", "systemctl --user")
loginctl := env("LOGINCTL", "loginctl")

# Each agent gets its own copy of the aq skill: deploy/skills/<agent>/aq.
# An empty value skips that agent.
claude_skills := env("CLAUDE_SKILLS", home_directory() / ".claude/skills")
codex_skills := env("CODEX_SKILLS", home_directory() / ".codex/skills")

# FORCE=1 restarts or stops the daemon even with a non-empty queue.
force := env("FORCE", "")

[private]
default:
    @just --list

# bin/aq
build:
    {{ go }} build -o bin/aq ./cmd/aq

# go test -race ./...
test:
    {{ go }} test -race ./...

# integration and e2e tests against the real systemd user manager
test-integration:
    {{ go }} test -tags integration -p 1 ./...

# install/uninstall recipes into a temporary tree
test-install:
    bash test/install_test.sh

# Warns before restarting or stopping a daemon that still has work: tasks
# survive a restart, but agents' background `aq wait` waiters are cut off.
[private]
check-idle action:
    #!/usr/bin/env bash
    set -euo pipefail
    aq="{{ bindir }}/aq"
    [ -x "$aq" ] || exit 0
    out=$("$aq" list 2>/dev/null) || exit 0 # daemon not running: nothing to lose
    n=$(($(printf '%s\n' "$out" | wc -l) - 1))
    [ "$n" -gt 0 ] || exit 0
    echo "warning: aq still has $n queued or running item(s):" >&2
    printf '%s\n' "$out" >&2
    echo "{{ action }} the daemon now: tasks keep running and are picked up again," >&2
    echo "but agents' background 'aq wait' waiters are cut off and miss the result." >&2
    if [ -n "{{ force }}" ]; then
        echo "FORCE is set: continuing." >&2
        exit 0
    fi
    if [ -t 0 ]; then
        read -r -p "Continue anyway? [y/N] " answer
        case "$answer" in [yY]*) exit 0 ;; esac
    else
        echo "Not a terminal: rerun with FORCE=1 to continue." >&2
    fi
    exit 1

# build and install the binary, user units, config, skills; (re)start aq.service
install: build (check-idle "restarting")
    #!/usr/bin/env bash
    set -euo pipefail
    install -d "{{ bindir }}" "{{ unitdir }}" "{{ confdir }}"
    # Replace by rename: the old binary may still be running.
    install -m 0755 bin/aq "{{ bindir }}/.aq.new"
    mv -f "{{ bindir }}/.aq.new" "{{ bindir }}/aq"
    install -m 0644 deploy/systemd/aq.slice "{{ unitdir }}/aq.slice"
    if [ "{{ bindir }}" = "{{ home_directory() / ".local/bin" }}" ]; then
        install -m 0644 deploy/systemd/aq.service "{{ unitdir }}/aq.service"
    else
        sed 's|%h/.local/bin/aq|{{ bindir }}/aq|' deploy/systemd/aq.service > "{{ unitdir }}/aq.service"
        chmod 0644 "{{ unitdir }}/aq.service"
    fi
    test -e "{{ confdir }}/config.toml" || install -m 0600 deploy/config.example.toml "{{ confdir }}/config.toml"
    for target in "claude:{{ claude_skills }}" "codex:{{ codex_skills }}"; do
        agent=${target%%:*}; dir=${target#*:}
        [ -n "$dir" ] || continue
        install -d "$dir"
        stage=$(mktemp -d "$dir/.aq.XXXXXX")
        trap 'if test -d "$stage/old" && ! test -e "$dir/aq"; then mv "$stage/old" "$dir/aq"; fi; rm -rf "$stage"' EXIT
        cp -R "deploy/skills/$agent/aq" "$stage/aq"
        if test -e "$dir/aq"; then mv "$dir/aq" "$stage/old"; fi
        mv "$stage/aq" "$dir/aq"
        rm -rf "$stage"
        trap - EXIT
    done
    echo "hooks are opt-in: see deploy/hooks/claude-settings.json and deploy/hooks/codex-hooks.json"
    {{ systemctl }} daemon-reload
    {{ systemctl }} enable aq.service
    # restart rather than start: a reinstall must pick up the new binary.
    {{ systemctl }} restart aq.service
    if [ "$({{ loginctl }} show-user "$(id -un)" -p Linger --value 2>/dev/null)" != yes ]; then
        echo "hint: linger is off; aq and its tasks stop when you log out."
        echo "      enable it with: loginctl enable-linger $(id -un)"
    fi

# stop and remove the service, binary, units and skills; keeps the database, logs and config
uninstall: (check-idle "stopping")
    -{{ systemctl }} disable --now aq.service
    rm -f "{{ bindir }}/aq" "{{ unitdir }}/aq.service" "{{ unitdir }}/aq.slice"
    for dir in "{{ claude_skills }}" "{{ codex_skills }}"; do [ -z "$dir" ] || rm -rf "$dir/aq"; done
    {{ systemctl }} daemon-reload

# uninstall, then delete the database and all task logs; the config is kept
purge: uninstall
    rm -rf "{{ statedir }}"
