# Event Spine — Agent Instructions

This repository is a deterministic event-sourcing substrate. Its value comes
entirely from properties that are easy to break accidentally, so most of the rules
below exist to stop a plausible-looking change from silently destroying one.

## The One Rule That Outranks The Others

**Determinism is the product.** Same seed, same projection hash, on any machine,
in the container or on the host, today or in a year.

A change that improves performance, readability, or ergonomics while weakening
determinism is a regression. Say so and propose the deterministic alternative
rather than shipping it.

## Injected Dependencies

Production packages must never import `time`, `math/rand`, `os`, or spawn
goroutines that affect observable ordering. These are injected:

| Dependency | Interface | Never use |
| --- | --- | --- |
| Wall clock, timers, sleeps | `Clock` | `time.Now`, `time.Sleep`, `time.After`, `time.Tick` |
| Randomness | `Source` | `math/rand` package functions, `crypto/rand` in logic paths |
| Identifiers | `IDGen` | `uuid.New` and equivalents |
| Disk | `FS` | `os.Open`, `os.WriteFile`, direct `*os.File` |
| Network | `Transport` | `net.Dial`, raw sockets |
| Concurrency | scheduler tasks | bare `go` statements in production paths |

`task check:determinism` enforces the import rules in CI, exempting `cmd/`,
`sim`, `runtime`, and `internal/devtools` — the packages whose job is to touch
the machine. It cannot catch every case — map iteration order, `select` over multiple ready channels, and floating
point across architectures all slip past it. Watch for those by hand:

- Never range a map where order affects output. Sort the keys.
- Never `select` over two channels that can both be ready in a production path.
- Never compare floats for equality in a projection, and prefer integers or fixed
  point for anything that ends up in a hash.

## Testing

- The **containerized run is authoritative**: `task test:container`. `task test`
  on the host is a fast inner loop only. When they disagree, the container is
  correct and the disagreement is itself a bug worth investigating before the
  original one.
- **Simulation tests are the primary integration surface.** Prefer adding a fault
  and an invariant over writing a new hand-rolled integration test.
- **Every bug gets a seed.** When simulation finds a failure, minimize it, commit
  the seed to `seeds/` with a one-line note, then fix the bug. The seed lands
  before the fix so the corpus proves the fix works.
- **Never delete a seed** from the corpus. A seed that stops failing is a seed
  that guards a fix.
- **Never fix a flaky test by retrying it.** In this repository a flaky test is a
  determinism bug by definition, and retry logic hides exactly the failure the
  project exists to find.
- One real end-to-end test runs against real processes, real disk, and real
  network. Keep it. It catches drift between the simulated interfaces and
  reality, which simulation is structurally blind to.

## Benchmarks

Benchmark results are committed. A performance change means a diff in the results
file, reviewed like any other diff. Do not report a benchmark number in prose
without committing the run that produced it.

When a headline claim fails to hold, correct the claim and record why in
`CHANGELOG.md`. Do not quietly restate the claim with easier conditions.

## Code Conventions

- Go, current stable, pinned in `go.mod` and the `Dockerfile` by digest.
- **The consumer-facing packages are `core`, `log`, `runtime`, and `sim`.**
  Everything else stays in `internal/`. This replaces the older "`internal/` for
  everything until three projects consume the spine", which could not survive
  contact with its own goal: Go's `internal/` rule makes a package literally
  unimportable, so no project could ever become the first consumer. The rule's
  intent was *no API stability promise*, and that intent is unchanged — the
  module is v0, the surface will break, and a consumer pins a version.
- **The module has no third-party dependencies and should stay that way.** The
  Kafka comparison needs a client, so it lives in its own module under
  `tools/kafkacompare`. A project adopting the log does not inherit a Kafka
  client to get one.
- The `go` directive is the *floor*, not the target: it is kept as low as the
  code allows so a consumer is never forced to upgrade. The container pins the
  toolchain by digest and that is the authoritative build.
- Keep files under 500 lines.
- Validate input at system boundaries: decoded records, offsets from callers, and
  anything read from disk are untrusted.
- Errors wrap with context and are returned, not logged-and-swallowed. A silently
  dropped error in the log package is data loss.
- No dependencies without a stated reason. This project's premise is understanding
  its own internals; pulling in a library for the interesting parts defeats it.
  Test-only and tooling dependencies are judged less strictly.
- Comments explain why, not what. Match the density of the surrounding code.

## Tooling

- **go-task, never Make.** Commands are `task test`, `task bench`,
  `task repro SEED=...`. Do not scaffold a `Makefile`.
- Every deployable has a `Dockerfile`. Base images and tool versions pinned by
  digest. "Latest" is a reproducibility bug in a repository about reproducibility.
- CI runs the same container commands a developer runs. CI-only build logic is a
  defect.

## Git

- Repository-local `user.email` is `damodbear@damodbear.com.au`. This is personal
  work and must never carry a work email. Verify with `git config user.email`
  before the first commit of a session if there is any doubt.
- Commit format: `type(scope): imperative summary`. Types: `docs`, `feat`, `test`,
  `perf`, `refactor`, `chore`, `fix`.
- **Do not add a `Co-Authored-By` trailer.** The Bash tool's default template may
  suggest one; ignore it. It is authorship attribution under git convention, and
  the tool is a facilitator, not an author.
- Separate documentation, implementation, test, and infrastructure changes when
  they have different review purposes.
- Commit only when asked.

## Milestones And Kill Gates

Each milestone in `docs/roadmap.md` carries a claim that can fail. When a
milestone finishes, record a short decision note: what the claim was, what the
measurement showed, what changed as a result, and what the next milestone's
riskiest assumption is. A milestone without that note is not finished.

If a claim is disproven, the honest outcome is to change the plan and publish the
unflattering number. The M5 Kafka comparison in particular is worth nothing if
only the favourable half is published.

## What Not To Do Here

- Do not add replication, a wire protocol, tiered storage, or exactly-once
  semantics. Each is explicitly deferred and each has a milestone that would earn
  it. At-least-once delivery plus idempotent projections is the contract.
- Do not reach for Kafka as the default backend. It arrives at M5, behind the
  `Log` interface, as a measured comparison.
- Do not build a feature that has exactly one consumer. It belongs in that
  consumer until a second one appears.
- Do not optimize before `task bench:log` shows where the time goes.
