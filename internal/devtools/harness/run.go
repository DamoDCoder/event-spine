package harness

import (
	"errors"
	"fmt"

	"github.com/DamoDCoder/event-spine/internal/log"
	"github.com/DamoDCoder/event-spine/internal/sim"
)

// Run executes one configuration and returns the invariant it broke, or nil.
//
// A fault stopping the workload is not a failure: the machine is allowed to
// stop. What is checked is what the disk holds afterwards.
func Run(cfg Config) error {
	fs := NewFS(cfg.Faults)
	w, err := runWorkload(fs, cfg)
	if err != nil && !isInjected(err) && !(w.corrupted && isDamage(err)) {
		return err
	}
	return check(fs, w, cfg.Shape.withDefaults())
}

// CleanOps returns how many filesystem operations the configuration's workload
// performs with no faults injected, and the signature of that stream.
//
// A fault's position is an index into that stream, so the stream is what says
// whether a stored seed still means what it meant. Change the log's I/O pattern
// — add a directory sync, say — and every seed in the corpus silently moves to
// a different point in the run. The seeds keep passing and stop testing what
// they were recorded for, which is how a corpus rots without anyone noticing.
//
// The count catches a stream that got longer. The signature also catches one
// the same length in a different order, which is what swapping two syncs
// produces and what a count would call unchanged.
func CleanOps(cfg Config) (int, uint64, error) {
	fs := NewFS(nil)
	if _, err := runWorkload(fs, Config{
		Seed:       cfg.Seed,
		Steps:      cfg.Steps,
		Durability: cfg.Durability,
		Shape:      cfg.Shape,
	}); err != nil {
		return 0, 0, err
	}
	return fs.Ops(), fs.Signature(), nil
}

// isInjected reports whether an error is the harness's own doing.
func isInjected(err error) bool {
	return errors.Is(err, ErrCrashed) || errors.Is(err, errInjected)
}

// isDamage reports whether the log refused to proceed because the bytes on disk
// were wrong.
//
// After a bit flip that is the correct answer rather than a failure: a log that
// reads damaged bytes and carries on is the one worth failing. What must still
// hold is checked by check, which is why this only decides whether the run
// stops here.
func isDamage(err error) bool {
	return errors.Is(err, log.ErrCorrupt) || errors.Is(err, log.ErrTorn)
}

// Report is the outcome of a sweep or a matrix run.
type Report struct {
	// Runs is how many configurations were executed.
	Runs int

	// Failures are the configurations that broke an invariant, minimized.
	Failures []Failure

	// Coverage is what the runs actually did. A sweep that stopped
	// compacting would report zero failures and mean nothing by it.
	Compactions int
	Dropped     int
	Snapshots   int
	Faults      map[Kind]int

	// Modes and Shapes count how many runs used each, because an axis that
	// stopped varying is exactly the mistake that cost two bugs: a sweep
	// pinned to one shape reports the same clean result as a sweep across
	// all of them.
	Modes  map[string]int
	Shapes map[string]int
}

// Failure is one configuration that broke an invariant.
type Failure struct {
	Config Config
	Err    error
}

// Matrix crashes one seed's workload at every filesystem operation in turn.
//
// This is the M2 crash matrix, kept because exhaustive enumeration of one fault
// is a different instrument from a random sweep over many: it proves a property
// at every point rather than sampling until bored.
func Matrix(seed int64, maxPoints int) (Report, error) {
	clean := NewFS(nil)
	w, err := runWorkload(clean, Config{Seed: seed})
	if err != nil {
		return Report{}, fmt.Errorf("harness: the clean run failed: %w", err)
	}
	if err := check(clean, w, Shape{}.withDefaults()); err != nil {
		return Report{}, fmt.Errorf("harness: the clean run broke an invariant: %w", err)
	}

	report := Report{
		Compactions: w.compactions,
		Dropped:     w.dropped,
		Snapshots:   w.snapshots,
		Faults:      map[Kind]int{},
	}

	points := clean.Ops()
	if maxPoints > 0 && maxPoints < points {
		points = maxPoints
	}

	for at := 1; at <= points; at++ {
		cfg := Config{Seed: seed, Faults: []Fault{{Kind: Crash, At: at}}}
		report.Runs++
		report.Faults[Crash]++
		if err := Run(cfg); err != nil {
			report.Failures = append(report.Failures, Failure{Config: cfg, Err: err})
		}
	}

	if points < clean.Ops() {
		return report, fmt.Errorf("harness: only %d of %d points were exercised", points, clean.Ops())
	}
	return report, nil
}

