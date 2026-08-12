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
	scratch  []byte
	maxBytes int64
}

// CreateSegment makes a new empty segment beginning at base.
func CreateSegment(fs core.FS, base Offset, opts Options) (*Segment, error) {
	opts = opts.withDefaults()
	name := SegmentName(base)
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
	opts = opts.withDefaults()

	base, ok := ParseSegmentName(name)
	if !ok {
		return nil, Recovery{}, fmt.Errorf("log: %q is not a segment file name", name)
	}
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
		r, err := s.readRecordAt(pos, rec.Next)
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

		s.noteIndex(rec.Next, pos)
		pos += int64(r.Len)
		rec.Next++
		rec.Records++
	}

	rec.Valid = size
	return rec, nil
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
	if s.sealed {
		return 0, fmt.Errorf("log: append to %s: %w", s.name, ErrSealed)
	}

	off := s.next
	s.scratch = s.scratch[:0]
	buf, err := Append(s.scratch, off, e)
	if err != nil {
		return 0, err
	}
	s.scratch = buf

	if _, err := s.file.Append(buf); err != nil {
		// The file may now hold a partial record. That is exactly the
		// torn tail recovery exists to truncate, so the segment refuses
		// further appends rather than writing a valid record after a
		// partial one, which would make the tear unfindable.
		s.sealed = true
		return 0, fmt.Errorf("log: append to %s: %w", s.name, err)
	}

	s.noteIndex(off, s.size)
	s.size += int64(len(buf))
	s.next++
	return off, nil
}

// Read returns the record at off.
//
// It binary searches the sparse index for the greatest indexed offset at or
// below off, then scans forward. The scan is bounded by the index interval, so
// its cost is set by configuration rather than by how large the segment is.
func (s *Segment) Read(off Offset) (Record, error) {
	if off < s.base || off >= s.next {
		return Record{}, fmt.Errorf("%w: %d is outside segment %s, which holds [%d, %d)",
			ErrNotFound, off, s.name, s.base, s.next)
	}

	i := sort.Search(len(s.index), func(i int) bool { return s.index[i].offset > off })
	entry := s.index[i-1]

	pos, at := entry.pos, entry.offset
	for pos < s.size {
		r, err := s.readRecordAt(pos, at)
		if err != nil {
			return Record{}, err
		}
		if at == off {
			return r, nil
		}
		pos += int64(r.Len)
		at++
	}
	// Unreachable while the index and the file agree. It is a returned
	// error rather than a panic because "the index disagrees with the file"
	// is a bug worth a message that names the offset.
	return Record{}, fmt.Errorf("%w: %d is within segment %s but was not found on disk", ErrNotFound, off, s.name)
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
