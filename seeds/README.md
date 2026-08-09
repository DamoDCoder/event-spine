# Seed Corpus

> Copy this file to `seeds/README.md` in the new repository.

Every seed that ever found a bug lives here, forever. CI replays all of them on
every change. This is the regression suite, and it grows for free.

## Format

One file per seed, named `<seed>.md`:

```markdown
---
seed: 8412
found: 2026-08-14
milestone: M2
fixed_in: a1b2c3d
---

Crash between segment write and rename during compaction left the reader
positioned in a segment that no longer existed. Recovery reported success while
silently skipping 412 records.

Minimized to: one compaction, one crash, one reader at offset 3200.
```

## Rules

- **Commit the seed before the fix.** The corpus should fail, then pass. A seed
  committed after the fix proves nothing.
- **Minimize first.** Run `task sim:sweep` with `--minimize`, or minimize by hand:
  replay while removing fault injections one at a time, keeping any removal that
  still reproduces. An unminimized seed at step 400,000 is a bug report nobody
  opens.
- **Never delete a seed.** A seed that stops failing is a seed that guards a fix.
  If it is genuinely obsolete because the feature is gone, say so in the file
  rather than removing it.
- **One seed per distinct bug.** Ten seeds that all catch the same off-by-one make
  the corpus slow without making it stronger.

## Reproducing

```bash
task repro SEED=8412     # reconstruct the exact failure
task sim:corpus          # replay everything here
```

If `task repro` needs anything beyond the seed — a specific machine, a leftover
data directory, an environment variable — the harness has a hole in it. Closing
that hole matters more than fixing the bug that exposed it.
