# M5 — The Comparison Protocol

**Date:** 2026-08-13
**Status:** written before any number was measured, and committed before the
Kafka backend was implemented. That ordering is the point of the document.

## Why This Exists Before The Numbers

[m4-replay-devtools.md](m4-replay-devtools.md) named M5's riskiest assumption:

> the two systems can be measured on a workload that is fair to both. Kafka's
> equivalent knobs — `acks`, `flush.ms`, replication factor — are not the same
> knobs, and the honest comparison depends entirely on which pairs are declared
> equivalent before the numbers are taken. Choosing them afterwards is how
> benchmarks lie.

`CLAUDE.md` puts it more bluntly: *the M5 Kafka comparison is worth nothing if
only the favourable half is published.* So the equivalences, the workload, the
exclusions, and the predictions are all fixed here, in advance, where they can
embarrass the author later.

## The Claim Under Test

From [roadmap.md](../roadmap.md):

> The owned log lands within 2x of Kafka's throughput. If it is 10x slower,
> publish that honestly and say what Kafka is doing that this does not.

## Declared Knob Equivalences

Neither system has the other's settings. These pairs are declared equivalent
**now**, on the principle of *what has to reach durable storage before the write
is acknowledged*.

| Spine mode | Kafka configuration | Why these are the pair |
| --- | --- | --- |
| `sync` | `acks=all`, `log.flush.interval.messages=1`, one broker | Both fsync before acknowledging every record. |
| `batch` (`SyncRecords=N`) | `acks=all`, `log.flush.interval.messages=N`, one broker | Both fsync once per N records and acknowledge the rest from memory. |
| `os` | `acks=1`, Kafka's default flush settings | Neither forces a flush; both leave it to the operating system. |

One broker and `replication.factor=1` throughout, because the spine has no
replication. **That is a setting chosen to make the comparison possible, and it
removes Kafka's main durability advantage.** It is listed under Kafka's column
in the operational surface below rather than being quietly dropped.

Held identical on both sides: one partition, one producer, no compression, the
same record sizes drawn from the same seed, the same total record count, the
same batch sizes at the API, no consumer running during append measurement.

## The Workload

The harness generates records from a seed, so both systems receive a
byte-identical stream in the same order:

- **Sizes:** 64 B and 1 KiB records, matching `bench/log.txt` so the spine's
  numbers here can be checked against the numbers already published.
- **Volume:** enough records per configuration for the batch modes to cross
  several flush intervals; fixed per run so two runs are comparable.
- **Shape:** one producer, appending; then one consumer, reading from offset 0
  through the tail.

Measured on both: append throughput, p50/p99/p999 append latency, and sequential
read throughput.

## Where Both Systems Run

**Both run inside containers on the same machine**, Kafka from
`apache/kafka:3.9.0` pinned by digest, the spine from this repository's own
image. On a Mac that means both pay the same virtualised-filesystem cost, which
is the closest to fair this hardware allows.

The host-native spine numbers in `bench/log.txt` are **not** comparable to these
and will not be quoted beside them.

## The Structural Unfairness, Stated Up Front

**The spine is a library in the same process as its caller. Kafka is a broker on
the other side of a socket.** An append to the spine is a function call; an
append to Kafka is a serialisation, a syscall, a network hop, a broker, and a
response. That gap is not a measurement artefact to be tuned away — it is the
architectural difference — but it means a latency number comparing them is
comparing two different things.

This will be reported as two numbers rather than one ratio: the spine's
in-process append, and Kafka's client-observed append. The spine has no wire
protocol (deliberately deferred), so a like-for-like network comparison is not
available and its absence will be stated in the result rather than papered over.

## What Is Not Compared, And Why

Each of these is something Kafka does that the spine does not. They are listed
here so the result cannot be read as "the spine is better":

- **Replication and failover.** Configured away to make the comparison possible.
- **Multiple partitions and consumer group rebalancing.** The spine has one
  writer and no rebalancing protocol.
- **Cross-language clients, a wire protocol, an ecosystem.** The spine has one
  consumer: Go code in the same binary.
- **Exactly-once semantics and transactions.** Explicitly out of scope for the
  spine; at-least-once plus idempotent projections is its contract.
- **Operating a broker.** Kafka's number includes a running process someone has
  to configure, monitor, upgrade, and restart. The spine's includes a file.

## What Will Not Be Claimed

**No durability claim will be published from this comparison.**

[m2-filesystem-model.md](m2-filesystem-model.md) set an explicit trigger: the
first published durability claim needs a fault-injecting block device behind it,
or it is a claim about a belief. That device does not exist here yet. So M5
publishes throughput, latency, and operational surface, and says nothing about
which system loses less data on power loss — including no implication of it.

That is a reduction in what M5 was going to be worth, and it is recorded as one
rather than presented as a scope decision.

## Predictions, Recorded In Advance

Written before the backend exists, so the result can contradict them:

1. The spine will beat Kafka on `os`-mode append throughput by a large factor —
   more than 5x — and most of that will be the process boundary rather than the
   log.
2. In `sync` mode the two will be much closer, within 2x, because both are
   waiting on the same fsync and the disk sets the pace.
3. Kafka will win on sequential read throughput per byte, or come close, because
   its batched fetch protocol was built for exactly that and the spine reads
   record by record.
4. The spine's p999 will be worse than its p99 by a wider margin than Kafka's,
   because the spine has no background flush and pays segment rolls inline.

If (1) holds and (2) does not, the interesting finding is about fsync rather
than about either implementation, and the report should say so.

## What Would Falsify The Milestone's Premise

The premise is that writing the log was worth it. These outcomes would count
against it, and each will be published if it occurs:

- The spine is more than 10x slower than Kafka in any mode.
- The spine's advantage disappears once the process boundary is accounted for,
  which would mean the measured difference is about deployment rather than
  design.
- Kafka's configuration turns out to have no honest equivalent to a spine mode,
  which would mean the interface hides a difference that matters.
