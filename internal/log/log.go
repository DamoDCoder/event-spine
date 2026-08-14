package log

import (
	"fmt"
	"sort"

	"github.com/DamoDCoder/event-spine/internal/core"
)

// Durability is when the log asks the filesystem to make writes durable, from
// the table in docs/log-design.md.
//
// The zero value is Batch, which is the documented default. A caller that
// forgets to choose gets the mode the design recommends rather than the mode
// that loses the most.
type Durability int

const (
	// Batch syncs every SyncRecords appends, or every SyncInterval of
	// logical time, whichever comes first. A crash loses at most that much.
	Batch Durability = iota

	// Sync syncs every record. Nothing acknowledged is lost on a crash,
	// and every append pays for it.
	Sync

	// OS never syncs. The page cache decides, so a crash can lose anything
	// not yet written back. This exists to measure the throughput ceiling
	// and is not a durability mode anything should ship on.
	OS
)

func (d Durability) String() string {
	switch d {
	case Batch:
		return "batch"
	case Sync:
		return "sync"
	case OS:
		return "os"
	default:
		return fmt.Sprintf("Durability(%d)", int(d))
	}
}

// DefaultSyncRecords is how many appends batch mode allows between syncs when
// the caller names neither a record count nor an interval.
const DefaultSyncRecords = 1024

// Config configures a log.
type Config struct {
	// Segment configures each segment file.
	Segment Options

	// Durability selects when appends are made durable.
	Durability Durability

	// SyncRecords is the batch-mode record count between syncs. Ignored
	// unless Durability is Batch.
	SyncRecords int

	// SyncInterval is the batch-mode logical span between syncs. It
	// requires Clock, because a duration with no clock to measure it
	// against is a silent no-op. Ignored unless Durability is Batch.
	SyncInterval core.Duration

	// Clock is only consulted for SyncInterval, and only at append time.
	// The log never starts a timer and never runs a goroutine: a sync
	// deadline is checked by the appending caller, so it lands at a point
	// the simulation controls rather than whenever a runtime timer fires.
	Clock core.Clock
}

func (c Config) withDefaults() Config {
	c.Segment = c.Segment.withDefaults()
	if c.SyncRecords <= 0 && c.SyncInterval <= 0 {
		c.SyncRecords = DefaultSyncRecords
	}
	return c
}

// Log is a sequence of segments over one directory in the injected filesystem.
//
// Exactly one segment is active. Every other segment is sealed, immutable, and
// opened only when a read needs it, so opening a log costs one segment scan
// rather than one per segment on disk.
type Log struct {
	fs  core.FS
	cfg Config

	// bases holds every segment's base offset in ascending order. It is the
	// list a read binary searches, and it is kept sorted rather than
	// derived from a map, because ranging a map to find a segment would
	// make lookup order depend on hash iteration.
	bases []Offset

	// sealed caches segments that have been opened for reading. The map is
	// never ranged: bases is what supplies order.
	sealed map[Offset]*Segment

	active    *Segment
	sinceSync int
	lastSync  core.Time

	// commits is the consumer groups' offsets log, opened on the first
	// mention of a group. A log nobody consumes creates no file to say so.
	commits *commits
}

// Open opens or creates a log in the directory the filesystem is rooted at.
//
// It returns the recovery result for the active segment. An empty directory
// gives a zero Recovery and a fresh segment at offset 0.
//
// Recovery is reported rather than acted on. A torn tail is truncated, because
// that is the normal result of a crash mid-append, but a corrupt one is handed
// back with the log still usable so the caller can decide whether a disk that
// returned wrong bytes is one to keep writing to.
func Open(fs core.FS, cfg Config) (*Log, Recovery, error) {
	cfg = cfg.withDefaults()
	if cfg.Durability == Batch && cfg.SyncInterval > 0 && cfg.Clock == nil {
		return nil, Recovery{}, fmt.Errorf("log: SyncInterval is %d but no Clock was injected", cfg.SyncInterval)
	}

	names, err := fs.List()
	if err != nil {
		return nil, Recovery{}, fmt.Errorf("log: list segments: %w", err)
	}

	// A directory listing is untrusted. Anything that is not a segment name
	// is ignored rather than parsed: a stray file must not become an
	// offset, and refusing to open the log because someone left a note in
	// the directory would be worse.
	var bases []Offset
	for _, name := range names {
		if base, ok := ParseSegmentName(name); ok {
			bases = append(bases, base)
		}
	}
	sort.Slice(bases, func(i, j int) bool { return bases[i] < bases[j] })

	l := &Log{fs: fs, cfg: cfg, bases: bases, sealed: map[Offset]*Segment{}}

	if len(bases) == 0 {
		active, err := l.createSegment(0)
		if err != nil {
			return nil, Recovery{}, err
		}
		l.active = active
		l.bases = []Offset{0}
		return l, Recovery{}, nil
	}

	// The highest-numbered segment is the one a crash could have caught
	// mid-append, so it is the only one scanned here.
	activeBase := bases[len(bases)-1]
	active, rec, err := OpenSegment(fs, SegmentName(activeBase), cfg.Segment)
	if err != nil {
		return nil, Recovery{}, err
	}
	l.active = active
	if cfg.Clock != nil {
		l.lastSync = cfg.Clock.Now()
	}
	return l, rec, nil
}

