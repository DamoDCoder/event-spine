# Owned Log Design

Why write a log when Kafka exists: Kafka is where partitioning, offsets, consumer
groups, replication, and backpressure *go to be invisible*. Operating it teaches
configuration. Writing one teaches the system. This log is small enough to finish
and real enough to break, which is the whole point.

Kafka is not abandoned. It becomes an implementation behind the same interface,
and the comparison between the two becomes the artifact.

## Scope

In scope for v1:

- Append-only segmented storage on local disk.
- Monotonic offsets, per-record CRC, length-prefixed framing.
- Multiple independent readers and named consumer groups with committed offsets.
- Snapshots and compaction.
- Crash recovery to the last durable record.
- Configurable durability: fsync every record, every N records, or every interval.

Out of scope for v1, each deferred with intent:

- Replication and leader election. Single writer, single node. If this arrives, it
  arrives as its own milestone with its own kill gate, and Raft is the reference.
- Network protocol. v1 is an in-process library. A wire protocol is only needed
  when a second process needs the log, and that is the milestone that earns it.
- Tiered or object storage.
- Exactly-once delivery semantics. At-least-once plus idempotent projections is
  the contract, and the projection is where duplicates die.

## Record Format

Fixed header, variable payload. Little-endian.

```text
offset  size  field
0       8     record offset (uint64)
8       4     total record length in bytes, header included (uint32)
12      4     CRC32C of everything after this field
16      8     logical timestamp (uint64, from the injected clock, never wall time)
24      2     schema version (uint16)
26      2     key length (uint16)
28      n     key bytes
28+n    m     payload bytes
```

Design notes worth defending later:

- **CRC covers the payload and the trailing header fields, not the length.** The
  length has to be trusted before the CRC can be checked, so a corrupt length is
  detected by the record failing to parse at the expected boundary, not by CRC.
- **The timestamp is logical, taken from the injected clock.** A wall-clock
  timestamp in a record makes replay non-deterministic, which would defeat the
  central claim of the entire project.
- **Schema version is per record, not per segment.** Mixed-version segments are
  the normal case during a rolling schema change and pretending otherwise creates
  a migration problem that did not need to exist.

## Segments

- A segment is a file named for the offset of its first record: `00000000000000000000.log`.
- A segment rolls when it exceeds a configured size (default 64 MiB) or age.
- Each segment has a sparse index file mapping offset to file position, one entry
  per configured byte interval (default 4 KiB), loaded into memory on open.
- The active segment is append-only and never rewritten. Sealed segments are
  immutable, which is what makes compaction and snapshotting safe to do
  concurrently with appends.

Lookup of offset `o`: binary search the segment list for the owning segment,
binary search its sparse index for the greatest indexed offset `<= o`, then scan
forward. Scan distance is bounded by the index interval, so lookup cost is
bounded by configuration and not by log size.

## Offsets And Consumer Groups

- Offsets are assigned by the writer, monotonic, and gapless within a partition.
- A reader is a cursor: an offset plus a segment handle. Readers are cheap and
  there can be many.
- A consumer group is a named committed offset stored in a separate, small,
  fsynced offsets log. Group commits are themselves records, so the commit history
  is replayable — useful for answering "when did this consumer fall behind?".
- Committing is explicit. The group offset advances when the consumer says the
  projection is durable, not when the record was read. This is the difference
  between at-least-once and at-most-once, and it is a one-line difference that
  every project in the backlog should be able to demonstrate on purpose.

## Partitions

v1 supports partitions as independent logs under one directory, with a key-hash
partitioner. This exists in v1 for one reason: ordering guarantees only hold
within a partition, and every project in this backlog has a natural key
(organism, match, tenant, symbol). Deferring partitions would let those projects
develop a false assumption of global ordering that would be expensive to unpick.

## Snapshots And Compaction

- A **snapshot** is a serialized projection plus the offset it was folded to.
  Restoring means loading the snapshot and replaying from that offset forward.
  Snapshot cadence is a tuning knob, and the tradeoff — snapshot cost against
  recovery time — is a graph worth publishing.
- **Compaction** retains only the newest record per key in sealed segments,
  writing a new segment and atomically swapping it in via rename. Offsets are
  preserved, so compaction creates gaps. Readers must tolerate gaps, and the
  simulation harness must inject them, because "reader assumes offsets are
  contiguous after compaction" is the exact bug this design invites.

## Durability And Recovery

Durability modes, selectable per log:

| Mode | fsync | Loses on crash | Use |
| --- | --- | --- | --- |
| `sync` | every record | nothing | correctness benchmarks, financial-shaped demos |
| `batch` | every N records or T interval | up to N records or T of writes | default |
| `os` | never, OS flushes | up to the page cache | throughput ceiling measurement only |

Recovery on open:

1. Find the active segment, the highest-numbered file.
2. Scan forward from the last index entry, validating length framing and CRC.
3. Stop at the first record that fails to parse or fails CRC.
4. Truncate the file to the end of the last valid record.
5. Report the recovered offset and the number of bytes discarded.

Step 4 is the one that requires care and the one the fault injector should attack
hardest. A torn final record is the normal outcome of a crash mid-append, and
truncating it is correct. Truncating a *valid* record because of an unrelated bug
is silent data loss, so recovery reports what it discarded and the simulation
asserts the discarded bytes never exceed the in-flight write.

## Interface

The interface is deliberately narrow, so Kafka can sit behind it without
distorting either side.

```go
type Log interface {
    Append(ctx context.Context, records []Record) ([]Offset, error)
    Reader(ctx context.Context, from Offset) (Reader, error)
    Group(ctx context.Context, name string) (Group, error)
    Snapshot(ctx context.Context, offset Offset, state []byte) error
    Close() error
}
```

Anything Kafka can do that this interface cannot express is a feature the projects
should not depend on until a project genuinely needs it.

## Benchmarks To Publish

Each is a graph, not a sentence. Committed results, so regressions show as diffs.

- Append throughput and p50/p99/p999 latency against record size, across the three
  durability modes.
- Recovery time against log size and snapshot cadence.
- Compaction cost and the read-amplification it removes.
- Consumer lag under a deliberately slow consumer, showing where backpressure
  lands and what it costs.
- The headline comparison: this log against Kafka on an identical workload, with
  the operational surface of each stated honestly alongside the numbers.

## Failure Modes The Simulation Must Inject

Listed here rather than in the simulation document because they are properties of
this design specifically:

- Crash between write and fsync.
- Crash mid-record, producing a torn tail.
- Crash during compaction, after the new segment is written and before the rename.
- Disk full on append and on compaction.
- Bit flip in a payload, in a header, and in the length field.
- Clock moving backwards between records.
- A consumer group committing an offset ahead of what it durably processed.
- Reader positioned at an offset that compaction has since removed.
