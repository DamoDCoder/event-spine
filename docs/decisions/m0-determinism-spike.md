# M0 — Determinism Spike Against Signal Garden

**Date:** 2026-08-10
**Subject:** `signal-garden` at commit `bb17562` (M0 batch simulation plus M1 live
engine and gRPC/REST surface).
**Status:** complete. The findings below are the design input for this
repository's M1 injection interfaces.

## The Claim Under Test

> Signal Garden's existing M0 can be made bit-reproducible across host and
> container. If it cannot, find out why before building anything on top of it.

## What Was Measured

Signal Garden's own CLI was the instrument — no code was added to that repository
for the batch measurements. The `signalgarden` binary prints a `snapshot` line
containing `domain.Garden.Hash()`, a SHA-256 over every organism's
`id|moisture|health|stage`.

| Run | Configuration | Result |
| --- | --- | --- |
| 100 separate host processes | `-seed 42 -ticks 200 -organisms 20 -duplicate-every 3` | 100/100 identical: `d3d4ce19ecd1b035d51635d7a9cc8b311a2fc69a61c98d475643b5f188fac485` |
| 1,000 seeds, host (darwin/arm64) | `-ticks 120 -organisms 32 -duplicate-every 3`, seeds 1–1000 | digest of the seed→hash table: `37264b9654d1e56fc83645c2e33513498a1a413232bb430204543d024c6c7bf4` |
| 1,000 seeds, container (linux/arm64) | same, inside `golang:1.26-bookworm@sha256:6c5605ab…` | byte-identical table, same digest |
| 100 seeds, container (linux/amd64) | same, seeds 1–100 | byte-identical to the host's first 100 rows |
| Duplicate delivery | `-duplicate-every 3` versus `0` | 400 duplicates published and dropped by idempotency key; snapshot hash unchanged |

**The batch path is genuinely deterministic, not deterministic-looking.** It holds
across processes, across host and container, and across architectures. Idempotent
processing holds under deliberate redelivery.

The live path is where it stops holding.

> Leaks A and B are drawn in [concepts.md](../concepts.md).

## Leaks Found

### A. A live run is not reproducible from `(seed, controls, target tick)` — high

`engine.go:544` selects over the ticker channel, the command channel, and quit.
When a tick and a control command are both ready, Go picks uniformly at random.
The control change is staged and applied on a tick boundary, so the *shape* of the
run is well formed, but *which* tick it lands on is decided by the scheduler.

Measured with a temporary probe: 40 repetitions of one identical scenario —
seed 7, 16 organisms, 180 ticks, a control change issued as soon as the client
observes tick ≥ 40, tick interval 1 µs to make ticks and commands contend.

- 9 distinct `EffectiveTick` values, spanning ticks 41–49
- **7 distinct final projection hashes across 40 identical runs**
- terminal garden alive 5/16, so the projection was still sensitive to input

At the shipped 200 ms tick interval the race window is invisible: the same probe
at 1 ms produced one effective tick and one hash across 40 runs. The bug is not
that this is likely in production, it is that reproducibility depends on timing
margin rather than on construction.

Signal Garden's design already knows the remedy — `sim.SetControls` documents that
"the tick at which it applied is the only timing fact replay needs," and the
`control_changed` event carries `OccurredAt: tick`. But nothing persists that
event stream yet, so today a live run genuinely cannot be replayed. The remedy
exists on paper only.

**Design input for M1:** the replay key is the event log, never the run
configuration. A run is reproducible because its events were durable, not because
its inputs were recorded.

### B. Terminal-state hash equality passes vacuously — high, methodological

This is the finding worth the spike on its own.

The same probe at 600 ticks reported **one hash across 40 runs** while the
effective tick still varied across 12 distinct values. The runs had genuinely
diverged; the check could not see it. The garden had reached an absorbing state —
`alive=0/16`, and a dead organism absorbs events without effect — so every
divergent history folded to the same terminal projection.

A determinism test that compares only the final projection is strongest exactly
where the system is least interesting, and blind exactly where divergence is
cheapest to introduce.

