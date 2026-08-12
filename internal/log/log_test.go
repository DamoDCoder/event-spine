package log

import (
	"bytes"
	"errors"
	"testing"

	"github.com/DamoDCoder/event-spine/internal/core"
	"github.com/DamoDCoder/event-spine/internal/sim"
)

// tinySegments rolls often enough that a short test crosses several segment
// boundaries, which is where the interesting bugs are: a lookup that assumed
// one file, or a roll that lost the offset it was meant to continue from.
func tinySegments() Config {
	return Config{Segment: Options{MaxBytes: 512, IndexInterval: 128}}
}

func appendN(t *testing.T, l *Log, from, n int) {
	t.Helper()
	for i := from; i < from+n; i++ {
		offs, err := l.Append(event(i))
		if err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
		if len(offs) != 1 || offs[0] != Offset(i) {
			t.Fatalf("append %d was assigned %v, want [%d]", i, offs, i)
		}
	}
}

// readAll checks every offset in [0, n) reads back as the event that was
// written, in an order that crosses segment boundaries in both directions.
func readAll(t *testing.T, l *Log, n int) {
	t.Helper()
	for _, i := range interleave(n) {
		rec, err := l.Read(Offset(i))
		if err != nil {
			t.Fatalf("Read %d: %v", i, err)
		}
		want := event(i)
		if rec.Offset != Offset(i) || rec.Event.Key != want.Key || rec.Event.Time != want.Time {
			t.Fatalf("offset %d read back as %+v, want %+v", i, rec, want)
		}
		if !bytes.Equal(rec.Event.Payload, want.Payload) {
			t.Fatalf("offset %d payload %x, want %x", i, rec.Event.Payload, want.Payload)
		}
	}
}

// interleave walks the range from both ends, so consecutive reads land in
// different segments and a lookup cannot pass by accident of sequential access.
func interleave(n int) []int {
	out := make([]int, 0, n)
	for i := range n / 2 {
		out = append(out, i, n-1-i)
	}
	if n%2 == 1 {
		out = append(out, n/2)
	}
	return out
}

func TestLogRollsAndReadsAcrossSegments(t *testing.T) {
	bothFilesystems(t, func(t *testing.T, fs core.FS) {
		l, _, err := Open(fs, tinySegments())
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer l.Close()

		const n = 400
		appendN(t, l, 0, n)

		if got := len(l.Segments()); got < 3 {
			t.Fatalf("%d records at 512 bytes a segment produced %d segments", n, got)
		}
		if l.Next() != Offset(n) {
			t.Fatalf("next offset is %d, want %d", l.Next(), n)
		}
		readAll(t, l, n)

		// Segment bases are ascending, gapless in the sense that each
		// one begins where the previous ended, and the last is the
		// active one.
		bases := l.Segments()
		for i := 1; i < len(bases); i++ {
			if bases[i] <= bases[i-1] {
				t.Fatalf("segment bases are not ascending: %v", bases)
			}
			rec, err := l.Read(bases[i])
			if err != nil {
				t.Fatalf("the first record of segment %d is unreadable: %v", bases[i], err)
			}
			if rec.Offset != bases[i] {
				t.Fatalf("segment %d begins at offset %d", bases[i], rec.Offset)
			}
		}

		for _, off := range []Offset{Offset(n), Offset(n + 1), 1 << 40} {
			if _, err := l.Read(off); !errors.Is(err, ErrNotFound) {
				t.Fatalf("Read %d gave %v, want ErrNotFound", off, err)
			}
		}
	})
}

// A batch append is not atomic, but it must assign the same offsets one at a
// time would, including across the roll it triggers.
func TestBatchAppendAssignsContiguousOffsets(t *testing.T) {
	bothFilesystems(t, func(t *testing.T, fs core.FS) {
		l, _, err := Open(fs, tinySegments())
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer l.Close()

		batch := make([]core.Event, 200)
		for i := range batch {
			batch[i] = event(i)
		}
		offs, err := l.Append(batch...)
		if err != nil {
			t.Fatalf("Append: %v", err)
		}
		if len(offs) != len(batch) {
			t.Fatalf("appended %d events and got %d offsets", len(batch), len(offs))
		}
		for i, off := range offs {
			if off != Offset(i) {
				t.Fatalf("event %d was assigned offset %d", i, off)
			}
		}
		if len(l.Segments()) < 2 {
			t.Fatal("the batch did not cross a segment boundary, so the roll inside it was never exercised")
		}
		readAll(t, l, len(batch))
	})
}

