# M3 — The Simulation Harness

**Date:** 2026-08-12
**Status:** finished. The kill gate held, and half of what the harness found was
in the harness.

## The Claim Under Test

From [roadmap.md](../roadmap.md):

> The harness finds at least one real bug the existing tests missed. If it finds
> nothing across a nightly sweep, the fault model is too gentle.

## What Was Built

The disk and clock half of the catalogue in
[simulation-testing.md](../simulation-testing.md): crash, write error, short
write, fsync failure, bit flip, and a clock that jumps backwards. Network faults
are absent because the spine has no network, and a catalogue listing faults
nothing can experience would look more thorough than the harness is.

A run is a `Config`: seed, steps, faults. Those three fields reproduce it on any
machine, which is what lets a corpus entry *hold* a failure rather than describe
one. Faults are written `kind@position[:arg]`, because a seed file is read by
people at least as often as by the parser.

- **Swarm testing** decides which fault kinds are in play per run rather than
  only when they fire. A quarter of runs get each kind, so most carry one or
  two and a few carry none. Uniform probabilities explore a narrower space than
  they appear to, because every run then looks like the average run.
- **Invariants after every step**, not only after recovery. A log that goes
  wrong at step 3 and looks fine at step 40 is most of the ways a log goes
  wrong.
- **Minimization** removes faults one at a time and then shortens the run by
  binary search, matching the failure by message so that a shorter run failing
  for an unrelated reason is not mistaken for a smaller repro. Seed 8 shrank
  from 40 steps and two flips to 3 steps and one.

## The Gate: One Real Bug, Found On The First Sweep

**A single flipped bit in a record's offset field was accepted.** The reader
returned offset 536870912 from a log holding `[0, 5)`; recovery takes a
segment's next offset from the last record it scanned, so one flipped high bit
moved the log's tail to 4611686018427387911 and every later append was assigned
an offset from there.

The offset field sat outside CRC coverage deliberately, and the reasoning was
sound when it was written: a reader always knows which offset comes next, so
`DecodeAt` compares and a flip is caught. **M2's compaction work invalidated
that reasoning and nothing noticed.** Once records can be missing, a scan can
only require offsets to *ascend*, so a flip landing above the expected offset
passes. The comment on `readRecordFrom` names this exact case as the one it
cannot see — written down, and then left alone.

The existing unit test even pinned the number of flips invisible to the CRC,
with a comment saying that widening coverage later should show up as a failing
count rather than as nothing. It did. What no existing test did was notice that
the *reason* for the exclusion had expired, because that reason lived in one
file and the change that killed it lived in another. A random bit in a random
byte does not care which file an argument lives in.

Recorded as [seeds/0008.md](../../seeds/0008.md), failing, before the fix in
`ff1cded`.

## Two Findings That Were Contracts, Not Bugs

**A commit that returns an error may still take effect.** `Group.Commit` writes
its record and then syncs it; a failed sync leaves the record on a disk that any
restart short of a power cut still reads. The log cannot promise otherwise
without a second write to confirm the first, doubling the cost of every commit
to close a window at-least-once delivery already covers. So the invariant was
made *precise* rather than comfortable: a group never resumes at an offset
nobody asked it to. A failed commit can only move a group to a position the
consumer itself named, and therefore to one it had already processed.
[seeds/0072.md](../../seeds/0072.md).

**A compaction that returns an error may already have happened**, for the same
reason one layer up: the rename precedes the directory sync.
[seeds/1889.md](../../seeds/1889.md).

That is three appearances of one shape, counting the partial `CompactAll` in
[seeds/0017.md](../../seeds/0017.md). It is now stated as a property of the
design rather than fixed a third time: **an operation that renames or writes
before it syncs can fail and still have happened.** Both `Commit` and `Compact`
say so in their contracts.

## Half The Findings Were The Harness

Of six corpus entries, three are the checker: [0017](../../seeds/0017.md),
[1870](../../seeds/1870.md), and the invariant correction in
[0072](../../seeds/0072.md). Each was a false positive, and each is kept and
labelled as such in its first line.

