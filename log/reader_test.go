package log

import (
	"bytes"
	"errors"
	"testing"

	"github.com/DamoDCoder/event-spine/core"
	"github.com/DamoDCoder/event-spine/sim"
)

// drain reads until the reader catches up and returns what it saw.
func drain(t *testing.T, r *Reader) []Record {
	t.Helper()
	var got []Record
	for {
		rec, err := r.Next()
		if errors.Is(err, ErrEndOfLog) {
			return got
		}
		if err != nil {
			t.Fatalf("Next at offset %d: %v", r.Offset(), err)
		}
		got = append(got, rec)
	}
}

func TestAReaderWalksEveryRecordInOrder(t *testing.T) {
	bothFilesystems(t, func(t *testing.T, fs core.FS) {
		l, _, err := Open(fs, tinySegments())
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer l.Close()

		const n = 400
		appendN(t, l, 0, n)
		if len(l.Segments()) < 3 {
			t.Fatal("the log did not roll, so no boundary was crossed")
		}

		r, err := l.Reader(0)
		if err != nil {
			t.Fatalf("Reader: %v", err)
		}
		got := drain(t, r)

		if len(got) != n {
			t.Fatalf("read %d records, want %d", len(got), n)
		}
		for i, rec := range got {
			want := event(i)
			if rec.Offset != Offset(i) {
				t.Fatalf("record %d has offset %d", i, rec.Offset)
			}
			if rec.Event.Key != want.Key || !bytes.Equal(rec.Event.Payload, want.Payload) {
				t.Fatalf("record %d read back as %+v, want %+v", i, rec.Event, want)
			}
		}
		if r.Offset() != Offset(n) {
			t.Fatalf("a drained reader sits at %d, want %d", r.Offset(), n)
		}
	})
}

// Catching up is not finishing. A reader that reaches the tail, waits, and is
// then given more records continues from where it stopped.
func TestAReaderResumesAfterCatchingUp(t *testing.T) {
	bothFilesystems(t, func(t *testing.T, fs core.FS) {
		l, _, err := Open(fs, tinySegments())
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer l.Close()

		appendN(t, l, 0, 50)
		r, err := l.Reader(0)
		if err != nil {
			t.Fatalf("Reader: %v", err)
		}
		if got := drain(t, r); len(got) != 50 {
			t.Fatalf("first pass read %d records, want 50", len(got))
		}

		// Twice, because the second batch also has to cross the roll the
		// first one left the cursor sitting against.
		for round := range 2 {
			appendN(t, l, 50+round*50, 50)
			got := drain(t, r)
			if len(got) != 50 {
				t.Fatalf("round %d read %d records, want 50", round, len(got))
			}
			if got[0].Offset != Offset(50+round*50) {
				t.Fatalf("round %d resumed at offset %d, want %d", round, got[0].Offset, 50+round*50)
			}
		}
	})
}

// A reader created at the tail is a consumer that is keeping up, not an error.
func TestAReaderAtTheTailIsValid(t *testing.T) {
	fs := sim.NewFS()
	l, _, err := Open(fs, tinySegments())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer l.Close()

	appendN(t, l, 0, 10)
	r, err := l.Reader(l.Next())
	if err != nil {
		t.Fatalf("Reader at the tail: %v", err)
	}
	if _, err := r.Next(); !errors.Is(err, ErrEndOfLog) {
		t.Fatalf("a reader at the tail returned %v, want ErrEndOfLog", err)
	}

	appendN(t, l, 10, 5)
	got := drain(t, r)
	if len(got) != 5 || got[0].Offset != 10 {
		t.Fatalf("a tail reader saw %d records starting at %v", len(got), got)
	}
}

func TestSeekMovesTheCursor(t *testing.T) {
	fs := sim.NewFS()
	l, _, err := Open(fs, tinySegments())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer l.Close()

	const n = 200
	appendN(t, l, 0, n)
	r, err := l.Reader(0)
	if err != nil {
		t.Fatalf("Reader: %v", err)
	}

	// Backwards, forwards, and across segment boundaries in both
	// directions.
	for _, off := range []Offset{150, 3, 199, 0, 87} {
		if err := r.Seek(off); err != nil {
			t.Fatalf("Seek %d: %v", off, err)
		}
		if r.Offset() != off {
			t.Fatalf("after Seek %d the cursor is at %d", off, r.Offset())
		}
		rec, err := r.Next()
		if err != nil {
			t.Fatalf("Next after Seek %d: %v", off, err)
		}
		if rec.Offset != off {
			t.Fatalf("Seek %d then Next read offset %d", off, rec.Offset)
		}
	}

	// Seeking to the tail is allowed; past it is not.
	if err := r.Seek(l.Next()); err != nil {
		t.Fatalf("Seek to the tail: %v", err)
	}
	for _, off := range []Offset{Offset(n + 1), 1 << 40} {
		if err := r.Seek(off); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Seek %d gave %v, want ErrNotFound", off, err)
		}
	}
	if _, err := l.Reader(Offset(n + 1)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a reader past the tail was created without error")
	}
}

// A reader on a reopened log opens sealed segments as it reaches them, which is
// the path a consumer takes after a restart.
func TestAReaderOnAReopenedLogCrossesColdSegments(t *testing.T) {
	fs := sim.NewFS()
	const n = 300

	l, _, err := Open(fs, tinySegments())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	appendN(t, l, 0, n)
	if err := l.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, _, err := Open(fs, tinySegments())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()

	r, err := reopened.Reader(0)
	if err != nil {
		t.Fatalf("Reader: %v", err)
	}
	got := drain(t, r)
	if len(got) != n {
		t.Fatalf("read %d records after reopen, want %d", len(got), n)
	}
	for i, rec := range got {
		if rec.Offset != Offset(i) {
			t.Fatalf("record %d has offset %d", i, rec.Offset)
		}
	}
}

// The cursor and the random-access path must agree. They take different routes
// to the same bytes — one advances by record length, the other searches the
// sparse index — and a disagreement means one of them is walking the file
// wrongly.
func TestAReaderAgreesWithRandomAccess(t *testing.T) {
	fs := sim.NewFS()
	l, _, err := Open(fs, Config{Segment: Options{MaxBytes: 512, IndexInterval: 64}})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer l.Close()

	const n = 250
	appendN(t, l, 0, n)

	r, err := l.Reader(0)
	if err != nil {
		t.Fatalf("Reader: %v", err)
	}
	for i := range n {
		scanned, err := r.Next()
		if err != nil {
			t.Fatalf("Next %d: %v", i, err)
		}
		sought, err := l.Read(Offset(i))
		if err != nil {
			t.Fatalf("Read %d: %v", i, err)
		}
		if scanned.Offset != sought.Offset || scanned.Len != sought.Len {
			t.Fatalf("offset %d: scan gave %+v, lookup gave %+v", i, scanned, sought)
		}
		if !bytes.Equal(scanned.Event.Payload, sought.Event.Payload) {
			t.Fatalf("offset %d: scan and lookup disagree on the payload", i)
		}
	}
}
