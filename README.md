# Event Spine

A small, owned event-sourcing substrate in Go: an append-only log, a deterministic
command/event/projection core, deterministic simulation testing, and replay
devtools.

Every bug reproduces from one integer.

```bash
task repro SEED=8 STEPS=3 FAULTS="bitflip@5:458021"
```

## Using It

The spine is a library with **no third-party dependencies**. It runs in your
process; there is no server.

```go
import (
    "github.com/DamoDCoder/event-spine/log"
    "github.com/DamoDCoder/event-spine/runtime"
)

fs, _ := runtime.NewFS("/var/lib/yourapp/log")
l, recovery, _ := log.Open(fs, log.Config{})
defer l.Close()
```

**Read [docs/adopting.md](docs/adopting.md) before integrating.** It covers the
five behaviours that surprise people, all of them found by simulation or by
cutting the power, and each linked to the seed that found it.

> **v0: the surface will break.** Pin a version. The packages a consumer imports
> are `core`, `log`, `runtime`, and `sim`; everything else is `internal/`.

## Why This Exists

Kafka is where partitioning, offsets, consumer groups, replication, and
backpressure go to be invisible. Operating it teaches configuration. Writing a log
teaches the system. This one is small enough to finish and real enough to break.

Kafka is not abandoned — it sits behind the same `Log` interface, introduced late,
and the measured comparison between the two is one of the deliverables.

The second reason is deterministic simulation testing. Containers make a project
reproducible across machines; simulation makes it reproducible across time and
concurrency, which is where the expensive bugs live.

## Quick Start

Requires a container runtime. Nothing else — no Go toolchain, no version manager,
no local services.

```bash
git clone https://github.com/DamoDCoder/event-spine && cd event-spine

task test:container      # authoritative test run, inside the image
task verify:determinism  # 1,000 seeds, one projection hash
task bench:log           # append throughput and latency
task repro SEED=8412     # reconstruct a specific failure exactly
```

With a Go toolchain installed, `task test` is the fast inner loop. It is not the
source of truth. When the host and the container disagree, **the container is
right**, and a determinism difference between the two is the most valuable bug
this project can find.

## What Is In Here

| Package | Responsibility |
| --- | --- |
| `internal/core` | Command → validate → event → projection. No I/O, no wall clock, no unseeded randomness. |
| `internal/log` | Segmented append-only log: offsets, consumer groups, snapshots, compaction, crash recovery. |
| `internal/sim` | Virtual clock, seeded scheduler, fault injection, invariant checks, seed minimization. |
| `internal/devtools` | Scrub a log, branch it at an offset, diff two projections, step a failing seed. |

Everything is `internal/` until three projects consume this. Nothing here has
earned a stable public API yet.

## The Determinism Rule

Production packages never import `time`, `math/rand`, or `os` directly. Wall
clock, randomness, identifier generation, disk, and network are injected
interfaces, supplied by the real runtime in production and by the simulator in
tests.

```go
type Deps struct {
    Clock Clock
    Rand  Source
    IDs   IDGen
    FS    FS
    Net   Transport
}
```

`task check:determinism` enforces this in CI. The rule is enforced mechanically
because it decays the moment it is left to discipline, and everything else in this
repository depends on it holding.

## Headline Claims

Each is falsifiable and measured. A claim that fails reshapes the milestone that
owns it; it does not get quietly downgraded.

| Claim | Command | Milestone |
| --- | --- | --- |
| Same seed produces a byte-identical projection hash across 1,000 runs, host and container | `task verify:determinism` | M1 |
| 1M single-producer appends per second, p99 append latency under 2ms, on one laptop | `task bench:log` | M2 |
| A crash at any injected point recovers to the last durable offset with no torn record | `task sim:crash-matrix` | M2 |
| Every simulation-found bug reproduces from its seed alone | `task sim:corpus` | M3 |
| Log throughput within 2x of Kafka on an identical workload | `task bench:compare` | M5 |

## Documentation

- [docs/concepts.md](docs/concepts.md) — the implemented ideas, drawn: the
  determinism boundary, why the hash chain replaced a terminal hash, what the
  record checksum does not cover, and where the filesystem model stops being
  measured
- [docs/roadmap.md](docs/roadmap.md) — M0 to M5, each with a kill gate
- [docs/architecture.md](docs/architecture.md) — components and their boundaries
- [docs/log-design.md](docs/log-design.md) — record format, segments, recovery, durability modes
- [docs/simulation-testing.md](docs/simulation-testing.md) — the harness, fault catalogue, seed corpus
- [seeds/README.md](seeds/README.md) — the regression corpus and how to add to it

Decision notes record what each milestone claimed, what the measurement showed,
and what changed as a result:

- [docs/decisions/m0-determinism-spike.md](docs/decisions/m0-determinism-spike.md)
- [docs/decisions/m1-deterministic-core.md](docs/decisions/m1-deterministic-core.md)
- [docs/decisions/m2-filesystem-model.md](docs/decisions/m2-filesystem-model.md)

## Consumers

The spine is the substrate for a backlog of projects that otherwise reimplement
the same plumbing: Signal Garden (storage and throughput), Eventide Arena
(consistency under adversarial networks), and a tier of smaller simulations. A
consumer never reimplements the event core, the log, or the test harness.

## Non-Goals

- Not a production Kafka replacement. It is a Kafka comparison and an interface
  Kafka can sit behind.
- No distributed consensus in v1. Single writer, single node. Replication is a
  later optional milestone with its own kill gate.
- No general-purpose framework ambitions. A feature with exactly one consumer
  belongs in that consumer.
- No public API stability promise before three projects depend on this.
