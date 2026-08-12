package log

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sort"

	"github.com/DamoDCoder/event-spine/internal/core"
)

// CommitsFile holds every consumer group's committed offsets.
//
// It lives beside the segments and carries a name no segment can have, so a
// directory listing cannot mistake one for the other. It is a file of framed
// records like any segment, which is what gives it checksums and torn-tail
// recovery without a second implementation of either.
const CommitsFile = "commits.groups"

// commitSchema is the schema version of a commit record. It is separate from
// the schema of whatever the log carries, because a commit is this package's
// own record rather than a caller's event.
const commitSchema = 1

// commitPayloadLen is the encoded size of a committed offset.
const commitPayloadLen = 8

// ErrNoGroup is returned for a group that has never committed.
var ErrNoGroup = errors.New("log: group has not committed")

// Commit is one entry in the commits log.
//
// The history is kept rather than overwritten, because "when did this consumer
// fall behind?" is a question about the sequence of commits and not about the
// last one. docs/log-design.md asks for exactly that.
type Commit struct {
	// Seq is the commit's position in the commits log, which orders
	// commits across every group.
	Seq Offset

	// Group is the consumer group that committed.
	Group string

	// Offset is what it committed: the offset it will resume from.
	Offset Offset

	// Time is the injected clock's reading when the commit was made, or
	// zero when no clock was injected. It is logical time, never wall
	// time, so a replayed commit history is the same history.
	Time core.Time
}

// commits is the commits log and the state replayed from it.
type commits struct {
	seg *Segment

	// at is the latest committed offset per group. It is never ranged
	// without sorting: a map walk that reached output would make the order
	// of two groups depend on hash iteration.
	at map[string]Offset
}

// Group is a named cursor position that advances only when a consumer says so.
//
// Committing is explicit and separate from reading. That one difference is what
// makes delivery at-least-once rather than at-most-once: a consumer that
// crashes between reading a record and committing it sees that record again,
// and a projection that is idempotent does not care.
type Group struct {
	log  *Log
	name string
}

// Group returns a handle for the named consumer group, creating nothing on
// disk. A group exists once it commits.
func (l *Log) Group(name string) (*Group, error) {
	if name == "" {
		return nil, fmt.Errorf("log: a consumer group needs a name")
	}
	if len(name) > core.MaxKeyLen {
		return nil, fmt.Errorf("log: group name is %d bytes, limit is %d", len(name), core.MaxKeyLen)
	}
	if err := l.openCommits(); err != nil {
		return nil, err
	}
	return &Group{log: l, name: name}, nil
}

// Name returns the group's name.
func (g *Group) Name() string { return g.name }

// Commit records that the group has durably processed everything before off,
// and makes that record durable before returning.
//
// The commit is synced whatever the log's durability mode says. A commit that
// is not on disk is a promise to redeliver that the next crash breaks in the
// wrong direction: the consumer would resume past records it never processed.
func (g *Group) Commit(off Offset) error {
	l := g.log
	if off < l.First() || off > l.Next() {
		return fmt.Errorf("%w: cannot commit %d, the log holds [%d, %d]",
			ErrNotFound, off, l.First(), l.Next())
	}
	if err := l.openCommits(); err != nil {
		return err
	}

	var payload [commitPayloadLen]byte
	binary.LittleEndian.PutUint64(payload[:], uint64(off))

	e := core.Event{
		Key:     g.name,
		Schema:  commitSchema,
		Payload: payload[:],
	}
	if l.cfg.Clock != nil {
		e.Time = l.cfg.Clock.Now()
	}

	if _, err := l.commits.seg.Append(e); err != nil {
		return fmt.Errorf("log: commit %s at %d: %w", g.name, off, err)
	}
	if err := l.commits.seg.Sync(); err != nil {
		return fmt.Errorf("log: commit %s at %d: %w", g.name, off, err)
	}

	// In memory only after it is on disk, so a caller that reads back its
	// own commit is reading something that survived.
	l.commits.at[g.name] = off
	return nil
}

// Committed returns the group's committed offset.
//
// A group that has never committed returns the log's first offset with
// ErrNoGroup: a new consumer starts at the beginning, and the error says the
// position is a default rather than a decision anyone made.
func (g *Group) Committed() (Offset, error) {
	if err := g.log.openCommits(); err != nil {
		return 0, err
	}
	off, ok := g.log.commits.at[g.name]
	if !ok {
		return g.log.First(), fmt.Errorf("%w: %s", ErrNoGroup, g.name)
	}
	return off, nil
}

