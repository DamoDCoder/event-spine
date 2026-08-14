package main

import (
	"crypto/sha256"
	"encoding/binary"
	"flag"
	"fmt"
	"os"

	"github.com/DamoDCoder/event-spine/core"
	"github.com/DamoDCoder/event-spine/sim"
)

// verifyDomain separates the aggregate digest from every other SHA-256 here.
var verifyDomain = []byte("event-spine/verify/determinism/v1\x00")

func verify(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("verify: a claim is required, try: verify determinism")
	}
	switch args[0] {
	case "determinism":
		return verifyDeterminism(args[1:])
	default:
		return fmt.Errorf("verify: unknown claim %q", args[0])
	}
}

func verifyDeterminism(args []string) error {
	fs := flag.NewFlagSet("verify determinism", flag.ContinueOnError)
	var (
		seeds    = fs.Int("seeds", 1000, "number of seeds to run")
		commands = fs.Int("commands", 500, "commands per run")
		accounts = fs.Int("accounts", 16, "accounts in the ledger")
		repeat   = fs.Int("repeat", 2, "times to run each seed, comparing the results")
		verbose  = fs.Bool("v", false, "print every seed's chain digest")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *seeds < 1 {
		return fmt.Errorf("seeds must be at least 1, got %d", *seeds)
	}
	if *repeat < 1 {
		return fmt.Errorf("repeat must be at least 1, got %d", *repeat)
	}

	// The aggregate is a hash over every seed's chain, folded in seed
	// order. One value to compare between a host run and a container run,
	// and no way for a divergence in one seed to cancel out against
	// another.
	agg := sha256.New()
	agg.Write(verifyDomain)
	agg.Write(binary.LittleEndian.AppendUint64(nil, uint64(*commands)))
	agg.Write(binary.LittleEndian.AppendUint64(nil, uint64(*accounts)))

	var (
		steps    int64
		rejected int64
		absorbed int
	)

	for seed := 1; seed <= *seeds; seed++ {
		w := sim.Workload{Seed: int64(seed), Commands: *commands, Accounts: *accounts}

		first, err := sim.Run(w)
		if err != nil {
			return fmt.Errorf("seed %d: %w", seed, err)
		}

		// Repeating in-process is not redundant with repeating the
		// binary. Go randomizes map iteration per range statement, so a
		// projection that ranges a map can disagree with itself inside
		// one process — which is the cheapest possible place to catch
		// it.
		for i := 1; i < *repeat; i++ {
			again, err := sim.Run(w)
			if err != nil {
				return fmt.Errorf("seed %d, repeat %d: %w", seed, i, err)
			}
			if again.Chain != first.Chain {
				return fmt.Errorf(
					"seed %d is not deterministic within one process:\n  run 1 chain %s\n  run %d chain %s",
					seed, first.Chain, i+1, again.Chain)
			}
		}

		// The M0 lesson, enforced mechanically: a run whose projection
		// stopped responding is not evidence of anything, so it fails
		// the gate rather than contributing a hash that would agree
		// with any other absorbed run.
		if first.Absorbed {
			absorbed++
			return fmt.Errorf(
				"seed %d reached an absorbing state: the projection was unchanged for the last %d of %d steps, so its digest is not evidence of determinism",
				seed, first.StepsSinceChange, first.Steps)
		}

		agg.Write(binary.LittleEndian.AppendUint64(nil, uint64(seed)))
		agg.Write(first.Chain[:])

		steps += first.Steps
		rejected += first.Rejected

		if *verbose {
			fmt.Fprintf(os.Stdout, "seed %-6d chain %s steps %d rejected %d\n",
				seed, first.Chain, first.Steps, first.Rejected)
		}
	}

	var digest core.Digest
	copy(digest[:], agg.Sum(nil))

	fmt.Fprintf(os.Stdout, "seeds     %d\n", *seeds)
	fmt.Fprintf(os.Stdout, "repeat    %d\n", *repeat)
	fmt.Fprintf(os.Stdout, "steps     %d\n", steps)
	fmt.Fprintf(os.Stdout, "rejected  %d\n", rejected)
	fmt.Fprintf(os.Stdout, "absorbed  %d\n", absorbed)
	fmt.Fprintf(os.Stdout, "digest    %s\n", digest)
	return nil
}
