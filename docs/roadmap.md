# Event Spine Roadmap

Every milestone produces something runnable, measurable, and publishable. A
milestone that only produces internal code with no user-visible or measurable
outcome does not belong here.

Each milestone carries a **kill gate**: a claim that, if disproven, changes the
plan rather than being quietly downgraded. This is the mechanism the rest of the
backlog was missing.

| Milestone | Outcome | Public artifact | Kill gate |
| --- | --- | --- | --- |
| M0 | Determinism spike | Written finding plus a hash-equality test | Signal Garden's existing M0 can be made bit-reproducible across host and container. If it cannot, find out why before building anything on top of it. |
| M1 | Deterministic core plus injected dependencies | `task verify:determinism` output over 1,000 seeds | 1,000 seeds produce one projection hash. Any divergence is a design fault, not a flake. |
| M2 | Owned log with crash recovery | Append throughput and recovery-time graphs | 1M appends/sec single producer, p99 under 2ms, and recovery from a crash at any injected point loses nothing acknowledged as durable. |
| M3 | Simulation harness with fault injection and minimization | Seed corpus with the bugs it caught | The harness finds at least one real bug the existing tests missed. If it finds nothing across a nightly sweep, the fault model is too gentle. |
| M4 | Replay devtools | A recorded walkthrough of debugging a real failure by seed | A bug can be diagnosed from seed to root cause without adding a single print statement. |
| M5 | Kafka backend behind the same interface | Published comparison report | The owned log lands within 2x of Kafka's throughput. If it is 10x slower, publish that honestly and say what Kafka is doing that this does not. |

## M0 — Determinism Spike

Two days at most. Take Signal Garden's existing M0 simulation and answer one
question: is it actually deterministic, or is it deterministic-looking?

- Hash the projection after a fixed run. Run it 100 times. Compare.
- Run it in the container and on the host. Compare.
- Find every read of wall time, map iteration order, unseeded randomness, and
  goroutine-ordering dependency.

The likely finding is that it is *mostly* deterministic, with two or three
specific leaks. That finding is the milestone. Write it down before fixing it,
because the list of leaks is the design input for M1.

## M1 — Deterministic Core

Extract the command/event/projection cycle with every nondeterministic dependency
injected: `Clock`, `Source`, `IDGen`, `FS`, `Transport`.

Add the CI check that fails the build when a production package imports `time`,
`math/rand`, or `os` directly. Adding this check early is much cheaper than adding
it later.

Exit: `task verify:determinism` runs 1,000 seeds, in the container and on the
host, and prints one hash.

## M2 — Owned Log

Implement [log-design.md](log-design.md): segments, offsets, groups, snapshots,
compaction, recovery, three durability modes.

Benchmarks are part of the milestone, not a follow-up. Results are committed so
regressions show up as diffs.

Exit: the throughput and recovery claims hold, and the crash matrix passes with
no acknowledged record lost.

## M3 — Simulation Harness

Scheduler, virtual clock, fault catalogue, invariant checks after every step,
automatic minimization, seed corpus, nightly sweep.

Build minimization in this milestone rather than deferring it. An unminimized
failing seed is a bug report nobody wants to open, and a harness people avoid is a
harness that decays.

Exit: at least one real bug found and committed as a minimized seed, with CI
replaying the whole corpus.

## M4 — Replay Devtools

Scrub, branch, projection diff, and step-a-seed. Absorbs what the backlog listed
as Replay Arcade.

The demo is the artifact: a short recorded walkthrough of taking a nightly-sweep
failure, minimizing it, scrubbing to the divergent event, and diffing the two
projections.

Exit: a real failure diagnosed end to end with no print statements added.

## M5 — Kafka Comparison

Implement the `Log` interface over Kafka. Run the same workload through both.
Publish the comparison with the operational surface of each stated honestly
alongside the numbers.

This is the milestone that pays back the decision to write a log at all: not
because the owned log wins, but because after this you can say precisely what
Kafka gives you and what it costs, with your own measurements.

Exit: the comparison is published, including the parts that are unflattering.

## Optional M6 — Replication

Only if a project genuinely needs it, with its own kill gate. Single-writer Raft
over the segment log, correctness verified by the simulation harness under
partitions. Deliberately last, and deliberately optional. Most of the backlog's
value is reachable without it.

## Feedback Checkpoints

After each milestone, record a short decision note covering: what the claim was,
what the measurement showed, what changed as a result, and what the next
milestone's riskiest assumption is. One paragraph is enough. A milestone without
a decision note is not finished.
