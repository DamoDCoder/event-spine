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
- Consumer groups: explicit commits in their own fsynced commits log, kept as
  records so the commit history replays.
- `internal/devtools/crash`, `spine sim crash-matrix`, `spine sim corpus`, and
  `spine repro`: crash the log at every filesystem operation and check what
  recovery kept.
- Compaction: the newest record per key in a sealed segment, swapped in by
  rename, with the offsets of dropped records left behind as gaps.
- Snapshots: a serialized projection and the offset it was folded to, framed as
  a record so a torn one is detectable.
- The M2 result, `docs/decisions/m2-owned-log.md`.
- The simulation harness: a fault catalogue of crashes, write errors, short
  writes, fsync failures, bit flips, and backwards clock jumps; swarm-randomized
  fault configurations; invariants checked after every step; automatic
  minimization; `spine sim sweep`; and the M3 result in
  `docs/decisions/m3-simulation-harness.md`.
- `ops` and `signature` in seed front matter, and the `drifted` warning that
  reports when a stored seed no longer fires where it was recorded.
- `internal/devtools/compare`, `spine bench compare`, and `task bench:compare`:
  the same workload through the owned log and through Apache Kafka, both in
  containers on one machine, with the protocol fixed in advance and the result
  in `docs/decisions/m5-kafka-comparison.md`.
- Two dependencies, the first in this repository: `franz-go` and its `kadm`
  admin package, for speaking to Kafka. The reason is that Kafka's wire protocol
  is not this project's internals — the owned log is — so implementing it by
  hand would be effort spent on the part that is not the point.
- `scripts/powercut.sh` and `task verify:powercut`: cut the power under a real
  kernel, real ext4, and a real block device, with a control round that must
  fail. The result is in `docs/decisions/power-loss.md`.
- The simulation harness varies the log's durability mode and its shape — segment
  size, index interval, payload size, batch size, sync interval — recorded in
  seed front matter and defaulting to the old constants so existing seeds keep
  their meaning. Sweeps report how many modes and shapes they used, and a test
  asserts they vary.
- `sim.FS.CrashExtend` and `sim.FS.CrashTorn`: the power cut that leaves a file
  longer than its contents with zeros in the gap, and the one that keeps a
  prefix of the unsynced bytes. Real ext4 does the first and the simulator could
  not, which is why a bug that needed it went unfound for four milestones; the
  second is the most ordinary shape of all and was missing too. All three crash
  shapes are fault kinds, so the sweep draws between them and the crash matrix
  runs every point in each.
- The power cut runs under xfs as well as ext4, from a pinned image stage
  carrying both sets of tools.
- A second harness workload, `restart`: it closes and reopens the log as it goes,
  checking that a clean restart loses nothing and that commits and snapshots come
  back exactly, and it keeps a reader alive across restarts and compactions.
  Recorded in seed front matter and defaulting to the old rhythm.
- `log.Restore`: the newest snapshot and a reader positioned after it, which is
  the documented recovery path with the one available off-by-one removed.
- `docs/adopting.md`: the integration guide — the five behaviours that surprise
  people, each linked to the seed that found it, the single-owner rule, choosing
  a durability mode, using `sim` in a consumer's own tests, and what the spine
  deliberately does not do. Runnable examples in `log/example_test.go`.
- `spine replay`: scrub a seed step by step, inspect one step in full, list the
  filesystem operations with the faults that fired on them, or diff a failing
  run against the same seed with no faults. The M4 result is in
  `docs/decisions/m4-replay-devtools.md`, and `task demo:replay` reproduces the
  walkthrough in `docs/walkthrough-replay.md`.

### Changed

- **The spine is importable.** `core`, `log`, `runtime`, and `sim` move out of
  `internal/`; `internal/devtools` stays. The old rule — everything internal
  until three projects consume the spine — could not survive contact with its
  own goal, because Go's `internal/` rule makes a package literally unreachable
  and no project could ever become the first consumer. The intent was *no API
  stability promise*, and that is unchanged: v0, the surface will break, pin a
  version.
- **The module has no third-party dependencies.** The Kafka comparison moved to
  its own module under `tools/kafkacompare`, because it needs a client, that
  client needs Go 1.25, and a project adopting the log should inherit neither.
  The spine's `go` directive is 1.24 — the floor is kept as low as the code
  allows so a consumer is never forced to upgrade, while the container still
  pins the toolchain by digest as the authoritative build.
- **The public surface is trimmed to what a consumer needs.** `log` no longer
  exports `Segment`, `OpenSegment`, `SegmentName`, `ParseSegmentName`, the
  record framing functions, or the file-name constants: they are how the log is
  built rather than what it offers, and every one of them would have constrained
  a future change. `sim` no longer exports the ledger workload, which is the
  determinism gate's fixture and now lives in `internal/devtools/ledger` — a
  project adopting the spine wants a clock, a filesystem, and a seeded source,
  not this repository's toy ledger.
