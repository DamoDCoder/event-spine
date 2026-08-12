package log

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/DamoDCoder/event-spine/internal/core"
)

// Segment defaults, from docs/log-design.md.
const (
	// DefaultMaxBytes is when a segment rolls.
	DefaultMaxBytes = 64 << 20

	// DefaultIndexInterval is how much a segment may grow between sparse
	// index entries. It bounds the forward scan a lookup performs, so
	// lookup cost is set by configuration rather than by log size.
	DefaultIndexInterval = 4 << 10

	// SegmentSuffix is the extension every segment file carries.
	SegmentSuffix = ".log"

	// segmentDigits zero-pads a segment name so names sort lexically in
	// offset order. A directory listing is then already in the right order,
	// which is one fewer thing to get wrong.
	segmentDigits = 20
)

// ErrSealed is returned by an append to a segment that has been sealed. Sealed
// segments are immutable, which is what makes compaction and snapshotting safe
// to run beside an active writer.
var ErrSealed = errors.New("log: segment is sealed")

// ErrNotFound is returned when a lookup names an offset the segment does not
// hold. After compaction this is expected: compaction preserves offsets, so it
// creates gaps, and a reader that assumed contiguity is the exact bug
// docs/log-design.md predicts this design invites.
var ErrNotFound = errors.New("log: offset not found")

// Options configure a segment.
type Options struct {
	// MaxBytes is the size at which the segment reports itself full.
	MaxBytes int64

	// IndexInterval is the byte interval between sparse index entries.
	IndexInterval int64
}

func (o Options) withDefaults() Options {
	if o.MaxBytes <= 0 {
		o.MaxBytes = DefaultMaxBytes
	}
	if o.IndexInterval <= 0 {
		o.IndexInterval = DefaultIndexInterval
	}
	return o
}

// SegmentName returns the file name for a segment beginning at base.
func SegmentName(base Offset) string {
	return fmt.Sprintf("%0*d%s", segmentDigits, uint64(base), SegmentSuffix)
}

// ParseSegmentName returns the base offset encoded in a segment file name.
//
// Names come from directory listings, which are untrusted: a file someone
// dropped in the log directory must be rejected rather than parsed into an
// offset that then indexes something.
func ParseSegmentName(name string) (Offset, bool) {
	digits, ok := strings.CutSuffix(name, SegmentSuffix)
	if !ok || len(digits) != segmentDigits {
		return 0, false
	}
	base, err := strconv.ParseUint(digits, 10, 64)
	if err != nil {
		return 0, false
	}
	return Offset(base), true
}

// indexEntry maps an offset to its byte position in the segment file.
type indexEntry struct {
	offset Offset
	pos    int64
}

// Segment is one append-only file plus the sparse index over it.
//
// The index is built in memory rather than written to its own file.
// docs/log-design.md describes an index file, and this deviates deliberately:
// an index on disk is a second thing a crash can tear, and it is reconstructible
// from the segment it indexes. It stays in memory until `task bench:log` shows
// that rebuilding it on open is where the time goes. Optimising before that
// measurement would be trading a real failure mode for an imagined saving.
type Segment struct {
	file core.File
	name string
	opts Options

	base Offset
	next Offset
	size int64

	index    []indexEntry
	indexed  int64 // file position of the most recent index entry
	sealed   bool
	maxBytes int64

	// Buffers reused across appends. scratch stages the records of one
	// call, marks holds the file position each of them will occupy, and one
	// exists so a single-event append can reach the batch path without
	// allocating a slice to do it.
	scratch []byte
	marks   []int64
	one     [1]core.Event
}

// CreateSegment makes a new empty segment beginning at base.
func CreateSegment(fs core.FS, base Offset, opts Options) (*Segment, error) {
	return createNamed(fs, SegmentName(base), base, opts)
}

