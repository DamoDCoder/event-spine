package harness

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/DamoDCoder/event-spine/core"
	"github.com/DamoDCoder/event-spine/log"
)

// check reopens the disk the run left behind and asserts what survived.
//
// The invariants loosen exactly as far as the faults that fired require, and no
// further. Compaction licenses a missing record only when something newer for
// its key survives; a bit flip licenses a record being unreadable but never a
// record coming back wrong. A checker that loosened by default would agree with
// any log at all.
func check(fs *FS, w *witness, shape Shape) error {
	logCfg := log.Config{Segment: log.Options{
		MaxBytes:      shape.SegmentBytes,
		IndexInterval: shape.IndexInterval,
	}}

	l, rec, err := log.Open(fs.recovered(), logCfg)
	if err != nil {
		if w.corrupted() {
			// A log that refuses to open a corrupted disk is a log
			// reporting damage rather than guessing past it.
			return nil
		}
		return fmt.Errorf("reopen: %w", err)
	}
	defer l.Close()

	if !w.corrupted() {
		if err := assert(rec.Corrupt == nil,
			"recovery reported corruption where none was injected: %v", rec.Corrupt); err != nil {
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
	}

	if err := checkDurableRecords(l, w); err != nil {
		return err
	}
	if err := checkScan(l, w); err != nil {
		return err
	}
	if err := checkGroups(l, w); err != nil {
		return err
	}
	if err := checkSnapshot(l, w); err != nil {
		return err
	}

	// The log is usable again: the next append continues the sequence
	// rather than reusing an offset a reader may already have seen.
	next := l.Next()
	offs, err := l.Append(core.Event{Key: "after", Schema: 1, Payload: []byte("recovered")})
	if err != nil {
		if w.corrupted() {
			return nil
		}
		return fmt.Errorf("appending after recovery: %w", err)
	}
	return assert(len(offs) == 1 && offs[0] == next,
		"the first append after recovery was assigned %v, want %d", offs, next)
}

// checkDurableRecords asserts that everything acknowledged is present and
// unchanged, or absent for a reason a fault licensed.
func checkDurableRecords(l *log.Log, w *witness) error {
	for i := range w.durableCount {
		// Above the lowest tail the log ever regressed to, this offset
		// may hold a record a later append put there. The witness is out
		// of date rather than the log being wrong, which is the same
		// allowance checkScan makes and for the same reason.
		if log.Offset(i) >= w.trusted {
			break
		}
		want := w.events[i]

		got, err := l.Read(log.Offset(i))
		switch {
		case err == nil:
		case err != nil && w.corrupted():
			// Damaged bytes may be unreadable or truncated away.
			// Checked before compaction's rule, because a record
			// that corruption removed has no reason to satisfy it —
			// judging one by the other is how a checker reports a
			// bug in the wrong component.
			continue
		case errors.Is(err, log.ErrNotFound) && w.compacted:
			if err := assert(supersededLater(w, i),
				"record %d is gone and no later record shares its key %q", i, want.Key); err != nil {
				return err
			}
			continue
		default:
			return fmt.Errorf("durable record %d was lost: %w", i, err)
		}

		if got.Event.Key != want.Key || got.Event.Time != want.Time || got.Event.Schema != want.Schema {
			return fmt.Errorf("durable record %d came back as %+v, want %+v", i, got.Event, want)
		}
		if !bytes.Equal(got.Event.Payload, want.Payload) {
			return fmt.Errorf("durable record %d has the wrong payload", i)
		}
	}
	return nil
}

// checkScan asserts that whatever survived reads back in ascending order,
// within the log's own bounds.
func checkScan(l *log.Log, w *witness) error {
	r, err := l.Reader(l.First())
	if err != nil {
		return fmt.Errorf("reader after the run: %w", err)
	}

	previous := l.First()
	for first := true; ; first = false {
		got, err := r.Next()
		if errors.Is(err, log.ErrEndOfLog) {
			return nil
		}
		if err != nil {
			if w.corrupted() {
				return nil
			}
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

		// Recovery may keep less than was written; it may never invent.
		// Every record that survived has to be the one the workload
		// appended at that offset, byte for byte.
		//
		// This is where a run may legitimately hold records the caller
		// was never told about: a short write can store three records
		// of a four record batch, and Append reports the whole batch
		// failed. Those three are real records at real offsets, and
		// recovery keeping them is correct — so the check is that they
		// are the right records, not that they are absent.
		// Corruption does not excuse a wrong record: the checksum covers
		// the key, the time, the schema, and the payload, so a read that
		// succeeded must return what was written.
		//
		// It does excuse an offset the witness no longer knows. Once
		// recovery has truncated the log, later appends reuse the
		// offsets the lost records had, and comparing those against what
		// used to be there is the checker being out of date rather than
		// the log being wrong.
		if i := int(got.Offset); i < len(w.events) && got.Offset < w.trusted {
			want := w.events[i]
			if err := assert(got.Event.Key == want.Key && got.Event.Time == want.Time,
				"offset %d came back as %+v, want %+v", got.Offset, got.Event, want); err != nil {
				return err
			}
			if err := assert(bytes.Equal(got.Event.Payload, want.Payload),
				"offset %d came back with the wrong payload", got.Offset); err != nil {
				return err
			}
		}
		previous = got.Offset
	}
}

// checkGroups asserts that a commit which returned is a commit that survived.
func checkGroups(l *log.Log, w *witness) error {
	for name, want := range w.commits {
		g, err := l.Group(name)
		if err != nil {
			if w.corrupted() {
				continue
			}
			return fmt.Errorf("group %s after the run: %w", name, err)
		}
		got, err := g.Committed()
		if err != nil {
			if w.corrupted() {
				continue
			}
			return fmt.Errorf("group %s lost its commit at %d: %w", name, want, err)
		}
		// The position is the last commit that returned, or another the
		// consumer asked for. A commit whose fsync failed has still
		// written its record, and a restart that is not a crash replays
		// it — so the log cannot promise the failed commit did not
		// happen. What it can promise, and what is asserted here, is
		// that a group never resumes at an offset nobody asked it to.
		if err := assert(got == want || attempted(w, name, got) || w.corrupted(),
			"group %s resumed at %d, which is neither the committed %d nor any offset it asked for",
			name, got, want); err != nil {
			return err
		}
		// A commit ahead of the tail is possible once corruption has
		// truncated the events out from under it: the commits log is a
		// separate file and survives damage to a segment. The consumer
		// is then pointed past the end of a log that lost records,
		// which is a true report of a damaged disk rather than a fault
		// in the log.
		if err := assert(got <= l.Next() || w.corrupted(),
			"group %s is committed at %d, past the log's tail at %d", name, got, l.Next()); err != nil {
			return err
		}
	}
	return nil
}

// attempted reports whether the group ever asked to commit this offset.
func attempted(w *witness, group string, off log.Offset) bool {
	for _, tried := range w.attempts[group] {
		if tried == off {
			return true
		}
	}
	return false
}

// checkSnapshot asserts that a snapshot whose write returned is still there.
func checkSnapshot(l *log.Log, w *witness) error {
	if w.snapshot == nil {
		return nil
	}

	snap, err := l.LatestSnapshot()
	if err != nil {
		if w.corrupted() {
			return nil
		}
		return fmt.Errorf("the snapshot at %d was lost: %w", w.snapshot.Offset, err)
	}
	if err := assert(snap.Offset >= w.snapshot.Offset,
		"the newest snapshot is at %d, behind the %d that was acknowledged",
		snap.Offset, w.snapshot.Offset); err != nil {
		return err
	}
	if snap.Offset == w.snapshot.Offset && !w.corrupted() {
		return assert(bytes.Equal(snap.State, w.snapshot.State),
			"the snapshot at %d came back with %d bytes of state, want %d",
			snap.Offset, len(snap.State), len(w.snapshot.State))
	}
	return nil
}
