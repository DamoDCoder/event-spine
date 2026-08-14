// Command spine is the repository's runtime and its measuring instrument.
//
// It wires the concrete implementations of core's injected dependencies, which
// is why this package is exempt from the import rule in
// scripts/check-determinism.sh. Everything below the command line receives its
// dependencies rather than reaching for them.
package main

import (
	"fmt"
	"os"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "spine: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return fmt.Errorf("a subcommand is required")
	}

	switch args[0] {
	case "verify":
		return verify(args[1:])
	case "sim":
		return simulate(args[1:])
	case "repro":
		return repro(args[1:])
	case "replay":
		return replay(args[1:])
	case "bench":
		return benchmark(args[1:])
	case "-h", "--help", "help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown subcommand %q", args[0])
	}
}

// usage lists only what exists. The Taskfile names subcommands that arrive with
// the milestones that earn them; printing them here before they work would be a
// promise the binary cannot keep.
func usage() {
	fmt.Fprint(os.Stderr, `usage: spine <subcommand>

subcommands:
  verify determinism [--seeds N] [--commands N] [--accounts N] [--repeat N]
        Run N seeds and print one digest over all of them.

  sim crash-matrix [--seed N] [--seeds N] [--points N]
        Crash the log at every filesystem operation and check what survived.

  sim corpus [--dir seeds]
        Replay every crash point in the regression corpus.

  repro --seed N [--steps N] [--faults "kind@position"]
        Reconstruct one failure exactly.

  replay --seed N [--steps N] [--faults "..."] [--at STEP] [--ops] [--diff]
        Step a seed, scrub to a step, or diff it against the same seed
        with no faults.

  bench compare [--broker host:port] [--records N] [--sizes 64,1024]
        Run one workload through the owned log and through Kafka.

  help  Print this message.
`)
}
