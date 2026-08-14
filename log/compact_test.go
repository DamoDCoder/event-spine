package log

import (
	"errors"
	"fmt"
	"testing"

	"github.com/DamoDCoder/event-spine/core"
	"github.com/DamoDCoder/event-spine/sim"
)

// keyed builds an event for a named key, so a test can control which records
// supersede which.
func keyed(key string, n int) core.Event {
	return core.Event{
		Key:     key,
		Time:    core.Time(n),
		Schema:  1,
		Payload: fmt.Appendf(nil, "%s-%d", key, n),
	}
}

// fillKeyed appends n events cycling over keys, so every key appears several
// times and only its last record should survive a compaction.
func fillKeyed(t *testing.T, l *Log, n, keys int) {
	t.Helper()
	for i := range n {
		if _, err := l.Append(keyed(fmt.Sprintf("k%d", i%keys), i)); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
}

func TestCompactionKeepsTheNewestRecordPerKey(t *testing.T) {
	bothFilesystems(t, func(t *testing.T, fs core.FS) {
		l, _, err := Open(fs, Config{Segment: Options{MaxBytes: 1 << 10, IndexInterval: 128}})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer l.Close()

		const keys = 4
		fillKeyed(t, l, 300, keys)
		if err := l.Sync(); err != nil {
			t.Fatalf("Sync: %v", err)
		}

		base := l.Segments()[0]
		before, err := recordsIn(l, base)
		if err != nil {
			t.Fatalf("reading the segment before compaction: %v", err)
		}

		c, err := l.Compact(base)
		if err != nil {
			t.Fatalf("Compact: %v", err)
		}
		if c.Kept != keys {
			t.Fatalf("compaction kept %d records, want one per key (%d)", c.Kept, keys)
		}
		if c.Dropped != len(before)-keys {
			t.Fatalf("compaction dropped %d records, want %d", c.Dropped, len(before)-keys)
		}
		if c.After >= c.Before {
			t.Fatalf("the segment grew from %d to %d bytes", c.Before, c.After)
		}

		// The survivors are the last record each key had, with their
		// original offsets.
		want := map[string]Record{}
		for _, rec := range before {
			want[rec.Event.Key] = rec
		}
		after, err := recordsIn(l, base)
		if err != nil {
			t.Fatalf("reading the segment after compaction: %v", err)
		}
		if len(after) != len(want) {
			t.Fatalf("the compacted segment holds %d records, want %d", len(after), len(want))
		}
		for _, got := range after {
			expected := want[got.Event.Key]
			if got.Offset != expected.Offset {
				t.Fatalf("key %s survived at offset %d, want %d", got.Event.Key, got.Offset, expected.Offset)
			}
			if got.Event.Time != expected.Event.Time {
				t.Fatalf("key %s survived as the record from time %d, want %d",
					got.Event.Key, got.Event.Time, expected.Event.Time)
			}
		}
	})
}

// Offsets are preserved, so a dropped record leaves a hole rather than shifting
// everything after it. A consumer's committed offset must mean the same thing
// after a compaction as before one.
func TestCompactionLeavesGapsRatherThanRenumbering(t *testing.T) {
	fs := sim.NewFS()
	l, _, err := Open(fs, Config{Segment: Options{MaxBytes: 1 << 10, IndexInterval: 128}})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer l.Close()

	fillKeyed(t, l, 300, 4)
	tail := l.Next()
	base := l.Segments()[0]

	before, err := recordsIn(l, base)
	if err != nil {
		t.Fatalf("before: %v", err)
	}
	if _, err := l.Compact(base); err != nil {
		t.Fatalf("Compact: %v", err)
	}

	survivors := map[Offset]bool{}
	after, err := recordsIn(l, base)
	if err != nil {
		t.Fatalf("after: %v", err)
	}
	for _, rec := range after {
		survivors[rec.Offset] = true
	}

	// Every offset that survived reads back as itself, and every offset
	// that did not is reported missing rather than silently returning its
	// neighbour.
	for _, rec := range before {
		got, err := l.Read(rec.Offset)
		if survivors[rec.Offset] {
			if err != nil {
				t.Fatalf("surviving offset %d is unreadable: %v", rec.Offset, err)
			}
			if got.Offset != rec.Offset {
				t.Fatalf("reading %d returned offset %d", rec.Offset, got.Offset)
			}
			continue
		}
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("reading the compacted offset %d gave %v, want ErrNotFound", rec.Offset, err)
		}
	}

	// The log's tail did not move: compaction removes records, it does not
	// renumber the ones that remain or the ones that follow.
	if l.Next() != tail {
		t.Fatalf("the tail moved to %d, want %d", l.Next(), tail)
	}
}

