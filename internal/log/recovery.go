package log

// Opening a segment is where a crash is discovered. It is separated from the
// rest of the segment so that the read path and the recovery path can be read
// independently: everything here runs once, before any caller has been told
// anything, and everything there runs on every append.

import (
	"errors"
	"fmt"

	"github.com/DamoDCoder/event-spine/internal/core"
)

// Recovery describes what opening a segment found.
//
// Discarded is the number of bytes truncated from the tail. The simulation
// asserts it never exceeds the write that was in flight, because truncating
// more than that is silent data loss wearing recovery's clothes.
type Recovery struct {
	// Records is how many valid records were found.
	Records int64

	// Valid is the byte length the segment was truncated to.
	Valid int64

	// Discarded is how many bytes were removed from the tail.
	Discarded int64

	// Next is the offset the segment will assign next.
	Next Offset

	// Torn is set when the tail ended mid-record, which is the normal
	// outcome of a crash during an append.
	Torn bool

	// Corrupt is set when the tail was present but wrong: a failed
	// checksum, or framing that does not parse. Unlike Torn this is not
	// normal, and the caller decides whether to continue. Recovery reports
	// it rather than deciding, because "truncate and carry on" and "stop,
	// something is wrong with this disk" are both defensible and only the
	// caller knows which.
	Corrupt error
}

// OpenSegment opens an existing segment, scans it, and truncates any torn or
// corrupt tail.
//
// The scan is a full pass over the file. docs/log-design.md describes scanning
// forward from the last index entry instead, which is only possible when the
// index survived the crash — and this implementation keeps no index file
// precisely so that there is nothing to survive. A full scan on open is the
// price, and `task bench:log` is what decides whether it is too high.
func OpenSegment(fs core.FS, name string, opts Options) (*Segment, Recovery, error) {
	return openSegment(fs, name, opts, true)
}

// OpenSealedSegment opens a segment that is not the active one, and refuses to
// change it.
//
// A sealed segment has no legitimate torn tail: nothing has appended to it
// since it was sealed and synced. So a tail found here is a fault rather than a
// crash artefact, and truncating it would be deleting records that were
// acknowledged as durable. It is reported as an error and the file is left
// exactly as it was found, for a human to look at.
func OpenSealedSegment(fs core.FS, name string, opts Options) (*Segment, error) {
	s, rec, err := openSegment(fs, name, opts, false)
	if err != nil {
		return nil, err
	}
	switch {
	case rec.Corrupt != nil:
		s.Close()
		return nil, fmt.Errorf("log: sealed segment %s is damaged: %w", name, rec.Corrupt)
	case rec.Torn:
		s.Close()
		return nil, fmt.Errorf("log: sealed segment %s ends mid-record with %d bytes after offset %d: %w",
			name, rec.Discarded, rec.Next, ErrTorn)
	}
	s.Seal()
	return s, nil
}

func openSegment(fs core.FS, name string, opts Options, truncate bool) (*Segment, Recovery, error) {
	base, ok := ParseSegmentName(name)
	if !ok {
		return nil, Recovery{}, fmt.Errorf("log: %q is not a segment file name", name)
	}
	return openNamed(fs, name, base, opts, truncate)
}

// openNamed opens a record file whose name does not encode its base offset.
//
// The commits log is one: it lives in the same directory as the segments and
// must not be mistaken for one, so it carries a name no segment can have. It is
// still a file full of framed records, and reusing the segment gives it the
// same checksums and the same torn-tail recovery rather than a second
// implementation of both.
func openNamed(fs core.FS, name string, base Offset, opts Options, truncate bool) (*Segment, Recovery, error) {
	opts = opts.withDefaults()

	f, err := fs.Open(name)
	if err != nil {
		return nil, Recovery{}, fmt.Errorf("log: open segment %s: %w", name, err)
	}

	s := &Segment{
		file:     f,
		name:     name,
		opts:     opts,
		base:     base,
		next:     base,
		index:    []indexEntry{{offset: base, pos: 0}},
		maxBytes: opts.MaxBytes,
	}

	rec, err := s.scan()
	if err != nil {
		f.Close()
		return nil, Recovery{}, err
	}
	if rec.Discarded > 0 && truncate {
		if err := f.Truncate(rec.Valid); err != nil {
			f.Close()
			return nil, Recovery{}, fmt.Errorf("log: truncate %s to %d: %w", name, rec.Valid, err)
		}
	}
	s.size = rec.Valid
	s.next = rec.Next
	s.count = int(rec.Records)
	return s, rec, nil
}

// scan reads the segment from the start, validating every record.
func (s *Segment) scan() (Recovery, error) {
	size, err := s.file.Size()
	if err != nil {
		return Recovery{}, fmt.Errorf("log: size %s: %w", s.name, err)
	}

	rec := Recovery{Next: s.base}
	var pos int64

	for pos < size {
		// The offset a record carries is checked against the lowest one
		// it could legally have rather than against an exact value.
		// Compaction preserves offsets while dropping records, so after
		// it runs a segment's offsets ascend with gaps in them, and a
		// scan that demanded the next integer would call a compacted
		// segment corrupt.
		r, err := s.readRecordFrom(pos, rec.Next)
		switch {
		case err == nil:
		case errors.Is(err, ErrTorn):
			rec.Torn = true
			rec.Valid, rec.Discarded = pos, size-pos
			return rec, nil
		case errors.Is(err, ErrCorrupt):
			rec.Corrupt = fmt.Errorf("log: %s at byte %d: %w", s.name, pos, err)
			rec.Valid, rec.Discarded = pos, size-pos
			return rec, nil
		default:
			return Recovery{}, err
		}

		s.noteIndex(r.Offset, pos)
		pos += int64(r.Len)
		rec.Next = r.Offset + 1
		rec.Records++
	}

	rec.Valid = size
	return rec, nil
}
