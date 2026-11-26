#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
T=$(mktemp -d)
trap 'rm -rf "$T"' EXIT
# Never touch the live installation: every destination, both service commands
# and the runtime dir (the daemon socket) point into $T.
opts=("BINDIR=$T/bin" "UNITDIR=$T/units" "CONFDIR=$T/conf" "STATEDIR=$T/state" "CLAUDE_SKILLS=$T/s1" "CODEX_SKILLS=$T/s2" "OPENCLAUDE_SKILLS=$T/s3" "OPENCODE_SKILLS=$T/s4" SYSTEMCTL=true LOGINCTL=true "AQ_RUNTIME_DIR=$T/run")
for key in BINDIR UNITDIR CONFDIR STATEDIR CLAUDE_SKILLS CODEX_SKILLS OPENCLAUDE_SKILLS OPENCODE_SKILLS SYSTEMCTL LOGINCTL AQ_RUNTIME_DIR; do
 found=false
 for arg in "${opts[@]}"; do [[ $arg == "$key="* ]] && found=true; done
 $found || { echo "missing override: $key" >&2; exit 1; }
done
for dir in "$T/bin" "$T/units" "$T/conf" "$T/state" "$T/s1" "$T/s2" "$T/s3" "$T/s4"; do
 [[ $dir == "$T/"* && $T != / ]] || exit 1
done
run() { env "${opts[@]}" "$@" just "$recipe" </dev/null; }
# set -e lets a failing "! grep" pass: an absent pattern needs its own check,
# and a file grep cannot read (exit 2) proves nothing either.
absent() {
    local rc=0
    grep -q "$1" "$2" || rc=$?
    [ "$rc" -eq 1 ] || { echo "'$1' in $2: grep exit $rc, want 1" >&2; exit 1; }
}
# The hint about the OpenCode adapter; grep takes [ literally only when escaped.
adapter_hint='hint: \[\[agents\]\]'

# A daemon with work in its queue is listed before the restart, which proceeds.
mkdir -p "$T/bin"
printf '#!/bin/sh\nprintf "ID  NAME  STATE\\nabc  tests  RUNNING\\n"\n' > "$T/bin/aq"
chmod +x "$T/bin/aq"
recipe=install
run >"$T/out" 2>"$T/err"
grep -q "1 queued or running" "$T/err"
grep -q "reconnect" "$T/err"
test -f "$T/units/aq.service"
# The example config it just installed lists every agent name and the OpenCode
# adapter: nothing to hint about.
absent "hint: agent_processes" "$T/out"
absent "$adapter_hint" "$T/out"
grep -q "hooks are opt-in" "$T/out"
grep -q "OpenCode has no hook" "$T/out"