// Reader returns a cursor positioned where the group resumes.
//
// A consumer that crashed after reading and before committing gets the records
// it did not commit a second time. That is the at-least-once contract, made
// concrete rather than described.
func (g *Group) Reader() (*Reader, error) {
	off, err := g.Committed()
	if err != nil && !errors.Is(err, ErrNoGroup) {
		return nil, err
	}
	return g.log.Reader(off)
}

// Groups returns the names of every group that has committed, sorted.
func (l *Log) Groups() ([]string, error) {
	if err := l.openCommits(); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(l.commits.at))
	for name := range l.commits.at {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// CommitHistory returns every commit in the order it was made.
//
// It re-reads the commits log rather than keeping the history in memory. The
// history is for answering questions after the fact, and holding every commit
// a long-running log ever made would be paying for that answer continuously.
func (l *Log) CommitHistory() ([]Commit, error) {
	if err := l.openCommits(); err != nil {
		return nil, err
	}

	seg := l.commits.seg
	history := make([]Commit, 0, seg.Next())
	for seq := Offset(0); seq < seg.Next(); seq++ {
		rec, err := seg.Read(seq)
		if err != nil {
			return nil, fmt.Errorf("log: read commit %d: %w", seq, err)
		}
		off, err := decodeCommit(rec.Event)
		if err != nil {
			return nil, fmt.Errorf("log: commit %d: %w", seq, err)
		}
		history = append(history, Commit{
			Seq:    seq,
			Group:  rec.Event.Key,
			Offset: off,
			Time:   rec.Event.Time,
		})
	}
	return history, nil
}

// openCommits opens the commits log, creating it if this is the first commit,
// and replays it into memory.
//
// It is lazy because a log with no consumers should not create a file to say
// so, and idempotent because every entry point calls it.
func (l *Log) openCommits() error {
	if l.commits != nil {
		return nil
	}

	// Commits are small and the log is never rolled, so the segment size
	// that matters for events does not apply. Compaction of this file is
	// deferred with the rest of compaction; until then the history is the
	// feature, and it grows by one record per commit.
	opts := Options{MaxBytes: 1 << 62, IndexInterval: l.cfg.Segment.IndexInterval}

	seg, _, err := openNamed(l.fs, CommitsFile, 0, opts, true)
	if errors.Is(err, core.ErrNotExist) {
		seg, err = createNamed(l.fs, CommitsFile, 0, opts)
		if err == nil {
			// The directory entry, for the same reason a segment
			// needs one: a commits log that a crash can remove
			// entirely would silently reset every consumer group to
			// the beginning. See seed 0001.
			err = l.fs.Sync()
		}
	}
	if err != nil {
		return fmt.Errorf("log: open %s: %w", CommitsFile, err)
	}

	c := &commits{seg: seg, at: map[string]Offset{}}
	if err := c.replay(); err != nil {
		seg.Close()
		return err
	}
	l.commits = c
	return nil
}

// replay walks the commits log and keeps the last commit per group.
func (c *commits) replay() error {
	for seq := Offset(0); seq < c.seg.Next(); seq++ {
		rec, err := c.seg.Read(seq)
		if err != nil {
			return fmt.Errorf("log: replay commit %d: %w", seq, err)
		}
		off, err := decodeCommit(rec.Event)
		if err != nil {
			return fmt.Errorf("log: replay commit %d: %w", seq, err)
		}
		c.at[rec.Event.Key] = off
	}
	return nil
}

// decodeCommit reads the committed offset out of a commit record.
//
// The record came off a disk, so its shape is checked rather than assumed. A
// payload of the wrong length is corruption the checksum did not catch, which
// means it was written by something that was not this code.
func decodeCommit(e core.Event) (Offset, error) {
	if e.Schema != commitSchema {
		return 0, fmt.Errorf("%w: commit schema is %d, want %d", ErrCorrupt, e.Schema, commitSchema)
	}
	if len(e.Payload) != commitPayloadLen {
		return 0, fmt.Errorf("%w: commit payload is %d bytes, want %d", ErrCorrupt, len(e.Payload), commitPayloadLen)
	}
	return Offset(binary.LittleEndian.Uint64(e.Payload)), nil
}
