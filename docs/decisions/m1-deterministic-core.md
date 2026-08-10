# M1 — Deterministic Core

**Date:** 2026-08-10
**Status:** complete.

## The Claim Under Test

> 1,000 seeds produce one projection hash. Any divergence is a design fault, not
> a flake.

## What Was Measured

The gate's checks are drawn in [concepts.md](../concepts.md).

`task verify:determinism` runs the ledger workload in `internal/sim` — credits,
debits, and transfers over 16 accounts, 500 commands per seed — and folds every
seed's chain digest into one aggregate.

| Environment | Digest |
| --- | --- |
| Host, darwin/arm64 | `17f6c8f9b1aa7703fa759c301d6684ed85d899c2162a62cd1c89eb871bd89c96` |
| Container, linux/arm64 | identical |
| Container, linux/amd64 | identical |

> **The digest changed on 2026-08-10, after this milestone closed.** It was
> originally `63b11be7e2656a89f4fdc3b6b093ef6c8dd832e027bf3c3768c035ef777d0f69`.
> `Event.AppendCanonical` claimed to encode the durable record's body but
> carried a payload-length field the record format in `docs/log-design.md` does
> not have, so an in-memory digest and one recomputed from a replayed segment
> would have disagreed. Dropping the redundant field changed every chain, and
> therefore the aggregate. The claim this milestone tested is unaffected — one
> digest, three environments — and the change was made while `seeds/` was still
> empty, which is the only time it is cheap.

Across those 1,000 seeds: 534,626 events folded, 46,736 commands rejected for
insufficient funds, 0 runs absorbed. Each seed is also run twice within one
process and the two chains compared, because Go randomizes map iteration per
range statement and a projection can therefore disagree with itself without
leaving the process.

**The claim held.** One hash, three environments, two architectures.

## The Gate Was Checked For Teeth

A gate that has never failed is a gate nobody has tested. The ledger's digest was
temporarily rewritten to range a map instead of its balance slice, and the gate
rejected seed 1 immediately, on the in-process comparison, before the aggregate
was ever computed:

```text
spine: seed 1 is not deterministic within one process:
  run 1 chain 7636c1000650f0bcbcf900de7c9912dad9891bfa0534b94a7909e5b23f408f07
  run 2 chain 94ff7f9f93dd92d2cc87446d8c4256b2efb1825662420cacf89fff92d2bded13
```

The injected fault was reverted and the digest returned to the value above. This
is the exact class of bug `scripts/check-determinism.sh` cannot see, which is why
the note in `CLAUDE.md` about reviewing map iteration by hand now has a
mechanical backstop as well.

## What Changed As A Result

The M0 spike's findings drove four changes that were not in the original M1
description:

1. **`Scheduler` joined the injected dependency set.** M0's leak A — a `select`
   between a ready timer and a ready command — is neither time, randomness, nor
   I/O, so `Clock`, `Source`, `IDGen`, `FS`, and `Transport` between them did not
   cover it. It is now `core.Scheduler`, and `sim.Scheduler` draws from a stream
   seeded separately from the workload's, so changing how many events a command
   emits does not silently reshuffle every later interleaving and invalidate the
   seed corpus.
2. **The gate compares a hash chain, not a terminal projection.** M0's leak B
   showed terminal-hash equality reporting 40 divergent runs as identical once
   the projection was absorbed.
3. **An absorbed run fails the gate rather than passing it.** Agreement between
   two absorbed runs is evidence about the absorbing state, not about
   determinism, so `verify determinism` refuses to count it.
4. **The generator is written out and its output is pinned to the published
   splitmix64 vectors.** A committed seed's entire value is reproducing its
   failure years later, and a seed corpus that silently stops meaning anything
   because the standard library's generator changed would be worse than no
   corpus. Integer-only arithmetic is also what makes the linux/amd64 column
   above match.

`FS` and `Transport` are still absent from `Deps`. Neither has a consumer until
M2, and this repository does not build ahead of the first one.

## Next Milestone's Riskiest Assumption

M2 assumes an injected `FS` can be faithful enough that passing the crash matrix
in simulation means something about real disks. That is a much stronger
assumption than the ones M1 rested on: a simulated filesystem models fsync
ordering, partial writes, and torn tails as the author *believes* they behave,
and a belief that is wrong produces a green crash matrix and real data loss.

The single end-to-end test against real disk that `CLAUDE.md` insists on keeping
is the only thing standing between that assumption and a silent failure. It
should be written early in M2 rather than at the end, and it should be the test
that fails first when the model drifts.
