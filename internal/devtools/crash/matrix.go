package crash

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/DamoDCoder/event-spine/internal/core"
	"github.com/DamoDCoder/event-spine/internal/log"
	"github.com/DamoDCoder/event-spine/internal/sim"
)

// Steps is how many workload actions one run performs. It is fixed rather than
// seeded: the seed decides what happens, and a run whose length also varied
// would make two seeds incomparable.
const Steps = 40

// groupNames are the consumer groups the workload commits for. Two, because one
// group cannot catch a commit written under the wrong name.
var groupNames = []string{"ledger", "audit"}

// Report is the outcome of a matrix run.
type Report struct {
	Seed int64

	// Ops is how many filesystem operations the clean run performed, which
	// is how many crash points there are.
	Ops int

	// Points is how many of them were exercised.
	Points int

	// Compactions, Dropped, and Snapshots are what the clean run actually
	// did. A matrix that stopped compacting would pass every invariant it
	// checks, so the coverage is reported rather than assumed.
	Compactions int
	Dropped     int
	Snapshots   int

	// Failures are the crash points whose recovery broke an invariant.
	Failures []Failure
}

// Failure is one crash point that did not recover correctly.
type Failure struct {
	Point int
	Err   error
}

// Matrix crashes the workload at every filesystem operation in turn and checks
// what recovery kept.
//
// The claim it tests is the one M2 is judged on: a crash at any injected point
// loses nothing that was acknowledged as durable.
func Matrix(seed int64, maxPoints int) (Report, error) {
	// A clean run first, to count the points and to prove the workload
	// itself passes its own invariants without a crash.
	clean := NewFS(0)
	w, err := runWorkload(clean, seed)
	if err != nil {
		return Report{}, fmt.Errorf("crash: the clean run failed: %w", err)
	}
	if err := check(clean, w); err != nil {
		return Report{}, fmt.Errorf("crash: the clean run broke an invariant: %w", err)
	}

	report := Report{
		Seed:        seed,
		Ops:         clean.Ops(),
		Compactions: w.compactions,
		Dropped:     w.dropped,
		Snapshots:   w.snapshots,
	}
	points := clean.Ops()
	if maxPoints > 0 && maxPoints < points {
		points = maxPoints
	}

	for at := 1; at <= points; at++ {
		fs := NewFS(at)
		w, err := runWorkload(fs, seed)
		if err != nil && !errors.Is(err, ErrCrashed) {
			report.Failures = append(report.Failures, Failure{Point: at, Err: err})
			continue
		}
		report.Points++
		if err := check(fs, w); err != nil {
			report.Failures = append(report.Failures, Failure{Point: at, Err: err})
		}
	}

	if points < clean.Ops() {
		return report, fmt.Errorf("crash: only %d of %d points were exercised", points, clean.Ops())
	}
	return report, nil
}

// Replay runs one crash point, which is what a corpus seed reduces to.
func Replay(seed int64, at int) error {
	fs := NewFS(at)
	w, err := runWorkload(fs, seed)
	if err != nil && !errors.Is(err, ErrCrashed) {
		return err
	}
	return check(fs, w)
}

// witness is what the workload was told was durable, which is the only thing
// recovery is required to keep.
type witness struct {
	// events is every event appended, in offset order. The first
	// durableCount of them were acknowledged by a Sync that returned.
	events       []core.Event
	durableCount int

	// commits is the last committed offset per group whose Commit
	// returned. A commit that returned is durable by construction: the
	// commits log syncs before it does.
	commits map[string]log.Offset

	// largestWrite is the biggest single write the run issued, which bounds
	// how much a torn tail may discard.
	largestWrite int64

	// compacted records that a compaction returned, after which a durable
	// record may legitimately be gone: compaction drops superseded records
	// and keeps their offsets as gaps.
	compacted bool

	// snapshot is the last snapshot whose write returned, and is therefore
	// durable. A later one exists only if its rename also completed.
	snapshot *log.Snapshot

	// Coverage counters. A matrix that quietly stopped compacting would
	// still pass every invariant it checks, which is the comfortable kind
	// of broken: these are what let a test assert the run did the work.
	compactions int
	dropped     int
	snapshots   int
}

