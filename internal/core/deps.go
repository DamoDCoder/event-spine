// Package core holds the deterministic command, event, and projection cycle.
//
// Nothing here performs I/O and nothing here reads the wall clock. Time,
// randomness, identifiers, and the choice between two simultaneously ready
// events all arrive as injected interfaces, so a run is a pure function of its
// seed and its inputs.
//
// The interfaces live in this package rather than a separate one because
// `internal/log` and `internal/sim` both import core already, and a package
// that holds nothing but interface declarations would add an import edge
// without adding an idea.
package core

// Time is logical time, in nanoseconds since the start of a run.
//
// It is not `time.Time` and does not convert to one. A wall-clock timestamp
// that reaches a durable record makes replay non-deterministic, which is the
// one failure this repository exists to prevent, so the type system refuses the
// conversion rather than trusting a comment.
type Time int64

// Duration is a logical span in nanoseconds.
type Duration int64

// Clock is the only source of time.
//
// It supplies timers as well as the current instant. The M0 spike found a live
// run whose reproducibility depended on when a timer fired relative to a
// command, and an injected `Now()` alone would not have caught it — see
// docs/decisions/m0-determinism-spike.md.
type Clock interface {
	Now() Time

	// Timer returns a timer due d from now. Timers do not deliver on a
	// channel and do not run on their own goroutine: the scheduler polls
	// them, so a timer becoming ready is an event in the simulation rather
	// than a race against one.
	Timer(d Duration) Timer
}

// Timer is a deadline the scheduler polls.
type Timer interface {
	// Deadline is the logical instant at which the timer becomes ready.
	Deadline() Time

	// Ready reports whether the timer is due as of now, and false once it
	// has been cancelled.
	Ready(now Time) bool

	Cancel()
}

// Source is seeded randomness. The seed is the reproduction key for an entire
// run, so every draw in a production path comes from here.
type Source interface {
	// Uint64 returns the next value in the sequence.
	Uint64() uint64

	// Intn returns a value in [0, n). It panics for n <= 0, because a
	// caller asking for a choice among no alternatives has a bug that
	// silently returning zero would hide.
	Intn(n int) int
}

// IDGen produces identifiers.
//
// Implementations return a deterministic sequence per run. A UUIDv4 is a
// nondeterministic identifier wearing a respectable name and does not belong
// behind this interface.
type IDGen interface {
	NextID(kind string) string
}

// Scheduler decides which of several simultaneously ready alternatives runs
// next.
//
// This interface exists because of the M0 spike. A `select` over a ready timer
// and a ready command picks uniformly at random, and that choice decided which
// tick a control change landed on: 40 identical scenarios produced 7 distinct
// projection hashes. The choice is neither time, nor randomness, nor I/O, so
// none of the other interfaces here covers it. Making it explicit is what turns
// "reproducible when the timing margin is generous" into "reproducible".
type Scheduler interface {
	// Choose returns the index in [0, ready) of the alternative to run.
	// Callers must present alternatives in a stable order — sorted, or
	// fixed by construction — because Choose can only be deterministic if
	// its input is.
	Choose(ready int) int
}

// Deps is the injected dependency set a run is built from.
//
// `FS` and `Transport` are named in docs/architecture.md and are deliberately
// absent here: neither has a consumer until the owned log arrives at M2, and
// this repository does not build a feature ahead of its first consumer. They
// join this struct in the milestone that needs them.
type Deps struct {
	Clock Clock
	Rand  Source
	IDs   IDGen
	Sched Scheduler
}

// Validate reports whether every dependency is present.
//
// A nil dependency is a wiring mistake that would otherwise surface as a nil
// pointer panic deep inside a fold, at which point the seed that produced it is
// the only clue left.
func (d Deps) Validate() error {
	switch {
	case d.Clock == nil:
		return errMissingDep("Clock")
	case d.Rand == nil:
		return errMissingDep("Rand")
	case d.IDs == nil:
		return errMissingDep("IDs")
	case d.Sched == nil:
		return errMissingDep("Sched")
	}
	return nil
}