- `BenchmarkLogReadCold` and `BenchmarkLogOpen` use a `b.N` loop rather than
  `b.Loop()`. Both open a fresh log every iteration inside a `StopTimer` region,
  and `b.Loop()` continues on timed duration alone, so they never converged: a
  six second benchmark took thirty minutes and died on the timeout.

### Fixed

- A live `Reader` used a segment handle that compaction had closed, and a byte
  position pointing into a layout the replacement file does not have. The log
  now counts segment replacements and a cursor re-resolves when the count moves.
  Found with no fault injected by the restart workload, `seeds/0012.md`.

- `Log.Sync` said it made everything appended so far durable and did not: rolling
  in `os` mode sealed the outgoing segment without syncing it, and `Sync` only
  touched the active one, so a power cut left a hole in the middle of the log
  with records on both sides of it. Found by `task verify:powercut` against real
  ext4, not by simulation — the workload had run only in batch mode, where a
  roll syncs the outgoing segment and the gap cannot open.
- A sealed segment could hold acknowledged records and an unsynced tail, and
  recovery refuses such a segment outright — so the acknowledged prefix went
  with it. Rolling now syncs the outgoing segment in every mode, restoring the
  premise recovery leans on, and the list of syncs owed is gone. Found by the
  second power cut, after the first fix for this area had passed twenty.
- A segment closed by compaction stayed on the list of syncs owed, so the next
  `Sync` tried to flush a file that no longer existed. `seeds/0068.md`, found
  with no fault injected at all once durability modes entered the sweep.
- `Reader.Next` dereferenced a nil cursor when every offset from the cursor to
  the tail was missing, which a crash can produce by truncating one segment and
  leaving the next one empty. A panic rather than an error, reachable since
  compaction landed. `seeds/0290.md`.

- A flipped bit in a record's offset field was accepted, putting the log's tail
  at 4611686018427387911 and assigning every later append an offset from there.
  The offset field was outside CRC coverage on the reasoning that a reader
  always knows which offset comes next — a reasoning compaction invalidated and
  nothing noticed. The checksum now covers it. Found by the first swarm sweep,
  `seeds/0008.md`.
- A crash could remove a whole segment, including records whose `Sync` had
  returned. The log fsynced the segment file and never the directory that named
  it, and a file whose directory entry was never synced does not survive a power
  cut. Found by the crash matrix on its first run, recorded as `seeds/0001.md`.

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

M2 crash matrix, on 2026-08-12:

- The harness earned its place on its first run. Seed 1 failed at 45 of its 51
  crash points, all with the same cause: the log fsynced a segment file and
  never the directory naming it, so a crash removed the whole segment along with
  records whose `Sync` had returned. Recovery then reported a healthy empty log,
  which is the worst shape a loss can take.
- The bug was reachable only because the simulated filesystem models the POSIX
  rule that a file needs its directory entry synced to exist after a power cut.
  A model that had been generous about this would have agreed with the log and
  found nothing.
- The seed is committed as `seeds/0001.md` and was failing before the fix
  landed, so `task sim:corpus` proves the fix rather than asserting it.
- After the fix: 9,960 crash points across 200 seeds, no loss. That is the M2
  recovery claim's evidence, and it is a simulation claim rather than a hardware
  one — the caveat above still stands.

M2 compaction and snapshots, on 2026-08-12:

- Making the read path tolerate gaps came before compaction could create any.
  The recovery scan, the index lookup, and the cursor all assumed offsets were
  contiguous, and each would have called a compacted segment corrupt. They now
  require offsets to ascend rather than to increment, which is what survives of
  the exact check once records can legitimately be missing.
- Compaction's scope is one segment. A key whose newest record lives in a later
  segment keeps its older copy, so this reclaims less than a whole-log pass
  would. Recorded as a limitation rather than described as a design: a
  cross-segment pass needs to read every later segment to know what is
  superseded, and the crash safety and the gaps were the parts worth testing
  first.
- Seed 0017 caught the harness rather than the log, and is committed saying so.
  Crediting a partially-crashed `CompactAll` with none of its finished
  compactions made the checker demand records that compaction had legitimately
  removed. A checker that is wrong in this direction hides every later failure
  of the thing it checks.
- The crash matrix now reports what it exercised. 300 seeds, 18,760 crash
  points, 419 compactions dropping 3,154 records, 354 snapshots, no loss. Those
  counters are asserted in the test suite, because a matrix that quietly stopped
  compacting would report zero failures and mean nothing by it.
