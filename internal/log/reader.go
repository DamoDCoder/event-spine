package log

import (
	"errors"
	"fmt"
)

// ErrEndOfLog means the reader has caught up with the writer.
//
// It is not io.EOF and it does not mean the log is finished. A log's tail is a
// place a reader waits, not a place it stops, and a consumer that treated
// catching up as the end would exit the moment it got fast enough.
var ErrEndOfLog = errors.New("log: reader has caught up")

// Reader is a cursor over the log: an offset and the segment that holds it, as
// docs/log-design.md describes. Readers are cheap and there can be many.
//
// A reader is not safe for concurrent use, and neither is the log it reads.
// Concurrency arrives with the scheduler, which will own the ordering decisions
// a mutex would otherwise make invisible.
type Reader struct {
	log *Log
	seg *Segment

	// pos is the byte position of the record at next, within seg. Holding
	// it is the whole point of a cursor: a scan advances by each record's
	// own length rather than searching the index for every offset.
	pos  int64
	next Offset

	// generation is the log's when this cursor last resolved a segment.
	// Compaction replaces a segment's file, which invalidates both the
	// handle and the position, so a cursor that finds the log has moved on
	// re-resolves rather than reading what it was holding.
	generation uint64
}

// Reader returns a cursor positioned at from.
//
// The offset must be one the log holds, or the offset it is about to assign: a
// reader created at the tail is a consumer that has caught up, which is the
// normal state of a consumer that is keeping up.
func (l *Log) Reader(from Offset) (*Reader, error) {
	if from < l.First() || from > l.Next() {
		return nil, fmt.Errorf("%w: %d is outside the log, which holds [%d, %d]",
			ErrNotFound, from, l.First(), l.Next())
	}
	return &Reader{log: l, next: from, generation: l.Generation()}, nil
}

// Next returns the record at the cursor and advances it.
//
// It returns ErrEndOfLog when the cursor reaches the writer. A reader that gets
// that error and calls Next again after more records are appended continues
// from where it stopped: the cursor is still valid, it was only waiting.
func (r *Reader) Next() (Record, error) {
	if r.next >= r.log.Next() {
		return Record{}, ErrEndOfLog
	}

	// The segment changes when the cursor starts, when it crosses a
	// boundary, and when compaction has replaced one underneath it.
	if r.seg == nil || r.next >= r.seg.Next() || r.generation != r.log.Generation() {
		if err := r.seek(r.next); err != nil {
			return Record{}, err
		}
	}

	// seek can legitimately find nothing at or after the cursor while the
	// log's tail is still above it: a crash that truncates one segment and
	// leaves the next one empty opens a hole running to the end, and every
	// offset in it is missing. Reaching that is the same thing as catching
	// up, and reading through the nil cursor instead is seeds/0290.md.
	if r.seg == nil {
		return Record{}, ErrEndOfLog
	}

	// The cursor asks for the next record at or after where it stands,
	// rather than for a particular offset. Compaction drops records and
	// keeps their offsets as gaps, and a reader that insisted on the next
	// integer would stop at the first one — the exact bug
	// docs/log-design.md predicts this design invites.
	rec, err := r.seg.readRecordFrom(r.pos, r.next)
	if err != nil {
		return Record{}, fmt.Errorf("log: read at or after offset %d: %w", r.next, err)
	}
	r.pos += int64(rec.Len)
	r.next = rec.Offset + 1
	return rec, nil
}

// Seek moves the cursor to off.
func (r *Reader) Seek(off Offset) error {
	if off < r.log.First() || off > r.log.Next() {
		return fmt.Errorf("%w: %d is outside the log, which holds [%d, %d]",
			ErrNotFound, off, r.log.First(), r.log.Next())
	}
	if off == r.log.Next() {
		// The tail has no record to locate yet. Drop the segment handle
		// so the next call finds whichever segment the writer has moved
		// on to by then.
		r.seg, r.pos, r.next = nil, 0, off
		r.generation = r.log.Generation()
		return nil
	}
	return r.seek(off)
}

// seek points the cursor at the first record at or after off.
//
// A compacted offset is not an error to seek to. The records are gone and their
// offsets remain as gaps, so a consumer holding an offset from before a
// compaction — the exact situation docs/log-design.md warns about — resumes at
// the next surviving record rather than failing.
func (r *Reader) seek(off Offset) error {
	for off < r.log.Next() {
		seg, err := r.log.segmentFor(off)
		if err != nil {
			return err
		}

		pos, found, err := seg.locateFrom(off)
		if err == nil {
			r.seg, r.pos, r.next = seg, pos, found
			r.generation = r.log.Generation()
			return nil
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}

		// Every remaining offset in this segment was compacted away.
		// The search continues in the next one, whose base is where the
		// hole ends.
		next, ok := r.log.baseAfter(seg.Base())
		if !ok {
			break
		}
		off = next
	}

	// Caught up: there is nothing at or after off yet.
	r.seg, r.pos, r.next = nil, 0, off
	r.generation = r.log.Generation()
	return nil
}

// Offset returns the offset the next call to Next will read.
func (r *Reader) Offset() Offset { return r.next }