// Append writes events in order and returns the offsets they were assigned.
//
// The events are appended one record at a time and the durability decision is
// made once, at the end, so a batch of a thousand records in Sync mode costs
// one fsync rather than a thousand. That is the only sense in which a batch is
// a unit: it is not atomic, and a crash can leave a prefix of it on disk.
//
// On error the offsets assigned before the failure are returned along with it.
// Records already written are durable-eligible and pretending otherwise would
// hide them from a caller trying to work out what survived.
func (l *Log) Append(events ...core.Event) ([]Offset, error) {
	if len(events) == 0 {
		return nil, nil
	}

	offsets := make([]Offset, 0, len(events))
	for rest := events; len(rest) > 0; {
		if l.active.Full() {
			if err := l.roll(); err != nil {
				return offsets, err
			}
		}

		// One write per segment touched, rather than one per record. A
		// batch that crosses a roll therefore costs two writes, which
		// is the floor: the second segment is a different file.
		n, first, err := l.active.AppendAll(rest)
		for i := range n {
			offsets = append(offsets, first+Offset(i))
		}
		l.sinceSync += n
		if err != nil {
			return offsets, err
		}
		if n == 0 {
			// Unreachable: the segment was not full on entry, so it
			// had room for at least one record. Returned rather than
			// looped on, because a silent infinite loop is the worse
			// way to find out this reasoning was wrong.
			return offsets, fmt.Errorf("log: segment %s accepted no records and reported no error", l.active.Name())
		}
		rest = rest[n:]
	}

	if err := l.maybeSync(); err != nil {
		return offsets, err
	}
	return offsets, nil
}

// createSegment creates a segment and makes its name durable.
//
// The directory sync is the point. Syncing a file makes its bytes durable and
// says nothing about the entry that names it: a file whose directory entry was
// never synced does not exist after a power cut, and every acknowledged record
// in it goes with the entry. Seed 0001 is that bug — a crash removed a whole
// segment holding records whose Sync had returned, and recovery reported a
// healthy empty log rather than a loss.
//
// It runs in every durability mode, including os. Creating a segment is a
// structural operation rather than a durability choice: without it a crash can
// leave a hole in the middle of the segment list instead of a torn tail at the
// end, and recovery is built to expect the second. The cost is one fsync per
// segment, which is once per 64 MiB by default.
func (l *Log) createSegment(base Offset) (*Segment, error) {
	seg, err := CreateSegment(l.fs, base, l.cfg.Segment)
	if err != nil {
		return nil, err
	}
	if err := l.fs.Sync(); err != nil {
		seg.Close()
		return nil, fmt.Errorf("log: sync the directory after creating %s: %w", seg.Name(), err)
	}
	return seg, nil
}

// roll seals the active segment and starts a new one at the next offset.
//
// The outgoing segment is synced first, in every durability mode including os.
// "Sealed" has to mean "durable", because recovery leans on it twice: it scans
// only the active segment, and it refuses a sealed segment with a damaged tail
// outright rather than truncating one.
//
// Two power cuts were needed to arrive at that sentence. os mode was exempt at
// first, on the reasoning that a mode asking for no syncs gains nothing from
// one, and a cut opened a hole in the middle of a log. The fix after that had
// os mode record the debt for the next Sync to pay, which passed twenty cuts
// and then failed: a machine that stops between the roll and that Sync leaves a
// sealed segment holding acknowledged records and an unsynced tail, and
// refusing the file takes the acknowledged prefix with it.
//
// So the invariant is restored rather than tracked. os mode still never syncs
// on its own between rolls; it pays one fsync per segment, which is the same
// bargain the directory sync already made for the same reason.
func (l *Log) roll() error {
	if err := l.active.Sync(); err != nil {
		return err
	}
	l.active.Seal()

	base := l.active.Next()
	next, err := l.createSegment(base)
	if err != nil {
		return err
	}

	// The sealed segment stays open, so a reader that was about to look at
	// it does not pay to reopen and rescan a file already in hand.
	l.sealed[l.active.Base()] = l.active
	l.active = next
	l.bases = append(l.bases, base)
	l.sinceSync = 0
	if l.cfg.Clock != nil {
		l.lastSync = l.cfg.Clock.Now()
	}
	return nil
}

