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
	case "-h", "--help", "help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown subcommand %q", args[0])
	}
}

// usage lists only what exists. The Taskfile names repro, bench, and sim
// subcommands that arrive with the milestones that earn them; printing them
// here before they work would be a promise the binary cannot keep.
func usage() {
	fmt.Fprint(os.Stderr, `usage: spine <subcommand>

subcommands:
  verify determinism [--seeds N] [--commands N] [--accounts N] [--repeat N]
        Run N seeds and print one digest over all of them.

  help  Print this message.
`)
}
