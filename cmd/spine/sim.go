package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/DamoDCoder/event-spine/internal/devtools/harness"
)

func simulate(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("sim: a simulation is required, try: sim sweep")
	}
	switch args[0] {
	case "crash-matrix":
		return simCrashMatrix(args[1:])
	case "sweep":
		return simSweep(args[1:])
	case "corpus":
		return simCorpus(args[1:])
	default:
		return fmt.Errorf("sim: unknown simulation %q", args[0])
	}
}

func simCrashMatrix(args []string) error {
	fs := flag.NewFlagSet("sim crash-matrix", flag.ContinueOnError)
	var (
		seeds  = fs.Int("seeds", 8, "number of seeds to run the matrix for")
		from   = fs.Int64("seed", 1, "first seed")
		points = fs.Int("points", 0, "crash points per seed, or 0 for all of them")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}

	var total harness.Report

	for i := range *seeds {
		seed := *from + int64(i)
		report, err := harness.Matrix(seed, *points)
		if err != nil {
			return fmt.Errorf("seed %d: %w", seed, err)
		}
		for _, f := range report.Failures {
			fmt.Printf("FAIL seed %d %s: %v\n", seed, harness.FormatFaults(f.Config.Faults), f.Err)
		}
		fmt.Printf("seed %d: %d crash points, %d failures\n", seed, report.Runs, len(report.Failures))

		total.Runs += report.Runs
		total.Failures = append(total.Failures, report.Failures...)
		total.Compactions += report.Compactions
		total.Dropped += report.Dropped
		total.Snapshots += report.Snapshots
	}

	// The coverage line is not decoration. A matrix that stopped compacting
	// or snapshotting would still report zero failures, and this is what
	// makes that visible.
	fmt.Printf("\ncrash-matrix: %d points across %d seeds, %d failures\n", total.Runs, *seeds, len(total.Failures))
	fmt.Printf("exercised: %d compactions dropping %d records, %d snapshots\n",
		total.Compactions, total.Dropped, total.Snapshots)
	if len(total.Failures) > 0 {
		return fmt.Errorf("crash-matrix: %d crash points lost data that was acknowledged as durable", len(total.Failures))
	}
	return nil
}

// simSweep runs fresh seeds with swarm-randomized fault configurations.
func simSweep(args []string) error {
	fs := flag.NewFlagSet("sim sweep", flag.ContinueOnError)
	var (
		count    = fs.Int("count", 1000, "how many seeds to run")
		from     = fs.Int64("seed", 1, "first seed")
		minimize = fs.Bool("minimize", false, "shrink each failure before reporting it")
		out      = fs.String("out", "", "directory to write new corpus entries to")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}

	report := harness.Sweep(*from, *count, *minimize)

	for _, f := range report.Failures {
		fmt.Printf("FAIL seed %d steps %d %s faults %s\n     %v\n",
			f.Config.Seed, f.Config.Steps, durabilityName(f.Config),
			harness.FormatFaults(f.Config.Faults), f.Err)
		if *out == "" {
			continue
		}
		path, err := writeCorpusEntry(*out, f)
		if err != nil {
			return err
		}
		fmt.Printf("     written to %s\n", path)
	}

	fmt.Printf("\nsweep: %d seeds, %d failures\n", report.Runs, len(report.Failures))
	fmt.Printf("injected: %s\n", formatCoverage(report.Faults))
	fmt.Printf("exercised: %d compactions dropping %d records, %d snapshots (sampled)\n",
		report.Compactions, report.Dropped, report.Snapshots)

	if len(report.Failures) > 0 {
		return fmt.Errorf("sweep: %d seeds broke an invariant", len(report.Failures))
	}
	return nil
}

// formatCoverage renders the fault histogram in a fixed order, because a map
// ranged straight into output would report a different line every run.
func formatCoverage(faults map[harness.Kind]int) string {
	parts := make([]string, 0, len(harness.Kinds))
	for _, kind := range harness.Kinds {
		parts = append(parts, fmt.Sprintf("%s %d", kind, faults[kind]))
	}
	return strings.Join(parts, ", ")
}

