# Deterministic Simulation Testing

The single upgrade that applies to every project in this backlog. It is cheap once
the spine exists, it is rare in a personal portfolio, and it changes what "tested"
means: not "the happy path ran", but "ten thousand hostile orderings ran, and the
three that failed are committed as seeds that will never pass silently again".

The reference points are FoundationDB's simulation framework and TigerBeetle's
VOPR. This is a small version of the same idea, sized for one person.

## The Contract

**Every bug reproduces from one integer.**

```bash
task repro SEED=8412
```

That command must reconstruct the exact failure — same interleaving, same
timings, same faults, same assertion — on any machine, in the container or on the
host, today or in a year. If a bug needs anything else to reproduce, the harness
has a hole in it and closing that hole is more important than fixing the bug.

## How It Works

The system under test is run inside a single-threaded loop with a virtual clock.
Nothing is concurrent; everything is *interleaved*, and the interleaving is chosen
by a seeded random source.

1. The seed initializes a PRNG. That PRNG is the only source of randomness in the
   entire run: scheduling, fault timing, message delays, payload contents.
2. All components register as tasks with the scheduler. A task yields at every
   point where real execution could block: I/O, sleeps, channel operations,
   network sends.
3. The scheduler picks the next runnable task using the PRNG, advances the virtual
   clock to the next scheduled event, and runs the task to its next yield point.
4. Faults are injected on a PRNG-driven schedule.
5. Invariants are checked after every step, not at the end. A violation reports
   the seed, the step number, and the event that broke it.

Time is advanced, never waited on. A simulated hour costs microseconds, so a test
can cover a week of clock skew, a hundred crashes, and ten thousand messages in
the time a single `time.Sleep(time.Second)` would have cost.

## What Must Be Injected

Because the simulation controls these, production code must never reach for them
directly. This is the discipline that makes the rest possible.

| Real dependency | Injected as | Simulation controls |
| --- | --- | --- |
| `time.Now` | `Clock` interface | virtual time, skew, jumps backwards |
| `time.Sleep`, timers | `Clock.After` | fires when the scheduler decides |
| `rand` | seeded `Source` | fully reproducible |
| goroutines | scheduler tasks | interleaving order |
| disk | `FS` interface | latency, errors, disk full, torn writes |
| network | `Transport` interface | delay, loss, reorder, duplicate, partition |
| UUIDs, IDs | `IDGen` interface | deterministic sequences |

A `go vet`-style check or a simple grep in CI should fail the build when
production packages import `time`, `math/rand`, or `os` directly. The rule is
worth enforcing mechanically because it decays the moment it is left to
discipline.

## Fault Catalogue

Generic faults, injected in every project:

- **Network:** delay drawn from a heavy-tailed distribution, packet loss,
  reordering, duplication, asymmetric partitions, partitions that heal.
- **Process:** crash and restart at any yield point, restart with a stale
  snapshot, slow process (starve a task for a long virtual interval).
- **Disk:** write error, read error, disk full, fsync failure, torn write, bit
  flip, slow disk.
- **Clock:** skew between processes, jumps forward, jumps backwards, a clock that
  stalls entirely.

Project-specific faults live with their project. The spine's log faults are listed
in [log-design.md](log-design.md).

## Invariants

Simulation is only as good as what it asserts. Each project declares its
invariants explicitly, checked after every step:

Spine invariants:

- Offsets are monotonic and gapless within a partition, except across compaction.
- A committed group offset is never ahead of a durably applied projection.
- Recovery never discards a record that was acknowledged as durable.
- The projection hash at offset `o` is identical no matter how the events reached
  it — one batch, many batches, or after a crash and replay.

Project invariants are the interesting ones, and writing them is the real design
work. For example, Eventide Arena: no match reaches two different final states
from the same command sequence; a reconnecting client converges to the
authoritative state within N events; a rejected command never mutates state.

## Coverage And The Seed Corpus

Random seeds alone plateau. Three practices keep it useful:

- **Swarm testing.** Each run randomizes its own fault *configuration*, not just
  fault timing — one run has no disk faults and heavy partitions, another the
  reverse. Uniform fault probabilities explore a narrower space than they appear
  to, because every run looks like the average run.
- **A committed seed corpus.** Every seed that ever found a bug is committed to
  `seeds/` with a one-line note about what it caught, and CI replays all of them
  on every change. This is the regression suite, and it grows for free.
- **A nightly random sweep.** Thousands of fresh seeds, with any failure
  automatically minimized and added to the corpus.

Report simulation coverage as a first-class metric: seeds run, faults injected,
unique failures found, corpus size. It belongs next to test coverage, and it is a
more honest number.

## Minimization

A failing seed at step 400,000 is a bad bug report. The harness should shrink it
automatically: replay the seed while removing fault injections one at a time,
keeping any removal that still reproduces the failure. The minimized trace is what
gets committed. This is worth building early — it is the difference between the
harness being used and being avoided.

## Relationship To Other Tests

Simulation replaces most hand-written integration tests. It does not replace:

- **Unit tests** for pure functions, encoding, and arithmetic. Faster and clearer.
- **Property tests** for algebraic laws, which state something simulation cannot.
- **One real end-to-end test per project**, run against real processes, real disk,
  and real network. Its job is to catch the case where the simulated interfaces
  have drifted from reality — the one failure mode simulation is structurally
  blind to.

That last one is not optional. A simulation that has diverged from the system it
models is worse than no simulation, because it is confidently wrong.

## Adoption Path

This does not need to arrive all at once, and the order matters — each step is
useful alone.

1. Inject the clock. Delete every `time.Sleep` from tests. This alone makes the
   suite fast and flaky-free.
2. Inject randomness and identifier generation. Assert projection hash equality
   across runs.
3. Add the scheduler and run the existing integration tests inside it.
4. Add fault injection, starting with process crashes.
5. Add minimization and the seed corpus.
6. Add the nightly sweep and report coverage.

Steps 1 and 2 are worth doing in Signal Garden immediately, before it grows, since
retrofitting injected dependencies into a mature codebase is the expensive version
of this work.