func TestReopenALogContinuesWhereItStopped(t *testing.T) {
	bothFilesystems(t, func(t *testing.T, fs core.FS) {
		const n = 300
		l, _, err := Open(fs, tinySegments())
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		appendN(t, l, 0, n)
		segments := len(l.Segments())
		if err := l.Sync(); err != nil {
			t.Fatalf("Sync: %v", err)
		}
		if err := l.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}

		reopened, rec, err := Open(fs, tinySegments())
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		defer reopened.Close()

		if rec.Discarded != 0 || rec.Torn || rec.Corrupt != nil {
			t.Fatalf("a cleanly closed log recovered as %+v", rec)
		}
		if reopened.Next() != Offset(n) {
			t.Fatalf("next offset after reopen is %d, want %d", reopened.Next(), n)
		}
		if got := len(reopened.Segments()); got != segments {
			t.Fatalf("reopened with %d segments, want %d", got, segments)
		}

		// Sealed segments are opened lazily, so this is the first read
		// that touches them.
		readAll(t, reopened, n)

		appendN(t, reopened, n, 50)
		readAll(t, reopened, n+50)
	})
}

// Only the active segment can hold a torn tail, and a crash must leave every
// sealed segment untouched.
func TestCrashLosesOnlyTheUnsyncedTail(t *testing.T) {
	fs := sim.NewFS()

	cfg := tinySegments()
	// A batch size no test will reach, so the only syncs are the explicit
	// one below and the one every roll performs.
	cfg.SyncRecords = 1 << 20
	l, _, err := Open(fs, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	const n = 300
	appendN(t, l, 0, n)
	if err := l.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if err := fs.Sync(); err != nil {
		t.Fatalf("FS.Sync: %v", err)
	}
	durable := l.Next()

	appendN(t, l, n, 40)
	fs.Crash()

	reopened, rec, err := Open(fs, cfg)
	if err != nil {
		t.Fatalf("reopen after crash: %v", err)
	}
	defer reopened.Close()

	if reopened.Next() < durable {
		t.Fatalf("recovery lost synced records: next is %d, want at least %d", reopened.Next(), durable)
	}
	if rec.Corrupt != nil {
		t.Fatalf("a crash was reported as corruption: %v", rec.Corrupt)
	}
	// Everything acknowledged as durable is still readable, sealed
	// segments included.
	readAll(t, reopened, int(durable))
}

// The directory is untrusted. A file nobody in this package wrote must be
// ignored rather than parsed into an offset.
func TestOpenIgnoresFilesThatAreNotSegments(t *testing.T) {
	fs := sim.NewFS()
	for _, junk := range []string{"README", "0.log", "0000000000000000000x.log", "notes.txt"} {
		f, err := fs.Create(junk)
		if err != nil {
			t.Fatalf("create %s: %v", junk, err)
		}
		if _, err := f.Append([]byte("not a record")); err != nil {
			t.Fatalf("write %s: %v", junk, err)
		}
		f.Close()
	}

	l, _, err := Open(fs, tinySegments())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer l.Close()

	if got := l.Segments(); len(got) != 1 || got[0] != 0 {
		t.Fatalf("stray files produced segments %v", got)
	}
	appendN(t, l, 0, 50)
	readAll(t, l, 50)
}

// A damaged sealed segment is not a crash artefact, so it is reported and the
// file is left alone for a human to look at.
func TestADamagedSealedSegmentIsReportedNotTruncated(t *testing.T) {
	fs := sim.NewFS()

	l, _, err := Open(fs, tinySegments())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	appendN(t, l, 0, 300)
	if err := l.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	bases := l.Segments()
	if len(bases) < 2 {
		t.Fatalf("expected several segments, got %v", bases)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Corrupt the body of the first record of the first sealed segment.
	name := SegmentName(bases[0])
	before := fileSize(t, fs, name)
	if err := flipByte(fs, name, crcStart); err != nil {
		t.Fatalf("flip: %v", err)
	}

	reopened, _, err := Open(fs, tinySegments())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()

	// The active segment is fine, so the log opens and reads from it.
	if _, err := reopened.Read(reopened.Next() - 1); err != nil {
		t.Fatalf("the active segment became unreadable: %v", err)
	}

	_, err = reopened.Read(bases[0])
	if err == nil {
		t.Fatal("a corrupt sealed segment read back without error")
	}
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt sealed segment reported as %v, want ErrCorrupt", err)
	}
	assertOnDisk(t, fs, name, before)
}

func fileSize(t *testing.T, fs core.FS, name string) int64 {
	t.Helper()
	f, err := fs.Open(name)
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	defer f.Close()
	size, err := f.Size()
	if err != nil {
		t.Fatalf("size %s: %v", name, err)
	}
	return size
}
