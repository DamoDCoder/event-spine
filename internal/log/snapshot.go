package log

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/DamoDCoder/event-spine/internal/core"
)

// Snapshot file naming. A snapshot is named for the offset it was folded to, so
// a directory listing sorts them and the newest is the last.
const (
	snapshotPrefix = "snapshot-"
	snapshotSuffix = ".state"
)

// ErrNoSnapshot is returned when nothing has been snapshotted yet.
var ErrNoSnapshot = errors.New("log: no snapshot")

// Snapshot is a serialized projection and the offset it was folded to.
//
// Restoring means loading the state and replaying from Offset forward, so the
// offset is part of the snapshot rather than something the caller remembers
// separately. A snapshot whose offset was lost is a blob nobody can safely use.
type Snapshot struct {
	// Offset is the first offset NOT folded into State: replay resumes
	// here. It is the same convention as a group's committed offset, so
	// the two numbers can be compared without a conversion nobody would
	// remember to make.
	Offset Offset

	// State is whatever the caller serialized. This package never
	// interprets it.
	State []byte
}

// snapshotName returns the file name for a snapshot at off.
func snapshotName(off Offset) string {
	return fmt.Sprintf("%s%0*d%s", snapshotPrefix, segmentDigits, uint64(off), snapshotSuffix)
}

// parseSnapshotName returns the offset a snapshot file is named for.
//
// Names come from a directory listing, so this rejects rather than guesses: a
// file someone dropped in the log directory must not become an offset.
func parseSnapshotName(name string) (Offset, bool) {
	digits, ok := strings.CutPrefix(name, snapshotPrefix)
	if !ok {
		return 0, false
	}
	digits, ok = strings.CutSuffix(digits, snapshotSuffix)
	if !ok || len(digits) != segmentDigits {
		return 0, false
	}
	off, err := strconv.ParseUint(digits, 10, 64)
	if err != nil {
		return 0, false
	}
	return Offset(off), true
}

// Snapshot writes a projection's state and the offset it was folded to.
//
// The state is framed as one record, so it carries a CRC and a torn write is
// detectable: a snapshot that half reached the disk must not be mistaken for a
// projection. It is written under a temporary name and renamed into place, so a
// crash leaves either the previous snapshot or this one and never a mixture.
//
// Older snapshots are removed once the new one is durable. A crash between the
// rename and the removals leaves extras behind, which is why loading picks the
// highest offset rather than assuming there is exactly one.
func (l *Log) Snapshot(off Offset, state []byte) error {
	if off < l.First() || off > l.Next() {
		return fmt.Errorf("%w: cannot snapshot at %d, the log holds [%d, %d]",
			ErrNotFound, off, l.First(), l.Next())
	}

	name := snapshotName(off)
	tmp := name + CompactingSuffix
	if err := l.removeStale(tmp); err != nil {
		return err
	}

	seg, err := createNamed(l.fs, tmp, off, l.cfg.Segment)
	if err != nil {
		return err
	}
	// The state is a payload and the offset is the record's offset, which
	// is why the empty key costs nothing: the snapshot's identity is the
	// number in its header, not a name beside it.
	err = seg.appendAt(off, core.Event{Schema: commitSchema, Payload: state})
	if err == nil {
		err = seg.Sync()
	}
	seg.Close()
	if err != nil {
		l.fs.Remove(tmp)
		return fmt.Errorf("log: write the snapshot at %d: %w", off, err)
	}

	if err := l.fs.Rename(tmp, name); err != nil {
		l.fs.Remove(tmp)
		return fmt.Errorf("log: swap in the snapshot at %d: %w", off, err)
	}
	if err := l.fs.Sync(); err != nil {
		return fmt.Errorf("log: sync the directory after snapshotting at %d: %w", off, err)
	}

	return l.removeOlderSnapshots(off)
}

// LatestSnapshot returns the newest snapshot, or ErrNoSnapshot.
func (l *Log) LatestSnapshot() (Snapshot, error) {
	offsets, err := l.snapshotOffsets()
	if err != nil {
		return Snapshot{}, err
	}
	if len(offsets) == 0 {
		return Snapshot{}, ErrNoSnapshot
	}

	// Highest offset wins. More than one can exist after a crash between
	// the rename and the cleanup, and the newest is the one that finished.
	latest := offsets[len(offsets)-1]
	name := snapshotName(latest)

	seg, rec, err := openNamed(l.fs, name, latest, l.cfg.Segment, false)
	if err != nil {
		return Snapshot{}, fmt.Errorf("log: open the snapshot at %d: %w", latest, err)
	}
	defer seg.Close()

	switch {
	case rec.Corrupt != nil:
		return Snapshot{}, fmt.Errorf("log: the snapshot at %d is damaged: %w", latest, rec.Corrupt)
	case rec.Torn || rec.Records != 1:
		return Snapshot{}, fmt.Errorf("%w: the snapshot at %d holds %d records and %d torn bytes",
			ErrCorrupt, latest, rec.Records, rec.Discarded)
	}

	record, err := seg.Read(latest)
	if err != nil {
		return Snapshot{}, fmt.Errorf("log: read the snapshot at %d: %w", latest, err)
	}
	return Snapshot{Offset: latest, State: record.Event.Payload}, nil
}

// snapshotOffsets returns every snapshot offset on disk, ascending.
func (l *Log) snapshotOffsets() ([]Offset, error) {
	names, err := l.fs.List()
	if err != nil {
		return nil, fmt.Errorf("log: list snapshots: %w", err)
	}

	// The listing is sorted and the names are zero-padded, so offsets come
	// out ascending without a second sort.
	var offsets []Offset
	for _, name := range names {
		if off, ok := parseSnapshotName(name); ok {
			offsets = append(offsets, off)
		}
	}
	return offsets, nil
}

// removeOlderSnapshots deletes every snapshot below keep.
//
// A snapshot exists to shorten a replay, and the second newest shortens nothing
// the newest does not. Keeping a history of them would be keeping the cost the
// snapshot was taken to avoid.
func (l *Log) removeOlderSnapshots(keep Offset) error {
	offsets, err := l.snapshotOffsets()
	if err != nil {
		return err
	}
	for _, off := range offsets {
		if off >= keep {
			continue
		}
		if err := l.fs.Remove(snapshotName(off)); err != nil && !errors.Is(err, core.ErrNotExist) {
			return fmt.Errorf("log: remove the snapshot at %d: %w", off, err)
		}
	}
	return nil
}
