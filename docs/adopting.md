# Adopting The Spine

For a project embedding the log. It states what the spine promises, what it
deliberately does not, and the handful of behaviours that surprise people —
every one of which was found by the simulation or by cutting the power, and each
links to the seed that found it.

> **This is v0.** The surface will break. Pin a version, expect to move.

The public surface is deliberately small: `log` exposes the log, its reader,
consumer groups, snapshots, compaction, and the errors you match on. Segments,
record framing, and file naming are not part of it — they are how the log is
built, not what it offers, and keeping them private is what lets them change.

## What It Is

An append-only log, in your process, as a library. Not a server, not a broker,
no network. One writer.

```go
import (
    "github.com/DamoDCoder/event-spine/core"
    "github.com/DamoDCoder/event-spine/log"
    "github.com/DamoDCoder/event-spine/runtime"
)

fs, err := runtime.NewFS("/var/lib/yourapp/log")   // a real directory
l, recovery, err := log.Open(fs, log.Config{})     // batch durability by default
defer l.Close()
```

`recovery` describes what opening found. Read the next section before ignoring
it.

## The Five Things That Surprise People

### 1. Recovery hands you a decision, not a verdict

`Open` truncates a torn tail — the normal result of a crash mid-append — and
tells you it did. It does **not** decide what a *corrupt* tail means:

```go
if recovery.Corrupt != nil {
    // Bytes were present and wrong. The log truncated them and kept going.
    // "Carry on" and "stop, this disk is lying" are both defensible and only
    // you know which.
}
```

`recovery.Discarded` is how many bytes went. A crash mid-append discards at most
the write that was in flight.

### 2. A failed commit may still have happened

`Group.Commit` writes its record and then syncs it. If the sync fails, the
record is already on the disk, and a restart short of a power cut reads it back.

The log cannot promise a failed commit did not take effect without doubling the
cost of every commit. What it does promise: **a group never resumes at an offset
nobody asked it to.** A failed commit can only move a group to a position you
named, and therefore to one you had already processed.

Found by one injected fsync failure — [seeds/0072.md](../seeds/0072.md).

### 3. The same is true of compaction

`Compact` renames the compacted segment into place and *then* syncs the
directory. A failure reported by the sync is a failure to make the swap durable
rather than a failure to make it. Do not assume a segment is unchanged because
compaction returned an error. Nothing is lost either way: compaction only
removes records a newer record for the same key supersedes.

[seeds/1889.md](../seeds/1889.md).

### 4. Offsets are stable while their records survive — and not across data loss

Compaction leaves **gaps**: it preserves offsets and drops records, so a
committed offset means the same record afterwards. `Read` on a dropped offset
returns `ErrNotFound`; a `Reader` walks past it to the next surviving record.

But if corruption forces recovery to truncate, the tail moves *back*, and later
appends are assigned offsets that different records used to hold. An offset you
were holding across that event points at something else. Systems that solve this
use leader epochs; the spine does not.

[seeds/0309.md](../seeds/0309.md).

### 5. Delivery is at-least-once, and that is the contract

Reading does not move a consumer group. Committing does, and it is explicit:

```go
g, err := l.Group("projections")
r, err := g.Reader()          // resumes at the committed offset
// ... process records ...
err = g.Commit(offset)        // only when your projection is durable
```

A consumer that crashes between reading and committing sees those records again.
Your projection must be idempotent. That is the whole deal, and it is the reason
there is no exactly-once anything.

## Ownership: One Goroutine

**The log and its readers are not safe for concurrent use.** No mutex, by
design: this repository's premise is that a run is a function of its inputs, and
lock ordering under contention is not.

Own the log from a single goroutine and let the rest of your program talk to it
through whatever you already use for that — a channel, a bus, a command queue.
If you find yourself wanting a mutex, put the log behind an owner goroutine
instead; the ordering then comes from your queue, which you can replay.

## Choosing A Durability Mode

| Mode | fsync | Loses on a power cut | Use |
| --- | --- | --- | --- |
| `log.Sync` | every `Append` call | nothing acknowledged | correctness demos, financial-shaped work |
| `log.Batch` | every `SyncRecords` (default 1024) | up to that many records | **the default** |
| `log.OS` | never, between rolls | anything not written back | throughput ceiling only |

Three things worth knowing:

- The decision is made **once per `Append` call**, not per record. A thousand
  records in one call costs one fsync in `Sync` mode.
- A segment roll syncs the outgoing segment in **every** mode, `os` included.
  "Sealed" has to mean "durable" — recovery leans on it — and two power cuts
  were needed to arrive at that sentence ([power-loss.md](decisions/power-loss.md)).
- `os` mode is for measuring a ceiling. It is not a mode to ship on.

## Snapshots And Replay

```go
snap, r, err := l.Restore()   // newest snapshot + a reader positioned after it
// load snap.State into your projection, then replay from r
```

`Snapshot.Offset` is the first record **not** folded into the state. `Restore`
exists so nobody has to get that off-by-one right by hand.

Snapshot cadence is yours to choose. The log never runs a background goroutine
and never starts a timer — nothing happens unless you call it, which is what
makes a run reproducible.

## Testing Your Project Against It

This is the part worth adopting even if the log were mediocre. `sim` gives your
own tests a deterministic world:

```go
fs := sim.NewFS()              // a crashable filesystem
clock := sim.NewClock()        // logical time, no wall clock
src := sim.NewSource(seed)     // seeded randomness

l, _, err := log.Open(fs, log.Config{Clock: clock})
// ... drive your projection ...
fs.Crash()                     // power cut: unsynced data is gone
// ... reopen and assert what survived ...
```

`fs.Crash()`, `fs.CrashExtend()`, and `fs.CrashTorn(percent)` are the three
shapes a crash takes: nothing unsynced survives, the file keeps its length with
zeros in the gap, or a prefix of the unsynced bytes lands. The middle one is
what ext4 actually does and the simulator could not produce it for four
milestones — a bug hid in the difference.

## What The Spine Does Not Do

Each is deferred deliberately, and each would need its own milestone:

- **No replication.** One machine, one copy.
- **No wire protocol.** In-process only; a consumer is Go code in your binary.
- **No partitions**, no consumer group rebalancing across processes.
- **No exactly-once semantics or transactions.**
- **No background work.** No goroutines, no timers. Compaction and snapshots
  happen when you call them.
- **No cross-segment compaction.** A key whose newest record is in a later
  segment keeps its older copy.

## What Has Actually Been Verified

So you can judge the claims rather than take them:

- 1,000 seeds produce one projection digest across three platforms
  ([m1](decisions/m1-deterministic-core.md)).
- Crash at every filesystem operation, in all three crash shapes, loses nothing
  acknowledged as durable ([m2](decisions/m2-owned-log.md)).
- 1,000 swept seeds across three durability modes, two workloads, and 239 shapes
  ([m3](decisions/m3-simulation-harness.md)).
- Real power cuts under ext4 and xfs, with a control that must fail
  ([power-loss.md](decisions/power-loss.md)).
- Measured against Kafka, both halves published
  ([m5](decisions/m5-kafka-comparison.md)).

**Not verified:** write reordering. Every write in those tests reaches the device
in the order it was issued; real controllers do not promise that. If you publish
a durability claim that depends on barrier ordering, that gap needs closing
first.
