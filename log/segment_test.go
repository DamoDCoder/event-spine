package log

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"github.com/DamoDCoder/event-spine/core"
	"github.com/DamoDCoder/event-spine/runtime"
	"github.com/DamoDCoder/event-spine/sim"
)

// Segments are exercised against both filesystems. The simulated one is where
// crashes are injected; the real one is what stops the simulated one from
// drifting into a filesystem that only exists in this repository.
func bothFilesystems(t *testing.T, run func(t *testing.T, fs core.FS)) {
	t.Helper()
	t.Run("simulated", func(t *testing.T) { run(t, sim.NewFS()) })
	t.Run("real", func(t *testing.T) {
		fs, err := runtime.NewFS(t.TempDir())
		if err != nil {
			t.Fatalf("real filesystem: %v", err)
		}
		run(t, fs)
	})
}

// assertOnDisk checks the file's actual length, not what recovery said about
// it. Reporting the right numbers while leaving a torn tail on disk would pass
// every other assertion here and still leave the next append writing a valid
// record after a partial one.
func assertOnDisk(t *testing.T, fs core.FS, name string, want int64) {
	t.Helper()
	f, err := fs.Open(name)
	if err != nil {
		t.Fatalf("open %s to check its size: %v", name, err)
	}
	defer f.Close()
	got, err := f.Size()
	if err != nil {
		t.Fatalf("size %s: %v", name, err)
	}
	if got != want {
		t.Fatalf("%s is %d bytes on disk, want %d", name, got, want)
	}
}

func event(i int) core.Event {
	return core.Event{
		Key:     fmt.Sprintf("acct-%04d", i%16),
		Time:    core.Time(i * 10),
		Schema:  1,
		Payload: bytes.Repeat([]byte{byte(i)}, i%23),
	}
}

func fill(t *testing.T, s *segment, n int) {
	t.Helper()
	fillFrom(t, s, 0, n)
}

// fillFrom appends n events starting at offset from, asserting that the
// segment assigns the offsets the caller expects. Offsets are gapless within a
// segment, so the expectation is exact rather than approximate.
func fillFrom(t *testing.T, s *segment, from, n int) {
	t.Helper()
	for i := from; i < from+n; i++ {
		off, err := s.Append(event(i))
		if err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
		if off != Offset(i) {
			t.Fatalf("append %d was assigned offset %d", i, off)
		}
	}
}

func TestSegmentNameRoundTrips(t *testing.T) {
	for _, base := range []Offset{0, 1, 42, 1 << 20, 1<<64 - 1} {
		name := segmentName(base)
		if len(name) != segmentDigits+len(segmentSuffix) {
			t.Fatalf("%q is %d characters, want %d", name, len(name), segmentDigits+len(segmentSuffix))
		}
		got, ok := parseSegmentName(name)
		if !ok || got != base {
			t.Fatalf("%q parsed as %d, %v; want %d", name, got, ok, base)
		}
	}

	// Names arrive from directory listings, which are untrusted.
	for _, junk := range []string{
		"", "notes.txt", "0.log", "0000000000000000000x.log",
		"00000000000000000000", "00000000000000000000.log.tmp",
		"-0000000000000000001.log", "0000000000000000000 .log",
	} {
		if _, ok := parseSegmentName(junk); ok {
			t.Fatalf("%q was accepted as a segment name", junk)
		}
	}
}

func TestSegmentAppendAndRead(t *testing.T) {
	bothFilesystems(t, func(t *testing.T, fs core.FS) {
		// A tiny index interval so the sparse index has many entries and
		// the forward scan after a lookup is actually exercised.
		s, err := newSegment(fs, 0, Options{IndexInterval: 64})
		if err != nil {
			t.Fatalf("newSegment: %v", err)
		}
		defer s.Close()

		const n = 300
		fill(t, s, n)

		if s.Next() != Offset(n) {
			t.Fatalf("next offset is %d, want %d", s.Next(), n)
		}
		if len(s.index) < 2 {
			t.Fatalf("the sparse index holds %d entries; the interval was never crossed", len(s.index))
		}

		// Read out of order, so a lookup cannot pass by accident of
		// sequential access.
		for _, i := range []int{299, 0, 150, 1, 298, 77, 42} {
			rec, err := s.Read(Offset(i))
			if err != nil {
				t.Fatalf("Read %d: %v", i, err)
			}
			want := event(i)
			if rec.Event.Key != want.Key || rec.Event.Time != want.Time {
				t.Fatalf("offset %d read back as %+v, want %+v", i, rec.Event, want)
			}
			if !bytes.Equal(rec.Event.Payload, want.Payload) {
				t.Fatalf("offset %d payload %x, want %x", i, rec.Event.Payload, want.Payload)
			}
		}

		for _, off := range []Offset{Offset(n), Offset(n + 1), 1 << 40} {
			if _, err := s.Read(off); !errors.Is(err, ErrNotFound) {
				t.Fatalf("Read %d gave %v, want ErrNotFound", off, err)
			}
		}
	})
}

func TestSegmentSealAndFull(t *testing.T) {
	bothFilesystems(t, func(t *testing.T, fs core.FS) {
		s, err := newSegment(fs, 0, Options{MaxBytes: 200})
		if err != nil {
			t.Fatalf("newSegment: %v", err)
		}
		defer s.Close()

		if s.Full() {
			t.Fatal("an empty segment reported itself full")
		}
		fill(t, s, 20)
		if !s.Full() {
			t.Fatalf("segment holds %d bytes with a 200 byte limit and is not full", s.Bytes())
		}

		s.Seal()
		if _, err := s.Append(event(99)); !errors.Is(err, ErrSealed) {
			t.Fatalf("append to a sealed segment gave %v, want ErrSealed", err)
		}
	})
}

func TestReopenACleanSegment(t *testing.T) {
	bothFilesystems(t, func(t *testing.T, fs core.FS) {
		s, err := newSegment(fs, 0, Options{IndexInterval: 128})
		if err != nil {
			t.Fatalf("newSegment: %v", err)
		}
		const n = 120
		fill(t, s, n)
		written := s.Bytes()
		if err := s.Sync(); err != nil {
			t.Fatalf("Sync: %v", err)
		}
		if err := s.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}

		reopened, rec, err := openSegmentTruncating(fs, segmentName(0), Options{IndexInterval: 128})
		if err != nil {
			t.Fatalf("OpenSegment: %v", err)
		}
		defer reopened.Close()

		switch {
		case rec.Records != n:
			t.Fatalf("recovered %d records, want %d", rec.Records, n)
		case rec.Discarded != 0:
			t.Fatalf("a clean segment discarded %d bytes", rec.Discarded)
		case rec.Valid != written:
			t.Fatalf("recovered %d valid bytes, want %d", rec.Valid, written)
		case rec.Next != Offset(n):
			t.Fatalf("next offset is %d, want %d", rec.Next, n)
		case rec.Torn:
			t.Fatal("a clean segment was reported torn")
		case rec.Corrupt != nil:
			t.Fatalf("a clean segment was reported corrupt: %v", rec.Corrupt)
		}

		// Reading and appending both continue from where the segment
		// left off.
		if _, err := reopened.Read(Offset(n - 1)); err != nil {
			t.Fatalf("Read after reopen: %v", err)
		}
		off, err := reopened.Append(event(n))
		if err != nil {
			t.Fatalf("Append after reopen: %v", err)
		}
		if off != Offset(n) {
			t.Fatalf("append after reopen was assigned %d, want %d", off, n)
		}
	})
}
