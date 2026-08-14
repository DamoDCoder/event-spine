package log

import (
	"errors"
	"fmt"
	"testing"

	"github.com/DamoDCoder/event-spine/core"
	"github.com/DamoDCoder/event-spine/sim"
)

// The headline recovery property, from docs/log-design.md: a torn tail is
// truncated, and the discarded bytes never exceed the write that was in flight.
//
// The tail is truncated at every byte position within the final record, so this
// is not one crash point but every crash point that record has.
func TestRecoveryDiscardsExactlyTheTornTail(t *testing.T) {
	bothFilesystems(t, func(t *testing.T, fs core.FS) {
		const n = 40
		last := event(n - 1)
		lastLen := int64(RecordLen(last))

		for cut := int64(1); cut < lastLen; cut++ {
			name := SegmentName(0)

			s, err := CreateSegment(fs, 0, Options{})
			if err != nil {
				t.Fatalf("CreateSegment: %v", err)
			}
			fill(t, s, n)
			full := s.Bytes()
			if err := s.Sync(); err != nil {
				t.Fatalf("Sync: %v", err)
			}
			if err := s.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}

			// Cut mid-record, exactly as a crash during this append
			// would have left it.
			f, err := fs.Open(name)
			if err != nil {
				t.Fatalf("open for truncation: %v", err)
			}
			if err := f.Truncate(full - cut); err != nil {
				t.Fatalf("truncate: %v", err)
			}
			f.Close()

			reopened, rec, err := OpenSegment(fs, name, Options{})
			if err != nil {
				t.Fatalf("cut %d: OpenSegment: %v", cut, err)
			}

			switch {
			case !rec.Torn:
				t.Fatalf("cut %d: a partial record was not reported torn", cut)
			case rec.Corrupt != nil:
				t.Fatalf("cut %d: a torn tail was reported corrupt: %v", cut, rec.Corrupt)
			case rec.Records != n-1:
				t.Fatalf("cut %d: recovered %d records, want %d", cut, rec.Records, n-1)
			case rec.Next != Offset(n-1):
				t.Fatalf("cut %d: next offset is %d, want %d", cut, rec.Next, n-1)
			case rec.Valid != full-lastLen:
				t.Fatalf("cut %d: kept %d bytes, want %d", cut, rec.Valid, full-lastLen)
			}

			// The property worth stating as a number. The record in
			// flight was lastLen bytes and cut of them never
			// reached the disk, so exactly lastLen-cut partial
			// bytes are there to discard — and recovery discards
			// those and not one byte more.
			if want := lastLen - cut; rec.Discarded != want {
				t.Fatalf("cut %d: discarded %d bytes, want %d", cut, rec.Discarded, want)
			}
			if rec.Discarded >= lastLen {
				t.Fatalf("cut %d: discarded %d bytes, which is the whole %d byte record in flight or more",
					cut, rec.Discarded, lastLen)
			}

			// The tail is gone from the file, not merely absent from
			// the report. Leaving it there would mean the next
			// append writes a valid record after a partial one.
			assertOnDisk(t, fs, name, rec.Valid)

			// Every record before the torn one survived intact.
			for i := range n - 1 {
				got, err := reopened.Read(Offset(i))
				if err != nil {
					t.Fatalf("cut %d: record %d was lost: %v", cut, i, err)
				}
				if got.Event.Key != event(i).Key {
					t.Fatalf("cut %d: record %d changed", cut, i)
				}
			}
			reopened.Close()

			if err := fs.Remove(name); err != nil {
				t.Fatalf("cleanup: %v", err)
			}
		}
	})
}

// A complete record whose bytes are wrong is not a torn tail. Recovery
// truncates it, but reports it, because "carry on" and "stop, this disk is
// lying to you" are both defensible and only the caller knows which.
func TestRecoveryReportsCorruptionSeparately(t *testing.T) {
	bothFilesystems(t, func(t *testing.T, fs core.FS) {
		const n = 20
		name := SegmentName(0)

		s, err := CreateSegment(fs, 0, Options{})
		if err != nil {
			t.Fatalf("CreateSegment: %v", err)
		}
		fill(t, s, n)
		if err := s.Sync(); err != nil {
			t.Fatalf("Sync: %v", err)
		}

		// Find where the last record starts, then corrupt a byte of its
		// body rather than its framing, so the checksum is what catches
		// it.
		lastStart := s.Bytes() - int64(RecordLen(event(n-1)))
		s.Close()

		if err := flipByte(fs, name, lastStart+crcStart); err != nil {
			t.Fatalf("flip: %v", err)
		}

		reopened, rec, err := OpenSegment(fs, name, Options{})
		if err != nil {
			t.Fatalf("OpenSegment: %v", err)
		}
		defer reopened.Close()

		if rec.Corrupt == nil {
			t.Fatal("a flipped byte was not reported as corruption")
		}
		if !errors.Is(rec.Corrupt, ErrCorrupt) {
			t.Fatalf("corruption reported as %v, want ErrCorrupt", rec.Corrupt)
		}
		if rec.Torn {
			t.Fatal("corruption was also reported as torn; they are different outcomes")
		}
		if rec.Records != n-1 {
			t.Fatalf("recovered %d records, want %d", rec.Records, n-1)
		}
		assertOnDisk(t, fs, name, rec.Valid)
		for i := range n - 1 {
			if _, err := reopened.Read(Offset(i)); err != nil {
				t.Fatalf("record %d before the corruption was lost: %v", i, err)
			}
		}
	})
}

