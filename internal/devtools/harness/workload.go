package harness

import (
	"errors"
	"fmt"

	"github.com/DamoDCoder/event-spine/internal/core"
	"github.com/DamoDCoder/event-spine/internal/log"
	"github.com/DamoDCoder/event-spine/internal/sim"
)

// DefaultSteps is how many workload actions a run performs when a config does
// not say. The seed decides what happens; the length is fixed so two seeds are
// comparable.
const DefaultSteps = 40

// groupNames are the consumer groups the workload commits for. Two, because one
// group cannot catch a commit written under the wrong name.
var groupNames = []string{"ledger", "audit"}

// witness is what the workload was told, which is the only thing recovery is
// required to honour.
type witness struct {
	// events is every event appended, in offset order. The first
	// durableCount of them were acknowledged by a Sync that returned.
	events       []core.Event
	durableCount int

	// commits is the last committed offset per group whose Commit
	// returned. A commit that returned is durable by construction: the
	// commits log syncs before it does.
	commits map[string]log.Offset

	// attempts is every offset a group asked to commit, returned or not.
	//
	// A commit whose fsync fails has still written its record, and a
	// restart that is not a crash replays it: the page cache holds what the
	// failed sync did not flush. So the position a group recovers to is not
	// always the last commit that succeeded — but it is always one the
	// consumer asked for, and therefore one it had processed. That is the
	// invariant, and it is weaker than the obvious one on purpose.
	attempts map[string][]log.Offset

	// largestWrite is the biggest single write the run issued, which bounds
	// how much a torn tail may discard.
	largestWrite int64

	// compacted records that a compaction returned, after which a durable
	// record may legitimately be gone: compaction drops superseded records
	// and keeps their offsets as gaps.
	compacted bool

	// corrupted records that a bit flip fired. After one, a durable record
	// may be unreadable or gone — but it may never come back wrong, which
	// is the invariant a checksum exists to keep.
	corrupted bool

	// snapshot is the last snapshot whose write returned, and is therefore
	// durable. A later one exists only if its rename also completed.
	snapshot *log.Snapshot

	// tail is the highest Next() the log ever reported. Offsets are never
	// reissued, so recovery may not come back above it either.
	tail log.Offset

	// Coverage counters. A harness that quietly stopped compacting would
	// still pass every invariant it checks, which is the comfortable kind
	// of broken: these are what let a test assert the run did the work.
	compactions int
	dropped     int
	snapshots   int
	steps       int
}

// runWorkload appends, syncs, commits, compacts, and snapshots until a fault
// stops it.
//
// Every action that returns an error ends the run. After a crash there is
// nothing left to drive, and after an injected disk failure the workload is a
// process that has just been told its disk is broken — continuing would be
// testing a caller nobody would write.
func runWorkload(fs *FS, cfg Config) (*witness, error) {
	return observeWorkload(fs, cfg, nil)
}

