// Package log is the segmented append-only log described in
// docs/log-design.md.
//
// Everything it touches on disk goes through the injected core.FS, and
// everything it decodes is untrusted: a record read back from a file may have
// been torn by a crash, flipped by a bad cable, or written by an older version
// of this code.
package log

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"

	"github.com/DamoDCoder/event-spine/core"
)

// Record framing, from the table in docs/log-design.md.
//
//	0   8  record offset (uint64)
//	8   4  total record length in bytes, header included (uint32)
//	12  4  CRC32C of everything after this field
//	16  8  logical timestamp (uint64)
//	24  2  schema version (uint16)
//	26  2  key length (uint16)
//	28  n  key bytes
//	28+n m payload bytes
//
// Bytes 16 onward are exactly core.Event.AppendCanonical's output, so the
// encoding a projection digest is computed over and the encoding on disk are
// the same bytes rather than two encodings that agree until one of them
// changes.
const (
	// HeaderLen is the fixed prefix before the key.
	HeaderLen = 28

	// crcStart is where the contiguous half of the CRC's coverage begins.
	// The offset field is covered too, out of line — see checksum.
	//
	// The length is deliberately outside it and stays there. The length has
	// to be trusted before the CRC can be located at all, so a corrupt
	// length is caught by the record failing to parse at its boundary, not
	// by the checksum.
	crcStart = 16

	offsetField = 0
	lengthField = 8
	crcField    = 12
	timeField   = 16
	schemaField = 24
	keyLenField = 26
)

// MaxRecordLen bounds what a decoder will believe a length field.
//
// Without it, a corrupt length claiming four gigabytes would send a reader off
// to allocate and read four gigabytes before discovering the checksum was never
// going to match. 64 MiB is the default segment size, so a record larger than
// this could not have been stored in the first place.
const MaxRecordLen = 64 << 20

// castagnoli is CRC32C, chosen over the IEEE polynomial because every
// architecture this runs on has an instruction for it.
var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// Offset identifies a record's position in the log. Offsets are assigned by the
// writer, monotonic, and gapless until compaction creates holes.
type Offset uint64

// Errors returned when decoding untrusted bytes. Callers classify on these:
// recovery treats a torn record at the end of a segment as normal and a corrupt
// record anywhere as a fault.
var (
	// ErrTorn means the buffer ended before the record did. At the end of
	// the active segment this is the expected outcome of a crash mid-append
	// and recovery truncates it. Anywhere else it is corruption.
	ErrTorn = errors.New("log: record is incomplete")

	// ErrCorrupt means the bytes are present but wrong: a failed checksum,
	// a length outside its bounds, or a key that runs past the record.
	ErrCorrupt = errors.New("log: record is corrupt")
)

// readLength returns the total-length field from a record header.
//
// A segment reads the header before the body, so it needs the length before it
// has enough bytes to Decode. The field is still untrusted: every caller
// bounds-checks it before using it to size a read.
func readLength(header []byte) uint32 {
	return binary.LittleEndian.Uint32(header[lengthField : lengthField+4])
}

// RecordLen returns the encoded size of an event, header included.
func RecordLen(e core.Event) int {
	return HeaderLen + len(e.Key) + len(e.Payload)
}

// Append encodes one record at the given offset and appends it to dst.
//
// It validates the event first: an event that cannot be framed is a bug in the
// caller, and encoding it anyway would put a record on disk that no decoder
// will accept.
func appendRecord(dst []byte, off Offset, e core.Event) ([]byte, error) {
	if err := e.Validate(); err != nil {
		return dst, err
	}
	total := RecordLen(e)
	if total > MaxRecordLen {
		return dst, fmt.Errorf("log: record is %d bytes, limit is %d", total, MaxRecordLen)
	}

	start := len(dst)
	dst = binary.LittleEndian.AppendUint64(dst, uint64(off))
	dst = binary.LittleEndian.AppendUint32(dst, uint32(total))
	dst = binary.LittleEndian.AppendUint32(dst, 0) // checksum, filled in below
	dst = e.AppendCanonical(dst)

	sum := checksum(dst[start:])
	binary.LittleEndian.PutUint32(dst[start+crcField:start+crcField+4], sum)
	return dst, nil
}

