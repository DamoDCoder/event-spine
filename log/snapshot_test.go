package log

import (
	"bytes"
	"errors"
	"testing"

	"github.com/DamoDCoder/event-spine/core"
	"github.com/DamoDCoder/event-spine/sim"
)

func TestASnapshotRoundTrips(t *testing.T) {
	bothFilesystems(t, func(t *testing.T, fs core.FS) {
		l, _, err := Open(fs, tinySegments())
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer l.Close()

		appendN(t, l, 0, 200)
		state := bytes.Repeat([]byte("projection"), 100)
		if err := l.Snapshot(120, state); err != nil {
			t.Fatalf("Snapshot: %v", err)
		}

		got, err := l.LatestSnapshot()
		if err != nil {
			t.Fatalf("LatestSnapshot: %v", err)
		}
		if got.Offset != 120 {
			t.Fatalf("the snapshot is at offset %d, want 120", got.Offset)
		}
		if !bytes.Equal(got.State, state) {
			t.Fatalf("the snapshot state came back as %d bytes, want %d", len(got.State), len(state))
		}
	})
}

// Restoring is loading the state and replaying from the snapshot's offset. The
// offset travels with the state, because a blob whose offset was lost cannot be
// used safely.
func TestASnapshotResumesTheReplay(t *testing.T) {
	fs := sim.NewFS()
	l, _, err := Open(fs, tinySegments())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	appendN(t, l, 0, 300)
	if err := l.Snapshot(180, []byte("folded to 180")); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
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

	snap, err := reopened.LatestSnapshot()
	if err != nil {
		t.Fatalf("LatestSnapshot after reopen: %v", err)
	}
	r, err := reopened.Reader(snap.Offset)
	if err != nil {
		t.Fatalf("Reader: %v", err)
	}
	rec, err := r.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if rec.Offset != snap.Offset {
		t.Fatalf("the replay resumed at %d, want the snapshot's %d", rec.Offset, snap.Offset)
	}
}

// A snapshot exists to shorten a replay, and the second newest shortens nothing
// the newest does not.
func TestANewSnapshotReplacesTheOldOne(t *testing.T) {
	fs := sim.NewFS()
	l, _, err := Open(fs, tinySegments())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer l.Close()
	appendN(t, l, 0, 300)

	for _, off := range []Offset{50, 130, 240} {
		if err := l.Snapshot(off, []byte{byte(off)}); err != nil {
			t.Fatalf("Snapshot %d: %v", off, err)
		}
	}

	offsets, err := l.snapshotOffsets()
	if err != nil {
		t.Fatalf("snapshotOffsets: %v", err)
	}
	if len(offsets) != 1 || offsets[0] != 240 {
		t.Fatalf("the directory holds snapshots %v, want only the newest", offsets)
	}

	got, err := l.LatestSnapshot()
	if err != nil {
		t.Fatalf("LatestSnapshot: %v", err)
	}
	if got.Offset != 240 {
		t.Fatalf("the surviving snapshot is at %d, want 240", got.Offset)
	}
}

// A snapshot that half reached the disk must never be mistaken for a
// projection, which is why the state is framed as a record rather than written
// raw.
func TestATornSnapshotIsRefused(t *testing.T) {
	fs := sim.NewFS()
	l, _, err := Open(fs, tinySegments())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer l.Close()

	appendN(t, l, 0, 100)
	state := bytes.Repeat([]byte("x"), 500)
	if err := l.Snapshot(60, state); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	name := snapshotName(60)
	f, err := fs.Open(name)
	if err != nil {
		t.Fatalf("open the snapshot: %v", err)
	}
	size, err := f.Size()
	if err != nil {
		t.Fatalf("size: %v", err)
	}
	if err := f.Truncate(size - 100); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	f.Close()

	if _, err := l.LatestSnapshot(); err == nil {
		t.Fatal("a torn snapshot loaded without error")
	}

	// A flipped byte in the state is caught by the record's checksum.
	if err := l.Snapshot(70, state); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if err := flipByte(fs, snapshotName(70), HeaderLen+10); err != nil {
		t.Fatalf("flip: %v", err)
	}
	_, err = l.LatestSnapshot()
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("a corrupted snapshot gave %v, want ErrCorrupt", err)
	}
}