// maybeSync applies the durability mode.
func (l *Log) maybeSync() error {
	switch l.cfg.Durability {
	case Sync:
		return l.Sync()
	case OS:
		return nil
	}

	if l.cfg.SyncRecords > 0 && l.sinceSync >= l.cfg.SyncRecords {
		return l.Sync()
	}
	if l.cfg.SyncInterval > 0 {
		if now := l.cfg.Clock.Now(); now-l.lastSync >= core.Time(l.cfg.SyncInterval) {
			return l.Sync()
		}
	}
	return nil
}

// Sync makes everything appended so far durable and resets the batch counters.
//
// Only the active segment needs it: every sealed segment was synced by the roll
// that sealed it, in every mode.
func (l *Log) Sync() error {
	if err := l.active.Sync(); err != nil {
		return err
	}
	l.sinceSync = 0
	if l.cfg.Clock != nil {
		l.lastSync = l.cfg.Clock.Now()
	}
	return nil
}

// Read returns the record at off, opening the segment that holds it if it is
// not already open.
func (l *Log) Read(off Offset) (Record, error) {
	s, err := l.segmentFor(off)
	if err != nil {
		return Record{}, err
	}
	return s.Read(off)
}

// segmentFor returns the segment holding off.
func (l *Log) segmentFor(off Offset) (*Segment, error) {
	if off < l.First() || off >= l.Next() {
		return nil, fmt.Errorf("%w: %d is outside the log, which holds [%d, %d)",
			ErrNotFound, off, l.First(), l.Next())
	}

	// The greatest base at or below off. bases is sorted, so this is a
	// search rather than a walk, and the log can hold a lot of segments
	// before a lookup notices.
	i := sort.Search(len(l.bases), func(i int) bool { return l.bases[i] > off }) - 1
	base := l.bases[i]

	if base == l.active.Base() {
		return l.active, nil
	}
	if s, ok := l.sealed[base]; ok {
		return s, nil
	}

	// Opened read-only: a sealed segment that has grown a torn tail since
	// it was sealed has been damaged by something other than a crash
	// mid-append, and truncating it would turn that into silent data loss.
	s, err := OpenSealedSegment(l.fs, SegmentName(base), l.cfg.Segment)
	if err != nil {
		return nil, err
	}
	l.sealed[base] = s
	return s, nil
}

// baseAfter returns the base of the segment following the one that begins at
// base, which is where a hole running to the end of a segment stops.
func (l *Log) baseAfter(base Offset) (Offset, bool) {
	i := sort.Search(len(l.bases), func(i int) bool { return l.bases[i] > base })
	if i == len(l.bases) {
		return 0, false
	}
	return l.bases[i], true
}

// First returns the lowest offset the log still holds.
func (l *Log) First() Offset { return l.bases[0] }

// Next returns the offset the next append will be assigned.
func (l *Log) Next() Offset { return l.active.Next() }

// Segments returns the base offset of every segment, ascending. The slice is a
// copy: a caller that sorted or truncated the log's own list would break every
// subsequent lookup.
func (l *Log) Segments() []Offset {
	out := make([]Offset, len(l.bases))
	copy(out, l.bases)
	return out
}

// Close releases every open segment without syncing. A caller that wants
// durability calls Sync first, so that "close" never quietly becomes "block on
// the disk".
//
// Segments are closed in offset order, and the first error is returned after
// every file has been closed: returning early would leak the rest.
func (l *Log) Close() error {
	var first error
	if l.commits != nil {
		if err := l.commits.seg.Close(); err != nil {
			first = err
		}
		l.commits = nil
	}
	for _, base := range l.bases {
		s := l.sealed[base]
		if base == l.active.Base() {
			s = l.active
		}
		if s == nil {
			continue
		}
		if err := s.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}