// A reader must tolerate gaps, which docs/log-design.md names as the exact bug
// this design invites.
func TestAReaderWalksPastCompactedOffsets(t *testing.T) {
	fs := sim.NewFS()
	l, _, err := Open(fs, Config{Segment: Options{MaxBytes: 1 << 10, IndexInterval: 128}})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer l.Close()

	fillKeyed(t, l, 400, 4)
	if _, err := l.CompactAll(); err != nil {
		t.Fatalf("CompactAll: %v", err)
	}

	r, err := l.Reader(l.First())
	if err != nil {
		t.Fatalf("Reader: %v", err)
	}
	got := drain(t, r)
	if len(got) == 0 {
		t.Fatal("the reader saw nothing after compaction")
	}

	// Ascending, with gaps, and every record readable on its own.
	for i, rec := range got {
		if i > 0 && rec.Offset <= got[i-1].Offset {
			t.Fatalf("record %d has offset %d, after %d", i, rec.Offset, got[i-1].Offset)
		}
		if _, err := l.Read(rec.Offset); err != nil {
			t.Fatalf("record at offset %d is unreadable by lookup: %v", rec.Offset, err)
		}
	}
	if got[len(got)-1].Offset != l.Next()-1 {
		t.Fatalf("the scan ended at offset %d, want the tail at %d", got[len(got)-1].Offset, l.Next()-1)
	}
}

// A consumer holding an offset from before a compaction is the situation the
// design warns about. Seeking to a removed offset lands on the next surviving
// record rather than failing.
func TestSeekingToACompactedOffsetLandsOnTheNextRecord(t *testing.T) {
	fs := sim.NewFS()
	l, _, err := Open(fs, Config{Segment: Options{MaxBytes: 1 << 10, IndexInterval: 128}})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer l.Close()

	fillKeyed(t, l, 400, 4)
	before, err := recordsIn(l, l.Segments()[0])
	if err != nil {
		t.Fatalf("before: %v", err)
	}
	if _, err := l.CompactAll(); err != nil {
		t.Fatalf("CompactAll: %v", err)
	}

	// An offset that compaction removed, taken from the first segment.
	// Offset 0 is a perfectly ordinary answer here, so the search reports
	// whether it found one rather than leaning on a zero value.
	var (
		removed Offset
		found   bool
	)
	for _, rec := range before {
		if _, err := l.Read(rec.Offset); errors.Is(err, ErrNotFound) {
			removed, found = rec.Offset, true
			break
		}
	}
	if !found {
		t.Fatal("compaction removed nothing, so there is no gap to seek into")
	}

	r, err := l.Reader(l.First())
	if err != nil {
		t.Fatalf("Reader: %v", err)
	}
	if err := r.Seek(removed); err != nil {
		t.Fatalf("seeking to the compacted offset %d: %v", removed, err)
	}
	rec, err := r.Next()
	if err != nil {
		t.Fatalf("Next after seeking into a gap: %v", err)
	}
	if rec.Offset <= removed {
		t.Fatalf("seeking to %d delivered offset %d, which is not after the gap", removed, rec.Offset)
	}

	// A group committed at a removed offset resumes there too, since that
	// is the same path a restarted consumer takes.
	g, err := l.Group("stale")
	if err != nil {
		t.Fatalf("Group: %v", err)
	}
	if err := g.Commit(removed); err != nil {
		t.Fatalf("Commit at a compacted offset: %v", err)
	}
	gr, err := g.Reader()
	if err != nil {
		t.Fatalf("Group reader: %v", err)
	}
	if _, err := gr.Next(); err != nil {
		t.Fatalf("a group resuming at a compacted offset: %v", err)
	}
}

