# Seed Corpus

Every seed that ever found a bug lives here, forever. CI replays all of them on
every change. This is the regression suite, and it grows for free.

## Format

One file per seed, named `<seed>.md`:

```markdown
---
seed: 8
steps: 3
faults: bitflip@5:458021
found: 2026-08-12
milestone: M3
fixed_in: ff1cded
---

A single flipped bit in a record's offset field was accepted, and the reader
returned offset 536870912 from a log holding [0, 5).

Minimized to three workload steps and one flipped bit.
```

`seed`, `steps`, and `faults` are the whole reproduction: nothing else is
needed, on any machine, ever. `fixed_in` is the commit that made the seed pass,
or `pending` while it still reproduces.

### Faults

Written `kind@position` or `kind@position:argument`, space separated. Position
is a filesystem operation for the disk faults and a workload step for
`clockback`, since a clock fault is not something the disk does.

| Kind | Argument | What it models |
| --- | --- | --- |
| `crash` | — | The machine stops. Unsynced writes are gone; files whose directory entry was never synced disappear entirely. |
| `writeerror` | — | One append fails. |
| `shortwrite` | bytes accepted | Part of a write lands and the caller is told it failed. |
| `syncerror` | — | One fsync fails. |
| `bitflip` | byte to flip | One bit of one byte already on the disk is inverted. |
| `clockback` | nanoseconds | The injected clock jumps backwards, never below zero. |

`faults: none` is a run with no faults at all, which is a legitimate corpus
entry when the bug was in the workload rather than under it.

The older entries carry `point: N` instead, from before there was a catalogue.
It is still parsed and means `crash@N`. A seed nobody can replay is deleted in
every sense that matters.

## Rules

- **Commit the seed before the fix.** The corpus should fail, then pass. A seed
  committed after the fix proves nothing.
- **Minimize first.** `task sim:sweep` does it automatically, or
  `spine sim sweep --seed N --count 1 --minimize` for one. The minimizer removes
  faults one at a time and then shortens the run, keeping only what still
  reproduces the same failure. An unminimized seed at step 400,000 is a bug
  report nobody opens.
- **Never delete a seed.** A seed that stops failing is a seed that guards a fix.
  If it is genuinely obsolete because the feature is gone, say so in the file
  rather than removing it.
- **One seed per distinct bug.** Ten seeds that all catch the same off-by-one
  make the corpus slow without making it stronger.
- **Seeds that caught the harness are kept too**, and say so in the first line.
  A checker that reports the wrong component is worse than one that misses: it
  sends someone to read working code, and it teaches everyone to skim the
  output.

## Reproducing

```bash
task repro SEED=8 STEPS=3 FAULTS="bitflip@5:458021"
task sim:corpus                     # replay everything here
task sim:sweep COUNT=2000           # fresh seeds, minimized, written here
```

If `task repro` needs anything beyond the front matter — a specific machine, a
leftover data directory, an environment variable — the harness has a hole in it.
Closing that hole matters more than fixing the bug that exposed it.