// createNamed makes a record file whose name does not encode its base offset.
// See openNamed for why that exists.
func createNamed(fs core.FS, name string, base Offset, opts Options) (*Segment, error) {
	opts = opts.withDefaults()
	f, err := fs.Create(name)
	if err != nil {
		return nil, fmt.Errorf("log: create segment %s: %w", name, err)
	}
	return &Segment{
		file:     f,
		name:     name,
		opts:     opts,
		base:     base,
		next:     base,
		index:    []indexEntry{{offset: base, pos: 0}},
		maxBytes: opts.MaxBytes,
	}, nil
}

// readRecordAt reads and validates the record beginning at pos.
//
// It reads the header first and the body second, rather than guessing at a read
// size, because the header is what says how long the record is and a guess
// would either over-read past the end of the file or under-read and need
// stitching.
func (s *Segment) readRecordAt(pos int64, want Offset) (Record, error) {
	header := make([]byte, HeaderLen)
	n, err := s.file.ReadAt(header, pos)
	switch {
	case errors.Is(err, io.EOF):
		return Record{}, fmt.Errorf("%w: %d header bytes at %d", ErrTorn, n, pos)
	case err != nil:
		return Record{}, fmt.Errorf("log: read %s at %d: %w", s.name, pos, err)
	}

	total := int(readLength(header))
	switch {
	case total < HeaderLen:
		return Record{}, fmt.Errorf("%w: length field is %d, below the %d byte header", ErrCorrupt, total, HeaderLen)
	case total > MaxRecordLen:
		return Record{}, fmt.Errorf("%w: length field is %d, above the %d byte limit", ErrCorrupt, total, MaxRecordLen)
	}

	buf := make([]byte, total)
	copy(buf, header)
	if total > HeaderLen {
		n, err := s.file.ReadAt(buf[HeaderLen:], pos+HeaderLen)
		switch {
		case errors.Is(err, io.EOF):
			return Record{}, fmt.Errorf("%w: record claims %d bytes, %d available", ErrTorn, total, HeaderLen+n)
		case err != nil:
			return Record{}, fmt.Errorf("log: read %s at %d: %w", s.name, pos, err)
		}
	}
	return DecodeAt(buf, want)
}

// noteIndex records a sparse index entry when the segment has grown past the
// configured interval since the last one.
func (s *Segment) noteIndex(off Offset, pos int64) {
	if pos-s.indexed < s.opts.IndexInterval || pos == 0 {
		return
	}
	s.index = append(s.index, indexEntry{offset: off, pos: pos})
	s.indexed = pos
}

// Append writes one event and returns the offset it was assigned.
//
// Append does not sync. Durability is the caller's choice, because the three
// modes in docs/log-design.md differ only in when the caller asks for it.
func (s *Segment) Append(e core.Event) (Offset, error) {
	// The one-element slice is a field rather than a local, so taking a
	// slice of it does not escape to the heap on every append. bench/log.txt
	// is why that is worth a line of explanation: at 1.4 microseconds an
	// append, one allocation is a measurable share of the operation.
	s.one[0] = e
	n, off, err := s.AppendAll(s.one[:])
	if err != nil {
		return 0, err
	}
	if n != 1 {
		return 0, fmt.Errorf("log: append to %s wrote %d records, want 1", s.name, n)
	}
	return off, nil
}

