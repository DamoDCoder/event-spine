# M5 — The Kafka Comparison

**Date:** 2026-08-14
**Status:** finished. The protocol was fixed in
[m5-comparison-protocol.md](m5-comparison-protocol.md) before the backend
existed; this note reports what came back, including the prediction that was
wrong and the unfairness the protocol did not catch until the numbers exposed it.

Full run: [bench/compare.txt](../../bench/compare.txt), reproduced with
`task bench:compare`.

## The Claim Under Test

> The owned log lands within 2x of Kafka's throughput. If it is 10x slower,
> publish that honestly and say what Kafka is doing that this does not.

The spine is not slower. It is faster in every mode, by between 1.4x and 23x —
which turns out to say more about where the two systems put their process
boundary than about either log.

## The Numbers

Both in containers on one machine, Apache Kafka 3.9.0 pinned by digest, one
partition, one replica, no compression, 50,000 records per configuration, three
runs per configuration, median reported.

**64 byte records**

| Mode | spine records/sec | kafka records/sec | ratio | spine p99 | kafka p99 |
| --- | --- | --- | --- | --- | --- |
| `sync` | 2,222 | 1,650 | 1.35x | 1.8 ms | 2.1 ms |
| `batch` | 1,679,738 | 423,269 | 4.0x | 1.7 ms | 10.3 ms |
| `os` | 17,525,154 | 744,590 | 23.5x | 48 µs | 4.4 ms |

**1 KiB records**

| Mode | spine records/sec | kafka records/sec | ratio | spine p99 | kafka p99 |
| --- | --- | --- | --- | --- | --- |
| `sync` | 2,002 | 1,443 | 1.39x | 1.9 ms | 2.3 ms |
| `batch` | 597,461 | 259,823 | 2.3x | 2.5 ms | 9.1 ms |
| `os` | 2,300,918 | 405,840 | 5.7x | 167 µs | 13.0 ms |

**Sequential read, records/sec**

| Size | Mode | spine | kafka |
| --- | --- | --- | --- |
| 64 B | `batch` | 1,129,930 | 3,822,459 |
| 64 B | `os` | 1,110,990 | 4,199,504 |
| 1 KiB | `batch` | 735,643 | 755,281 |
| 1 KiB | `os` | 744,154 | 640,385 |

> **Re-run on 2026-08-15**, after rolling was made to sync in every mode. Every
> ratio holds: 1.39x, 3.3x, and 20.6x at 64 bytes against the 1.35x, 4.0x, and
> 23.5x above, which is run-to-run variance rather than a change.
>
> The `os` row is worth a sentence, because `bench/log.txt` moved 29% on the
> same change and this did not. The comparison writes 50,000 records of 64 bytes
> into a log with the default 64 MiB segments — about 3 MB, so it never rolls
> and never pays the new sync. The benchmark that moved writes enough to roll
> repeatedly. Both numbers are honest and they measure different amounts of
> segment turnover, which is the kind of thing a committed results file is for.

## The Protocol Was Wrong, And The Numbers Said So

The first complete run reported the spine at 317,448 records/sec against Kafka's
98,518 in `sync` mode — a 3.2x win. That number was wrong, in the spine's
favour, by the batch size.

The protocol paired spine `sync` with Kafka's `flush.messages=1` on the grounds
that *both fsync before acknowledging every record*. That is true of Kafka. It
is not true of the spine: the spine decides durability **once per `Append`
call**, so a 256-record call in `sync` mode is one fsync against Kafka's 256.

Handing over one record at a time in the `sync` rows makes "every record"
literally true on both sides, and the result changes character completely:
**2,222 against 1,650 — 1.35x, not 3.2x.** Both systems are waiting on the same
fsync and the disk sets the pace.

This is the failure the protocol was written to prevent, arriving one layer
below where it was looking: the equivalence was declared in advance and honestly,
and the implementation of one side of it did not match the words. Writing the
protocol first is what made it findable — there was a sentence to check the code
against.

## The Predictions, Scored

Recorded before the backend existed:

**1. The spine beats Kafka on `os`-mode append by more than 5x, and most of it
is the process boundary. — Held.** 23.5x at 64 bytes, 5.7x at 1 KiB. An append
to the spine is a function call that ends in a `write(2)`; an append to Kafka is
a serialisation, a socket, a broker, and a response. The second half of the
prediction is not something these numbers can prove, and the section below says
what would.

**2. `sync` mode lands within 2x, because both wait on the same fsync. — Held,
at 1.35x and 1.39x**, and only visible after the pairing above was corrected.
This is the most informative row in the table: when durability is the constraint,
the two systems are nearly the same speed, because neither is doing anything
except waiting for a disk.

**3. Kafka wins sequential read, or comes close. — Half wrong.** At 64 bytes
Kafka reads 3.4x to 3.8x faster, as predicted: its fetch protocol ships batches
and the spine's cursor decodes record by record. At 1 KiB the advantage
disappears — Kafka is *slower* than the spine in two of three modes. Per-record
overhead is what Kafka amortises, and at 1 KiB there is proportionally less of
it to amortise.

