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
	return &Reader{log: l, next: from}, nil
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
	// boundary, and never otherwise.
	if r.seg == nil || r.next >= r.seg.Next() {
		if err := r.seek(r.next); err != nil {
			return Record{}, err
		}
	}

	rec, err := r.seg.readRecordAt(r.pos, r.next)
	if err != nil {
		return Record{}, fmt.Errorf("log: read offset %d: %w", r.next, err)
	}
	r.pos += int64(rec.Len)
	r.next++
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
		return nil
	}
	return r.seek(off)
}

// seek points the cursor at an offset the log holds.
func (r *Reader) seek(off Offset) error {
	seg, err := r.log.segmentFor(off)
	if err != nil {
		return err
	}
	pos, err := seg.locate(off)
	if err != nil {
		return err
	}
	r.seg, r.pos, r.next = seg, pos, off
	return nil
}

// Offset returns the offset the next call to Next will read.
func (r *Reader) Offset() Offset { return r.next }