**Design input for M1:** `task verify:determinism` must compare a hash *chain*
folded over every step, not a terminal snapshot, and must assert the run stayed
live — a run that reaches an absorbing state proves nothing after the step it got
there. Both properties belong in the harness, not in each test.

### C. Snapshot fan-out is lossy, so the stream is not a replayable input — medium

`engine.go:607` drops a frame for any subscriber whose buffer is full, rather than
stalling the run. That is the right call for a UI. It also means the frame
sequence a consumer observes is a function of how fast that consumer ran: the
probe saw 12 distinct drop counts across 40 identical runs.

**Design input for M1:** projections fold the log. Streams are for display, and
nothing that folds into durable state may read one.

### D. Map iteration in production paths — low

`engine.go:459` (`Registry.Close` over `g.runs`), `engine.go:613` (`publish` over
`r.subs`), `engine.go:633` (`closeSubs` over `r.subs`). None is currently
observable: the accounting they perform is order-independent, and run shutdown
has no cross-run interaction. They are listed because the rule is mechanical for a
reason — each becomes a real leak the first time the loop body stops commuting.

`processor.go:102` and `service/garden.go:269` also range maps, but both copy
map-to-map and are order-independent by construction. `run.go:129` ranges a map
and then sorts, which is correct.

### E. A wall-clock field in the durable envelope — latent

`event.Event.RecordedAt` is a `time.Time` on the durable record. No production
code populates it today; it is always the zero value, which is why the batch path
survives. It is a trap armed for whoever first writes an ingest timestamp into it.

Event Spine's record format in `docs/log-design.md` already excludes wall clock
entirely — the header carries a logical timestamp from the injected clock and has
no field a wall-clock value could occupy. That decision is now measured rather
than asserted; keep it.

Wall-clock values in the *view* types (`Run.StartedAt/UpdatedAt/FinishedAt`,
`GardenSnapshot.ObservedAt`, `TelemetrySnapshot.ObservedAt/Uptime`) are contract
surface, not projection state, and are correct where they are. They must never
enter a hash.

## Why The Existing Tests Missed Leak A

`internal/engine`'s tests drive a `ManualClock` whose tick channels are
unbuffered, so `Tick` returns only once the loop has taken the tick. That is
excellent test design — it is why those tests do not sleep and do not flake — and
it also means tick and command can never be ready at the same instant. The suite
is structurally incapable of observing the race. `TestEngineMatchesBatchRun` pins
the live engine to the batch run under exactly the conditions where the race
cannot occur.

This is the general shape to watch for in Event Spine: **a deterministic test
harness hides scheduler nondeterminism unless the harness itself enumerates the
interleavings.** The M3 scheduler must choose interleavings from the seed rather
than merely serializing them, or it will inherit this blind spot.

## What Changed As A Result

1. `verify:determinism` compares a per-step hash chain and asserts liveness, not
   a terminal projection hash. Leak B is the reason this is a harness property
   rather than a convention.
2. The `Clock` interface must cover the *tick source*, not only `Now()`. Leak A
   is a scheduling leak, and an injected `Now()` alone would not have caught it.
3. The simulation scheduler owns the choice between a ready timer and a ready
   command, and takes that choice from the seed. This is the M3 design's first
   fixed requirement.
4. The record format keeps its logical-timestamp-only header, now on evidence.
5. Replay is keyed on the event log. No API accepts "reproduce this run" with a
   configuration.

## Recommended Follow-Up In Signal Garden

The probe was temporary and has been removed; that repository is unchanged. The
race it found is real and currently untested there. Worth landing as a permanent
test in `internal/engine` — a contended-interval variant asserting that a live run
replays from its `control_changed` events — once Signal Garden has somewhere
durable to replay them from.

## Next Milestone's Riskiest Assumption

M1 assumes the five injected interfaces (`Clock`, `Source`, `IDGen`, `FS`,
`Transport`) are sufficient to close every leak. This spike found one that none of
them covers cleanly: Leak A is neither time nor randomness nor I/O, but the choice
between two ready events. If that choice is not itself an injected dependency,
M1's 1,000-seed gate will pass on a core that still cannot reproduce a
concurrent run — which is precisely the failure mode this spike exists to prevent.
