# Changelog

All notable changes to this project are documented here.

The project has not reached its first versioned release. Until then, work is
grouped under `Unreleased`.

## [Unreleased]

### Added

- Repository scaffold: pinned container build, containerized test target, and a
  `Taskfile.yml`.
- Project brief, roadmap with per-milestone kill gates, log design, and the
  deterministic simulation testing standard.
- A seed corpus directory for regression seeds.

- The M0 determinism spike finding, `docs/decisions/m0-determinism-spike.md`.
- `internal/core`: the command, event, and projection cycle, with logical time,
  a per-step hash chain, and absorbing-state detection.
- `internal/sim`: deterministic clock, splitmix64 source, sequential identifiers,
  seeded scheduler, and the ledger workload the determinism gate runs.
- `cmd/spine verify determinism`, and the M1 result in
  `docs/decisions/m1-deterministic-core.md`.
- The filesystem interface, a real adapter in `internal/runtime`, a crashable
  simulated one in `internal/sim`, and the tests that compare them:
  `docs/decisions/m2-filesystem-model.md`.
- `internal/log`: record framing with CRC32C, segments with a sparse in-memory
  index and torn-tail recovery, and the segmented log with three durability
  modes.
- `internal/devtools/bench` and `task bench:log`: append throughput, latency
  percentiles, recovery time, and cold-segment read cost, with the run committed
  to `bench/log.txt`.
- `log.Reader`: a cursor over the log, which is how a consumer streams it.

### Decision Notes

M0, measured against Signal Garden at `bb17562` on 2026-08-10:

- The batch path is genuinely deterministic. 1,000 seeds produce a byte-identical
  seed→hash table on darwin/arm64, in the pinned container on linux/arm64, and on
  linux/amd64. The claim survived.
- The live path is not reproducible from its configuration. A `select` over a
  ready tick and a ready command decides which tick a control change lands on:
  40 identical scenarios produced 7 distinct projection hashes. Replay is
  therefore keyed on the event log, never on run configuration.
- Terminal-state hash equality can pass vacuously. The same probe over a longer
  run reported one hash across 40 genuinely divergent runs, because the
  projection had reached an absorbing state. `verify:determinism` compares a
  per-step hash chain and asserts liveness as a result.
- The choice between two simultaneously ready events is a nondeterministic
  dependency that none of `Clock`, `Source`, `IDGen`, `FS`, or `Transport`
  covers. It becomes the scheduler's, and the scheduler takes it from the seed.

M1, on 2026-08-10:

- 1,000 seeds produce one digest, `63b11be7…`, identical on darwin/arm64, in the
  pinned container on linux/arm64, and on linux/amd64. The claim held.
- The gate was checked for teeth. Rewriting the ledger's digest to range a map
  made it reject seed 1 on the in-process comparison, which is the class of bug
  the import check cannot see.
- An absorbed run fails the gate rather than passing it, and the generator's
  output is pinned to the published splitmix64 vectors, because a seed corpus
  whose generator drifts is a set of numbers that used to mean something.

M2, in progress:

- The filesystem model and the tests that check it landed before the log, so the
  tests could not be written to agree with the code they guard.
- Its semantics are differentiated against a real disk across every failure mode
  the interface defines, and a real process killed mid-record leaves a real torn
  tail on a real filesystem.
- The durability half is still only a belief. SIGKILL destroys a process, not a
  machine, so nothing here has yet observed unsynced data being lost. No
  durability claim is publishable until a fault-injecting block device says
  otherwise, and the note records that rather than quietly assuming it.

M2 throughput, measured on 2026-08-12, darwin/arm64, Apple M2 Max, single
producer, 64 byte records, `task bench:log`, full run in `bench/log.txt`:

- **The 1M appends/sec claim does not hold.** The fastest mode measured is `os`,
  which never syncs, at 1,436 ns/op — about 696,000 appends/sec, 30% short. The
  default `batch` mode reaches about 151,000/sec, and `sync` about 233/sec.
  The claim stands as written in the roadmap and unmet, rather than being
  restated against the mode that came closest.
- The p99 half holds where the mode allows it: 3.8 µs in `os` and 6.9 µs in
  `batch`, both far inside the 2 ms budget. `sync` misses it at 8.1 ms, because
  Go's `os.File.Sync` on darwin issues `F_FULLFSYNC` and waits for the drive's
  own cache. That is a platform property, and the Linux number will differ; the
  comment in `internal/runtime/fs.go` claimed the opposite and has been
  corrected.
- Where the time goes is now measured rather than guessed: every record costs its
  own `write(2)`. Handing 256 events to one `Append` call amortises the fsync —
  `sync` mode improves 175-fold, from 4.29 ms to 24 µs per record — and leaves
  per-record throughput in `os` mode unchanged at 1.43 µs, because the syscall
  count did not change. Buffering encoded records and writing once per call is
  therefore the next optimisation, and it has a number to beat.
