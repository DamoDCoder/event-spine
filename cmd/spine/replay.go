package main

import (
	"flag"
	"fmt"

	"github.com/DamoDCoder/event-spine/internal/devtools/harness"
)

// replay is the devtool the M4 kill gate is about: diagnosing a failure from
// its seed without adding a print statement to the log.
func replay(args []string) error {
	fs := flag.NewFlagSet("replay", flag.ContinueOnError)
	var (
		seed   = fs.Int64("seed", 0, "the seed to replay")
		steps  = fs.Int("steps", 0, "workload steps, or 0 for the default")
		faults = fs.String("faults", "", "faults to inject, as kind@position[:arg], space separated")
		mode   = fs.String("durability", "", "log durability mode: batch, sync, or os")
		shape  = fs.String("shape", "", "run shape, as seg=N index=N payload=N batch=N syncrecords=N")
		at     = fs.Int("at", -1, "scrub to this step and describe it in full")
		ops    = fs.Bool("ops", false, "print the filesystem operations, with the faults that fired")
		diff   = fs.Bool("diff", false, "compare against the same seed with no faults, and report where they part")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *seed == 0 {
		return fmt.Errorf("replay: --seed is required")
	}

	parsed, err := harness.ParseFaults(*faults)
	if err != nil {
		return err
	}
	parsedShape, err := harness.ParseShape(*shape)
	if err != nil {
		return err
	}
	cfg := harness.Config{
		Seed:       *seed,
		Steps:      *steps,
		Faults:     parsed,
		Durability: *mode,
		Shape:      parsedShape,
	}

	if *diff {
		return replayDiff(cfg)
	}

	trace := harness.Replay(cfg)
	printHeader(cfg, trace)

	switch {
	case *at >= 0:
		return printStep(trace, *at)
	case *ops:
		printOps(trace)
	default:
		printScrub(trace)
	}

	printOutcome(trace)
	return nil
}

func printHeader(cfg harness.Config, trace harness.Trace) {
	fmt.Printf("seed %d  steps %d  faults %s\n", cfg.Seed, cfg.Steps, harness.FormatFaults(cfg.Faults))
	fmt.Printf("signature %016x over %d operations\n\n", trace.Signature, len(trace.Ops))
}

// printScrub is the whole run, one line per step. This is the view that answers
// "when did it go wrong" before anyone asks "why".
func printScrub(trace harness.Trace) {
	for _, s := range trace.States {
		fmt.Println(harness.FormatState(s))
	}
}

// printOps is the disk's side of the same run.
func printOps(trace harness.Trace) {
	for _, op := range trace.Ops {
		line := fmt.Sprintf("op %3d  step %2d  %-8s %s", op.Index, op.Step, op.Kind, op.Name)
		if op.Fault != nil {
			line += fmt.Sprintf("   <- %s fired here", op.Fault.Kind)
		}
		fmt.Println(line)
	}
}

// printStep describes one step in full: the state after it, and the operations
// the step performed.
func printStep(trace harness.Trace, at int) error {
	var found *harness.State
	for i := range trace.States {
		if trace.States[i].Step == at {
			found = &trace.States[i]
			break
		}
	}
	if found == nil {
		return fmt.Errorf("replay: the run has no step %d; it reached %d", at, len(trace.States))
	}

	fmt.Println(harness.FormatState(*found))
	fmt.Println()
	fmt.Printf("operations during step %d:\n", at)
	for _, op := range trace.Ops {
		if op.Step != at {
			continue
		}
		line := fmt.Sprintf("  op %3d  %-8s %s", op.Index, op.Kind, op.Name)
		if op.Fault != nil {
			line += fmt.Sprintf("   <- %s fired here", op.Fault.Kind)
		}
		fmt.Println(line)
	}

	printOutcome(trace)
	return nil
}

// replayDiff is the counterfactual: this seed with its faults, against the same
// seed without them.
func replayDiff(cfg harness.Config) error {
	faulted, clean, div := harness.Diff(cfg, harness.Without(cfg))

	fmt.Printf("seed %d  faults %s\n", cfg.Seed, harness.FormatFaults(cfg.Faults))
	fmt.Printf("  faulted: signature %016x, %d steps\n", faulted.Signature, len(faulted.States))
	fmt.Printf("  clean:   signature %016x, %d steps\n\n", clean.Signature, len(clean.States))

	switch {
	case div.Step < 0:
		fmt.Println("the two runs agree at every step: the faults changed nothing observable")
	case div.Truncated:
		fmt.Printf("the runs agree through step %d, and then one of them stops\n\n", div.Step-1)
		fmt.Printf("  faulted: %s\n", describeOrAbsent(faulted, div.Step))
		fmt.Printf("  clean:   %s\n", describeOrAbsent(clean, div.Step))
	default:
		fmt.Printf("first divergence at step %d\n\n", div.Step)
		fmt.Printf("  faulted: %s\n", harness.FormatState(div.A))
		fmt.Printf("  clean:   %s\n", harness.FormatState(div.B))
		fmt.Println()
		printDivergingOps(faulted, div.Step)
	}

	fmt.Println()
	printOutcome(faulted)
	return nil
}

func describeOrAbsent(trace harness.Trace, step int) string {
	for _, s := range trace.States {
		if s.Step == step {
			return harness.FormatState(s)
		}
	}
	return fmt.Sprintf("step %d never completed", step)
}

// printDivergingOps shows what the disk was asked to do during the step where
// the runs parted, which is where the explanation usually is.
func printDivergingOps(trace harness.Trace, step int) {
	fmt.Printf("what the faulted run did during step %d:\n", step)
	for _, op := range trace.Ops {
		if op.Step != step {
			continue
		}
		line := fmt.Sprintf("  op %3d  %-8s %s", op.Index, op.Kind, op.Name)
		if op.Fault != nil {
			line += fmt.Sprintf("   <- %s fired here", op.Fault.Kind)
		}
		fmt.Println(line)
	}
}

func printOutcome(trace harness.Trace) {
	fmt.Println()
	if trace.Stopped != nil {
		fmt.Printf("the run stopped: %v\n", trace.Stopped)
	}
	if trace.Failure != nil {
		fmt.Printf("INVARIANT BROKEN: %v\n", trace.Failure)
		return
	}
	fmt.Println("every invariant held")
}
