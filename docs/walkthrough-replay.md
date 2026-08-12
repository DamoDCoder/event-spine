# Debugging A Real Failure By Seed

This is the M4 artifact: one bug, found by the sweep and diagnosed from its seed
to its root cause **without adding a print statement to the log**.

The bug is real and was real — it is the one recorded in
[seeds/0008.md](../seeds/0008.md), fixed in `ff1cded`. To make the walkthrough
reproducible rather than a screenshot, `task demo:replay` checks out a worktree
with that one fix reverted and replays the commands below. Every block of output
here is copied from that run.

## 1. The corpus says something is wrong

```
$ task sim:corpus
FAIL 0008.md (seed 8 bitflip@5:458021): after step 2: the scan read offset 536870912, outside [0, 5)
corpus: 6 seeds, 1 failures, 0 drifted
```

That is the whole bug report: a seed, a step count, and a fault. Everything
below comes from those three numbers.

## 2. Scrub the run to find when it went wrong

```
$ spine replay --seed 8 --steps 3 --faults "bitflip@5:458021"
seed 8  steps 3  faults bitflip@5:458021
signature 393668c5b6c17ef7 over 5 operations

step  0  offsets [0,0)          records   0  segments 1  snapshot none digest cbf29ce484222325
step  1  offsets [0,1)          records   1  segments 1  snapshot none digest 44eb4afb85959026
step  2  offsets [0,536870913)  records   1  segments 1  snapshot none digest 29bb3e9201a2ee86

the run stopped: after step 2: the scan read offset 536870912, outside [0, 5)
INVARIANT BROKEN: after step 2: the scan read offset 536870912, outside [0, 5)
```

Steps 0 and 1 are ordinary. At step 2 the log's tail becomes 536870913 while it
still holds one record. A log with one record and half a billion offsets behind
it is not a log that lost data — it is a log that believes something impossible.

## 3. Ask what should have happened instead

```
$ spine replay --seed 8 --steps 3 --faults "bitflip@5:458021" --diff
seed 8  faults bitflip@5:458021
  faulted: signature 393668c5b6c17ef7, 3 steps
  clean:   signature 393668c5b6c17ef7, 3 steps

first divergence at step 2

  faulted: step  2  offsets [0,536870913)  records   1  segments 1  digest 29bb3e9201a2ee86
  clean:   step  2  offsets [0,5)          records   5  segments 1  digest d02dd71f451ce911

what the faulted run did during step 2:
  op   5  append   00000000000000000000.log   <- bitflip fired here
```

The counterfactual is the same seed with the fault removed. Both runs perform
the identical sequence of filesystem operations — the signatures match — and
they agree until step 2, where the clean run holds five records at offsets
`[0, 5)` and the faulted one holds one.

So the flip did not destroy four records. It changed what the log thought the
first one *was*.

## 4. Look at the operation the fault landed on

```
$ spine replay --seed 8 --steps 3 --faults "bitflip@5:458021" --ops
op   1  step  0  create   00000000000000000000.log
op   2  step  0  syncdir  .
op   3  step  0  sync     00000000000000000000.log
op   4  step  1  append   00000000000000000000.log
op   5  step  2  append   00000000000000000000.log   <- bitflip fired here
```

One bit, in a record appended to the active segment. Nothing else touched the
disk.

## 5. Read the number

536870912 is `0x20000000`: a single set bit, the 30th. The record's offset
should have been 1. The flip did not corrupt a payload or a key — it landed in
the **offset field**, and the log accepted it.

At that point the root cause is one question: why did the checksum not catch a
flipped bit? The answer is in the framing comment in `internal/log/record.go`,
which says the offset is deliberately outside CRC coverage because a reader
always knows which offset comes next and compares against it.

That reasoning was true when it was written and stopped being true when
compaction landed. Compaction preserves offsets while dropping records, so a
scan can only require offsets to **ascend** — and a flip that moves an offset
*further along* passes an ascending check. `readRecordFrom` even names this case
as the one it cannot see.

The consequence follows from there: recovery takes a segment's next offset from
the last record it scanned, so the impossible offset became the log's tail, and
every later append would have been assigned an offset from there.

## What this demonstrates, and what it does not

No print statement was added, no debugger attached, and no test written to
explore the failure. The tools used were the corpus, the scrub, the
counterfactual diff, and the operation trace — and each of them ran the same
execution the failing check ran, which is the property the whole exercise
depends on.

It is worth saying plainly that the first version of these tools **did not** have
that property. Replaying seed 0008 reported that every invariant held, because
reading the log after each step opened files, and a fault's position is an
ordinal in the stream of filesystem operations. The tool moved the fault it was
supposed to be showing. That is recorded in
[m4-replay-devtools.md](decisions/m4-replay-devtools.md); the fix was to inspect
a copy of the disk rather than the running log.

The bug diagnosed here is one the harness had already found and named. A
walkthrough of a *fresh* failure would be stronger evidence, and the honest
position is that this demonstrates the tools work on a known bug rather than
that they would have found an unknown one unaided.