That ratio is worth stating plainly rather than quietly rounding down. A checker
that blames the wrong component is worse than one that misses: it sends someone
to read code that is working, and it teaches everyone to skim the output — which
is how a harness stops being used. The pattern in all three was the same:
**a weakening applied for the wrong reason.** A record missing after a bit flip
was held to compaction's promise; a compaction that had happened was credited as
if it had not. The rule that came out of it is that every branch in the checker
now names what licenses it, and corruption is tested before compaction because a
record corruption removed has no reason to satisfy compaction's rule.

## What Was Measured

**2,000 seeds, 0 failures**, after the fixes. Injected across them: 507 crashes,
960 write errors, 905 short writes, 903 fsync failures, 1,010 bit flips, 951
clock jumps. Exercised in the sampled clean runs: 174 compactions dropping 1,362
records, 149 snapshots.

The M2 crash matrix is kept alongside the sweep, because exhaustive enumeration
of one fault is a different instrument from random sampling over many: it proves
a property at every point rather than sampling until bored.

Coverage is reported as a first-class number by every command, and asserted in
the test suite. A sweep that quietly stopped compacting would report zero
failures and mean nothing by it.

## The Corpus Was Already Rotting

A fault's position is an index into the run's stream of filesystem operations.
Change the log's I/O pattern and every stored seed moves to a different point.

Seed 0001 was recorded against a run of 51 operations. That run is 84 operations
now — the directory sync that fixed it, plus the compaction and snapshot actions
added later, both lengthened the stream. **`crash@7` is no longer the crash that
failed.** The seed passes, and it has not proven anything since the day it was
committed.

This was found by looking rather than by a test, which is the uncomfortable
part: nothing in the harness would ever have reported it. Seed files now carry
`ops`, `task sim:corpus` prints `drifted` when the count has moved, and the two
affected seeds say so at the top of their files. It is a warning rather than a
failure, because a seed that fires somewhere else is still worth running and is
no longer evidence about the bug named in its file.

Making a seed *pin* its meaning — replaying against a recorded operation
signature rather than an integer — is M4 work, and it is named below.

## What Is Deliberately Not Covered

- **No network faults.** There is no network. They arrive with the milestone
  that adds one.
- **No disk-full distinct from write error, no read errors, no slow disk.** The
  first two would be a few lines and were left until something depends on
  telling them apart; a slow disk means nothing without concurrency.
- **No process restart with a stale snapshot, and no multi-process runs.** Both
  need the scheduler to own more than one actor than it does today.
- **A group committing ahead of what it durably processed** is in
  `log-design.md`'s list and is not injected. It is a consumer's bug rather than
  the log's, and injecting it would make every run fail for a reason the log did
  not cause. What the log owes here — that it never advances a group to an
  offset nobody asked for — is asserted.
- **One workload.** Every seed drives the same shape of run. A second workload
  would explore states this one cannot reach, and there is no evidence yet about
  how much that matters.

## Next Milestone's Riskiest Assumption

M4 is replay devtools: scrub, branch, projection diff, step-a-seed, and a
recorded walkthrough of debugging a real failure by seed.

The riskiest assumption is that **a seed's meaning is stable enough to build a
debugger on.** M3 ended by discovering that it is not: fault positions are
indices into a stream that any change to the log's I/O pattern shifts, and the
corpus had been quietly decaying for a milestone before anyone looked. A tool
that scrubs to "the divergent event" for a seed whose faults now fire elsewhere
is a tool that confidently shows the wrong thing, which is worse than no tool.
`ops` makes the drift visible; it does not make a seed replay what it used to.
M4 has to decide whether a seed pins an operation signature, or whether fault
positions become logical (this append, this sync) rather than ordinal.

The second assumption, unchanged from M2 and still unpaid: **none of this has
met real power loss.** Everything above is evidence about a model of a disk. The
trigger in [m2-filesystem-model.md](m2-filesystem-model.md) — a fault-injecting
block device before the first published durability claim — has not fired yet
because nothing has been published yet. M5 publishes.
