package harness

import (
	"errors"
	"fmt"

	"github.com/DamoDCoder/event-spine/log"
	"github.com/DamoDCoder/event-spine/sim"
)

// restart is the second workload: a consumer that keeps stopping.
//
// The mixed workload opens the log once, reads a record at a time, and never
// puts it away. That leaves two things no seed ever reached — recovery followed
// by resuming from a committed offset, and a cursor that outlives the segments
// underneath it — and the second is where docs/log-design.md says the bugs are.
//
// It returns the log to run the next step against, which is a different one
// after a restart.
func (w *witness) restart(l *log.Log, src *sim.Source, step int, shape Shape) (*log.Log, error) {
	switch src.Intn(10) {
	case 0, 1, 2, 3:
		return l, w.act(l, src, step, shape)

	case 4, 5:
		return l, w.consume(l, src)

	case 6:
		return w.reopen(l, shape)

	default:
		return l, w.maintain(l, src)
	}
}

// consume reads through the long-lived cursor.
//
// The cursor is the point: it is created once and kept across compactions and
// restarts, so a record it has already delivered must never be delivered again
// and the offsets it returns must never go backwards. A reader that reset
// itself quietly would be a consumer replaying records it had already
// processed, which is the failure at-least-once delivery is supposed to bound.
func (w *witness) consume(l *log.Log, src *sim.Source) error {
	if w.reader == nil {
		r, err := l.Reader(l.First())
		if err != nil {
			return err
		}
		w.reader = r

		// Starting over from the beginning is a new consumer, not the
		// old one going backwards. It happens when corruption truncated
		// the log below where the cursor had reached, and asserting
		// monotonicity across that discontinuity would be asserting
		// something nobody promised.
		w.delivered = false
	}

	for range 1 + src.Intn(8) {
		rec, err := w.reader.Next()
		if errors.Is(err, log.ErrEndOfLog) {
			return nil
		}
		if err != nil {
			return err
		}

		if w.delivered && rec.Offset <= w.lastDelivered {
			return fmt.Errorf("the reader delivered offset %d after %d", rec.Offset, w.lastDelivered)
		}
		w.lastDelivered, w.delivered = rec.Offset, true
	}
	return nil
}

// reopen closes the log and opens it again, which is a restart rather than a
// crash: nothing is lost, and everything durable has to still be there.
func (w *witness) reopen(l *log.Log, shape Shape) (*log.Log, error) {
	before := l.Next()

	// What the log said about itself, to check against what it says after.
	groups := map[string]log.Offset{}
	for _, name := range groupNames {
		g, err := l.Group(name)
		if err != nil {
			return l, err
		}
		if off, err := g.Committed(); err == nil {
			groups[name] = off
		}
	}
	snapshot := int64(-1)
	if snap, err := l.LatestSnapshot(); err == nil {
		snapshot = int64(snap.Offset)
	}

	if err := l.Close(); err != nil {
		return l, err
	}

	// The cursor's segments went with the log it came from.
	w.reader = nil

	reopened, _, err := log.Open(w.fs, log.Config{
		Segment:    log.Options{MaxBytes: shape.SegmentBytes, IndexInterval: shape.IndexInterval},
		Durability: w.durability,
	})
	if err != nil {
		return nil, fmt.Errorf("reopen: %w", err)
	}
	w.restarts++

	// A restart is not a crash: nothing was dropped, so nothing may be
	// missing. Unless a bit flip fired, in which case recovery truncating
	// from the damage is the log reporting a broken disk rather than losing
	// data — the same weakening the main checks apply, for the same reason,
	// and seeds/0109.md is what happens without it.
	if !w.corrupted() {
		if reopened.Next() < before {
			return reopened, fmt.Errorf("a clean restart lost records: tail was %d, is %d",
				before, reopened.Next())
		}
		for name, want := range groups {
			g, err := reopened.Group(name)
			if err != nil {
				return reopened, err
			}
			got, err := g.Committed()
			if err != nil {
				return reopened, fmt.Errorf("group %s lost its commit at %d across a restart: %w",
					name, want, err)
			}
			if got != want {
				return reopened, fmt.Errorf("group %s was committed at %d and came back at %d",
					name, want, got)
			}
		}
		if snapshot >= 0 {
			snap, err := reopened.LatestSnapshot()
			if err != nil {
				return reopened, fmt.Errorf("the snapshot at %d did not survive a restart: %w", snapshot, err)
			}
			if int64(snap.Offset) < snapshot {
				return reopened, fmt.Errorf("the snapshot was at %d and came back at %d",
					snapshot, snap.Offset)
			}
		}
	}

	// The cursor resumes where it left off, which is what a consumer does
	// after a restart it did not choose.
	if w.delivered {
		r, err := reopened.Reader(w.lastDelivered + 1)
		if err != nil {
			// Past the tail after a restart is possible when the
			// records the cursor had seen were never durable.
			if !errors.Is(err, log.ErrNotFound) {
				return reopened, err
			}
		} else {
			w.reader = r
		}
	}
	return reopened, nil
}
