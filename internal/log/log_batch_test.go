package log

import (
	"errors"
	"testing"

	"github.com/DamoDCoder/event-spine/internal/core"
	"github.com/DamoDCoder/event-spine/internal/sim"
)

// The whole point of staging a batch in one buffer. bench/log.txt found every
// record costing its own write(2), so this asserts the syscall count directly
// rather than trusting a throughput number to notice a regression.
func TestABatchCostsOneWritePerSegment(t *testing.T) {
	fs := &countingFS{FS: sim.NewFS()}
	l, _, err := Open(fs, Config{Segment: noRoll(), Durability: OS})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer l.Close()

	batch := make([]core.Event, 500)
	for i := range batch {
		batch[i] = event(i)
	}
	if _, err := l.Append(batch...); err != nil {
		t.Fatalf("Append: %v", err)
	}

	if fs.writes != 1 {
		t.Fatalf("a batch of %d records took %d writes, want 1", len(batch), fs.writes)
	}
}

// A batch that crosses a roll pays one write per segment, because the second
// segment is a different file. That is the floor, and anything above it means
// the batching stopped at the boundary instead of resuming after it.
func TestABatchCrossingARollWritesOncePerSegment(t *testing.T) {
	fs := &countingFS{FS: sim.NewFS()}
	l, _, err := Open(fs, Config{Segment: Options{MaxBytes: 512}, Durability: OS})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer l.Close()

	batch := make([]core.Event, 300)
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

	segments := len(l.Segments())
	if segments < 2 {
		t.Fatalf("the batch did not roll: %d segment", segments)
	}
	if fs.writes != segments {
		t.Fatalf("a batch across %d segments took %d writes, want %d", segments, fs.writes, segments)
	}
}

// An event that cannot be framed is a bug in the caller, and the records before
// it in the batch are not the caller's mistake. They are written, their offsets
// are returned, and the error comes back with them.
func TestAnInvalidEventKeepsThePrefixOfItsBatch(t *testing.T) {
	bothFilesystems(t, func(t *testing.T, fs core.FS) {
		l, _, err := Open(fs, Config{Segment: noRoll(), Durability: OS})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer l.Close()

		bad := event(2)
		bad.Schema = 0 // a schema version of zero fails Validate

		offs, err := l.Append(event(0), event(1), bad, event(3))
		if !errors.Is(err, core.ErrInvalidEvent) {
			t.Fatalf("Append returned %v, want ErrInvalidEvent", err)
		}
		if len(offs) != 2 {
			t.Fatalf("got offsets %v, want the two valid records before the bad one", offs)
		}
		if l.Next() != 2 {
			t.Fatalf("next offset is %d, want 2", l.Next())
		}

		// The prefix is genuinely on disk, and nothing after the invalid
		// event followed it.
		for i := range 2 {
			if _, err := l.Read(Offset(i)); err != nil {
				t.Fatalf("record %d from the prefix was lost: %v", i, err)
			}
		}
		if _, err := l.Read(2); !errors.Is(err, ErrNotFound) {
			t.Fatalf("reading past the invalid event gave %v, want ErrNotFound", err)
		}

		// The log still works: the caller fixes the event and retries.
		if _, err := l.Append(event(2)); err != nil {
			t.Fatalf("append after an invalid event: %v", err)
		}
	})
}

// A write that fails may have left any prefix of the batch on disk, and there
// is no way to tell which. Nothing is acknowledged, and the segment seals so
// that a later valid record cannot hide the tear.
func TestAFailedWriteAcknowledgesNothingFromTheBatch(t *testing.T) {
	// One successful write, then failure: the first batch lands and the
	// second is what the disk refuses.
	fs := &failingFS{FS: sim.NewFS(), failAfter: 1}
	l, _, err := Open(fs, Config{Segment: noRoll(), Durability: OS})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer l.Close()

	if _, err := l.Append(event(0), event(1), event(2)); err != nil {
		t.Fatalf("the first batch: %v", err)
	}
	if l.Next() != 3 {
		t.Fatalf("next offset is %d after three records, want 3", l.Next())
	}

	offs, err := l.Append(event(3), event(4), event(5))
	if err == nil {
		t.Fatal("the failing batch returned no error")
	}
	if len(offs) != 0 {
		t.Fatalf("a failed write acknowledged offsets %v", offs)
	}
	if l.Next() != 3 {
		t.Fatalf("next offset moved to %d after a failed write", l.Next())
	}
	if _, err := l.Append(event(3)); !errors.Is(err, ErrSealed) {
		t.Fatalf("the segment accepted an append after a failed write: %v", err)
	}
}

// A single append is the batch path with one element, and it must not allocate
// a slice to get there. The segment reuses a one-element array for it, which
// bench/log.txt is the reason for.
func TestASingleAppendDoesNotAllocateItsBatch(t *testing.T) {
	fs := sim.NewFS()
	l, _, err := Open(fs, Config{Segment: noRoll(), Durability: OS})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer l.Close()

	e := event(1)
	// The returned offsets slice is one allocation the caller asked for.
	// Anything above that is the batch plumbing leaking into the single
	// append path.
	got := testing.AllocsPerRun(100, func() {
		if _, err := l.Append(e); err != nil {
			t.Fatalf("Append: %v", err)
		}
	})
	if got > 1 {
		t.Fatalf("a single append made %v allocations, want at most 1", got)
	}
}