// checksum computes a record's CRC32C over the offset field and everything from
// crcStart onwards, skipping the length and the checksum itself.
//
// The offset used to be outside this, on the reasoning that a reader always
// knows which offset comes next and DecodeAt compares against it. Compaction
// ended that: once records can be missing, a scan can only require offsets to
// ascend, and a flipped bit that moves an offset further along passes the
// comparison. seeds/0008.md is that bug — one flipped bit put the log's tail at
// 4611686018427387911 and every later append was assigned an offset from there.
//
// Two ranges rather than one is the price of keeping the length outside, which
// is still right: the length is what locates the checksum, so it cannot be
// protected by it.
func checksum(record []byte) uint32 {
	sum := crc32.Update(0, castagnoli, record[offsetField:offsetField+8])
	return crc32.Update(sum, castagnoli, record[crcStart:])
}

// Record is one decoded record.
//
// Event.Key and Event.Payload alias the buffer they were decoded from. A caller
// that retains either beyond the life of that buffer must copy it: the log
// hands records to projections, and a projection is forbidden from retaining a
// payload for exactly this reason.
type Record struct {
	Offset Offset
	Event  core.Event

	// Len is the record's encoded size, so a scanner advances by it rather
	// than recomputing it from the event.
	Len int
}

// Decode reads the record at the front of b.
//
// It never trusts the buffer. Every field that indexes into the record is
// checked against the record's own bounds before it is used, because these
// bytes came off a disk.
func decodeRecord(b []byte) (Record, error) {
	if len(b) < HeaderLen {
		return Record{}, fmt.Errorf("%w: %d bytes available, header is %d", ErrTorn, len(b), HeaderLen)
	}

	total := int(binary.LittleEndian.Uint32(b[lengthField : lengthField+4]))
	switch {
	case total < HeaderLen:
		return Record{}, fmt.Errorf("%w: length field is %d, below the %d byte header", ErrCorrupt, total, HeaderLen)
	case total > MaxRecordLen:
		return Record{}, fmt.Errorf("%w: length field is %d, above the %d byte limit", ErrCorrupt, total, MaxRecordLen)
	case total > len(b):
		return Record{}, fmt.Errorf("%w: record claims %d bytes, %d available", ErrTorn, total, len(b))
	}

	want := binary.LittleEndian.Uint32(b[crcField : crcField+4])
	if got := checksum(b[:total]); got != want {
		return Record{}, fmt.Errorf("%w: checksum is %08x, want %08x", ErrCorrupt, got, want)
	}

	keyLen := int(binary.LittleEndian.Uint16(b[keyLenField : keyLenField+2]))
	if HeaderLen+keyLen > total {
		// Reachable only when a flipped key length happens to survive the
		// checksum, which it cannot, or when the checksum itself was
		// flipped to match. Checked anyway: the alternative is a slice
		// bound panic on hostile input.
		return Record{}, fmt.Errorf("%w: key length %d runs past the %d byte record", ErrCorrupt, keyLen, total)
	}

	rec := Record{
		Offset: Offset(binary.LittleEndian.Uint64(b[offsetField : offsetField+8])),
		Len:    total,
		Event: core.Event{
			Time:    core.Time(binary.LittleEndian.Uint64(b[timeField : timeField+8])),
			Schema:  binary.LittleEndian.Uint16(b[schemaField : schemaField+2]),
			Key:     string(b[HeaderLen : HeaderLen+keyLen]),
			Payload: b[HeaderLen+keyLen : total],
		},
	}
	if err := rec.Event.Validate(); err != nil {
		return Record{}, fmt.Errorf("%w: %w", ErrCorrupt, err)
	}
	return rec, nil
}

// DecodeAt reads the record at the front of b and checks that it carries the
// offset the caller expected.
//
// This is the check that covers the offset field, which the CRC does not. A
// reader always knows which offset should come next, so a flipped offset is
// detectable — but only if somebody compares. Prefer this over Decode wherever
// the expected offset is known, which is everywhere except a tool inspecting an
// unknown file.
func decodeRecordAt(b []byte, want Offset) (Record, error) {
	rec, err := decodeRecord(b)
	if err != nil {
		return rec, err
	}
	if rec.Offset != want {
		return Record{}, fmt.Errorf("%w: record carries offset %d, expected %d", ErrCorrupt, rec.Offset, want)
	}
	return rec, nil
}
