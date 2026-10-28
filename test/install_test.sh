#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
T=$(mktemp -d)
trap 'rm -rf "$T"' EXIT
# Never call the live entrypoints. Execute the exact recipes via private aliases.
python3 - "$T/Makefile" <<'PY'
import sys
s=open('Makefile').read()
s=s.replace('\ninstall: build', '\naq-test-install: build').replace('\nuninstall:', '\naq-test-uninstall:')
open(sys.argv[1],'w').write(s)
PY
opts=("BINDIR=$T/bin" "UNITDIR=$T/units" "CONFDIR=$T/conf" "STATEDIR=$T/state" "CLAUDE_SKILLS=$T/s1" "CODEX_SKILLS=$T/s2" SYSTEMCTL=true LOGINCTL=true)
# Every writable destination and both service commands must be explicitly overridden.
for key in BINDIR UNITDIR CONFDIR STATEDIR CLAUDE_SKILLS CODEX_SKILLS SYSTEMCTL LOGINCTL; do
 found=false
 for arg in "${opts[@]}"; do [[ $arg == "$key="* ]] && found=true; done
 $found || { echo "missing override: $key" >&2; exit 1; }
done
for dir in "$T/bin" "$T/units" "$T/conf" "$T/state" "$T/s1" "$T/s2"; do
 [[ $dir == "$T/"* && $T != / ]] || exit 1
done
make -f "$T/Makefile" aq-test-install "${opts[@]}"
test -f "$T/s1/aq/SKILL.md"
test -f "$T/s2/aq/references/aq-reference.md"
# Each agent gets its own version of the skill.
cmp -s deploy/skills/claude/aq/SKILL.md "$T/s1/aq/SKILL.md"
cmp -s deploy/skills/codex/aq/SKILL.md "$T/s2/aq/SKILL.md"
rg -q '^Environment=PATH=' "$T/units/aq.service"
# Replacement of an existing skill must remove stale contents.
touch "$T/s1/aq/stale"
make -f "$T/Makefile" aq-test-install "${opts[@]}"
test ! -e "$T/s1/aq/stale"
make -f "$T/Makefile" aq-test-uninstall "${opts[@]}"
test ! -e "$T/s1/aq"
test ! -e "$T/s2/aq"
test ! -e "$T/bin/aq"
test -f "$T/conf/config.toml"
echo 'install recipes and uninstall recipes: PASS (isolated aliases)'
