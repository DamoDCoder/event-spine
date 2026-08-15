package log

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"testing"

	"github.com/DamoDCoder/event-spine/core"
	"github.com/DamoDCoder/event-spine/sim"
)

func sampleEvent(src *sim.Source) core.Event {
	key := make([]byte, src.Intn(12))
	for i := range key {
		key[i] = byte('a' + src.Intn(26))
	}
	payload := make([]byte, src.Intn(40))
	for i := range payload {
		payload[i] = byte(src.Intn(256))
	}
	return core.Event{
		Key:     string(key),
		Time:    core.Time(src.Intn(1 << 30)),
		Schema:  uint16(src.Intn(8) + 1),
		Payload: payload,
	}
}

func TestRoundTrip(t *testing.T) {
	src := sim.NewSource(1)
	for i := range 500 {
		want := sampleEvent(src)
		off := Offset(i)

		buf, err := appendRecord(nil, off, want)
		if err != nil {
			t.Fatalf("Append: %v", err)
		}
		if len(buf) != RecordLen(want) {
			t.Fatalf("encoded %d bytes, RecordLen says %d", len(buf), RecordLen(want))
		}

		got, err := decodeRecordAt(buf, off)
		if err != nil {
			t.Fatalf("DecodeAt: %v", err)
		}
		if got.Offset != off {
			t.Fatalf("offset %d, want %d", got.Offset, off)
		}
		if got.Len != len(buf) {
			t.Fatalf("Len %d, want %d", got.Len, len(buf))
		}
		if got.Event.Key != want.Key || got.Event.Time != want.Time || got.Event.Schema != want.Schema {
			t.Fatalf("header round-tripped as %+v, want %+v", got.Event, want)
		}
		if !bytes.Equal(got.Event.Payload, want.Payload) {
			t.Fatalf("payload round-tripped as %x, want %x", got.Event.Payload, want.Payload)
		}
	}
}

// The record body is byte-for-byte the encoding a projection digest is computed
// over. If these ever diverge, a digest taken in memory and one taken after a
// replay would disagree, and the failure would only appear after a restart.
func TestBodyIsTheCanonicalEncoding(t *testing.T) {
	src := sim.NewSource(7)
	for range 100 {
		e := sampleEvent(src)
		buf, err := appendRecord(nil, 42, e)
		if err != nil {
			t.Fatalf("Append: %v", err)
		}
		if got, want := buf[crcStart:], e.AppendCanonical(nil); !bytes.Equal(got, want) {
			t.Fatalf("record body is %x, canonical encoding is %x", got, want)
		}
	}
}

// A crash mid-append leaves a prefix of a record. Every prefix must be reported
// as torn rather than as corrupt, because recovery truncates a torn tail and
// treats corruption as a fault worth stopping for.
func TestEveryPrefixIsTornNotCorrupt(t *testing.T) {
	e := core.Event{Key: "acct-0001", Time: 99, Schema: 1, Payload: []byte("some payload bytes")}
	full, err := appendRecord(nil, 5, e)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}

	for n := range len(full) {
		_, err := decodeRecord(full[:n])
		if !errors.Is(err, ErrTorn) {
			t.Fatalf("a %d byte prefix of a %d byte record decoded as %v, want ErrTorn", n, len(full), err)
		}
	}

	if _, err := decodeRecord(full); err != nil {
		t.Fatalf("the complete record failed to decode: %v", err)
	}
}

// Every single-bit flip, at every position, classified by what catches it.
//
// This test is as much documentation as verification: the CRC covers the body
// and nothing else, so it states exactly which corruption the checksum catches
// and which the reader has to catch by knowing where it is.
func TestEverySingleBitFlipIsDetected(t *testing.T) {
	e := core.Event{Key: "acct-0001", Time: 1234, Schema: 3, Payload: []byte("payload")}
	const off Offset = 77

	clean, err := appendRecord(nil, off, e)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}

	// Every bit of every byte, with no exceptions. The offset field used to
	// be one: it sat outside CRC coverage because a reader always knew
	// which offset came next and DecodeAt compared against it. Compaction
	// ended that — a scan across gaps can only require offsets to ascend —
	// and seeds/0008.md is the flipped bit that walked through the gap it
	// left.
	for bytePos := range clean {
		for bit := range 8 {
			corrupt := append([]byte(nil), clean...)
			corrupt[bytePos] ^= 1 << bit

			_, err := decodeRecord(corrupt)
			if err == nil {
				t.Fatalf("byte %d bit %d: a flipped record decoded cleanly", bytePos, bit)
			}
			if !errors.Is(err, ErrCorrupt) && !errors.Is(err, ErrTorn) {
				t.Fatalf("byte %d bit %d: unexpected error %v", bytePos, bit, err)
			}

			// DecodeAt is strictly stronger, never weaker.
			if _, err := decodeRecordAt(corrupt, off); err == nil {
				t.Fatalf("byte %d bit %d: DecodeAt accepted what Decode rejected", bytePos, bit)
			}
		}
	}
}