func TestCompactionRefusesTheActiveSegment(t *testing.T) {
	fs := sim.NewFS()
	l, _, err := Open(fs, tinySegments())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer l.Close()
	fillKeyed(t, l, 100, 4)

	bases := l.Segments()
	active := bases[len(bases)-1]
	if _, err := l.Compact(active); err == nil {
		t.Fatal("compacting the active segment was allowed")
	}
	if _, err := l.Compact(1 << 40); !errors.Is(err, ErrNotFound) {
		t.Fatalf("compacting a segment that does not exist gave %v, want ErrNotFound", err)
	}
}

// A compacted segment survives a reopen: the file on disk is the compacted one,
// and its index is rebuilt from what is actually there.
func TestACompactedSegmentReopens(t *testing.T) {
	fs := sim.NewFS()
	cfg := Config{Segment: Options{MaxBytes: 1 << 10, IndexInterval: 128}}

	l, _, err := Open(fs, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	fillKeyed(t, l, 300, 4)
	if err := l.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	base := l.Segments()[0]
	if _, err := l.Compact(base); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	want, err := recordsIn(l, base)
	if err != nil {
		t.Fatalf("after compaction: %v", err)
	}
	tail := l.Next()
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, rec, err := Open(fs, cfg)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	if rec.Corrupt != nil {
		t.Fatalf("a compacted log reopened as corrupt: %v", rec.Corrupt)
	}
	if reopened.Next() != tail {
		t.Fatalf("the tail after reopen is %d, want %d", reopened.Next(), tail)
	}

	got, err := recordsIn(reopened, base)
	if err != nil {
		t.Fatalf("after reopen: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("the compacted segment holds %d records after reopen, want %d", len(got), len(want))
	}
	for i := range got {
		if got[i].Offset != want[i].Offset {
			t.Fatalf("record %d is offset %d after reopen, want %d", i, got[i].Offset, want[i].Offset)
		}
	}
}

// recordsIn returns every record the segment beginning at base still holds.
func recordsIn(l *Log, base Offset) ([]Record, error) {
	end := l.Next()
	if next, ok := l.baseAfter(base); ok {
		end = next
	}

	r, err := l.Reader(base)
	if err != nil {
		return nil, err
	}

	var out []Record
	for {
		rec, err := r.Next()
		if errors.Is(err, ErrEndOfLog) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		if rec.Offset >= end {
			return out, nil
		}
		out = append(out, rec)
	}
}

// A reader with nothing left to read must say so rather than crash.
//
// A crash can truncate a segment's unsynced tail while the segment created after
// it survives empty, because creating a segment syncs the directory and writing
// to it does not. That leaves a hole running from the truncation point to the
// tail. Reader.Next asks seek for the next surviving record, seek correctly
// finds none, and Next then dereferenced the cursor's segment, which seek had
// set to nil. A panic rather than an error, found by the sweep once os mode was
// in the rotation: seeds/0290.md.
func TestAReaderInAHoleRunningToTheTailStops(t *testing.T) {
	fs := sim.NewFS()
	cfg := Config{Segment: Options{MaxBytes: 512, IndexInterval: 64}, Durability: OS}

	l, _, err := Open(fs, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// Enough to roll, so a later segment exists, and synced early so the
	// crash leaves the first segment holding records and the last holding
	// none.
	appendN(t, l, 0, 8)
	if err := l.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if err := fs.Sync(); err != nil {
		t.Fatalf("FS.Sync: %v", err)
	}
	appendN(t, l, 8, 120)
	if len(l.Segments()) < 2 {
		t.Fatalf("the log did not roll: %d segment", len(l.Segments()))
	}
	fs.Crash()

	reopened, _, err := Open(fs, cfg)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()

	// The hole is everything between what survived and the tail.
	r, err := reopened.Reader(reopened.First())
	if err != nil {
		t.Fatalf("Reader: %v", err)
	}
	for {
		_, err := r.Next()
		if errors.Is(err, ErrEndOfLog) {
			break
		}
		if err != nil {
			t.Fatalf("Next at %d: %v", r.Offset(), err)
		}
	}

	// And seeking directly into the hole, which is where a consumer holding
	// an offset from before the crash lands.
	if reopened.Next() > reopened.First() {
		if err := r.Seek(reopened.Next() - 1); err != nil {
			t.Fatalf("Seek into the hole: %v", err)
		}
		for {
			_, err := r.Next()
			if errors.Is(err, ErrEndOfLog) {
				return
			}
			if err != nil {
				t.Fatalf("Next after seeking into the hole: %v", err)
			}
		}
	}
}

// A reader outlives the segments underneath it.
//
// Compaction writes a replacement, renames it into place, and closes the handle
// it replaced. A cursor sitting in that segment was then holding a closed file,
// and its byte position pointed into a layout that no longer existed. Both are
// use-after-compaction, and no seed reached them for four milestones because
// every workload read one record at a time and let go of the cursor in between.
//
// Found by the restart workload, which keeps a cursor across compactions on
// purpose: seeds/0012.md.
func TestAReaderSurvivesCompactionUnderneathIt(t *testing.T) {
	fs := sim.NewFS()
	l, _, err := Open(fs, Config{Segment: Options{MaxBytes: 512, IndexInterval: 64}})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer l.Close()

	fillKeyed(t, l, 300, 4)

	r, err := l.Reader(l.First())
	if err != nil {
		t.Fatalf("Reader: %v", err)
	}

	// Part way through, so the cursor is inside a segment compaction is
	// about to replace.
	var last Offset
	for range 20 {
		rec, err := r.Next()
		if err != nil {
			t.Fatalf("Next before compaction: %v", err)
		}
		last = rec.Offset
	}

	if _, err := l.CompactAll(); err != nil {
		t.Fatalf("CompactAll: %v", err)
	}

	// The cursor keeps working, keeps ascending, and never hands back a
	// record it already delivered.
	for {
		rec, err := r.Next()
		if errors.Is(err, ErrEndOfLog) {
			break
		}
		if err != nil {
			t.Fatalf("Next after compaction: %v", err)
		}
		if rec.Offset <= last {
			t.Fatalf("the reader delivered offset %d after %d", rec.Offset, last)
		}
		last = rec.Offset
	}
	if last != l.Next()-1 {
		t.Fatalf("the scan ended at %d, want the tail at %d", last, l.Next()-1)
	}
}

// A reader whose remaining offsets were all compacted away has nothing left to
// read either.
func TestAReaderPastTheLastSurvivingRecordStops(t *testing.T) {
	fs := sim.NewFS()
	l, _, err := Open(fs, Config{Segment: Options{MaxBytes: 512, IndexInterval: 64}})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer l.Close()

	// One key, so compaction keeps exactly one record per segment and the
	// offsets after it are all gaps.
	for i := range 200 {
		if _, err := l.Append(keyed("one", i)); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
	if _, err := l.CompactAll(); err != nil {
		t.Fatalf("CompactAll: %v", err)
	}

	r, err := l.Reader(l.First())
	if err != nil {
		t.Fatalf("Reader: %v", err)
	}
	for {
		_, err := r.Next()
		if errors.Is(err, ErrEndOfLog) {
			break
		}
		if err != nil {
			t.Fatalf("Next at %d: %v", r.Offset(), err)
		}
	}

	// And again from an offset inside the gap that runs to the tail.
	if err := r.Seek(l.Next() - 1); err != nil {
		t.Fatalf("Seek into the trailing gap: %v", err)
	}
	for {
		_, err := r.Next()
		if errors.Is(err, ErrEndOfLog) {
			return
		}
		if err != nil {
			t.Fatalf("Next after seeking into the gap: %v", err)
		}
	}
}