// flipByte rebuilds a file with one bit of one byte flipped.
//
// The filesystem interface deliberately offers no write-at-offset, because the
// log only ever appends and an interface that cannot overwrite cannot corrupt a
// sealed segment by accident. A test that wants to simulate a bad cable
// therefore has to be this explicit about it, which is the right amount of
// friction.
func flipByte(fs core.FS, name string, at int64) error {
	f, err := fs.Open(name)
	if err != nil {
		return err
	}
	size, err := f.Size()
	if err != nil {
		f.Close()
		return err
	}
	buf := make([]byte, size)
	if _, err := f.ReadAt(buf, 0); err != nil {
		f.Close()
		return err
	}
	f.Close()

	buf[at] ^= 0x01
	if err := fs.Remove(name); err != nil {
		return err
	}
	out, err := fs.Create(name)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = out.Append(buf)
	return err
}

// A crash loses everything that was not synced, and recovery must land exactly
// on the last synced record rather than somewhere near it.
func TestRecoveryAfterASimulatedCrashLandsOnTheLastSyncedRecord(t *testing.T) {
	for _, syncAfter := range []int{1, 7, 19, 40} {
		t.Run(fmt.Sprintf("sync-after-%d", syncAfter), func(t *testing.T) {
			fs := sim.NewFS()

			s, err := CreateSegment(fs, 0, Options{})
			if err != nil {
				t.Fatalf("CreateSegment: %v", err)
			}
			fill(t, s, syncAfter)
			if err := s.Sync(); err != nil {
				t.Fatalf("Sync: %v", err)
			}
			if err := fs.Sync(); err != nil {
				t.Fatalf("FS.Sync: %v", err)
			}
			durable := s.Bytes()

			// Written but never synced: a power cut takes these.
			fillFrom(t, s, syncAfter, 10)

			fs.Crash()

			reopened, rec, err := OpenSegment(fs, SegmentName(0), Options{})
			if err != nil {
				t.Fatalf("OpenSegment: %v", err)
			}
			defer reopened.Close()

			if rec.Valid != durable {
				t.Fatalf("recovered %d bytes, want the %d that were synced", rec.Valid, durable)
			}
			if rec.Records != int64(syncAfter) {
				t.Fatalf("recovered %d records, want %d", rec.Records, syncAfter)
			}
			if rec.Next != Offset(syncAfter) {
				t.Fatalf("next offset is %d, want %d", rec.Next, syncAfter)
			}
			if rec.Discarded != 0 {
				t.Fatalf("discarded %d bytes; a record boundary crash should leave nothing to truncate", rec.Discarded)
			}
			assertOnDisk(t, fs, SegmentName(0), rec.Valid)
			for i := range syncAfter {
				if _, err := reopened.Read(Offset(i)); err != nil {
					t.Fatalf("synced record %d was lost: %v", i, err)
				}
			}
		})
	}
}

// A failed write may leave a partial record. Writing a valid record after it
// would make the tear unfindable, so the segment refuses to continue.
func TestAFailedAppendSealsTheSegment(t *testing.T) {
	fs := &failingFS{FS: sim.NewFS(), failAfter: 5}

	s, err := CreateSegment(fs, 0, Options{})
	if err != nil {
		t.Fatalf("CreateSegment: %v", err)
	}
	defer s.Close()

	fill(t, s, 5)
	if _, err := s.Append(event(5)); err == nil {
		t.Fatal("the failing append returned no error")
	}
	if _, err := s.Append(event(6)); !errors.Is(err, ErrSealed) {
		t.Fatalf("the segment accepted an append after a failed write: %v", err)
	}
}

// failingFS fails the Nth append onwards, so the seal-on-failure path is
// reachable without a real disk filling up.
type failingFS struct {
	core.FS
	failAfter int
	appends   int
}

func (f *failingFS) Create(name string) (core.File, error) {
	file, err := f.FS.Create(name)
	if err != nil {
		return nil, err
	}
	return &failingFile{File: file, fs: f}, nil
}

func (f *failingFS) Open(name string) (core.File, error) {
	file, err := f.FS.Open(name)
	if err != nil {
		return nil, err
	}
	return &failingFile{File: file, fs: f}, nil
}

type failingFile struct {
	core.File
	fs *failingFS
}

func (f *failingFile) Append(p []byte) (int, error) {
	if f.fs.appends >= f.fs.failAfter {
		return 0, errors.New("failingFS: no space left on device")
	}
	f.fs.appends++
	return f.File.Append(p)
}
