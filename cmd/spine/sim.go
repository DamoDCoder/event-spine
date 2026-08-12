package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/DamoDCoder/event-spine/internal/devtools/crash"
)

func simulate(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("sim: a simulation is required, try: sim crash-matrix")
	}
	switch args[0] {
	case "crash-matrix":
		return simCrashMatrix(args[1:])
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

	var (
		totalPoints int
		failed      int
	)
	for i := range *seeds {
		seed := *from + int64(i)
		report, err := crash.Matrix(seed, *points)
		if err != nil {
			return fmt.Errorf("seed %d: %w", seed, err)
		}
		totalPoints += report.Points

		for _, f := range report.Failures {
			failed++
			fmt.Printf("FAIL seed %d point %d: %v\n", seed, f.Point, f.Err)
		}
		fmt.Printf("seed %d: %d crash points, %d failures\n", seed, report.Points, len(report.Failures))
	}

	fmt.Printf("\ncrash-matrix: %d points across %d seeds, %d failures\n", totalPoints, *seeds, failed)
	if failed > 0 {
		return fmt.Errorf("crash-matrix: %d crash points lost data that was acknowledged as durable", failed)
	}
	return nil
}

// simCorpus replays every seed in the regression corpus.
//
// A corpus entry is a crash point that once broke an invariant. Replaying them
// all is the regression suite, and it grows for free.
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

	var failed int
	for _, e := range entries {
		if err := crash.Replay(e.seed, e.point); err != nil {
			failed++
			fmt.Printf("FAIL %s (seed %d point %d): %v\n", e.file, e.seed, e.point, err)
			continue
		}
		fmt.Printf("ok   %s (seed %d point %d)\n", e.file, e.seed, e.point)
	}

	fmt.Printf("\ncorpus: %d seeds, %d failures\n", len(entries), failed)
	if failed > 0 {
		return fmt.Errorf("corpus: %d seeds still reproduce", failed)
	}
	return nil
}

func repro(args []string) error {
	fs := flag.NewFlagSet("repro", flag.ContinueOnError)
	var (
		seed  = fs.Int64("seed", 0, "the seed to reconstruct")
		point = fs.Int("point", 0, "the crash point, or 0 to run the whole matrix for the seed")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *seed == 0 {
		return fmt.Errorf("repro: --seed is required")
	}

	if *point > 0 {
		if err := crash.Replay(*seed, *point); err != nil {
			return fmt.Errorf("seed %d point %d reproduces: %w", *seed, *point, err)
		}
		fmt.Printf("seed %d point %d: no failure\n", *seed, *point)
		return nil
	}

	report, err := crash.Matrix(*seed, 0)
	if err != nil {
		return err
	}
	for _, f := range report.Failures {
		fmt.Printf("FAIL seed %d point %d: %v\n", *seed, f.Point, f.Err)
	}
	fmt.Printf("seed %d: %d crash points, %d failures\n", *seed, report.Points, len(report.Failures))
	if len(report.Failures) > 0 {
		return fmt.Errorf("seed %d reproduces at %d points", *seed, len(report.Failures))
	}
	return nil
}

// corpusEntry is one seed file: the two integers a failure reduces to, and the
// file that explains it.
type corpusEntry struct {
	file  string
	seed  int64
	point int
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
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].seed != entries[j].seed {
			return entries[i].seed < entries[j].seed
		}
		return entries[i].point < entries[j].point
	})
	return entries, nil
}

func parseCorpusEntry(file, body string) (corpusEntry, error) {
	entry := corpusEntry{file: file}
	var haveSeed bool

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
			entry.seed, haveSeed = n, true
		case "point":
			n, err := strconv.Atoi(value)
			if err != nil {
				return entry, fmt.Errorf("corpus: %s: point %q is not a number", file, value)
			}
			entry.point = n
		}
	}

	if !haveSeed {
		return entry, fmt.Errorf("corpus: %s has no seed in its front matter", file)
	}
	if entry.point <= 0 {
		return entry, fmt.Errorf("corpus: %s has no crash point in its front matter", file)
	}
	return entry, nil
}
