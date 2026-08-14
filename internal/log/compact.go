package log

import (
	"errors"
	"fmt"

	"github.com/DamoDCoder/event-spine/internal/core"
)

// CompactingSuffix marks the half-written segment a compaction is building.
//
// A crash leaves one of these behind. It is not a segment name, so recovery
// ignores it and the original file is still in place: the rename is what makes
// a compaction happen, and until it runs nothing has.
const CompactingSuffix = ".compacting"

// Compaction reports what compacting one segment removed.
type Compaction struct {
	// Base is the segment that was compacted.
	Base Offset

	// Kept and Dropped count records. Dropped records leave their offsets
	// behind as gaps, because offsets are never reassigned.
	Kept    int
	Dropped int

	// Before and After are the segment's size in bytes.
	Before int64
	After  int64
}

// Compact rewrites a sealed segment keeping only the newest record per key.
//
// Offsets are preserved, so compaction creates gaps rather than renumbering
// anything: a consumer's committed offset means the same thing after a
// compaction as before it, which is the property that makes compaction safe to
// run beside a live consumer.
//
// Only sealed segments are compacted. The active one is being appended to, and
// rewriting a file underneath a writer is a race that no amount of care in this
// function would fix.
//
// A Compact that returns an error may still have taken effect. The rename
// happens before the directory sync, so a failure reported by the sync is a
// failure to make a swap durable rather than a failure to make it. The caller
// cannot tell the two apart and must not assume the segment is unchanged — the
// harness found this with one injected fsync failure, seeds/1889.md. Nothing
// is lost either way: compaction only ever removes records a newer record for
// the same key supersedes.
//
// The scope is one segment. A key whose newest record lives in a later segment
// still keeps its older copy here, so this reclaims less than a whole-log
// compaction would. That is deliberate for now: a cross-segment pass has to read
// every later segment to know what is superseded, and doing it per segment first
// gets the crash-safety and the gaps — the parts with teeth — under test.
func (l *Log) Compact(base Offset) (Compaction, error) {
	if base == l.active.Base() {
		return Compaction{}, fmt.Errorf("log: cannot compact segment %d while it is the active one", base)
	}
	if _, ok := l.baseAfter(base); !ok {
		return Compaction{}, fmt.Errorf("%w: no segment begins at %d", ErrNotFound, base)
	}

	src, err := l.segmentFor(base)
	if err != nil {
		return Compaction{}, err
	}
	result := Compaction{Base: base, Before: src.Bytes()}

	survivors, err := survivorsOf(src)
	if err != nil {
		return Compaction{}, err
	}

	// Nothing to do is worth answering quickly and without touching the
	// disk: a compaction that rewrote a segment identically would spend a
	// rename to change nothing.
	if len(survivors) == src.records() {
		result.Kept = len(survivors)
		result.After = result.Before
		return result, nil
	}

	name := SegmentName(base)
	tmp := name + CompactingSuffix
	written, err := l.writeCompacted(tmp, base, src, survivors)
	if err != nil {
		// The original is untouched, so cleaning up the temporary file
		// is all that is owed. A failure to remove it is not worth
		// failing the compaction over: recovery ignores the name.
		l.fs.Remove(tmp)
		return Compaction{}, err
	}

	// The rename is the atomic swap. A crash before it leaves the original
	// segment and a stray temporary file; a crash after it leaves the
	// compacted segment. There is no third outcome, which is the whole
	// reason compaction is written this way.
	if err := l.fs.Rename(tmp, name); err != nil {
		l.fs.Remove(tmp)
		return Compaction{}, fmt.Errorf("log: swap in the compacted %s: %w", name, err)
	}
	if err := l.fs.Sync(); err != nil {
		return Compaction{}, fmt.Errorf("log: sync the directory after compacting %s: %w", name, err)
	}

	// The cached handle points at the file that was just replaced. Closing
	// and forgetting it means the next read opens the compacted one, with
	// an index built from what is actually there.
	if err := src.Close(); err != nil {
		return Compaction{}, err
	}
	delete(l.sealed, base)

	// Any reader sitting in that segment is now holding a closed handle and
	// a position into a file that has been replaced. Bumping the generation
	// is how they find out: the alternative is reference counting every
	// segment, which is a lot of machinery for an event that happens once
	// per compaction.
	l.generation++

	result.Kept = len(survivors)
	result.Dropped = src.records() - len(survivors)
	result.After = written
	return result, nil
}

// survivorsOf returns the offsets to keep: the last record for each key.
//
// The map holds offsets rather than records, so a segment full of large
// payloads costs one integer per key here rather than a second copy of itself.
func survivorsOf(src *Segment) (map[Offset]bool, error) {
	newest := map[string]Offset{}

	pos := int64(0)
	at := src.Base()
	for pos < src.Bytes() {
		rec, err := src.readRecordFrom(pos, at)
		if err != nil {
			return nil, fmt.Errorf("log: scan %s for compaction: %w", src.Name(), err)
		}
		newest[rec.Event.Key] = rec.Offset
		pos += int64(rec.Len)
		at = rec.Offset + 1
	}

	survivors := make(map[Offset]bool, len(newest))
	for _, off := range newest {
		survivors[off] = true
	}
	return survivors, nil
}

// writeCompacted writes the surviving records to tmp and returns its size.
//
// The records are copied in offset order, which a map cannot supply: the
// survivors are looked up by offset while walking the source in the order it is
// already in, so nothing here depends on how Go feels about iterating a map.
func (l *Log) writeCompacted(tmp string, base Offset, src *Segment, survivors map[Offset]bool) (int64, error) {
	if err := l.removeStale(tmp); err != nil {
		return 0, err
	}

	dst, err := createNamed(l.fs, tmp, base, l.cfg.Segment)
	if err != nil {
		return 0, err
	}
	defer dst.Close()

	pos := int64(0)
	at := src.Base()
	for pos < src.Bytes() {
		rec, err := src.readRecordFrom(pos, at)
		if err != nil {
			return 0, fmt.Errorf("log: read %s for compaction: %w", src.Name(), err)
		}
		pos += int64(rec.Len)
		at = rec.Offset + 1

		if !survivors[rec.Offset] {
			continue
		}
		if err := dst.appendAt(rec.Offset, rec.Event); err != nil {
			return 0, fmt.Errorf("log: write the compacted %s: %w", tmp, err)
		}
	}

	// Durable before the rename, or the swap could publish a file whose
	// contents never reached the disk.
	if err := dst.Sync(); err != nil {
		return 0, err
	}
	return dst.Bytes(), nil
}

// removeStale deletes a temporary file left by a compaction that crashed.
func (l *Log) removeStale(tmp string) error {
	err := l.fs.Remove(tmp)
	if err == nil || errors.Is(err, core.ErrNotExist) {
		return nil
	}
	return fmt.Errorf("log: remove the stale %s: %w", tmp, err)
}

// CompactAll compacts every sealed segment and returns what each one removed.
func (l *Log) CompactAll() ([]Compaction, error) {
	// The active segment's base is skipped, and the list is copied because
	// compaction mutates nothing in it but reading it while it changes is a
	// habit worth not forming.
	bases := l.Segments()
	active := l.active.Base()

	var out []Compaction
	for _, base := range bases {
		if base == active {
			continue
		}
		c, err := l.Compact(base)
		if err != nil {
			return out, err
		}
		out = append(out, c)
	}
	return out, nil
}