// The length field stays outside the checksum, because the length is what
// locates the checksum. It is caught by the record failing to parse at its own
// boundary instead, and that is a different mechanism worth its own test.
func TestAFlippedLengthIsCaughtByFramingRatherThanTheChecksum(t *testing.T) {
	e := core.Event{Key: "acct-0001", Time: 1234, Schema: 3, Payload: []byte("payload")}

	clean, err := appendRecord(nil, 77, e)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}

	for bytePos := lengthField; bytePos < lengthField+4; bytePos++ {
		for bit := range 8 {
			corrupt := append([]byte(nil), clean...)
			corrupt[bytePos] ^= 1 << bit

			if _, err := decodeRecord(corrupt); err == nil {
				t.Fatalf("byte %d bit %d: a flipped length decoded cleanly", bytePos, bit)
			}
		}
	}
}

func TestDecodeRejectsMalformedFraming(t *testing.T) {
	e := core.Event{Key: "k", Time: 1, Schema: 1, Payload: []byte("v")}
	clean, err := appendRecord(nil, 1, e)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}

	withLength := func(n uint32) []byte {
		b := append([]byte(nil), clean...)
		binary.LittleEndian.PutUint32(b[lengthField:lengthField+4], n)
		return b
	}

	cases := map[string]struct {
		in   []byte
		want error
	}{
		"length below the header": {withLength(HeaderLen - 1), ErrCorrupt},
		"length above the limit":  {withLength(MaxRecordLen + 1), ErrCorrupt},
		"length past the buffer":  {withLength(uint32(len(clean) + 1)), ErrTorn},
		"empty buffer":            {nil, ErrTorn},
		"header only":             {clean[:HeaderLen], ErrTorn},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeRecord(tc.in); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

// A key length that runs past the record cannot survive the checksum, so it is
// only reachable by corrupting the checksum to match. Checked because the
// alternative to a bounds check is a slice bound panic on hostile input.
func TestDecodeRejectsAKeyLengthPastTheRecord(t *testing.T) {
	e := core.Event{Key: "k", Time: 1, Schema: 1, Payload: []byte("v")}
	b, err := appendRecord(nil, 1, e)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}

	binary.LittleEndian.PutUint16(b[keyLenField:keyLenField+2], 0xffff)
	// Recompute the checksum so the framing check is what rejects it, not
	// the CRC.
	sum := crc32.Checksum(b[crcStart:], castagnoli)
	binary.LittleEndian.PutUint32(b[crcField:crcField+4], sum)

	if _, err := decodeRecord(b); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("got %v, want ErrCorrupt", err)
	}
}

func TestAppendRejectsAnInvalidEvent(t *testing.T) {
	if _, err := appendRecord(nil, 0, core.Event{Schema: 0}); !errors.Is(err, core.ErrInvalidEvent) {
		t.Fatalf("got %v, want ErrInvalidEvent", err)
	}
}

// Records concatenate, which is what a segment is.
func TestRecordsScanBackInOrder(t *testing.T) {
	src := sim.NewSource(3)
	var (
		buf  []byte
		want []core.Event
	)
	for i := range 200 {
		e := sampleEvent(src)
		var err error
		buf, err = appendRecord(buf, Offset(i), e)
		if err != nil {
			t.Fatalf("Append: %v", err)
		}
		want = append(want, e)
	}

	var pos int
	for i := range want {
		rec, err := decodeRecordAt(buf[pos:], Offset(i))
		if err != nil {
			t.Fatalf("record %d at byte %d: %v", i, pos, err)
		}
		if rec.Event.Key != want[i].Key || !bytes.Equal(rec.Event.Payload, want[i].Payload) {
			t.Fatalf("record %d decoded as %+v, want %+v", i, rec.Event, want[i])
		}
		pos += rec.Len
	}
	if pos != len(buf) {
		t.Fatalf("scanned %d of %d bytes", pos, len(buf))
	}

	if _, err := decodeRecord(buf[pos:]); !errors.Is(err, ErrTorn) {
		t.Fatalf("decoding past the last record gave %v, want ErrTorn", err)
	}
}

func TestDecodeAtRejectsTheWrongOffset(t *testing.T) {
	b, err := appendRecord(nil, 10, core.Event{Key: "k", Time: 1, Schema: 1})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if _, err := decodeRecordAt(b, 11); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("got %v, want ErrCorrupt", err)
	}
	if _, err := decodeRecordAt(b, 10); err != nil {
		t.Fatalf("the right offset was rejected: %v", err)
	}
}

func TestRecordLenMatchesTheEncoding(t *testing.T) {
	src := sim.NewSource(11)
	for range 200 {
		e := sampleEvent(src)
		b, err := appendRecord(nil, 0, e)
		if err != nil {
			t.Fatalf("Append: %v", err)
		}
		if RecordLen(e) != len(b) {
			t.Fatalf("RecordLen says %d, encoding is %d bytes: %s",
				RecordLen(e), len(b), fmt.Sprintf("key=%d payload=%d", len(e.Key), len(e.Payload)))
		}
	}
}