// observeWorkload is runWorkload with a hook the replay tools use to record
// what the log looked like after every step.
func observeWorkload(fs *FS, cfg Config, observe func(*log.Log, int)) (*witness, error) {
	src := sim.NewSource(cfg.Seed)
	clock := sim.NewClock()

	// Clock faults are the workload's rather than the disk's, keyed by step.
	clockFaults := map[int][]Fault{}
	for _, f := range cfg.Faults {
		if f.Kind == ClockBack {
			clockFaults[f.At] = append(clockFaults[f.At], f)
		}
	}

	// Segments small enough that a forty step run rolls several times, and
	// a batch sync interval that leaves unsynced records lying around for a
	// crash to take.
	logCfg := log.Config{
		Segment:     log.Options{MaxBytes: 4 << 10, IndexInterval: 256},
		Durability:  log.Batch,
		SyncRecords: 16,
		Clock:       clock,
	}

	// The witness exists before the log does, because a fault may fall
	// inside Open itself and the checks still have to run against
	// something.
	w := &witness{
		commits:  map[string]log.Offset{},
		attempts: map[string][]log.Offset{},
	}

	l, _, err := log.Open(fs, logCfg)
	if err != nil {
		return w, err
	}
	defer l.Close()

	steps := cfg.Steps
	if steps <= 0 {
		steps = DefaultSteps
	}

	for step := range steps {
		w.steps = step + 1
		fs.AtStep(step)
		clock.Advance(core.Duration(1 + src.Intn(100)))

		// A clock that jumps backwards is a fault the log has to
		// survive rather than a state it may assume away: a batch sync
		// deadline computed before the jump sits in what is now the
		// future.
		for _, f := range clockFaults[step] {
			back := f.Arg
			if back <= 0 {
				back = 1000
			}
			// Never below zero. core.Time is nanoseconds since the
			// start of the run, so a negative reading is not a
			// clock going backwards — it is a clock that has left
			// its own definition, and every event built from it
			// fails validation for a reason no real clock would
			// produce. The fault being modelled is the jump, not
			// the impossible value.
			now := max(clock.Now()-core.Time(back), 0)
			clock.Set(now)
			fs.record(f)
		}

		err := w.act(l, src, step)

		// A bit flip that fired weakens what the checks may demand: a
		// record whose bytes were corrupted on the platter may be
		// unreadable or truncated away. It may never come back wrong,
		// which is the part a checksum is responsible for.
		w.corrupted = fs.hasFired(BitFlip)

		if observe != nil && err == nil {
			observe(l, step)
		}
		if err != nil {
			return w, err
		}
		if err := checkLive(l, w); err != nil {
			return w, fmt.Errorf("after step %d: %w", step, err)
		}
	}

	return w, nil
}

// act performs one workload action, chosen by the seed.
func (w *witness) act(l *log.Log, src *sim.Source, step int) error {
	switch src.Intn(10) {
	case 0, 1, 2, 3, 4, 5:
		batch := make([]core.Event, 1+src.Intn(4))
		for i := range batch {
			batch[i] = workloadEvent(src, step)
		}
		if size := encodedSize(batch); size > w.largestWrite {
			w.largestWrite = size
		}
		offs, err := l.Append(batch...)
		w.events = append(w.events, batch[:len(offs)]...)
		return err

	case 6, 7:
		if err := l.Sync(); err != nil {
			return err
		}
		// Everything appended so far is now on the disk, and the caller
		// has been told so.
		w.durableCount = len(w.events)
		return nil

	case 8:
		name := groupNames[src.Intn(len(groupNames))]
		g, err := l.Group(name)
		if err != nil {
			return err
		}
		// Only offsets whose records are durable. Committing ahead of
		// what was durably processed is a consumer's bug rather than
		// the log's, and injecting it here would make every fault fail
		// for a reason the log did not cause.
		if w.durableCount == 0 {
			return nil
		}
		off := log.Offset(src.Intn(w.durableCount + 1))

		// Recorded before the call, because a commit that fails may
		// still have written its record.
		w.attempts[name] = append(w.attempts[name], off)

		if err := g.Commit(off); err != nil {
			return err
		}
		w.commits[name] = off
		return nil

	default:
		return w.maintain(l, src)
	}
}

