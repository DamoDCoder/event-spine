# Power Loss, Finally Measured

**Date:** 2026-08-14
**Status:** the trigger set in M2 has fired. Two real bugs came out of it, one
of which no amount of simulation was going to find.

## The Debt This Pays

[m2-filesystem-model.md](m2-filesystem-model.md) was blunt about what had not
been tested:

> `SIGKILL` destroys a process, not a machine. The page cache survives it, so
> bytes appended without `fsync` are still readable afterwards — the
> process-kill test cannot observe the loss that `sim.FS.Crash` models.
> **The simulator's crash is power loss, and this repository has not yet tested
> against power loss.**

It named the trigger — the first published durability claim — and M4 and M5 both
recorded it as still owed. M5 then published throughput next to Kafka's while
explicitly refusing to say anything about data loss, which is honest and
unsatisfying in equal measure. This is that debt.

## How The Power Goes Off

`scripts/powercut.sh`, run by `task verify:powercut`:

1. The log lives on **ext4 on a loop device opened with `--direct-io`**, so a
   write that reaches the device reaches the backing file, with nothing of the
   host's caching in between.
2. **Background writeback is made lazy** for the duration
   (`dirty_expire_centisecs`, `dirty_writeback_centisecs`). Data the log did not
   fsync stays unwritten. This makes the test stricter, not weaker: more is at
   risk when the power goes.
3. The writer is **SIGKILLed and the backing file is snapshotted immediately**.
   The snapshot holds exactly what the block device received — which is the
   definition of what survives a power cut.
4. The snapshot is **mounted**, ext4 replays its journal as it would after any
   crash, and the log is asked what it kept.

The writer records durable counts to a file on a *different* filesystem, fsynced,
and only ever after `Sync` returned. That file is the claim under test: every
record it names has to still be there.

### Checked for teeth

A power cut that never loses anything is not a power cut, and every passing
round would then be evidence about writeback rather than about fsync. So a
control round claims durability **without ever syncing**, and it has to fail:

```
control: 120128 records were acknowledged durable, the log recovered to 120065
         the control failed as it must: unsynced data did not survive
```

## What It Found

### A hole in the middle of a log

The first twenty cuts passed — with 64 MiB segments, which meant a short run
never rolled. That was the harness being gentle rather than the log being right.
With segments small enough to roll, the very first cut failed:

```
durable record 576 was lost: 576 is outside segment 0000...0.log, which holds [0, 576)
```

Not a truncated tail: a **hole**, with records present on both sides of it.

`Log.Sync` said it made everything appended so far durable. Rolling in `os` mode
sealed the outgoing segment without syncing it — deliberately, on the reasoning
that a mode asking for no syncs gains nothing from one — and `Sync` only ever
touched the active segment. So after a roll, `Sync` returned true and left the
sealed segment's records in the page cache, where the cut took them.

The comment on `roll` predicted this exact failure and then exempted `os` mode
from the protection:

> A sealed segment holding writes that never reached the disk would be a hole no
> recovery pass goes looking for. OS mode is the exception it claims to be.

The mode promises not to sync *on its own*. It does not promise that an explicit
`Sync` is a lie. The first fix had `os` mode record what it owed for the next
`Sync` to pay, oldest first — which passed twenty cuts and was still wrong, for
a reason the next section gets to.

### Why simulation had not found it

The workload had run **only in batch mode** for two milestones, and in batch mode
a roll syncs the outgoing segment, so the gap cannot open. The fault catalogue
was rich; the configuration space was one point wide on an axis that mattered.

Durability mode is now part of a run's configuration, drawn from the seed like
everything else and recorded in seed front matter. Omitting it means batch, which
is what every seed written before the field existed was running, so the corpus
kept meaning what it meant and reported no drift.

The first sweep with modes varied failed **six of 200 seeds, with no fault
injected at all**:

- [seeds/0068.md](../../seeds/0068.md) — the fix above, one move later.
  Compaction closes a sealed segment when it swaps in the replacement, and the
  closed handle stayed on the list of syncs owed, so the next `Sync` tried to
  flush a file that no longer existed.
- [seeds/0290.md](../../seeds/0290.md) — a **nil pointer panic** that predates
  both. A crash can truncate one segment while the segment created after it
  survives empty, leaving a hole that runs to the tail; `Reader.Next` asked
  `seek` for the next surviving record, `seek` correctly found none and set the
  cursor to nil, and `Next` read through it.

That second one had been reachable since compaction landed. It took a real power
cut to motivate the mode sweep that exposed it.

### The first fix was the wrong shape, and the second cut said so

Twenty cuts passed after the fix above. Then one failed:

```
durable record 22066 was lost: sealed segment ...22066.log is damaged:
at byte 53126: length field is 0, below the 28 byte header
```

