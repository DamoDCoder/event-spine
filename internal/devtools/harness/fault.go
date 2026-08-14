// Package harness is the deterministic simulation harness described in
// docs/simulation-testing.md: a seeded workload, a catalogue of faults injected
// into it, invariants checked after every step, and automatic minimization of
// whatever fails.
//
// It lives under internal/devtools rather than internal/sim because the log's
// tests import the simulator, and a simulator that imported the log back would
// be an import cycle. The dependency runs one way: this package knows about
// both.
package harness

import (
	"fmt"
	"strconv"
	"strings"
)

// Kind is a fault the harness can inject.
//
// The catalogue is the disk and clock half of the one in
// docs/simulation-testing.md. Network faults are absent because the spine has
// no network yet, and a fault catalogue listing faults nothing can experience
// would be a list that looks more thorough than the harness is.
type Kind int

const (
	// Crash stops the machine. Everything not synced is lost, and files
	// whose directory entry was never synced disappear entirely.
	Crash Kind = iota

	// WriteError fails one append, as a disk returning an error would. The
	// log has to treat the record as unwritten and refuse to write another
	// after it.
	WriteError

	// ShortWrite stores part of the buffer and reports the shortfall. This
	// is a torn write without a crash: the tail is on disk and the caller
	// knows the write failed.
	ShortWrite

	// SyncError fails one fsync. Nothing after it may be reported durable.
	SyncError

	// BitFlip flips one bit of one byte already on disk, which is the fault
	// the CRC exists for. It is the one fault here that no amount of
	// correct fsync ordering protects against.
	BitFlip

	// ClockBack moves the injected clock backwards, which a batch-mode sync
	// deadline must survive: a deadline computed from a clock that went
	// backwards can sit unreachably far in the future.
	ClockBack

	// CrashZeros stops the machine leaving files longer than their contents,
	// with zeros in the gap. ext4 journals metadata separately from data, so
	// this is what it does; the simulator could not produce it for four
	// milestones and a bug hid in the difference. See power-loss.md.
	CrashZeros

	// CrashTorn stops the machine partway through writeback, so a prefix of
	// the unsynced bytes survives. Arg is that prefix as a percentage, and
	// whatever lands on the boundary is a record cut in half.
	CrashTorn
)

var kindNames = map[Kind]string{
	Crash:      "crash",
	WriteError: "writeerror",
	ShortWrite: "shortwrite",
	SyncError:  "syncerror",
	BitFlip:    "bitflip",
	ClockBack:  "clockback",
	CrashZeros: "crashzeros",
	CrashTorn:  "crashtorn",
}

// Kinds is every fault, in the order a swarm configuration considers them.
var Kinds = []Kind{Crash, WriteError, ShortWrite, SyncError, BitFlip, ClockBack, CrashZeros, CrashTorn}

// Crashes are the fault kinds that stop the machine. They differ only in what
// the disk is left holding, which is the thing a simulator is most likely to be
// optimistic about.
var Crashes = []Kind{Crash, CrashZeros, CrashTorn}

func (k Kind) String() string {
	if name, ok := kindNames[k]; ok {
		return name
	}
	return fmt.Sprintf("Kind(%d)", int(k))
}

func parseKind(s string) (Kind, bool) {
	for kind, name := range kindNames {
		if name == s {
			return kind, true
		}
	}
	return 0, false
}

// Fault is one injection: what, and where.
//
// At is a filesystem operation index for the disk faults and a workload step
// for ClockBack, because a clock fault is not something the disk does. The two
// coordinate systems are the price of keeping a fault addressable by a single
// integer, which is what makes a failing run a value a corpus file can hold.
//
// Arg is kind-specific: the byte to flip for BitFlip, the bytes to accept for
// ShortWrite, the logical nanoseconds to move backwards for ClockBack, and
// unused for the rest.
type Fault struct {
	Kind Kind
	At   int
	Arg  int64
}

func (f Fault) String() string {
	if f.Arg != 0 {
		return fmt.Sprintf("%s@%d:%d", f.Kind, f.At, f.Arg)
	}
	return fmt.Sprintf("%s@%d", f.Kind, f.At)
}

// Config is a complete description of one run: these fields reproduce it
// exactly, on any machine, which is what a corpus entry stores.
type Config struct {
	Seed   int64
	Steps  int
	Faults []Fault

	// Durability is the log mode the workload runs in. Empty means batch,
	// which is what every seed recorded before this field existed — so
	// omitting it keeps those seeds meaning what they meant.
	//
	// It exists because the workload ran only in batch mode for two
	// milestones, and that is why simulation missed the hole that
	// scripts/powercut.sh found on real ext4: in batch mode a roll syncs
	// the outgoing segment, so the gap it opened in os mode could not
	// appear here.
	Durability string

	// Shape is how big the run's records, batches, and segments are. A zero
	// shape is the one every seed used before it was configurable.
	Shape Shape
}

// FormatFaults renders a fault list as the corpus stores it.
func FormatFaults(faults []Fault) string {
	if len(faults) == 0 {
		return "none"
	}
	out := make([]string, len(faults))
	for i, f := range faults {
		out[i] = f.String()
	}
	return strings.Join(out, " ")
}

// ParseFaults reads a fault list back.
//
// The format is `kind@at` or `kind@at:arg`, space separated, because a corpus
// entry is read by people as often as by this parser and a JSON blob in front
// matter is neither.
func ParseFaults(s string) ([]Fault, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "none" {
		return nil, nil
	}

	var faults []Fault
	for field := range strings.FieldsSeq(s) {
		name, rest, ok := strings.Cut(field, "@")
		if !ok {
			return nil, fmt.Errorf("harness: %q is not kind@at", field)
		}
		kind, ok := parseKind(name)
		if !ok {
			return nil, fmt.Errorf("harness: %q is not a known fault", name)
		}

		at, arg := rest, ""
		if a, b, ok := strings.Cut(rest, ":"); ok {
			at, arg = a, b
		}

		fault := Fault{Kind: kind}
		n, err := strconv.Atoi(at)
		if err != nil {
			return nil, fmt.Errorf("harness: %q has a bad position: %w", field, err)
		}
		fault.At = n

		if arg != "" {
			v, err := strconv.ParseInt(arg, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("harness: %q has a bad argument: %w", field, err)
			}
			fault.Arg = v
		}
		faults = append(faults, fault)
	}
	return faults, nil
}