// maintain runs the operations a log performs on itself: compaction,
// snapshotting, and the reads a consumer makes.
func (w *witness) maintain(l *log.Log, src *sim.Source) error {
	switch src.Intn(3) {
	case 0:
		// Marked before the call, not after it. A compaction swaps its
		// segment in by rename and then syncs the directory, so a
		// failure reported by the second has already been done by the
		// first: CompactAll can return an error for work that took
		// effect. Crediting only what it returns makes the checks
		// demand records a compaction legitimately removed — which is
		// seeds/0017.md, and this is the same mistake one layer up.
		w.compacted = true

		cs, err := l.CompactAll()
		for _, c := range cs {
			w.dropped += c.Dropped
		}
		if err != nil {
			return err
		}
		w.compactions++
		return nil

	case 1:
		if w.durableCount == 0 {
			return nil
		}
		off := log.Offset(src.Intn(w.durableCount + 1))
		state := fmt.Appendf(nil, "folded to %d", off)
		if err := l.Snapshot(off, state); err != nil {
			return err
		}
		w.snapshot = &log.Snapshot{Offset: off, State: state}
		w.snapshots++
		return nil

	default:
		if len(w.events) == 0 {
			return nil
		}
		off := log.Offset(src.Intn(len(w.events)))
		_, err := l.Read(off)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, log.ErrNotFound):
			// After a compaction, an offset that is gone is the
			// ordinary answer rather than a fault.
			return nil
		case errors.Is(err, log.ErrCorrupt) && w.corrupted:
			// A flipped bit that the checksum caught is the
			// checksum working.
			return nil
		default:
			return err
		}
	}
}

// checkLive asserts the invariants that must hold at every step of a running
// log, not only after it is reopened.
//
// docs/simulation-testing.md is explicit that invariants are checked after
// every step. Checking only after recovery would miss a log that went wrong at
// step 3 and looked fine at step 40, which is most of the ways a log goes
// wrong.
func checkLive(l *log.Log, w *witness) error {
	if err := assert(l.Next() >= w.tail,
		"the log's tail went backwards, from %d to %d", w.tail, l.Next()); err != nil {
		return err
	}
	w.tail = l.Next()

	for name, committed := range w.commits {
		if err := assert(committed <= l.Next(),
			"group %s is committed at %d, past the tail at %d", name, committed, l.Next()); err != nil {
			return err
		}
	}

	// Offsets ascend and stay in bounds. Contiguity is not asserted:
	// compaction leaves gaps on purpose.
	r, err := l.Reader(l.First())
	if err != nil {
		return fmt.Errorf("reader: %w", err)
	}
	previous := l.First()
	for first := true; ; first = false {
		rec, err := r.Next()
		if errors.Is(err, log.ErrEndOfLog) {
			return nil
		}
		if errors.Is(err, log.ErrCorrupt) && w.corrupted {
			return nil
		}
		if err != nil {
			return fmt.Errorf("scanning after offset %d: %w", previous, err)
		}
		if !first {
			if err := assert(rec.Offset > previous,
				"the scan read offset %d after %d", rec.Offset, previous); err != nil {
				return err
			}
		}
		if err := assert(rec.Offset >= l.First() && rec.Offset < l.Next(),
			"the scan read offset %d, outside [%d, %d)", rec.Offset, l.First(), l.Next()); err != nil {
			return err
		}
		previous = rec.Offset
	}
}

// workloadEvent builds an event whose size varies, so records cross write
// boundaries at different places.
func workloadEvent(src *sim.Source, step int) core.Event {
	payload := make([]byte, src.Intn(200))
	for i := range payload {
		payload[i] = byte(src.Intn(256))
	}
	return core.Event{
		Key:     fmt.Sprintf("acct-%02d", src.Intn(8)),
		Time:    core.Time(step),
		Schema:  1,
		Payload: payload,
	}
}

func encodedSize(events []core.Event) int64 {
	var size int64
	for _, e := range events {
		size += int64(log.RecordLen(e))
	}
	return size
}

// supersededLater reports whether a later record shares the key of the one at
// index i.
//
// It is the necessary condition for compaction having dropped that record.
// Compaction keeps the newest record per key, so a record that is gone must
// have something newer with its key still on the log; a record that vanished
// with nothing superseding it is data loss whatever removed it.
func supersededLater(w *witness, i int) bool {
	for j := i + 1; j < len(w.events); j++ {
		if w.events[j].Key == w.events[i].Key {
			return true
		}
	}
	return false
}
