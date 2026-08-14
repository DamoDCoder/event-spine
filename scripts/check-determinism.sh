#!/usr/bin/env bash
# Copy to scripts/check-determinism.sh and chmod +x.
#
# Fails the build when a production package imports a nondeterministic stdlib
# package directly. Wall clock, randomness, and OS access are injected through
# interfaces so the simulator can control them.
#
# This catches the import-level violations only. Map iteration order, `select`
# over multiple ready channels, and float comparison in projections all pass this
# check and still break determinism. Review those by hand.

set -euo pipefail

FORBIDDEN='^(time|math/rand|math/rand/v2|os|net|crypto/rand)$'

# Directories exempt from the rule, as a regex over package import paths.
# - cmd/       : the real runtime, where the concrete implementations are built
# - sim, internal/devtools : the simulator and tooling need real I/O
# - runtime    : the adapters that implement the injected interfaces
#
# These moved out of internal/ when the spine became importable. The rule is
# about which packages may touch the machine, not about where they live, so the
# paths changed and the list did not.
EXEMPT='/(cmd|sim|internal/devtools|runtime)(/|$)'

violations=0

while IFS= read -r pkg; do
    if [[ "$pkg" =~ $EXEMPT ]]; then
        continue
    fi

    imports=$(go list -f '{{range .Imports}}{{.}}{{"\n"}}{{end}}' "$pkg")

    while IFS= read -r imp; do
        [[ -z "$imp" ]] && continue
        if [[ "$imp" =~ $FORBIDDEN ]]; then
            echo "determinism: $pkg imports $imp"
            violations=$((violations + 1))
        fi
    done <<< "$imports"
done < <(go list ./... )

# Test files are checked separately and more loosely: a test may import "time"
# for a duration constant, but must never call time.Now or time.Sleep.
if grep -rn --include='*_test.go' -E '\btime\.(Now|Sleep|After|Tick)\(' . ; then
    echo "determinism: tests must take time from the injected Clock, not the wall clock"
    violations=$((violations + 1))
fi

if [[ "$violations" -gt 0 ]]; then
    echo
    echo "FAIL: $violations determinism violation(s)."
    echo "Inject the dependency instead. See CLAUDE.md, 'Injected Dependencies'."
    exit 1
fi

echo "determinism: ok"