**4. The spine's p999 is worse than its p99 by a wider margin than Kafka's. —
Wrong, and backwards.** The spine's p999/p99 ratio is 1.02 in `batch` and 1.06
in `os` at 64 bytes; Kafka's is 1.37 and 3.34. Kafka has the longer tail in five
of six rows. The reasoning behind the prediction — that the spine pays segment
rolls inline with no background flush — was sound and simply too small to matter
next to what a broker does between requests.

The maxima are worse for Kafka by more than the percentiles suggest: 258 ms
against the spine's 49 ms in `sync` at 64 bytes.

## What This Does Not Show

**The `sync` numbers are not durability numbers.** A `sync`-mode append costs
450 µs per record here. The same code on the macOS host costs 4.3 ms, because
Go's `os.File.Sync` issues `F_FULLFSYNC` there and waits for the drive's own
cache. The containerised Linux filesystem is one layer of virtualisation away
from a physical disk and is around ten times cheaper per fsync. Both systems get
the same discount, so the *ratio* survives; the absolute numbers describe a
virtual disk.

**No durability claim is published here**, as the protocol said in advance.
Nothing in this repository has yet been tested against real power loss — the
trigger set in [m2-filesystem-model.md](m2-filesystem-model.md) — so this
comparison says which system is faster and nothing whatever about which loses
less.

**The process boundary is not separated from the log.** The honest form of
prediction 1's second half needs the spine behind a socket, and the spine has no
wire protocol, deliberately. So "the spine is 23x faster in `os` mode" and "an
in-process library beats a network broker" are not distinguished by this
experiment, and the second is the likelier explanation for most of the gap.

## What Kafka Gives That The Spine Does Not

Configured away to make the comparison possible, and each one is real:

- **Replication and failover.** Set to one replica here. It is Kafka's main
  durability story and the spine has none.
- **Partitions and consumer group rebalancing.** The spine has one writer, one
  partition, and no protocol for moving a consumer between processes.
- **A wire protocol and an ecosystem.** Any language, any host, Connect, Streams,
  and two decades of operational knowledge. The spine has one consumer: Go code
  in the same binary.
- **Exactly-once semantics and transactions.** Out of scope for the spine by
  design; at-least-once plus idempotent projections is its contract.

What Kafka costs, measured on this machine: a 411 MB image against the spine's
10.7 MB, and 299 MB of resident memory for an idle single-node broker against a
library that adds none. Plus a process to configure, monitor, upgrade, and
restart — which is the cost that does not appear in any benchmark and is the one
most likely to decide the question.

## What Was Not Implemented

The `Log` interface in [log-design.md](../log-design.md) has `Append`, `Reader`,
`Group`, `Snapshot`, and `Close`. The Kafka backend implements the first two and
`Close`.

- **`Group`** maps onto Kafka consumer groups, which store committed offsets in
  an internal topic. It is a genuine equivalent and was not needed to measure
  append and read throughput.
- **`Snapshot`** has no Kafka equivalent. The idiomatic answer is a
  log-compacted topic holding one record per key, which is a different mechanism
  with different semantics rather than an implementation of this one.

That second point is the interesting one: it is the first place the narrow
interface leaks. `log-design.md` claims the interface is narrow enough that
"Kafka can sit behind it without distorting either side", and `Snapshot` is a
counterexample.

## Does This Justify Writing The Log?

The roadmap's framing is that this milestone pays back the decision to write a
log at all, *not because the owned log wins*. On the numbers it does win, and the
honest reading is narrower than the numbers look:

- When durability is the binding constraint — `sync` mode, an fsync per record —
  the two systems are within 1.4x. Buying a broker costs almost nothing in
  throughput and buys replication.
- When it is not, the spine is 2x to 23x faster, and most of that is a process
  boundary rather than a better log.
- Kafka reads small records several times faster and the gap closes as records
  grow.
- The spine's tail latency is markedly tighter in every mode, which was the
  prediction that came out backwards and is the result that most favours it.

The defensible claim is: **for a single-process Go application that wants an
append-only log with deterministic replay and no operational surface, the owned
log is fast enough that Kafka's throughput is not an argument for adopting it.**
Replication, polyglot clients, and the ecosystem still are.

## Next Milestone's Riskiest Assumption

M6 is optional replication, and the roadmap is explicit that it needs its own
kill gate and a project that genuinely needs it. Nothing does yet.

The overdue item is not M6. It is the one deferred since M2 and named again in
M4: **nothing here has met real power loss.** Every durability property this
repository asserts rests on `sim.FS` modelling a disk correctly, and this
milestone has now published throughput numbers next to Kafka's while explicitly
declining to say anything about data loss — which is the honest position and an
unsatisfying one. A fault-injecting block device is the next thing worth
building, ahead of any new feature.
