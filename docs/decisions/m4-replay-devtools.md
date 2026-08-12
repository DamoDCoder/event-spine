# M4 — Replay Devtools

**Date:** 2026-08-13
**Status:** finished. The gate was met, and the first version of the tool failed
it silently — which is the part of this milestone worth reading.

## The Claim Under Test

From [roadmap.md](../roadmap.md):

> A bug can be diagnosed from seed to root cause without adding a single print
> statement.

The artifact is [walkthrough-replay.md](../walkthrough-replay.md), and
`task demo:replay` reproduces it: a scratch worktree with one commit reverted,
the bug back, and the same four commands run against it.

## What Was Built

`spine replay` over the harness, with three views of one recorded run:

| | |
| --- | --- |
| default | the scrub: one line per step — offsets, records, segments, group positions, snapshot, and a digest folding every record |
| `--ops` | the disk's side: every filesystem operation, with the faults that fired marked on the operation they landed on |
| `--at N` | one step in full: the state after it and the operations inside it |
| `--diff` | the counterfactual: this seed with its faults against the same seed without them, and the first step where they part |

The per-step digest is the projection in "projection diff". Two runs are
compared on what the log *means* — the records it holds — rather than on the
bytes underneath, so a divergence names a step rather than a byte offset.

`--diff` against the fault-free run is what turns "this seed fails" into "these
two runs perform identical operations and agree until step 2, where one holds
five records and the other holds one". That sentence is most of a diagnosis.

## M3's Risk, Paid Down

M3 ended by finding that the corpus decays: a fault's position is an ordinal in
the stream of filesystem operations, so any change to the log's I/O pattern
moves every stored seed somewhere else, silently. Seed 0001 was recorded against
51 operations in a run that is 84 now.

Counting operations catches a stream that got longer. It does not catch one the
same length in a different order — swapping two syncs, say — so a run now
carries an FNV signature over the whole stream, seed files record it, and
`task sim:corpus` reports drift on either. Seeds cannot be made to mean what
they used to; they can be made to say when they have stopped.

## The Tool Failed Its Own Gate First

The first walkthrough ran against the reintroduced bug and printed:

```
step  2  offsets [0,5)  records   5  ...
every invariant held
```

while the corpus, running the same configuration, reported the failure. **The
replay was debugging a run that never happened.**

Reading the log after every step opens files, and a fault's position is an
ordinal in the stream those opens joined. Every fault scheduled after the first
observation moved. The tool was displacing the thing it was built to show.

Suppressing the operation count fixed the arithmetic and not the problem, and
finding out why was the useful part. Opening a log also:

- **creates the commits file** when a consumer group is asked for, so the
  workload's later commit skips a create it would otherwise have performed;
- **caches segment handles**, so a later lookup skips its open;
- **truncates a torn tail** during recovery, which is a write.

Each changes what the run does next. Observation with side effects is not
observation. The tools now inspect a **copy** of the disk — `sim.FS.Clone` — and
the run never learns it was watched.

That changed what a step's state means, and the change is an improvement worth
stating: the scrub shows **what a restart would find at that step**, not what
the running process believes. A record appended and not yet synced is on the
disk and appears; a torn tail is truncated exactly as recovery would truncate
it.

### The test that was supposed to catch it

There was one. It compared `Replay`'s outcome against `Run`'s for a single
configuration — and that configuration passed either way at `HEAD`, because the
bug it referenced was already fixed. A test that agrees with anything.

The replacement compares the **operation stream itself** — count and signature —
across five configurations including every seed in the corpus. It fails loudly
on any perturbation, whether or not a bug happens to be present to expose it.
The lesson generalises past this milestone: a test over outcomes only tests the
paths where outcomes differ, and a tool's correctness is usually a property of
what it *did*, not of what it *reported*.

## What The Walkthrough Shows, And What It Does Not

Four commands, no print statements, from a corpus failure to "a single flipped
bit landed in the offset field, which the checksum did not cover because
compaction had quietly invalidated the reason for excluding it".

Two honest limits:

- **The bug was already known.** These tools were used on a failure the harness
  had found and named in M3. That demonstrates they work on a known bug; it does
  not demonstrate they would have led someone to an unknown one unaided.
- **The artifact is written, not recorded.** The roadmap asked for a recorded
  walkthrough. What exists is a transcript plus `task demo:replay`, which
  regenerates every block of it. That is more useful than a video and less
  compelling as a demo, and both halves of that are true.

## What Is Deliberately Not Built

- **No branching.** The roadmap listed scrub, branch, projection diff, and
  step-a-seed. Branching — resuming a run from step N with a different fault —
  is not here. `--diff` covers the case it was wanted for, which is comparing a
  failure against its counterfactual, and a second execution model would be a
  second thing to keep honest for a use nothing has asked for yet.
- **No interactive stepping.** The scrub prints every step; a prompt would add a
  mode that cannot be captured in a walkthrough or replayed in CI.
- **No time travel inside a step.** Faults are ordinals over filesystem
  operations, and the finest resolution the tools offer is the operation. A bug
  living between two operations of the same step is not addressable, and nothing
  has needed it.

## Next Milestone's Riskiest Assumption

M5 is the Kafka comparison, and the roadmap is explicit that publishing only the
favourable half is worth nothing.

The riskiest assumption is that **the two systems can be measured on a workload
that is fair to both**. Everything measured so far runs against `sim.FS` or a
local disk in one process, with durability modes this repository defined.
Kafka's equivalent knobs — `acks`, `flush.ms`, replication factor — are not the
same knobs, and the honest comparison depends entirely on which pairs are
declared equivalent before the numbers are taken. Choosing them afterwards is
how benchmarks lie, and choosing them in advance is the work.

The second, unchanged since M2 and now overdue: **nothing here has met real
power loss.** [m2-filesystem-model.md](m2-filesystem-model.md) set the trigger at
the first published durability claim. M5 publishes, so the fault-injecting block
device is no longer deferrable — a p99 or a "loses nothing acknowledged as
durable" claim next to Kafka's numbers, resting on a simulated disk, would be
exactly the unfalsifiable half the roadmap warns about.