// AppendAll writes as many of the events as the segment has room for, in one
// write, and returns how many it took along with the offset of the first.
//
// One write is the point. The first benchmark run found every record costing
// its own write(2), which is where the append path's time went — batching the
// fsync amortised durability but left the syscall count untouched. The caller
// is expected to hand the remainder to the next segment.
//
// A short return is normal and means the segment filled. An error after a
// non-zero count means the events up to that count were written and the one
// after it was rejected.
func (s *Segment) AppendAll(events []core.Event) (int, Offset, error) {
	if s.sealed {
		return 0, 0, fmt.Errorf("log: append to %s: %w", s.name, ErrSealed)
	}
	if len(events) == 0 {
		return 0, s.next, nil
	}

	first := s.next
	s.scratch = s.scratch[:0]
	s.marks = s.marks[:0]

	var encodeErr error
	for i, e := range events {
		// The position this record will occupy, which is the segment's
		// current size plus everything already staged in the buffer.
		pos := s.size + int64(len(s.scratch))

		buf, err := Append(s.scratch, first+Offset(i), e)
		if err != nil {
			// Append validates before it encodes, so the buffer still
			// holds exactly the records staged before this one. The
			// valid prefix is written and the error is returned after
			// it, because the alternative is discarding records the
			// caller was told nothing about.
			encodeErr = err
			break
		}
		s.scratch = buf
		s.marks = append(s.marks, pos)

		if s.size+int64(len(s.scratch)) >= s.maxBytes {
			break
		}
	}

	n := len(s.marks)
	if n == 0 {
		return 0, first, encodeErr
	}

	if _, err := s.file.Append(s.scratch); err != nil {
		// The file may now hold a partial record. That is exactly the
		// torn tail recovery exists to truncate, so the segment refuses
		// further appends rather than writing a valid record after a
		// partial one, which would make the tear unfindable.
		//
		// Nothing is acknowledged: the caller is told zero records
		// landed, and recovery will discard whatever reached the disk.
		s.sealed = true
		return 0, first, fmt.Errorf("log: append to %s: %w", s.name, err)
	}

	for i, pos := range s.marks {
		s.noteIndex(first+Offset(i), pos)
	}
	s.size += int64(len(s.scratch))
	s.next += Offset(n)
	return n, first, encodeErr
}

// Read returns the record at off.
func (s *Segment) Read(off Offset) (Record, error) {
	pos, err := s.locate(off)
	if err != nil {
		return Record{}, err
	}
	return s.readRecordAt(pos, off)
}

// locate returns the byte position of the record at off.
//
// It binary searches the sparse index for the greatest indexed offset at or
// below off, then scans forward. The scan is bounded by the index interval, so
// its cost is set by configuration rather than by how large the segment is.
//
// A sequential reader calls this once and then advances by each record's own
// length, which is why it is separate from Read: paying the search per record
// is what makes a scan cost more than the bytes it reads.
func (s *Segment) locate(off Offset) (int64, error) {
	if off < s.base || off >= s.next {
		return 0, fmt.Errorf("%w: %d is outside segment %s, which holds [%d, %d)",
			ErrNotFound, off, s.name, s.base, s.next)
	}

	i := sort.Search(len(s.index), func(i int) bool { return s.index[i].offset > off })
	entry := s.index[i-1]

	pos, at := entry.pos, entry.offset
	for pos < s.size {
		if at == off {
			return pos, nil
		}
		r, err := s.readRecordAt(pos, at)
		if err != nil {
			return 0, err
		}
		pos += int64(r.Len)
		at++
	}
	// Unreachable while the index and the file agree. It is a returned
	// error rather than a panic because "the index disagrees with the file"
	// is a bug worth a message that names the offset.
	return 0, fmt.Errorf("%w: %d is within segment %s but was not found on disk", ErrNotFound, off, s.name)
}

// Sync makes everything appended so far durable.
func (s *Segment) Sync() error {
	if err := s.file.Sync(); err != nil {
		return fmt.Errorf("log: sync %s: %w", s.name, err)
	}
	return nil
}

// Seal marks the segment immutable. It is idempotent.
func (s *Segment) Seal() { s.sealed = true }

// Full reports whether the segment has reached its configured size.
func (s *Segment) Full() bool { return s.size >= s.maxBytes }

// Base returns the first offset the segment can hold.
func (s *Segment) Base() Offset { return s.base }

// Next returns the offset the next append will be assigned.
func (s *Segment) Next() Offset { return s.next }

// Bytes returns the segment's current size on disk.
func (s *Segment) Bytes() int64 { return s.size }

// Name returns the segment's file name.
func (s *Segment) Name() string { return s.name }

// Close releases the underlying file without syncing it. A caller that wants
// durability asks for it.
func (s *Segment) Close() error {
	if err := s.file.Close(); err != nil {
		return fmt.Errorf("log: close %s: %w", s.name, err)
	}
	return nil
}