// simCorpus replays every configuration in the regression corpus.
//
// A corpus entry is a run that once broke an invariant. Replaying them all is
// the regression suite, and it grows for free.
func simCorpus(args []string) error {
	fs := flag.NewFlagSet("sim corpus", flag.ContinueOnError)
	dir := fs.String("dir", "seeds", "directory holding the seed corpus")
	if err := fs.Parse(args); err != nil {
		return err
	}

	entries, err := loadCorpus(*dir)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		fmt.Printf("corpus: no seeds in %s yet\n", *dir)
		return nil
	}

	var failed, drifted int
	for _, e := range entries {
		if err := harness.Run(e.cfg); err != nil {
			failed++
			fmt.Printf("FAIL %s (seed %d %s): %v\n", e.file, e.cfg.Seed, harness.FormatFaults(e.cfg.Faults), err)
			continue
		}
		fmt.Printf("ok   %s (seed %d %s)\n", e.file, e.cfg.Seed, harness.FormatFaults(e.cfg.Faults))

		// A passing seed that no longer fires where it was recorded is
		// still a passing seed, so this warns rather than fails. It is
		// reported at all because the alternative is a corpus that
		// quietly stops testing what it was built to test.
		if e.ops == 0 && e.signature == 0 {
			continue
		}
		ops, signature, err := harness.CleanOps(e.cfg)
		if err != nil {
			return fmt.Errorf("corpus: %s: %w", e.file, err)
		}
		switch {
		case e.ops != 0 && ops != e.ops:
			drifted++
			fmt.Printf("     drifted: the run is %d operations now, %d when the seed was recorded\n", ops, e.ops)
		case e.signature != 0 && signature != e.signature:
			drifted++
			fmt.Printf("     drifted: the run is the same length and a different shape (%016x, recorded %016x)\n",
				signature, e.signature)
		}
	}

	fmt.Printf("\ncorpus: %d seeds, %d failures, %d drifted\n", len(entries), failed, drifted)
	if failed > 0 {
		return fmt.Errorf("corpus: %d seeds still reproduce", failed)
	}
	return nil
}

func repro(args []string) error {
	fs := flag.NewFlagSet("repro", flag.ContinueOnError)
	var (
		seed   = fs.Int64("seed", 0, "the seed to reconstruct")
		steps  = fs.Int("steps", 0, "workload steps, or 0 for the default")
		faults = fs.String("faults", "", "faults to inject, as kind@position[:arg], space separated")
		point  = fs.Int("point", 0, "shorthand for --faults crash@N")
		mode   = fs.String("durability", "", "log durability mode: batch, sync, or os")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *seed == 0 {
		return fmt.Errorf("repro: --seed is required")
	}

	spec := *faults
	if spec == "" && *point > 0 {
		spec = fmt.Sprintf("crash@%d", *point)
	}

	if spec != "" {
		parsed, err := harness.ParseFaults(spec)
		if err != nil {
			return err
		}
		cfg := harness.Config{Seed: *seed, Steps: *steps, Faults: parsed, Durability: *mode}
		if err := harness.Run(cfg); err != nil {
			return fmt.Errorf("seed %d %s reproduces: %w", *seed, harness.FormatFaults(parsed), err)
		}
		fmt.Printf("seed %d %s: no failure\n", *seed, harness.FormatFaults(parsed))
		return nil
	}

	// With no faults named, run the crash matrix for the seed: every
	// filesystem operation in turn.
	report, err := harness.Matrix(*seed, 0)
	if err != nil {
		return err
	}
	for _, f := range report.Failures {
		fmt.Printf("FAIL seed %d %s: %v\n", *seed, harness.FormatFaults(f.Config.Faults), f.Err)
	}
	fmt.Printf("seed %d: %d crash points, %d failures\n", *seed, report.Runs, len(report.Failures))
	if len(report.Failures) > 0 {
		return fmt.Errorf("seed %d reproduces at %d points", *seed, len(report.Failures))
	}
	return nil
}

// corpusEntry is one seed file: the configuration a failure reduces to, and the
// file that explains it.
type corpusEntry struct {
	file string
	cfg  harness.Config

	// ops and signature describe the operation stream when the seed was
	// recorded. A fault's position is an index into that stream, so a
	// change in either means the seed no longer fires where it did. The
	// signature is the stronger of the two: it catches a stream that was
	// reordered without changing length.
	ops       int
	signature uint64
}

// writeCorpusEntry records a fresh failure as a seed file.
//
// It is written with `fixed_in: pending`, because a sweep finds bugs and does
// not fix them. The corpus is expected to be red until somebody does.
func writeCorpusEntry(dir string, f harness.Failure) (string, error) {
	path := filepath.Join(dir, fmt.Sprintf("%04d.md", f.Config.Seed))
	if _, err := os.Stat(path); err == nil {
		return "", fmt.Errorf("corpus: %s already exists; a seed is never overwritten", path)
	}

	ops, signature, err := harness.CleanOps(f.Config)
	if err != nil {
		return "", fmt.Errorf("corpus: count operations for seed %d: %w", f.Config.Seed, err)
	}

	body := fmt.Sprintf(`---
seed: %d
steps: %d
durability: %s
ops: %d
signature: %016x
faults: %s
found: (fill in)
milestone: M3
fixed_in: pending
---

Found by `+"`task sim:sweep`"+`. The invariant it broke:

    %v

Replace this paragraph with what went wrong and why, once it is understood. A
seed file whose body is still the sweep's own output is a bug report nobody has
read yet.

`+"```"+`
task repro SEED=%d FAULTS="%s"
`+"```"+`
`,
		f.Config.Seed, f.Config.Steps, durabilityName(f.Config), ops, signature,
		harness.FormatFaults(f.Config.Faults), f.Err,
		f.Config.Seed, harness.FormatFaults(f.Config.Faults))

	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return "", fmt.Errorf("corpus: write %s: %w", path, err)
	}
	return path, nil
}