// runWorkload appends, syncs, commits, and reads until the machine stops.
//
// The actions come from the seed, so a point is reproducible from two integers.
// Every action that returns an error stops the run: after a crash there is
// nothing left to drive.
func runWorkload(fs *FS, seed int64) (*witness, error) {
	src := sim.NewSource(seed)
	clock := sim.NewClock()

	// Segments small enough that a forty step run rolls several times, and
	// a batch sync interval that leaves unsynced records lying around for a
	// crash to take.
	cfg := log.Config{
		Segment:     log.Options{MaxBytes: 4 << 10, IndexInterval: 256},
		Durability:  log.Batch,
		SyncRecords: 16,
		Clock:       clock,
	}

	// The witness exists before the log does, because the crash point may
	// fall inside Open itself and the checks still have to run against
	// something.
	w := &witness{commits: map[string]log.Offset{}}

	l, _, err := log.Open(fs, cfg)
	if err != nil {
		return w, err
	}
	defer l.Close()

	for step := range Steps {
		clock.Advance(core.Duration(1 + src.Intn(100)))

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
			if err != nil {
				return w, err
			}

		case 6, 7:
			if err := l.Sync(); err != nil {
				return w, err
			}
			// Everything appended so far is now on the disk, and the
			// caller has been told so.
			w.durableCount = len(w.events)

		case 8:
			name := groupNames[src.Intn(len(groupNames))]
			g, err := l.Group(name)
			if err != nil {
				return w, err
			}
			// Only offsets whose records are durable. Committing
			// ahead of what was durably processed is its own fault
			// to inject, and injecting it here would make every
			// crash point fail for a reason the log did not cause.
			if w.durableCount == 0 {
				continue
			}
			off := log.Offset(src.Intn(w.durableCount + 1))
			if err := g.Commit(off); err != nil {
				return w, err
			}
			w.commits[name] = off

		case 9:
			switch src.Intn(3) {
			case 0:
				// Compaction: the crash point that matters is
				// between the compacted file being written and
				// the rename that swaps it in.
				cs, err := l.CompactAll()

				// Every compaction in the returned slice
				// finished and swapped its segment in, whether
				// or not a later one crashed. Crediting them
				// only when the whole call succeeds would make
				// the checks below expect records that a
				// compaction legitimately removed.
				for _, c := range cs {
					w.dropped += c.Dropped
					if c.Dropped > 0 {
						w.compacted = true
					}
				}
				if err != nil {
					return w, err
				}
				w.compactions++

			case 1:
				if w.durableCount == 0 {
					continue
				}
				off := log.Offset(src.Intn(w.durableCount + 1))
				state := fmt.Appendf(nil, "folded to %d", off)
				if err := l.Snapshot(off, state); err != nil {
					return w, err
				}
				w.snapshot = &log.Snapshot{Offset: off, State: state}
				w.snapshots++

			case 2:
				if len(w.events) == 0 {
					continue
				}
				off := log.Offset(src.Intn(len(w.events)))
				if _, err := l.Read(off); err != nil && !errors.Is(err, log.ErrNotFound) {
					// After a compaction, an offset that is
					// gone is the ordinary answer rather
					// than a fault.
					return w, err
				}
			}
		}
	}

	return w, nil
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

func encodedSize(events []core.Event) int64 {
	var size int64
	for _, e := range events {
		size += int64(log.RecordLen(e))
	}
	return size
}

