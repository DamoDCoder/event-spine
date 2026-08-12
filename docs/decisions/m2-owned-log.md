# M2 — The Owned Log

**Date:** 2026-08-12
**Status:** finished. Half the claim held, half did not, and the half that did
not is recorded here as it was written rather than as it could be restated.

Companion note: [m2-filesystem-model.md](m2-filesystem-model.md), written before
the log, which records what the simulated filesystem proves and what it only
asserts. Nothing here escapes the caveat in that note's final section.

## The Claim Under Test

From [roadmap.md](../roadmap.md):

> 1M appends/sec single producer, p99 under 2ms, and recovery from a crash at
> any injected point loses nothing acknowledged as durable.

Three claims in one sentence, and they did not all survive together.

## What Was Measured

Machine: darwin/arm64, Apple M2 Max. Full run committed to
[bench/log.txt](../../bench/log.txt), reproduced with `task bench:log`.
Benchmarks run on the host rather than in the container: the container is
authoritative for correctness, but on macOS it measures a virtualised filesystem
inside a VM, which is neither this machine nor a Linux server.

### Throughput, 64 byte records, single producer

| Mode | One record per call | 256 records per call |
| --- | --- | --- |
| `os` (never syncs) | 1.47 µs — 680,000/sec | 46 ns — 21.6M/sec |
| `batch` (1,024 records per sync) | 6.9 µs — 145,000/sec | 4.5 µs — 222,000/sec |
| `sync` (every record) | 4.3 ms — 233/sec | 18 µs — 55,000/sec |

**The 1M appends/sec claim holds in exactly one reading and fails in the
others.** Batched calls in `os` mode reach 21.6M records/sec. The default
`batch` mode reaches 222,000/sec and `sync` 55,000/sec. The roadmap named
neither a durability mode nor a call shape, so the honest verdict is that the
claim was underspecified and is met only where nothing is being made durable.

The gap is entirely fsync. Once records are batched into one write, `os` mode
spends 46 ns per record and `batch` mode spends 4.4 µs of amortised
`F_FULLFSYNC` on top of it. A Linux number will differ and this repository does
not have one yet.

### Latency

p99 is 3.8 µs in `os` and 6.9 µs in `batch`, both far inside the 2 ms budget.
`sync` mode misses it at 8.1 ms, because Go's `os.File.Sync` issues
`F_FULLFSYNC` on darwin and waits for the drive's own cache. **The p99 claim
holds in the two modes a caller would ship, and fails in the strictest one for a
platform reason rather than a design one.**

### Reads

| | 64 B records | 1 KiB records |
| --- | --- | --- |
| Cursor scan | 856 ns, 3 allocs | 1,118 ns, 3 allocs |
| Random by offset | 29.0 µs, 100 allocs | 3.6 µs, 7 allocs |

A streaming consumer must use a `Reader`. A random read searches the sparse
index and decodes forward from the nearest entry, and at a 4 KiB index interval
finding one 64 byte record means decoding about 45 of them.

### Recovery

Opening a log scans only the active segment, at about 1.2M records/sec: a
100,000 record segment reopens in 84 ms. The first read into a *sealed* segment
costs a full scan of it — 55 ms for 4 MiB — because this implementation
deliberately keeps no index file.

## The Crash Claim, And What It Cost To Believe

`spine sim crash-matrix` runs a seeded workload against a filesystem that dies
at a chosen operation, then reopens the disk and asserts what survived: every
record acknowledged by a `Sync` that returned, every commit a group was told was
durable, the newest snapshot, offsets that ascend and stay in bounds, no more
bytes discarded than the write in flight, and a log that still appends
afterwards.

**300 seeds, 18,760 crash points, 419 compactions dropping 3,154 records, 354
snapshots, no loss.** Those coverage counters are asserted by the test suite,
because a matrix that quietly stopped compacting would report zero failures and
mean nothing by it.

The claim is met **in simulation**. It is not met on hardware, because nothing
here has yet observed real power loss — see the final section of
[m2-filesystem-model.md](m2-filesystem-model.md).

### It found a real bug on its first run

Seed 1 failed at 45 of its 51 crash points. The log fsynced segment *files* and
never the *directory* naming them, so a crash removed an entire segment along
with records whose `Sync` had already returned — and recovery then reported a
healthy empty log, which is the worst shape a loss can take: it is
indistinguishable from a log nobody ever wrote to.

