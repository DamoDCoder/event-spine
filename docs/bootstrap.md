# Event Spine — Repository Handoff

> **Bootstrap complete on 2026-08-09.** This file is kept as the record of how the
> repository was seeded, not as an instruction list. The two placeholders were
> resolved to:
>
> - module path: `github.com/DamoDCoder/event-spine`
> - base image: `golang:1.26-bookworm@sha256:6c5605ab3a9a9fb3c4eafe5b3d63cdbf3881caf113262b67862547b54a9db599`
>
> The `cmd/` and `internal/` directories in the layout below do not exist yet.
> Git cannot track empty directories, and they arrive with M1's first code.

Everything needed to start the `event-spine` repository. The files in this
directory are seeds: copy them into the new repository, fill in the two
placeholders listed below, and make the first commit.

## Placeholders To Fill In

Two things cannot be decided from the cradle. Both appear as `<<...>>` markers:

| Placeholder | Where | What to put |
| --- | --- | --- |
| `<<module-path>>` | `go.mod`, import paths, README | The Go module path, for example `github.com/your-account/event-spine` |
| `<<base-digest>>` | `Dockerfile` | The pinned `sha256:` digest of the chosen Go base image, from `docker buildx imagetools inspect golang:1.26-bookworm` |

The digest is not optional. "Latest" and floating tags are reproducibility bugs,
and this repository's entire premise is reproducibility.

## Target Layout

```text
event-spine/
├── README.md                   # from this directory
├── CLAUDE.md                   # from this directory
├── CHANGELOG.md                # from this directory
├── Taskfile.yml                # from this directory
├── Dockerfile                  # from this directory
├── .dockerignore               # from this directory
├── .gitignore                  # from this directory, renamed from gitignore
├── go.mod
├── bench/                      # committed benchmark results, not ignored
├── scripts/
│   └── check-determinism.sh    # from this directory, chmod +x
├── docs/
│   ├── roadmap.md              # copy ../roadmap.md
│   ├── architecture.md         # extract from ../README.md section 3
│   ├── log-design.md           # copy ../log-design.md
│   └── simulation-testing.md   # copy ../simulation-testing.md
├── seeds/
│   └── README.md               # from this directory, as seeds/README.md
├── cmd/
│   └── spine/                  # CLI: repro, bench, verify
└── internal/
    ├── core/                   # deterministic command/event/projection
    ├── log/                    # segmented append-only log
    ├── sim/                    # virtual clock, scheduler, faults
    └── devtools/               # scrub, branch, diff
```

`internal/` rather than `pkg/` until three projects consume the spine. Nothing
here has earned a stable public API yet, and `internal/` makes that honest at the
compiler level instead of in a comment.

## Bootstrap Sequence

```bash
mkdir -p ~/projects/personal/event-spine && cd ~/projects/personal/event-spine
git init
git config user.email "damodbear@damodbear.com.au"
git config user.name "damien.pitman"
```

The `git config` lines are not optional and are not covered by a global default.
Personal work must never carry a work email, and a repository-local override is
the only setting that cannot be lost by a change to the global config.

Then copy the seed files, renaming the two that cannot keep their names in this
directory:

```bash
cp gitignore          ~/projects/personal/event-spine/.gitignore
cp seeds-README.md    ~/projects/personal/event-spine/seeds/README.md
cp check-determinism.sh ~/projects/personal/event-spine/scripts/
chmod +x ~/projects/personal/event-spine/scripts/check-determinism.sh
```

Fill in the placeholders, then verify the container path works before writing any
Go:

```bash
task test:container
```

That should succeed on an empty package set. If the containerized test target does
not work before there is code, it will never be retrofitted cleanly afterwards.

## First Commit Sequence

```text
chore(repo): initialize module, container build, and task runner
docs(project): add brief, roadmap, log design, and simulation standard
feat(core): add deterministic command, event, and projection types
test(core): assert projection hash equality across 1,000 seeds
feat(log): implement segmented append-only log with offsets
test(sim): add virtual clock scheduler and first fault injectors
feat(devtools): add log scrub, branch, and projection diff
perf(log): measure append throughput and recovery time
docs(showcase): publish the log versus Kafka comparison
```

The first two commits should land before any Go beyond `main.go` exists. The
standards are cheap to adopt on an empty repository and expensive to adopt on a
full one, which is the entire reason this handoff exists.

## M0 Is A Spike Against Signal Garden, Not This Repository

Worth stating plainly so it is not skipped: the spine's M0 is a two-day
determinism spike run against Signal Garden's *existing* M0 in its own
repository. It answers one question — is that code actually deterministic or only
deterministic-looking — and produces a written list of every nondeterminism leak.

That list is the design input for this repository's M1. Starting `internal/core`
before the spike is done means designing the injection interfaces against
guesses rather than against the actual leaks.

Record the finding in this repository as `docs/decisions/m0-determinism-spike.md`
even though the code being examined lives elsewhere.
