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

# Each agent gets its own copy of the aq skill: deploy/skills/<agent>/aq;
# OpenClaude uses the Claude Code skill, OpenCode has its own. An empty value
# skips that agent.
claude_skills := env("CLAUDE_SKILLS", home_directory() / ".claude/skills")
codex_skills := env("CODEX_SKILLS", home_directory() / ".codex/skills")
openclaude_skills := env("OPENCLAUDE_SKILLS", home_directory() / ".openclaude/skills")
opencode_skills := env("OPENCODE_SKILLS", home_directory() / ".config/opencode/skills")

default:
    @just --list

# Version from git (tag, commit, -dirty); 0.1.0-dev outside a git checkout.
version := `git describe --tags --always --dirty 2>/dev/null || echo 0.1.0-dev`

# bin/aq
build:
    {{ go }} build -ldflags "-X github.com/creatorofuniverses/agent-queue/internal/daemon.Version={{ version }}" -o bin/aq ./cmd/aq

# go test -race ./...
test:
    {{ go }} test -race ./...

# integration and e2e tests against the real systemd user manager
test-integration:
    {{ go }} test -tags integration -p 1 ./...

# install/uninstall recipes into a temporary tree
test-install:
    bash test/install_test.sh

# Shows what is in flight before the daemon restarts or stops. A restart is safe:
# tasks keep running and are picked up again, aq wait reconnects, and the
# in-memory run samples are saved and restored.
[private]
check-idle action:
    #!/usr/bin/env bash
    set -euo pipefail
    aq="{{ bindir }}/aq"
    [ -x "$aq" ] || exit 0
    out=$("$aq" list 2>/dev/null) || exit 0 # daemon not running
    n=$(($(printf '%s\n' "$out" | wc -l) - 1))
    [ "$n" -gt 0 ] || exit 0
    echo "note: {{ action }} aq with $n queued or running item(s):" >&2
    printf '%s\n' "$out" >&2
    echo "Tasks keep running and are picked up again; waiting clients reconnect." >&2

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
    # An existing config keeps its own list of agent processes. A hint, not a TOML
    # parser: comments are cut first, the key may be written stats.agent_processes,
    # and a name counts only as a whole string, as aq matches process names.
    agents=$(awk '/^[[:space:]]*([A-Za-z_][A-Za-z0-9_]*\.)*agent_processes[[:space:]]*=/{f=1} f{sub(/#.*/, ""); printf "%s ", $0} f && index($0, "]"){exit}' "{{ confdir }}/config.toml" 2>/dev/null || true)
    for missing in openclaude opencode; do
        if [ -n "$agents" ] && ! grep -q -e "\"$missing\"" -e "'$missing'" <<< "$agents"; then
            echo "hint: agent_processes in {{ confdir }}/config.toml does not list $missing."
            case "$missing" in
            openclaude) echo "      add it, or OpenClaude's sessions are not counted and its commands belong to cli or to the session of an agent it was started from." ;;
            opencode) echo "      add it, or OpenCode's processes are not counted and its commands are identified by the environment alone: an inherited variable of another agent can file them under that agent's session." ;;
            esac
        fi
    done
    # A config that names its own adapters needs the OpenCode one too. A hint again,
    # not a TOML parser: comments are cut first, so a commented-out adapter line
    # counts as absent. The adapter counts only as an assignment of the variable in quotes:
    # aq looks for OPENCODE_SESSION_ID itself, and a longer name such as
    # OPENCODE_SESSION_ID_OLD leaves OpenCode without an adapter. The installer never edits
    # the config.
    effective=$(sed 's/#.*//' "{{ confdir }}/config.toml" 2>/dev/null || true)
    re="identity_env[[:space:]]*=[[:space:]]*[\"']OPENCODE_SESSION_ID[\"']"
    if grep -qE '^[[:space:]]*(\[\[[[:space:]]*agents[[:space:]]*\]\]|agents[[:space:]]*=)' <<< "$effective" && ! grep -qE "$re" <<< "$effective"; then
        echo "hint: [[agents]] in {{ confdir }}/config.toml has no adapter for OpenCode."
        echo "      add one with identity_env = \"OPENCODE_SESSION_ID\" and delivery = \"client-wait\", or OpenCode's commands belong to cli and its MCP submissions are rejected with \"session identity unknown\"."
    fi
    # A symlink whose target is gone is still an entry: -e alone says it is not.
    present() { test -e "$1" || test -L "$1"; }
    for target in "claude:{{ claude_skills }}" "codex:{{ codex_skills }}" "claude:{{ openclaude_skills }}" "opencode:{{ opencode_skills }}"; do
        agent=${target%%:*}; dir=${target#*:}
        [ -n "$dir" ] || continue
        install -d "$dir"
        stage=$(mktemp -d "$dir/.aq.XXXXXX")
        trap 'if present "$stage/old" && ! present "$dir/aq"; then mv "$stage/old" "$dir/aq"; fi; rm -rf "$stage"' EXIT
        cp -R "deploy/skills/$agent/aq" "$stage/aq"
        if present "$dir/aq"; then mv "$dir/aq" "$stage/old"; fi
        mv "$stage/aq" "$dir/aq"
        rm -rf "$stage"
        trap - EXIT
    done
    echo "hooks are opt-in: see deploy/hooks/claude-settings.json (Claude Code and OpenClaude, ~/.openclaude/settings.json) and deploy/hooks/codex-hooks.json; OpenCode has no hook"
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
    for dir in "{{ claude_skills }}" "{{ codex_skills }}" "{{ openclaude_skills }}" "{{ opencode_skills }}"; do [ -z "$dir" ] || rm -rf "$dir/aq"; done
    {{ systemctl }} daemon-reload

# uninstall, then delete the database and all task logs; the config is kept
purge: uninstall
    rm -rf "{{ statedir }}"