- Recovery scans about 1.16M records/sec: a 100,000 record segment reopens in
  86 ms, at three allocations per record. Only the active segment is scanned, so
  this is a function of segment size and not of log size.
- The cost the in-memory index defers is 56 ms to open a cold 4 MiB sealed
  segment. That is the number that decides whether an index file belongs on disk
  after all, and it is not yet paid by anything that matters, because reads
  arrive in offset order and the active segment is already warm.
- Benchmarks run on the host, not in the container. The container is
  authoritative for correctness; for disk numbers on macOS it measures a VM's
  virtualised filesystem, which is neither this machine nor a Linux server.

M2 throughput after buffering the write, same machine and same day:

- One `Append` call now stages every record in one buffer and issues one
  `write(2)` per segment it touches. A 256 event batch of 64 byte records in
  `os` mode went from 366 µs to 10.8 µs per call — 1.43 µs to 42 ns per record,
  a factor of 34, at 1.5 GB/sec.
- **The 1M appends/sec claim now holds, but only under a reading worth stating
  precisely:** batched calls in `os` mode, which never syncs, reach about
  23.6M records/sec. The default `batch` mode reaches about 222,000/sec and
  `sync` about 55,000/sec, both of them fsync-bound rather than syscall-bound.
  The roadmap's claim names neither a durability mode nor a call shape, so it is
  recorded as met in one and unmet in the other rather than declared passed.
- Single-record calls are unchanged at 1.42 µs, because one record per call is
  still one write. The batching helps a caller who has a batch; it does not
  invent one.
- What the append path costs is now the fsync and nothing else. On darwin that
  is 4.5 ms of `F_FULLFSYNC`, which is why `batch` mode sits at 4.4 µs per
  record with a 1,024 record sync interval. The Linux number will be different
  and the default mode's honest figure needs a Linux machine that is not a VM on
  this laptop.
- Latency percentiles are unchanged: 6.7 µs p99 in `batch`, well inside the 2 ms
  budget. Recovery and read costs are unchanged, since neither touches the write
  path.

M2 reads, same machine, after adding the cursor:

- A cursor scan costs 863 ns per 64 byte record against 28.4 µs for the same
  record fetched by offset — 33 times cheaper, at 2 allocations against 97. The
  difference is the sparse index search and the forward decode that a random
  read repeats every time: at a 4 KiB index interval, finding one 64 byte record
  means decoding about 45 of them.
- The gap narrows to 3.2x at 1 KiB records (1,132 ns against 3,595 ns), for the
  same reason in reverse: larger records mean fewer of them between index
  entries, so a random read has less to walk.
- This is the number that says a streaming consumer must use a `Reader` rather
  than a loop over `Read`, and it is why the index interval is configuration
  rather than a constant.

Carried over from the ideas cradle, where this project was designed:

- The log is written rather than adopted. Kafka hides partitioning, offsets,
  consumer groups, and backpressure inside another binary at exactly the point
  those are the things worth learning. Kafka arrives at M5 behind the same `Log`
  interface, and the measured comparison is a deliverable.
- Determinism is the product. Wall clock, randomness, identifier generation,
  disk, and network are injected interfaces, and CI fails the build when a
  production package imports `time`, `math/rand`, or `os` directly. The rule is
  enforced mechanically because it decays when left to discipline.
- Deterministic simulation testing is adopted from the first milestone rather
  than retrofitted. Injecting dependencies into an empty repository costs
  nothing; injecting them into a mature one is a rewrite.
- Every bug reproduces from one integer. If a failure needs anything beyond its
  seed, the harness has a hole and closing it takes priority over the bug.
- Seeds are committed before their fixes, and never deleted.
- The containerized test run is authoritative and the host run is an inner loop.
  A determinism difference between the two is the most valuable bug this project
  can find, so the standard exists to surface it.
- Everything is `internal/` until three projects consume the spine. No public API
  promise before then.
- Replication, a wire protocol, tiered storage, and exactly-once semantics are
  deferred, each with the milestone that would earn it. At-least-once delivery
  plus idempotent projections is the contract.
- M0 is a determinism spike run against Signal Garden's existing M0 in its own
  repository, not against code here. Its output is a list of nondeterminism
  leaks, and that list is the design input for M1's injection interfaces.

## Commit Guidance

```text
<type>(<scope>): <imperative summary>
```

Types: `docs`, `feat`, `test`, `perf`, `refactor`, `chore`, `fix`. Keep each
commit to one coherent batch. Add a body when the tradeoff would be hard to infer
from the subject.

Do not add a `Co-Authored-By` trailer.
