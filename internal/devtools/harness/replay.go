package harness

import (
	"errors"
	"fmt"

	"github.com/DamoDCoder/event-spine/internal/log"
)

// State is what the log looked like after one workload step.
//
// It is deliberately small and comparable. Two runs of the same seed that
// diverge do so at a step, and the point of recording state per step is to name
// that step without a print statement in the log.
type State struct {
	Step int

	First    log.Offset
	Tail     log.Offset
	Segments int
	Records  int

	// Groups is each consumer group's committed offset, and Snapshot the
	// offset of the newest snapshot, or -1 when there is none.
	Groups   map[string]log.Offset
	Snapshot int64

	// Digest folds every record the log holds: offset, key, time, and
	// payload. It is the projection in "projection diff" — the thing two
	// runs are compared on, rather than the bytes underneath it.
	Digest uint64

	// Err is set when the log could not be read at this step, which after a
	// bit flip is a legitimate state rather than a failed replay.
	Err error
}

// Trace is a replayed run: what the disk did, what the log looked like, and
// what broke.
type Trace struct {
	Config Config

	// Ops is every filesystem operation, in order, with any fault that
	// fired attached to it. A fault's position is an index into this.
	Ops []Op

	// States is the log after each completed step.
	States []State

	// Signature identifies the operation stream. A seed whose signature has
	// changed is a seed that no longer means what it did.
	Signature uint64

	// Stopped is the error that ended the run, if a fault ended it.
	Stopped error

	// Failure is the invariant the run broke, if any.
	Failure error
}

// Replay runs a configuration and records everything the tools need.
//
// It is the same execution `Run` performs, with the recording turned on. A
// debugger that ran a different execution from the one being debugged would be
// its own kind of bug.
func Replay(cfg Config) Trace {
	fs := NewFS(cfg.Faults)
	fs.Tracing(true)

	trace := Trace{Config: cfg}
	w, err := observeWorkload(fs, cfg, func(l *log.Log, step int) {
		trace.States = append(trace.States, snapshotState(l, step))
	})

	trace.Ops = fs.Trace()
	trace.Signature = fs.Signature()
	if err != nil {
		trace.Stopped = err
	}
	if err != nil && !isInjected(err) && !(w.corrupted && isDamage(err)) {
		trace.Failure = err
		return trace
	}
	trace.Failure = check(fs, w)
	return trace
}

// snapshotState reads the log and folds it into one comparable value.
func snapshotState(l *log.Log, step int) State {
	state := State{
		Step:     step,
		First:    l.First(),
		Tail:     l.Next(),
		Segments: len(l.Segments()),
		Groups:   map[string]log.Offset{},
		Snapshot: -1,
	}

	if snap, err := l.LatestSnapshot(); err == nil {
		state.Snapshot = int64(snap.Offset)
	}
	for _, name := range groupNames {
		g, err := l.Group(name)
		if err != nil {
			continue
		}
		if off, err := g.Committed(); err == nil {
			state.Groups[name] = off
		}
	}

	r, err := l.Reader(l.First())
	if err != nil {
		state.Err = err
		return state
	}

	const (
		offset64 = 14695981039346656037
		prime64  = 1099511628211
	)
	digest := uint64(offset64)
	fold := func(b byte) {
		digest ^= uint64(b)
		digest *= prime64
	}

	for {
		rec, err := r.Next()
		if errors.Is(err, log.ErrEndOfLog) {
			break
		}
		if err != nil {
			// A log that cannot be read is a state worth recording
			// rather than an error worth aborting on: after a bit
			// flip it is the expected one.
			state.Err = err
			break
		}
		state.Records++
		for i := range 8 {
			fold(byte(rec.Offset >> (8 * i)))
		}
		for i := range len(rec.Event.Key) {
			fold(rec.Event.Key[i])
		}
		for i := range 8 {
			fold(byte(rec.Event.Time >> (8 * i)))
		}
		for _, b := range rec.Event.Payload {
			fold(b)
		}
	}

	state.Digest = digest
	return state
}

// Divergence is where two runs of a seed stopped agreeing.
type Divergence struct {
	// Step is the first step whose state differs, or -1 when the two runs
	// agree everywhere both of them reached.
	Step int

	A, B State

	// Truncated is set when one run simply stopped earlier than the other,
	// which is what a crash looks like: not a disagreement about a step,
	// but an absence of one.
	Truncated bool
}

// Diff replays two configurations and finds the first step where they part.
//
// The intended pair is a failing run and the same seed without its faults: the
// counterfactual. That is what turns "this seed fails" into "these two runs
// agree until step 14, and at step 14 the tail is 27 in one and 31 in the
// other", which is a sentence someone can act on.
func Diff(a, b Config) (Trace, Trace, Divergence) {
	ta, tb := Replay(a), Replay(b)

	n := min(len(ta.States), len(tb.States))
	for i := range n {
		if !sameState(ta.States[i], tb.States[i]) {
			return ta, tb, Divergence{Step: ta.States[i].Step, A: ta.States[i], B: tb.States[i]}
		}
	}

	if len(ta.States) != len(tb.States) {
		div := Divergence{Step: n, Truncated: true}
		if n < len(ta.States) {
			div.A = ta.States[n]
		}
		if n < len(tb.States) {
			div.B = tb.States[n]
		}
		return ta, tb, div
	}
	return ta, tb, Divergence{Step: -1}
}

func sameState(a, b State) bool {
	if a.First != b.First || a.Tail != b.Tail || a.Records != b.Records ||
		a.Segments != b.Segments || a.Snapshot != b.Snapshot || a.Digest != b.Digest {
		return false
	}
	if len(a.Groups) != len(b.Groups) {
		return false
	}
	for name, off := range a.Groups {
		if b.Groups[name] != off {
			return false
		}
	}
	return (a.Err == nil) == (b.Err == nil)
}

// Without returns the configuration with its faults removed, which is the run a
// failing seed is compared against.
func Without(cfg Config) Config {
	return Config{Seed: cfg.Seed, Steps: cfg.Steps}
}

// FormatState renders a state as one line of a scrub.
func FormatState(s State) string {
	groups := ""
	for _, name := range groupNames {
		if off, ok := s.Groups[name]; ok {
			groups += fmt.Sprintf(" %s=%d", name, off)
		}
	}

	snapshot := "none"
	if s.Snapshot >= 0 {
		snapshot = fmt.Sprintf("%d", s.Snapshot)
	}

	line := fmt.Sprintf("step %2d  offsets [%d,%d)  records %3d  segments %d  snapshot %-4s digest %016x%s",
		s.Step, s.First, s.Tail, s.Records, s.Segments, snapshot, s.Digest, groups)
	if s.Err != nil {
		line += fmt.Sprintf("  unreadable: %v", s.Err)
	}
	return line
}