The bug was reachable only because the simulated filesystem models the POSIX
rule that a file needs its directory entry synced to exist after a crash. That
rule was written into `sim.FS` in M2's first half, before the log existed to
disagree with it. **A model built to agree with the implementation would have
found nothing here**, which is the argument for the ordering M2 used.

The seed is committed as [seeds/0001.md](../../seeds/0001.md), failing, before
the fix.

### The fix has a price, and the benchmark shows it

Syncing the directory on every segment creation costs one `F_FULLFSYNC` per
segment. Amortised over a 64 MiB segment that is invisible per record, but it is
visible in the batched `os` figure: 10.7 µs to 11.9 µs per 256 record call,
about 10%, which is 4.3 ms of fsync spread across the ~2,900 calls a segment
holds. Recorded rather than tuned away. `os` mode's contract is that it never
syncs for durability, and this sync is not for durability — it is what keeps a
crash from leaving a hole in the middle of the segment list instead of a torn
tail at the end.

### The second finding was the harness

Seed 17 reported durable data loss at 13 crash points. The log was correct:
`CompactAll` compacts one segment at a time, and when the crash landed inside
the second, the call returned an error for a run whose first segment had already
been compacted and swapped in. The workload credited none of it and then
demanded records compaction had legitimately removed.

It is committed as [seeds/0017.md](../../seeds/0017.md) saying plainly that it
was the checker, because a checker wrong in that direction hides every later
failure of the thing it checks.

## What Changed As A Result

- **A directory sync after creating a segment or the commits log**, in every
  durability mode. Seed 0001.
- **One `write(2)` per `Append` call instead of one per record.** The first
  benchmark run found the syscall, not the fsync, was where a batch's time went;
  batching the fsync had amortised durability and left the syscall count
  untouched. 34× on batched `os` throughput.
- **No index file on disk**, against what `log-design.md` describes. An index is
  a second thing a crash can tear and is reconstructible from the segment it
  indexes. The price is the 55 ms cold-segment scan measured above, and the
  decision is revisitable the moment a workload pays it often.
- **Sealed segments open read-only.** A torn tail there was not left by a crash
  mid-append, so truncating it would delete acknowledged records. It is reported
  instead.
- **A commit syncs before it returns, whatever the log's durability mode.** An
  unsynced commit breaks redelivery in the wrong direction: the consumer resumes
  *past* records it never processed.
- **The read path checks that offsets ascend rather than increment.** Compaction
  preserves offsets while dropping records, and the three places that assumed
  contiguity — recovery scan, index lookup, cursor — would each have called a
  compacted segment corrupt. Gap tolerance cost random reads 3 allocations and
  about 2%: a lookup must now decode the record it lands on to learn whether it
  is the one asked for.

## What Is Deliberately Not Done

- **Compaction is per segment.** A key whose newest record lives in a later
  segment keeps its older copy here, so this reclaims less than a whole-log pass
  would. A cross-segment pass has to read every later segment to know what is
  superseded; the crash safety and the gaps were the parts worth testing first.
- **The commits log is never compacted.** It grows by one record per commit, and
  the history is currently the feature: it is what answers "when did this
  consumer fall behind?".
- **Stale `.compacting` files** left by a crash are removed when the same
  segment is compacted again, and not before. They are inert — recovery ignores
  any name that is not a segment — but a long-lived log that crashes often
  accumulates them.
- **No partitions, no replication, no wire protocol.** Each is deferred with the
  milestone that would earn it.

## Next Milestone's Riskiest Assumption

M3 is the simulation harness: fault catalogue, invariant checks after every
step, automatic minimization, nightly sweep. Its kill gate says the harness must
find at least one real bug the existing tests missed, and M2 has already shown
that a crash matrix can do that.

The riskiest assumption is that **the fault model is rich enough to be worth
sweeping.** M2's matrix injects exactly one fault: the machine stops between two
filesystem operations. `log-design.md` lists eight failure modes and this
covers three of them. Disk full mid-append, a bit flip in a header or a length
field, a clock moving backwards, and a group committing ahead of what it
durably processed are all still unexercised outside hand-written unit tests. A
nightly sweep over one fault type will produce a large number of green runs and
a feeling of safety that the eight-item list says is not earned.

The second assumption worth naming: **minimization is not built, and the current
approach does not scale to where M3 is going.** M2 finds crash points by
exhaustive enumeration, which works because a 40 step workload has about 50 of
them. A 400,000 step simulation has no such luxury, and an unminimized failing
seed is a bug report nobody opens. `seeds/README.md` already says this; M3 has
to make it true.