- Two costs turned up in the republished benchmark run and are recorded rather
  than tuned away. Gap tolerance costs a random read 3 allocations and about 2%,
  because a lookup must now decode the record it lands on to learn whether it is
  the one asked for. The directory sync that fixed seed 0001 costs about 10% of
  batched `os` throughput — 10.7 µs to 11.9 µs per 256 record call — which is
  one `F_FULLFSYNC` spread across the ~2,900 calls a 64 MiB segment holds.

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

M3, on 2026-08-12:

- The gate held on the first sweep. A single flipped bit in a record's offset
  field was accepted, because M2's compaction work invalidated the reasoning
  that kept the offset outside the checksum and nothing noticed. The argument
  lived in one file and the change that killed it lived in another; a random bit
  does not care which.
- An operation that renames or writes before it syncs can fail and still have
  happened. Three seeds found that shape — a partial `CompactAll`, a commit
  whose fsync failed, a compaction whose directory sync failed — so it is stated
  as a property of the design rather than fixed a third time.
- Half the corpus is the checker. Three of six entries are false positives the
  harness produced, kept and labelled, because a checker that blames the wrong
  component teaches everyone to skim its output.
- The corpus was already rotting. A fault's position is an index into the run's
  operation stream, and seed 0001 was recorded against 51 operations in a run
  that is 84 now: `crash@7` has not been the crash that failed for a milestone.
  Found by looking rather than by a test, which is the uncomfortable part.
- 2,000 seeds, 0 failures, after the fixes.

M4, on 2026-08-13:

- The gate held: one bug diagnosed from a corpus failure to its root cause in
  four commands, with no print statement added. The transcript is
  `docs/walkthrough-replay.md` and `task demo:replay` regenerates it by
  reverting one commit in a scratch worktree.
- The first version of the tool failed the gate silently. Replaying the failing
  seed reported that every invariant held, because reading the log after each
  step opened files and a fault's position is an ordinal in the stream those
  opens joined. The debugger moved the fault it was built to show.
- Suppressing the operation count fixed the arithmetic and not the problem.
  Opening a log also creates the commits file, caches segment handles, and
  truncates a torn tail. The tools now inspect a copy of the disk, and the run
  never learns it was watched.
- The test that should have caught it compared outcomes for one configuration
  that passed either way. Its replacement compares the operation stream across
  five configurations, which fails whether or not a bug is present to expose it.
- Seeds now carry a signature over the whole operation stream, not only its
  length. A stream reordered without changing length is drift a count calls
  unchanged.

M5, on 2026-08-14:

- The spine is faster than Kafka in every mode measured: 1.35x in `sync`, 4.0x
  in `batch`, 23.5x in `os` at 64 byte records. Most of that is where the two
  systems put their process boundary rather than a better log, and the
  experiment cannot separate the two because the spine has no wire protocol.
- The protocol written in advance caught its own error one layer down. The first
  complete run reported a 3.2x win in `sync` mode, wrong in the spine's favour
  by the batch size: the protocol paired the modes on "both fsync before
  acknowledging every record", which was true of Kafka and not of the spine,
  which decides durability once per `Append` call. Corrected to one record per
  call, the same row reads 1.35x.
- Two of four recorded predictions held. Kafka's sequential read advantage
  vanishes at 1 KiB records rather than merely narrowing, and the spine's tail
  latency is markedly *tighter* than Kafka's rather than looser — the prediction
  that came out backwards is the result that most favours the spine.
- Kafka costs 411 MB of image and 299 MB of resident memory for an idle broker,
  against 10.7 MB and none. It buys replication, partitions, a wire protocol,
  and an ecosystem, all of which were configured away to make the comparison
  possible.
- `Snapshot` has no Kafka equivalent, which is the first place the narrow `Log`
  interface leaks: `log-design.md` claims Kafka can sit behind it without
  distorting either side, and a compacted topic is a different mechanism rather
  than an implementation of that one.
- No durability claim was published, as the protocol said in advance. The
  `sync` numbers describe a virtualised disk, and nothing here has met real
  power loss.

Power loss, on 2026-08-14:

- The trigger set in M2 has fired. A real kernel, real ext4, and a real block
  device now agree with `sim.FS` about what an fsync buys, across 20 cuts with
  segments rolling throughout, and a control round that claims durability
  without syncing fails as it must.
- It found a hole in the middle of a log on the first cut that could roll, and
  the mode sweep it motivated found two more bugs, one of them a panic that had
  been reachable since compaction landed.
- What is still untested is narrower and stated: no physical drive, so no
  write-cache loss, no sector tearing, and no controller reordering. This is
  power loss below the filesystem, not below the disk.

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