// Sweep runs count seeds with swarm-randomized fault configurations, minimizing
// anything that fails.
//
// Swarm testing is the point: each run randomizes which faults are in play
// rather than only when they fire. Uniform fault probabilities explore a
// narrower space than they appear to, because every run then looks like the
// average run — docs/simulation-testing.md says so, and this is what it means
// in code.
func Sweep(from int64, count int, minimize bool) Report {
	report := Report{
		Faults: map[Kind]int{},
		Modes:  map[string]int{},
		Shapes: map[string]int{},
	}

	for i := range count {
		seed := from + int64(i)
		cfg := swarm(seed)

		report.Runs++
		for _, f := range cfg.Faults {
			report.Faults[f.Kind]++
		}
		mode := cfg.Durability
		if mode == "" {
			mode = "batch"
		}
		report.Modes[mode]++
		report.Shapes[cfg.Shape.String()]++

		// The coverage counters come from a clean run of the same seed,
		// since a faulted run stops early and would undercount what the
		// workload is capable of doing.
		if i%16 == 0 {
			clean := NewFS(nil)
			if w, err := runWorkload(clean, Config{
				Seed:       seed,
				Steps:      cfg.Steps,
				Durability: cfg.Durability,
				Shape:      cfg.Shape,
			}); err == nil {
				report.Compactions += w.compactions
				report.Dropped += w.dropped
				report.Snapshots += w.snapshots
			}
		}

		err := Run(cfg)
		if err == nil {
			continue
		}
		if minimize {
			cfg, err = Minimize(cfg, err)
		}
		report.Failures = append(report.Failures, Failure{Config: cfg, Err: err})
	}

	return report
}

// swarm builds one run's fault configuration from its seed.
//
// The seed picks which kinds are enabled and how many of each, so one run has
// no disk faults and three clock jumps while the next is nothing but bit flips.
func swarm(seed int64) Config {
	src := sim.NewSource(seed)
	cfg := Config{Seed: seed, Steps: DefaultSteps}

	// How many operations a run of this length performs, so faults land
	// where there is something to break. Measured rather than guessed: a
	// fault scheduled past the end of the run is a fault that does nothing.
	// The durability mode is part of the run, drawn from the seed like
	// everything else. Batch stays the most common because it is the
	// default a caller gets, but a sweep that only ever ran it missed a
	// hole in os mode for two milestones.
	switch src.Intn(4) {
	case 0:
		cfg.Durability = "os"
	case 1:
		cfg.Durability = "sync"
	}

	// The rest of the run's size comes from the seed too, for the same
	// reason: a constant is an axis nobody is looking along.
	cfg.Shape = shapeFor(src)

	probe := NewFS(nil)
	if _, err := runWorkload(probe, Config{
		Seed:       seed,
		Steps:      cfg.Steps,
		Durability: cfg.Durability,
		Shape:      cfg.Shape,
	}); err != nil {
		return cfg
	}
	ops := probe.Ops()
	if ops < 2 {
		return cfg
	}

	// Each kind is in or out for the whole run. A quarter of runs get each
	// kind, so most runs carry one or two and a few carry none — the ones
	// carrying none are the control group.
	for _, kind := range Kinds {
		if src.Intn(4) != 0 {
			continue
		}

		// A crash ends the run, so more than one is the same run twice.
		n := 1
		if kind != Crash {
			n = 1 + src.Intn(3)
		}
		for range n {
			fault := Fault{Kind: kind, At: 1 + src.Intn(ops)}
			switch kind {
			case BitFlip:
				fault.Arg = int64(src.Intn(1 << 20))
			case ShortWrite:
				fault.Arg = int64(1 + src.Intn(64))
			case ClockBack:
				fault.At = src.Intn(cfg.Steps)
				fault.Arg = int64(1 + src.Intn(10_000))
			}
			cfg.Faults = append(cfg.Faults, fault)
		}
	}
	return cfg
}

// Minimize shrinks a failing configuration to the smallest one that still
// fails.
//
// It removes faults one at a time and keeps every removal that still
// reproduces, then shortens the run the same way. A failing seed at step
// 400,000 is a bad bug report, and docs/simulation-testing.md calls this the
// difference between the harness being used and being avoided.
//
// The failure is matched by its message rather than by identity. Two different
// bugs can both fail a run, and a minimizer that accepted any failure at all
// would happily hand back a shorter run that fails for an unrelated reason.
func Minimize(cfg Config, failure error) (Config, error) {
	want := failure.Error()

	// The mode is never minimized away: it is what the run is, not a fault
	// injected into it.
	reproduces := func(candidate Config) bool {
		err := Run(candidate)
		return err != nil && err.Error() == want
	}

	// Faults first: they are what a reader of the seed file has to
	// understand, and each one removed is one less thing to explain.
	changed := true
	for changed {
		changed = false
		for i := range cfg.Faults {
			shorter := Config{Seed: cfg.Seed, Steps: cfg.Steps, Durability: cfg.Durability, Shape: cfg.Shape}
			shorter.Faults = append(shorter.Faults, cfg.Faults[:i]...)
			shorter.Faults = append(shorter.Faults, cfg.Faults[i+1:]...)

			if reproduces(shorter) {
				cfg = shorter
				changed = true
				break
			}
		}
	}

	// Then the run's length, by halving until it stops reproducing and
	// walking back up. A binary search rather than one step at a time,
	// because a 400,000 step run is the case this exists for.
	steps := cfg.Steps
	if steps <= 0 {
		steps = DefaultSteps
	}
	for span := steps / 2; span >= 1; span /= 2 {
		shorter := Config{
			Seed:       cfg.Seed,
			Steps:      steps - span,
			Faults:     cfg.Faults,
			Durability: cfg.Durability,
			Shape:      cfg.Shape,
		}
		if shorter.Steps > 0 && reproduces(shorter) {
			steps = shorter.Steps
		}
	}
	cfg.Steps = steps

	return cfg, Run(cfg)
}