Recovery leans on "sealed" twice — it scans only the active segment, and it
refuses a sealed segment with a damaged tail rather than truncating one — and
both lean on the same premise: nothing has appended to a sealed segment since it
was sealed **and synced**. Recording the sync as a debt for the next `Sync` to
pay broke that premise. A machine that stops between the roll and that `Sync`
leaves a sealed segment holding acknowledged records and an unsynced tail, and
refusing the file takes the acknowledged prefix with it.

The invariant is now restored rather than tracked: **rolling syncs the outgoing
segment in every mode**, `os` included. It still never syncs on its own between
rolls, and it pays one fsync per segment — the same bargain the directory sync
made for the same reason. The debt list and the compaction bookkeeping it needed
are both gone, and with them the class of bug that `seeds/0068.md` recorded.

### Why simulation could not have found it either

This is the part worth keeping. A crashed file in `sim.FS` reverted to its
synced bytes, so a sealed segment came back *short and clean* — no damaged tail,
nothing for recovery to refuse. Real ext4 journals metadata separately from
data, so the file came back **longer than its contents, with zeros in the gap**,
and a zero length field is a corrupt record rather than a missing one.

The simulator was not wrong about fsync. It was optimistic about crashes in a
way no fault in the catalogue could express, and it had been for four
milestones. `sim.FS.CrashExtend` models the other case now, and the regression
test for this bug uses it: against the old behaviour it fails with the same
error ext4 produced.

That is the second time this exercise has found a gap in the *model* rather than
in the log, and both times the gap was invisible from inside the simulation.

## Where It Stands Now

- **Every cut loses nothing acknowledged as durable**, with segments rolling
  throughout — up to 186 segments in a run — so the cut lands during appends,
  syncs, segment creations, and directory syncs.
- The control still fails, so the cut still has teeth. It had to be strengthened
  to keep them: once rolling syncs in every mode, a writer that never calls
  `Sync` is still flushed by its rolls, so the control now uses one segment
  large enough that nothing rolls and nothing is synced at all. A control that
  passes is a test that has stopped testing.
- 500 sweep seeds across three durability modes and 207 distinct shapes, no
  failures.
- 8 corpus seeds, no drift.
- `task ci` runs five cuts, and `task ci:nightly` runs fifty. The script fails
  loudly on a host that cannot give it a privileged container rather than
  skipping, because a skip leaves the claim looking checked.

## What This Still Does Not Prove

This is power loss **below the filesystem, not below the disk**.

- There is **no physical drive**. A real disk has a volatile write cache, and
  `fsync` only protects data in it if the drive honours cache-flush commands.
  Everything here runs on a virtual block device inside a VM, so drive-level
  cache loss, FUA handling, and sector tearing are all still unexercised.
- **ext4 only**, one mount configuration, one kernel.
- The snapshot is taken after `SIGKILL` rather than at a genuinely arbitrary
  instant. Writeback is made lazy to widen the window, but a cut that landed
  *during* a writeback of a partially-written page is not reproduced here.
- Nothing tests **reordering by a real controller**, which is the failure mode
  that makes journalling filesystems hard in the first place.

So the honest statement is narrower than "the durability claims are proven": a
real kernel, a real filesystem, and a real block device now agree with `sim.FS`
about what an fsync buys, and the log's fsync discipline survives 20 cuts at
points it chooses badly. The remaining gap needs hardware, or a hypervisor that
can cut power between two writes.

## What Changed As A Result

- `Log.Sync` covers segments a roll sealed, in every mode.
- Compaction forgets handles it has closed.
- `Reader.Next` reports catching up rather than dereferencing a nil cursor.
- The simulation harness varies durability mode, which is the coverage gap that
  let the first bug through.
- `task verify:powercut` exists and is worth running before any durability claim
  is published.

## The Next Riskiest Assumption

Two, and both are about coverage rather than correctness.

Both of the risks this note first named have been addressed, and the answer to
one of them is more interesting than the other.

**The shape is no longer fixed.** Segment size, index interval, payload size,
batch size, and the batch-mode sync interval are drawn from the seed and
recorded in seed front matter, defaulting to the old constants so the corpus
kept its meaning. A sweep now reports how many distinct shapes and modes it
used, and a test asserts it varies them — because an axis that quietly stops
varying reports the same clean result as one that never did.

Widening it found nothing. 500 seeds across 207 shapes, no failures. That is a
weaker result than the durability axis produced and is worth recording as one:
the axis that mattered was found by a real disk, not by widening the simulation.

**The power cut runs in CI.** Five cuts in `task ci`, fifty in `ci:nightly`.

What is left is the same gap in a narrower form. The simulator has now been
wrong twice about what a crash leaves behind — once about directory entries,
once about file length — and both times a real filesystem was needed to notice.
The next assumption worth doubting is that `CrashExtend` and `Crash` are the
only two shapes a real crash takes. A cut landing inside a page write, a
filesystem other than ext4, and a drive that reorders writes are all still
outside what either the model or this test can produce.