// durabilityName is the mode a seed file records, defaulting to the one every
// seed written before the field existed was running.
func durabilityName(cfg harness.Config) string {
	if cfg.Durability == "" {
		return "batch"
	}
	return cfg.Durability
}

// loadCorpus reads the seed files' front matter.
//
// The format is the handful of `key: value` lines documented in
// seeds/README.md, parsed here rather than with a YAML library. A dependency
// for four fields would be a dependency to keep working forever, and the
// project's premise is understanding its own internals.
func loadCorpus(dir string) ([]corpusEntry, error) {
	names, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("corpus: read %s: %w", dir, err)
	}

	var entries []corpusEntry
	for _, name := range names {
		if name.IsDir() || !strings.HasSuffix(name.Name(), ".md") || name.Name() == "README.md" {
			continue
		}
		path := filepath.Join(dir, name.Name())
		body, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("corpus: read %s: %w", path, err)
		}
		entry, err := parseCorpusEntry(name.Name(), string(body))
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}

	// Sorted, so a corpus run reports in the same order every time.
	sort.Slice(entries, func(i, j int) bool { return entries[i].cfg.Seed < entries[j].cfg.Seed })
	return entries, nil
}

func parseCorpusEntry(file, body string) (corpusEntry, error) {
	entry := corpusEntry{file: file}
	var haveSeed, haveFaults bool

	for line := range strings.SplitSeq(body, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)

		switch strings.TrimSpace(key) {
		case "seed":
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return entry, fmt.Errorf("corpus: %s: seed %q is not a number", file, value)
			}
			entry.cfg.Seed, haveSeed = n, true

		case "signature":
			n, err := strconv.ParseUint(value, 16, 64)
			if err != nil {
				return entry, fmt.Errorf("corpus: %s: signature %q is not hexadecimal", file, value)
			}
			entry.signature = n

		case "ops":
			n, err := strconv.Atoi(value)
			if err != nil {
				return entry, fmt.Errorf("corpus: %s: ops %q is not a number", file, value)
			}
			entry.ops = n

		case "durability":
			switch value {
			case "batch", "sync", "os":
				entry.cfg.Durability = value
			default:
				return entry, fmt.Errorf("corpus: %s: %q is not a durability mode", file, value)
			}

		case "steps":
			n, err := strconv.Atoi(value)
			if err != nil {
				return entry, fmt.Errorf("corpus: %s: steps %q is not a number", file, value)
			}
			entry.cfg.Steps = n

		case "faults":
			faults, err := harness.ParseFaults(value)
			if err != nil {
				return entry, fmt.Errorf("corpus: %s: %w", file, err)
			}
			entry.cfg.Faults, haveFaults = faults, true

		case "point":
			// The shorthand the corpus used before there was a
			// fault catalogue. Kept because seeds are never
			// deleted, and a seed nobody can replay is deleted in
			// every sense that matters.
			n, err := strconv.Atoi(value)
			if err != nil {
				return entry, fmt.Errorf("corpus: %s: point %q is not a number", file, value)
			}
			entry.cfg.Faults = []harness.Fault{{Kind: harness.Crash, At: n}}
			haveFaults = true
		}
	}

	if !haveSeed {
		return entry, fmt.Errorf("corpus: %s has no seed in its front matter", file)
	}
	if !haveFaults {
		return entry, fmt.Errorf("corpus: %s names no faults in its front matter", file)
	}
	return entry, nil
}