func TestSnapshotNamesAreNotSegments(t *testing.T) {
	name := snapshotName(1234)
	if _, ok := ParseSegmentName(name); ok {
		t.Fatalf("%q parses as a segment name", name)
	}
	off, ok := parseSnapshotName(name)
	if !ok || off != 1234 {
		t.Fatalf("%q parsed as %d, %v; want 1234", name, off, ok)
	}

	// The directory is untrusted, like every other listing this package
	// reads.
	for _, junk := range []string{
		"", "snapshot.state", "snapshot-.state", "snapshot-12.state",
		SegmentName(0), CommitsFile, "snapshot-0000000000000000000x.state",
	} {
		if _, ok := parseSnapshotName(junk); ok {
			t.Fatalf("%q was accepted as a snapshot name", junk)
		}
	}
}

// A log with snapshots and a commits file in its directory still finds exactly
// its segments.
func TestSnapshotsDoNotDisturbTheSegmentList(t *testing.T) {
	fs := sim.NewFS()
	l, _, err := Open(fs, tinySegments())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	appendN(t, l, 0, 200)
	if err := l.Snapshot(100, []byte("state")); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	g, err := l.Group("ledger")
	if err != nil {
		t.Fatalf("Group: %v", err)
	}
	if err := g.Commit(100); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	segments, tail := len(l.Segments()), l.Next()
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, _, err := Open(fs, tinySegments())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()

	if got := len(reopened.Segments()); got != segments {
		t.Fatalf("reopened with %d segments, want %d", got, segments)
	}
	if reopened.Next() != tail {
		t.Fatalf("the tail after reopen is %d, want %d", reopened.Next(), tail)
	}
}

func TestSnapshotRejectsOffsetsTheLogDoesNotHold(t *testing.T) {
	fs := sim.NewFS()
	l, _, err := Open(fs, tinySegments())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer l.Close()
	appendN(t, l, 0, 40)

	if err := l.Snapshot(l.Next(), []byte("everything")); err != nil {
		t.Fatalf("snapshotting the tail: %v", err)
	}
	if err := l.Snapshot(l.Next()+1, []byte("too far")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("snapshotting past the tail gave %v, want ErrNotFound", err)
	}

	if _, err := l.LatestSnapshot(); err != nil {
		t.Fatalf("the valid snapshot did not survive the rejected one: %v", err)
	}
}

func TestNoSnapshotIsReportedRatherThanGuessed(t *testing.T) {
	fs := sim.NewFS()
	l, _, err := Open(fs, tinySegments())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer l.Close()

	if _, err := l.LatestSnapshot(); !errors.Is(err, ErrNoSnapshot) {
		t.Fatalf("LatestSnapshot on a fresh log gave %v, want ErrNoSnapshot", err)
	}
}

// Restore is the documented recovery path, and the mistake it exists to prevent
// is replaying from the wrong offset.
func TestRestoreResumesExactlyWhereTheSnapshotStops(t *testing.T) {
	fs := sim.NewFS()
	l, _, err := Open(fs, tinySegments())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer l.Close()

	// No snapshot yet: replaying from nothing starts at the beginning.
	appendN(t, l, 0, 200)
	snap, r, err := l.Restore()
	if err != nil {
		t.Fatalf("Restore with no snapshot: %v", err)
	}
	if snap.Offset != l.First() || len(snap.State) != 0 {
		t.Fatalf("an empty restore reported %+v, want the first offset and no state", snap)
	}
	rec, err := r.Next()
	if err != nil || rec.Offset != l.First() {
		t.Fatalf("an empty restore replays from %d (%v), want %d", rec.Offset, err, l.First())
	}

	// With one, the reader starts at the first record the snapshot did not
	// fold in — not the one before it, and not the one after.
	state := []byte("folded to 120")
	if err := l.Snapshot(120, state); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	snap, r, err = l.Restore()
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if snap.Offset != 120 || !bytes.Equal(snap.State, state) {
		t.Fatalf("Restore returned %+v, want the snapshot at 120", snap)
	}
	rec, err = r.Next()
	if err != nil {
		t.Fatalf("Next after Restore: %v", err)
	}
	if rec.Offset != 120 {
		t.Fatalf("Restore replays from %d, want 120", rec.Offset)
	}
}
