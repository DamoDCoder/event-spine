#!/usr/bin/env bash
# Reproduce docs/walkthrough-replay.md.
#
# It checks out a worktree with one commit reverted — the fix that brought the
# record offset under the checksum — and replays the diagnosis against the bug
# that comes back. The point is that the walkthrough is a transcript of
# something anyone can run again, rather than output pasted into a document and
# trusted forever.
#
# The worktree is temporary and is removed on exit, including on failure.

set -euo pipefail

# The commit that fixed seeds/0008.md. Pinned rather than searched for: a demo
# that resolves "the fix" dynamically would quietly demo something else the day
# a message changes.
FIX=ff1cded

SEED=8
STEPS=3
FAULTS="bitflip@5:458021"

REPO=$(git rev-parse --show-toplevel)
WORKTREE=$(mktemp -d "${TMPDIR:-/tmp}/spine-demo-XXXXXX")

cleanup() {
    cd "$REPO"
    git worktree remove --force "$WORKTREE" >/dev/null 2>&1 || true
    rm -rf "$WORKTREE"
}
trap cleanup EXIT

rm -rf "$WORKTREE"
git -C "$REPO" worktree add -q --detach "$WORKTREE" HEAD
cd "$WORKTREE"

# Revert the fix itself and nothing else. The same commit touched the design
# note and the test that pinned the old behaviour; reverting those too would
# make the demo fail to build for a reason that is not the bug.
git revert --no-commit "$FIX" >/dev/null
git checkout HEAD -- internal/log/record_test.go docs/log-design.md internal/log/group.go

echo "### the bug is back: one commit reverted"
git --no-pager diff --stat HEAD -- internal/log/record.go
echo

echo "### 1. the corpus says something is wrong"
go run ./cmd/spine sim corpus --dir seeds 2>&1 | grep -E "FAIL|corpus:" || true
echo

echo "### 2. scrub the run"
go run ./cmd/spine replay --seed "$SEED" --steps "$STEPS" --faults "$FAULTS"
echo

echo "### 3. the counterfactual"
go run ./cmd/spine replay --seed "$SEED" --steps "$STEPS" --faults "$FAULTS" --diff
echo

echo "### 4. the operations, and where the fault landed"
go run ./cmd/spine replay --seed "$SEED" --steps "$STEPS" --faults "$FAULTS" --ops
echo

echo "### the walkthrough's claim: the corpus catches this and HEAD does not"
cd "$REPO"
go run ./cmd/spine sim corpus --dir seeds 2>&1 | tail -1