# The real binary now answers "daemon not running" (69) for $T/run: no warning.
run 2>"$T/err"
absent warning "$T/err"
test -f "$T/s1/aq/SKILL.md"
test -f "$T/s2/aq/references/aq-reference.md"
# Each agent gets its own version of the skill.
cmp -s deploy/skills/claude/aq/SKILL.md "$T/s1/aq/SKILL.md"
cmp -s deploy/skills/codex/aq/SKILL.md "$T/s2/aq/SKILL.md"
# OpenClaude gets the Claude Code skill.
cmp -s deploy/skills/claude/aq/SKILL.md "$T/s3/aq/SKILL.md"
# OpenCode gets its own, both files.
cmp -s deploy/skills/opencode/aq/SKILL.md "$T/s4/aq/SKILL.md"
cmp -s deploy/skills/opencode/aq/references/aq-reference.md "$T/s4/aq/references/aq-reference.md"
rg -q '^Environment=PATH=' "$T/units/aq.service"
grep -q "$T/bin/aq" "$T/units/aq.service"
# Replacement of an existing skill must remove stale contents.
touch "$T/s1/aq/stale"
run
test ! -e "$T/s1/aq/stale"
# An empty destination installs no skill for that agent and leaves the others.
rm -rf "$T/s3/aq"
env "${opts[@]}" OPENCLAUDE_SKILLS= just install </dev/null >/dev/null
test ! -e "$T/s3/aq"
test -f "$T/s1/aq/SKILL.md"
run
test -f "$T/s3/aq/SKILL.md"
rm -rf "$T/s4/aq"
env "${opts[@]}" OPENCODE_SKILLS= just install </dev/null >/dev/null
test ! -e "$T/s4/aq"
test -f "$T/s3/aq/SKILL.md"
run
test -f "$T/s4/aq/SKILL.md"
# A dangling symlink is an entry to replace too: -e is false for it.
rm -rf "$T/s3/aq"
ln -s "$T/gone/aq" "$T/s3/aq"
env "${opts[@]}" CLAUDE_SKILLS= just install </dev/null
test ! -L "$T/s3/aq"
cmp -s deploy/skills/claude/aq/SKILL.md "$T/s3/aq/SKILL.md"
# When the replacement fails halfway, the installer puts back what was there.
mkdir "$T/shim"
printf '#!/bin/sh\n[ "$FAIL_MV" = "$2" ] && case "$1" in */aq) exit 1 ;; esac\nexec %s "$@"\n' "$(command -v mv)" > "$T/shim/mv"
chmod +x "$T/shim/mv"
rm -rf "$T/s3/aq"
ln -s "$T/gone/aq" "$T/s3/aq"
! env "${opts[@]}" PATH="$T/shim:$PATH" FAIL_MV="$T/s3/aq" just install </dev/null >/dev/null 2>&1
[ "$(readlink "$T/s3/aq")" = "$T/gone/aq" ]
[ "$(ls -A "$T/s3")" = aq ]
# A config that lists agent processes without openclaude and opencode keeps its
# list: the installer only says so, one hint per missing name, on one line or
# several.
printf '[stats]\nagent_processes = ["claude", "codex"]\n' > "$T/conf/config.toml"
run >"$T/out"
grep -q "does not list openclaude" "$T/out"
grep -q "OpenClaude's sessions are not counted" "$T/out"
grep -q "does not list opencode" "$T/out"
grep -q "OpenCode's processes are not counted" "$T/out"
grep -q 'agent_processes = \["claude", "codex"\]' "$T/conf/config.toml"
printf '[stats]\nagent_processes = [\n  "claude",\n  "codex",\n]\n' > "$T/conf/config.toml"
run >"$T/out"
grep -q "hint: agent_processes" "$T/out"
# A comment is not part of the list, and the key may be written stats.agent_processes.
printf '[stats]\nagent_processes = [  # the agent] list\n  "claude",\n  "openclaude",\n]\n' > "$T/conf/config.toml"
run >"$T/out"
absent "does not list openclaude" "$T/out"
grep -q "does not list opencode" "$T/out"
printf '[stats]\nagent_processes = ["claude", "codex"] # openclaude not used\n' > "$T/conf/config.toml"
run >"$T/out"
grep -q "hint: agent_processes" "$T/out"
# Both names only in a comment: both hints.
printf '[stats]\nagent_processes = ["claude", "codex"] # "openclaude", "opencode"\n' > "$T/conf/config.toml"
run >"$T/out"
grep -q "does not list openclaude" "$T/out"
grep -q "does not list opencode" "$T/out"
printf 'stats.agent_processes = ["claude", "codex"]\n' > "$T/conf/config.toml"
run >"$T/out"
grep -q "hint: agent_processes" "$T/out"
# A name counts as a whole string, as aq matches process names.
printf '[stats]\nagent_processes = ["claude", "codex", "openclaude-preview", "opencode-preview"]\n' > "$T/conf/config.toml"
run >"$T/out"
grep -q "does not list openclaude" "$T/out"
grep -q "does not list opencode" "$T/out"
# With openclaude in the list only the opencode hint remains.
printf '[stats]\nagent_processes = [\n  "claude",\n  "openclaude",\n]\n' > "$T/conf/config.toml"
run >"$T/out"
absent "does not list openclaude" "$T/out"
grep -q "does not list opencode" "$T/out"
printf "[stats]\nagent_processes = ['codex', 'openclaude']\n" > "$T/conf/config.toml"
run >"$T/out"
absent "does not list openclaude" "$T/out"
grep -q "does not list opencode" "$T/out"
# With opencode in the list, in either quote style, only the openclaude hint remains.
printf "[stats]\nagent_processes = ['codex', 'opencode']\n" > "$T/conf/config.toml"
run >"$T/out"
grep -q "does not list openclaude" "$T/out"
absent "does not list opencode" "$T/out"
printf '[stats]\nagent_processes = ["codex", "opencode"]\n' > "$T/conf/config.toml"
run >"$T/out"
grep -q "does not list openclaude" "$T/out"
absent "does not list opencode" "$T/out"
# With both names in the list, or with no list of its own, nothing to hint about.
printf '[stats]\nagent_processes = ["claude", "openclaude", "codex", "opencode"]\n' > "$T/conf/config.toml"
run >"$T/out"
absent "hint: agent_processes" "$T/out"
printf '[stats]\nsample_interval = "30s"\n' > "$T/conf/config.toml"
run >"$T/out"
absent "hint: agent_processes" "$T/out"
# Without [[agents]] the default adapters apply, OpenCode's among them: no hint.
absent "$adapter_hint" "$T/out"
# A commented-out block is no block at all.
printf '[stats]\nagent_processes = ["claude", "openclaude", "codex", "opencode"]\n# [[agents]]\n# name = "claude"\n# identity_env = "CLAUDE_CODE_SESSION_ID"\n' > "$T/conf/config.toml"
run >"$T/out"
absent "$adapter_hint" "$T/out"
# A config with its own [[agents]] and no OpenCode adapter gets a hint, and is
# not edited.
printf '[stats]\nagent_processes = ["claude", "openclaude", "codex", "opencode"]\n\n[[agents]]\nname = "codex"\nidentity_env = "CODEX_THREAD_ID"\ndelivery = "push"\n\n[[agents]]\nname = "claude"\nidentity_env = "CLAUDE_CODE_SESSION_ID"\ndelivery = "client-wait"\n' > "$T/conf/config.toml"
run >"$T/out"
grep -q "$adapter_hint" "$T/out"
grep -q 'identity_env = "OPENCODE_SESSION_ID" and delivery = "client-wait"' "$T/out"
grep -q 'rejected with "session identity unknown"' "$T/out"
absent "hint: agent_processes" "$T/out"
absent 'name = "opencode"' "$T/conf/config.toml"
# A commented-out adapter line is not an adapter.
printf '\n# name = "opencode"\n# identity_env = "OPENCODE_SESSION_ID"\n' >> "$T/conf/config.toml"
run >"$T/out"
grep -q "$adapter_hint" "$T/out"
# With the OpenCode adapter the hint is gone.
printf '\n[[agents]]\nname = "opencode"\nidentity_env = "OPENCODE_SESSION_ID"\ndelivery = "client-wait"\n' >> "$T/conf/config.toml"
run >"$T/out"
absent "$adapter_hint" "$T/out"
# The header may be indented or spaced out.
printf '[stats]\nagent_processes = ["claude", "openclaude", "codex", "opencode"]\n\n  [[ agents ]]\nname = "claude"\nidentity_env = "CLAUDE_CODE_SESSION_ID"\n' > "$T/conf/config.toml"
run >"$T/out"
grep -q "$adapter_hint" "$T/out"
# An adapter on a longer variable name is none for OPENCODE_SESSION_ID: aq looks for
# that exact variable, and does not find an adapter on OPENCODE_SESSION_ID_OLD.
printf '[stats]\nagent_processes = ["claude", "openclaude", "codex", "opencode"]\n\n[[agents]]\nname = "opencode"\nidentity_env = "OPENCODE_SESSION_ID_OLD"\ndelivery = "client-wait"\n' > "$T/conf/config.toml"
run >"$T/out"
grep -q "$adapter_hint" "$T/out"
# The list may be an array of inline tables instead of [[agents]] blocks: without the
# OpenCode adapter the hint is due,
printf 'agents = [ { name = "codex", identity_env = "CODEX_THREAD_ID", delivery = "push", command = ["x", "{session}", "{text}"] }, { name = "claude", identity_env = "CLAUDE_CODE_SESSION_ID", delivery = "client-wait" } ]\n[stats]\nagent_processes = ["claude", "openclaude", "codex", "opencode"]\n' > "$T/conf/config.toml"
run >"$T/out"
grep -q "$adapter_hint" "$T/out"
# and with it, in any quote style, the hint is not.
printf "agents = [ { name = 'codex', identity_env = 'CODEX_THREAD_ID', delivery = 'push', command = ['x', '{session}', '{text}'] }, { name = 'claude', identity_env = 'CLAUDE_CODE_SESSION_ID', delivery = 'client-wait' }, { name = 'opencode', identity_env = 'OPENCODE_SESSION_ID', delivery = 'client-wait' } ]\n[stats]\nagent_processes = ['claude', 'openclaude', 'codex', 'opencode']\n" > "$T/conf/config.toml"
run >"$T/out"
absent "$adapter_hint" "$T/out"
printf 'agents = [ { name = "codex", identity_env = "CODEX_THREAD_ID", delivery = "push", command = ["x", "{session}", "{text}"] }, { name = "claude", identity_env = "CLAUDE_CODE_SESSION_ID", delivery = "client-wait" }, { name = "opencode", identity_env = "OPENCODE_SESSION_ID", delivery = "client-wait" } ]\n[stats]\nagent_processes = ["claude", "openclaude", "codex", "opencode"]\n' > "$T/conf/config.toml"
run >"$T/out"
absent "$adapter_hint" "$T/out"
# The example config, installed fresh, needs neither hint.
install -m 0600 deploy/config.example.toml "$T/conf/config.toml"
run >"$T/out"
absent "hint: agent_processes" "$T/out"
absent "$adapter_hint" "$T/out"
# A config without either entry gets both hints.
printf '[stats]\nagent_processes = ["claude", "codex"]\n\n[[agents]]\nname = "claude"\nidentity_env = "CLAUDE_CODE_SESSION_ID"\n' > "$T/conf/config.toml"
run >"$T/out"
grep -q "does not list openclaude" "$T/out"
grep -q "does not list opencode" "$T/out"
grep -q "$adapter_hint" "$T/out"
# The hints come from the config as aq reads it, not from its text. agents = []
# keeps the default adapters, OpenCode's among them.
printf 'agents = []\n[stats]\nagent_processes = ["claude", "openclaude", "codex", "opencode"]\n' > "$T/conf/config.toml"
run >"$T/out"
absent "does not list" "$T/out"
absent "$adapter_hint" "$T/out"
# A header with a quoted key is a list of its own: without the OpenCode adapter
# the hint is due.
printf '[["agents"]]\nname = "claude"\nidentity_env = "CLAUDE_CODE_SESSION_ID"\n[stats]\nagent_processes = ["claude", "openclaude", "codex", "opencode"]\n' > "$T/conf/config.toml"
run >"$T/out"
grep -q "$adapter_hint" "$T/out"
absent "does not list" "$T/out"
# A command that merely contains the assignment is no adapter.
printf "[[agents]]\nname = \"codex\"\nidentity_env = \"CODEX_THREAD_ID\"\ndelivery = \"push\"\ncommand = [\"codex\", \"identity_env = 'OPENCODE_SESSION_ID'\", \"{session}\", \"{text}\"]\n\n[[agents]]\nname = \"claude\"\nidentity_env = \"CLAUDE_CODE_SESSION_ID\"\n[stats]\nagent_processes = [\"claude\", \"openclaude\", \"codex\", \"opencode\"]\n" > "$T/conf/config.toml"
run >"$T/out"
grep -q "$adapter_hint" "$T/out"
# The key of the process list may be quoted.
printf '[stats]\n"agent_processes" = ["claude", "openclaude", "codex"]\n' > "$T/conf/config.toml"
run >"$T/out"
absent "does not list openclaude" "$T/out"
grep -q "does not list opencode" "$T/out"
# Two adapters on the variable of OpenCode leave its calls no session.
printf '[[agents]]\nname = "opencode"\nidentity_env = "OPENCODE_SESSION_ID"\ndelivery = "client-wait"\n\n[[agents]]\nname = "opencode2"\nidentity_env = "OPENCODE_SESSION_ID"\ndelivery = "client-wait"\n' > "$T/conf/config.toml"
run >"$T/out"
grep -q 'has 2 adapters with identity_env = "OPENCODE_SESSION_ID"' "$T/out"
absent "has no adapter for OpenCode" "$T/out"
# A config aq cannot read is said on stderr, and the install goes on: the daemon
# refuses such a config, not the installer.
printf '[stats]\nagent_process = ["claude"]\n' > "$T/conf/config.toml"
run >"$T/out" 2>"$T/err"
grep -q "warning: config .*config.toml: unknown keys: stats.agent_process" "$T/err"
grep -q "the daemon will not start with this config" "$T/err"
absent "does not list" "$T/out"
absent "$adapter_hint" "$T/out"
recipe=uninstall
run
test ! -e "$T/s1/aq"
test ! -e "$T/s2/aq"
test ! -e "$T/s3/aq"
test ! -e "$T/s4/aq"
test ! -e "$T/bin/aq"
test -f "$T/conf/config.toml"
echo 'install and uninstall recipes: PASS (isolated)'
