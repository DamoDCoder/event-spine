# M2 — What The Filesystem Model Proves, And What It Does Not

**Date:** 2026-08-10
**Status:** in progress. This note is written before the log rather than after,
and it will be updated as the log gives the model more surface to be wrong on.

## Why This Exists Before The Log

`docs/decisions/m1-deterministic-core.md` named M2's riskiest assumption:

> A simulated filesystem models fsync ordering, partial writes, and torn tails
> as the author *believes* they behave, and a belief that is wrong produces a
> green crash matrix and real data loss.

A test written after the log would be written to agree with the log. So the
filesystem interface, both implementations, and the tests that compare them
landed first, with nothing depending on them yet.

> The durability layers, and the gap between what is measured and what is
> believed, are drawn in [concepts.md](../concepts.md).

## The Model

`sim.FS` asserts four rules that a naive in-memory map does not:

1. **Appending changes what a reader sees. Only `Sync` changes what survives.**
2. **A directory entry is durable separately from the file it names.** A file
   whose contents were synced into a directory that was not synced does not
   survive a crash at all. This is the rule that makes crash-during-compaction
   the hazard `docs/log-design.md` says it is.
3. **Unlink is not deletion.** An open handle holds the inode, not the name, so
   it keeps working across `Remove` and `Rename`.
4. **`Create` refuses an existing name rather than truncating it**, because a
   segment silently emptied by a re-create is data loss that leaves no trace.

## What Has Been Measured

### Semantics: differential against a real disk

`TestFSDifferentialAgainstRealDisk` draws a seeded operation script — create,
open, append, read, truncate, remove, rename, close, sync, list, over both valid
and deliberately invalid names — and replays it against `sim.FS` and against a
real directory on a real disk, comparing the error classification, byte counts,
data read, file sizes, and the directory listing after every single step.

8 seeds × 200 operations. Every case below is reached and every one agrees:

| | |
| --- | --- |
| `create` | ok, already-exists, invalid name |
| `open` | ok, not-exists, invalid name |
| `append` | ok, closed handle |
| `readat` | ok, EOF, closed handle |
| `truncate` | ok, refused growth, closed handle |
| `remove` / `rename` | ok, not-exists, invalid name |
| `close` / `sync` / `list` | ok |

`TestDifferentialScriptReachesEveryFailureMode` pins that coverage, so a later
refactor cannot quietly turn the differential into a slow way of confirming that
two implementations agree about nothing interesting.

Rule 3 is asserted directly against both implementations by
`TestOpenHandleSurvivesRemoveAndRename`, since a random script reaches it only
by luck.

### A real process, killed mid-record, on a real disk

`TestRealProcessKilledMidRecordLeavesATornTail` forks the test binary. The child
writes 6 whole 16-byte records, syncs the file and the directory, appends the
first 8 bytes of a 7th, and then `SIGKILL`s itself. The kill point is chosen
rather than timed, so nothing races and the test cannot flake.

The parent then reopens the directory and finds exactly 6 whole records followed
by an 8-byte tail — a genuine partial record on a real filesystem, which is what
recovery will have to detect and truncate. It passes on darwin/arm64 and in the
Linux container.

Checked for teeth: removing the partial append from the child makes the parent
fail with `file is 96 bytes after the kill, want 6 whole records plus a 8 byte
tail`.

## What Is Still Only A Belief

**Rules 1 and 2 are not validated by anything.** `SIGKILL` destroys a process,
not a machine. The page cache survives it, so bytes appended without `fsync` are
still readable afterwards — the process-kill test cannot observe the loss that
`sim.FS.Crash` models, because on a killed process there is no loss to observe.

That is worth being blunt about: **the simulator's crash is power loss, and this
repository has not yet tested against power loss.** Everything in the crash
matrix that depends on "unsynced data is gone" is currently resting on the
author's reading of how filesystems behave, not on a measurement.

Validating it needs one of:

- a fault-injecting block device (`dm-flakey`, or a CrashMonkey-style harness)
  that drops unsynced writes on command;
- a virtual machine that can be power-cut between operations;
- an SSD and a willingness to pull its power repeatedly.

The first is the realistic one, and it is Linux-only, which is another reason
the container run is authoritative. It is deferred rather than dropped: the
trigger is the first durability claim this repository publishes. **A published
p99 or "loses nothing acknowledged as durable" claim without one of the above is
a claim about a belief, and this note is the record that says so.**

Two smaller caveats in the same family:

- On darwin, `Sync` issues `fsync`, which does not flush the drive's own write
  cache — that needs `F_FULLFSYNC`. A durability number measured on a Mac is a
  number about macOS.
- `sim.FS` models a flat directory with no subdirectories, no permissions, no
  hard links, and no concurrent writers. Each of those is a behaviour the real
  filesystem has and the model does not, and each is only safe while the log
  never uses it.

## Next

The log is built on this interface. The crash matrix in `docs/log-design.md`
enumerates the failure modes the simulator must inject; each one arrives with the
part of the log it attacks, and the differential script grows whenever the log
starts depending on a filesystem behaviour it does not currently compare.
