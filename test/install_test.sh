#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
T=$(mktemp -d)
trap 'rm -rf "$T"' EXIT
# Never touch the live installation: every destination, both service commands
# and the runtime dir (the daemon socket) point into $T.
opts=("BINDIR=$T/bin" "UNITDIR=$T/units" "CONFDIR=$T/conf" "STATEDIR=$T/state" "CLAUDE_SKILLS=$T/s1" "CODEX_SKILLS=$T/s2" SYSTEMCTL=true LOGINCTL=true "AQ_RUNTIME_DIR=$T/run")
for key in BINDIR UNITDIR CONFDIR STATEDIR CLAUDE_SKILLS CODEX_SKILLS SYSTEMCTL LOGINCTL AQ_RUNTIME_DIR; do
 found=false
 for arg in "${opts[@]}"; do [[ $arg == "$key="* ]] && found=true; done
 $found || { echo "missing override: $key" >&2; exit 1; }
done
for dir in "$T/bin" "$T/units" "$T/conf" "$T/state" "$T/s1" "$T/s2"; do
 [[ $dir == "$T/"* && $T != / ]] || exit 1
done
run() { env "${opts[@]}" "$@" just "$recipe" </dev/null; }

# A daemon with work in its queue stops install/uninstall unless FORCE is set.
mkdir -p "$T/bin"
printf '#!/bin/sh\nprintf "ID  NAME  STATE\\nabc  tests  RUNNING\\n"\n' > "$T/bin/aq"
chmod +x "$T/bin/aq"
recipe=install
if run 2>"$T/err"; then echo "install ran with a busy queue" >&2; exit 1; fi
grep -q "1 queued or running" "$T/err"
grep -q "FORCE=1" "$T/err"
test ! -e "$T/units/aq.service"
run FORCE=1 2>/dev/null

# The real binary now answers "daemon not running" (69) for $T/run: no warning.
run 2>"$T/err"
! grep -q warning "$T/err"
test -f "$T/s1/aq/SKILL.md"
test -f "$T/s2/aq/references/aq-reference.md"
# Each agent gets its own version of the skill.
cmp -s deploy/skills/claude/aq/SKILL.md "$T/s1/aq/SKILL.md"
cmp -s deploy/skills/codex/aq/SKILL.md "$T/s2/aq/SKILL.md"
rg -q '^Environment=PATH=' "$T/units/aq.service"
grep -q "$T/bin/aq" "$T/units/aq.service"
# Replacement of an existing skill must remove stale contents.
touch "$T/s1/aq/stale"
run
test ! -e "$T/s1/aq/stale"
recipe=uninstall
run
test ! -e "$T/s1/aq"
test ! -e "$T/s2/aq"
test ! -e "$T/bin/aq"
test -f "$T/conf/config.toml"
echo 'install and uninstall recipes: PASS (isolated)'