// check reopens the disk the crash left behind and asserts what survived.
func check(fs *FS, w *witness) error {
	cfg := log.Config{Segment: log.Options{MaxBytes: 4 << 10, IndexInterval: 256}}

	l, rec, err := log.Open(fs.recovered(), cfg)
	if err != nil {
		return fmt.Errorf("reopen after the crash: %w", err)
	}
	defer l.Close()

	if err := assert(rec.Corrupt == nil,
		"recovery reported corruption after a crash: %v", rec.Corrupt); err != nil {
		return err
	}
	if err := assert(rec.Discarded <= w.largestWrite,
		"recovery discarded %d bytes, more than the %d byte write in flight",
		rec.Discarded, w.largestWrite); err != nil {
		return err
	}
	if err := assert(l.Next() >= log.Offset(w.durableCount),
		"the log recovered to offset %d, below the %d records acknowledged as durable",
		l.Next(), w.durableCount); err != nil {
		return err
	}

	// Every acknowledged record is present and unchanged, unless compaction
	// dropped it — which it may only do when a newer record for the same
	// key is there to supersede it.
	for i := range w.durableCount {
		want := w.events[i]

		got, err := l.Read(log.Offset(i))
		if errors.Is(err, log.ErrNotFound) && w.compacted {
			if err := assert(supersededLater(w, i),
				"record %d is gone and no later record shares its key %q", i, want.Key); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return fmt.Errorf("durable record %d was lost: %w", i, err)
		}
		if got.Event.Key != want.Key || got.Event.Time != want.Time || got.Event.Schema != want.Schema {
			return fmt.Errorf("durable record %d came back as %+v, want %+v", i, got.Event, want)
		}
		if !bytes.Equal(got.Event.Payload, want.Payload) {
			return fmt.Errorf("durable record %d has the wrong payload", i)
		}
	}

	// Whatever survived scans cleanly, with offsets that ascend and stay
	// inside the log's own bounds. Contiguity is not asserted: compaction
	// leaves gaps on purpose, and a scan that demanded the next integer
	// would be the bug the design warns about wearing a test's clothes.
	r, err := l.Reader(l.First())
	if err != nil {
		return fmt.Errorf("reader after the crash: %w", err)
	}
	previous := l.First()
	for first := true; ; first = false {
		got, err := r.Next()
		if errors.Is(err, log.ErrEndOfLog) {
			break
		}
		if err != nil {
			return fmt.Errorf("scanning after offset %d: %w", previous, err)
		}
		if !first {
			if err := assert(got.Offset > previous,
				"the scan read offset %d after %d", got.Offset, previous); err != nil {
				return err
			}
		}
		if err := assert(got.Offset >= l.First() && got.Offset < l.Next(),
			"the scan read offset %d, outside the log's [%d, %d)", got.Offset, l.First(), l.Next()); err != nil {
			return err
		}
		previous = got.Offset
	}

	// A snapshot whose write returned is on disk, and nothing older than it
	// replaced it.
	if w.snapshot != nil {
		snap, err := l.LatestSnapshot()
		if err != nil {
			return fmt.Errorf("the snapshot at %d was lost: %w", w.snapshot.Offset, err)
		}
		if err := assert(snap.Offset >= w.snapshot.Offset,
			"the newest snapshot is at %d, behind the %d that was acknowledged",
			snap.Offset, w.snapshot.Offset); err != nil {
			return err
		}
		if snap.Offset == w.snapshot.Offset {
			if err := assert(bytes.Equal(snap.State, w.snapshot.State),
				"the snapshot at %d came back with %d bytes of state, want %d",
				snap.Offset, len(snap.State), len(w.snapshot.State)); err != nil {
				return err
			}
		}
	}

	// A commit that returned is a commit that survives, and it never names
	// an offset the recovered log does not have.
	for name, want := range w.commits {
		g, err := l.Group(name)
		if err != nil {
			return fmt.Errorf("group %s after the crash: %w", name, err)
		}
		got, err := g.Committed()
		if err != nil {
			return fmt.Errorf("group %s lost its commit at %d: %w", name, want, err)
		}
		if err := assert(got == want,
			"group %s resumed at %d, want the committed %d", name, got, want); err != nil {
			return err
		}
		if err := assert(got <= l.Next(),
			"group %s is committed at %d, past the log's tail at %d", name, got, l.Next()); err != nil {
			return err
		}
	}

	// The log is usable again: the next append continues the sequence
	// rather than reusing an offset a reader may already have seen.
	next := l.Next()
	offs, err := l.Append(core.Event{Key: "after", Schema: 1, Payload: []byte("recovered")})
	if err != nil {
		return fmt.Errorf("appending after recovery: %w", err)
	}
	return assert(len(offs) == 1 && offs[0] == next,
		"the first append after recovery was assigned %v, want %d", offs, next)
}
