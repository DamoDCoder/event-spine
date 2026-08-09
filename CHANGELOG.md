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

### Decision Notes

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
