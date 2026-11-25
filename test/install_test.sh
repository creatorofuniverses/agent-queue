#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
T=$(mktemp -d)
trap 'rm -rf "$T"' EXIT
# Never touch the live installation: every destination, both service commands
# and the runtime dir (the daemon socket) point into $T.
opts=("BINDIR=$T/bin" "UNITDIR=$T/units" "CONFDIR=$T/conf" "STATEDIR=$T/state" "CLAUDE_SKILLS=$T/s1" "CODEX_SKILLS=$T/s2" "OPENCLAUDE_SKILLS=$T/s3" SYSTEMCTL=true LOGINCTL=true "AQ_RUNTIME_DIR=$T/run")
for key in BINDIR UNITDIR CONFDIR STATEDIR CLAUDE_SKILLS CODEX_SKILLS OPENCLAUDE_SKILLS SYSTEMCTL LOGINCTL AQ_RUNTIME_DIR; do
 found=false
 for arg in "${opts[@]}"; do [[ $arg == "$key="* ]] && found=true; done
 $found || { echo "missing override: $key" >&2; exit 1; }
done
for dir in "$T/bin" "$T/units" "$T/conf" "$T/state" "$T/s1" "$T/s2" "$T/s3"; do
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

# A daemon with work in its queue is listed before the restart, which proceeds.
mkdir -p "$T/bin"
printf '#!/bin/sh\nprintf "ID  NAME  STATE\\nabc  tests  RUNNING\\n"\n' > "$T/bin/aq"
chmod +x "$T/bin/aq"
recipe=install
run >"$T/out" 2>"$T/err"
grep -q "1 queued or running" "$T/err"
grep -q "reconnect" "$T/err"
test -f "$T/units/aq.service"
# The example config it just installed lists openclaude: nothing to hint about.
absent "hint: agent_processes" "$T/out"

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
# A config that lists agent processes without openclaude keeps its list: the
# installer only says so, on one line or several.
printf '[stats]\nagent_processes = ["claude", "codex"]\n' > "$T/conf/config.toml"
run >"$T/out"
grep -q "hint: agent_processes" "$T/out"
grep -q 'agent_processes = \["claude", "codex"\]' "$T/conf/config.toml"
printf '[stats]\nagent_processes = [\n  "claude",\n  "codex",\n]\n' > "$T/conf/config.toml"
run >"$T/out"
grep -q "hint: agent_processes" "$T/out"
# A comment is not part of the list, and the key may be written stats.agent_processes.
printf '[stats]\nagent_processes = [  # the agent] list\n  "claude",\n  "openclaude",\n]\n' > "$T/conf/config.toml"
run >"$T/out"
absent "hint: agent_processes" "$T/out"
printf '[stats]\nagent_processes = ["claude", "codex"] # openclaude not used\n' > "$T/conf/config.toml"
run >"$T/out"
grep -q "hint: agent_processes" "$T/out"
printf 'stats.agent_processes = ["claude", "codex"]\n' > "$T/conf/config.toml"
run >"$T/out"
grep -q "hint: agent_processes" "$T/out"
# A name counts as a whole string, as aq matches process names.
printf '[stats]\nagent_processes = ["claude", "codex", "openclaude-preview"]\n' > "$T/conf/config.toml"
run >"$T/out"
grep -q "hint: agent_processes" "$T/out"
# With openclaude in the list, or with no list of its own, nothing to hint about.
printf '[stats]\nagent_processes = [\n  "claude",\n  "openclaude",\n]\n' > "$T/conf/config.toml"
run >"$T/out"
absent "hint: agent_processes" "$T/out"
printf "[stats]\nagent_processes = ['codex', 'openclaude']\n" > "$T/conf/config.toml"
run >"$T/out"
absent "hint: agent_processes" "$T/out"
printf '[stats]\nsample_interval = "30s"\n' > "$T/conf/config.toml"
run >"$T/out"
absent "hint: agent_processes" "$T/out"
recipe=uninstall
run
test ! -e "$T/s1/aq"
test ! -e "$T/s2/aq"
test ! -e "$T/s3/aq"
test ! -e "$T/bin/aq"
test -f "$T/conf/config.toml"
echo 'install and uninstall recipes: PASS (isolated)'
