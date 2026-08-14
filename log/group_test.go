package log

import (
	"errors"
	"testing"

	"github.com/DamoDCoder/event-spine/core"
	"github.com/DamoDCoder/event-spine/sim"
)

func TestAGroupCommitSurvivesAReopen(t *testing.T) {
	bothFilesystems(t, func(t *testing.T, fs core.FS) {
		l, _, err := Open(fs, tinySegments())
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		appendN(t, l, 0, 200)

		g, err := l.Group("projections")
		if err != nil {
			t.Fatalf("Group: %v", err)
		}
		if err := g.Commit(120); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		if err := l.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}

		reopened, _, err := Open(fs, tinySegments())
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		defer reopened.Close()

		g2, err := reopened.Group("projections")
		if err != nil {
			t.Fatalf("Group after reopen: %v", err)
		}
		off, err := g2.Committed()
		if err != nil {
			t.Fatalf("Committed: %v", err)
		}
		if off != 120 {
			t.Fatalf("the group resumed at %d, want 120", off)
		}
	})
}

// The at-least-once contract, made concrete. A consumer that reads records and
// crashes before committing sees them again.
func TestAGroupRedeliversWhatItDidNotCommit(t *testing.T) {
	fs := sim.NewFS()
	l, _, err := Open(fs, tinySegments())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	appendN(t, l, 0, 100)

	g, err := l.Group("ledger")
	if err != nil {
		t.Fatalf("Group: %v", err)
	}
	r, err := g.Reader()
	if err != nil {
		t.Fatalf("Reader: %v", err)
	}

	// Read thirty, commit ten. The twenty in between were processed and
	// never acknowledged, which is exactly the window redelivery covers.
	for i := range 30 {
		if _, err := r.Next(); err != nil {
			t.Fatalf("Next %d: %v", i, err)
		}
	}
	if err := g.Commit(10); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := l.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if err := fs.Sync(); err != nil {
		t.Fatalf("FS.Sync: %v", err)
	}
	fs.Crash()

	reopened, _, err := Open(fs, tinySegments())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()

	g2, err := reopened.Group("ledger")
	if err != nil {
		t.Fatalf("Group: %v", err)
	}
	r2, err := g2.Reader()
	if err != nil {
		t.Fatalf("Reader after crash: %v", err)
	}
	rec, err := r2.Next()
	if err != nil {
		t.Fatalf("Next after crash: %v", err)
	}
	if rec.Offset != 10 {
		t.Fatalf("the group resumed at %d, want the committed 10", rec.Offset)
	}
}

// A group that has never committed starts at the beginning, and says so rather
// than reporting a position someone chose.
func TestAnUncommittedGroupStartsAtTheBeginning(t *testing.T) {
	fs := sim.NewFS()
	l, _, err := Open(fs, tinySegments())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer l.Close()
	appendN(t, l, 0, 20)

	g, err := l.Group("fresh")
	if err != nil {
		t.Fatalf("Group: %v", err)
	}
	off, err := g.Committed()
	if !errors.Is(err, ErrNoGroup) {
		t.Fatalf("Committed on a new group returned %v, want ErrNoGroup", err)
	}
	if off != l.First() {
		t.Fatalf("a new group starts at %d, want %d", off, l.First())
	}

	r, err := g.Reader()
	if err != nil {
		t.Fatalf("Reader: %v", err)
	}
	rec, err := r.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if rec.Offset != l.First() {
		t.Fatalf("a new group's reader started at %d", rec.Offset)
	}
}

func TestGroupsAreIndependent(t *testing.T) {
	fs := sim.NewFS()
	l, _, err := Open(fs, tinySegments())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer l.Close()
	appendN(t, l, 0, 100)

	fast, err := l.Group("fast")
	if err != nil {
		t.Fatalf("Group: %v", err)
	}
	slow, err := l.Group("slow")
	if err != nil {
		t.Fatalf("Group: %v", err)
	}
	if err := fast.Commit(90); err != nil {
		t.Fatalf("Commit fast: %v", err)
	}
	if err := slow.Commit(12); err != nil {
		t.Fatalf("Commit slow: %v", err)
	}

	for _, tc := range []struct {
		g    *Group
		want Offset
	}{{fast, 90}, {slow, 12}} {
		off, err := tc.g.Committed()
		if err != nil {
			t.Fatalf("Committed %s: %v", tc.g.Name(), err)
		}
		if off != tc.want {
			t.Fatalf("group %s is at %d, want %d", tc.g.Name(), off, tc.want)
		}
	}

	names, err := l.Groups()
	if err != nil {
		t.Fatalf("Groups: %v", err)
	}
	if len(names) != 2 || names[0] != "fast" || names[1] != "slow" {
		t.Fatalf("Groups returned %v, want [fast slow] sorted", names)
	}
}

// The commit history is the reason commits are records rather than a value
// overwritten in place: it answers when a consumer fell behind.
func TestTheCommitHistoryIsReplayable(t *testing.T) {
	fs := sim.NewFS()
	clock := sim.NewClock()
	l, _, err := Open(fs, Config{Segment: Options{MaxBytes: 512}, Clock: clock, SyncRecords: 1 << 20})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	appendN(t, l, 0, 100)

	g, err := l.Group("ledger")
	if err != nil {
		t.Fatalf("Group: %v", err)
	}
	other, err := l.Group("audit")
	if err != nil {
		t.Fatalf("Group: %v", err)
	}

	want := []Commit{
		{Seq: 0, Group: "ledger", Offset: 10, Time: 0},
		{Seq: 1, Group: "audit", Offset: 5, Time: 100},
		{Seq: 2, Group: "ledger", Offset: 40, Time: 300},
	}
	if err := g.Commit(10); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	clock.Advance(100)
	if err := other.Commit(5); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	clock.Advance(200)
	if err := g.Commit(40); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The history is read back from disk on a fresh log, because a history
	// that only exists in the process that wrote it answers nothing after a
	// restart.
	reopened, _, err := Open(fs, Config{Segment: Options{MaxBytes: 512}, Clock: clock})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()

	got, err := reopened.CommitHistory()
	if err != nil {
		t.Fatalf("CommitHistory: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("history has %d commits, want %d: %+v", len(got), len(want), got)
	}
	for i, c := range got {
		if c != want[i] {
			t.Fatalf("commit %d is %+v, want %+v", i, c, want[i])
		}
	}
}

// A commit is durable when it returns, whatever the log's durability mode says.
// A commit that is not on disk is a promise to redeliver broken in the wrong
// direction.
func TestACommitSyncsEvenInOSMode(t *testing.T) {
	fs := &countingFS{FS: sim.NewFS()}
	l, _, err := Open(fs, Config{Segment: noRoll(), Durability: OS})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer l.Close()
	appendN(t, l, 0, 50)

	if fs.syncs != 0 {
		t.Fatalf("os mode synced %d times before any commit", fs.syncs)
	}
	g, err := l.Group("ledger")
	if err != nil {
		t.Fatalf("Group: %v", err)
	}
	if err := g.Commit(25); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if fs.syncs != 1 {
		t.Fatalf("a commit in os mode synced %d times, want 1", fs.syncs)
	}
}

func TestCommitRejectsOffsetsTheLogDoesNotHold(t *testing.T) {
	fs := sim.NewFS()
	l, _, err := Open(fs, tinySegments())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer l.Close()
	appendN(t, l, 0, 30)

	g, err := l.Group("ledger")
	if err != nil {
		t.Fatalf("Group: %v", err)
	}

	// The tail is committable: it means "everything so far".
	if err := g.Commit(l.Next()); err != nil {
		t.Fatalf("committing the tail: %v", err)
	}
	for _, off := range []Offset{l.Next() + 1, 1 << 40} {
		if err := g.Commit(off); !errors.Is(err, ErrNotFound) {
			t.Fatalf("committing %d gave %v, want ErrNotFound", off, err)
		}
	}

	if _, err := l.Group(""); err == nil {
		t.Fatal("a group with no name was accepted")
	}
}

// A crash during a commit leaves a torn record in the commits log. The commit
// never returned, so discarding it loses nothing anyone was told: the group
// resumes from its previous position and redelivers.
func TestATornCommitIsDiscardedAndTheRestSurvive(t *testing.T) {
	fs := sim.NewFS()
	l, _, err := Open(fs, tinySegments())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	appendN(t, l, 0, 100)

	g, err := l.Group("ledger")
	if err != nil {
		t.Fatalf("Group: %v", err)
	}
	if err := g.Commit(10); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := g.Commit(60); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Cut the last commit in half, which is what a crash between the write
	// and the sync leaves behind.
	f, err := fs.Open(CommitsFile)
	if err != nil {
		t.Fatalf("open the commits log: %v", err)
	}
	size, err := f.Size()
	if err != nil {
		t.Fatalf("size: %v", err)
	}
	if err := f.Truncate(size - 4); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	f.Close()

	reopened, _, err := Open(fs, tinySegments())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()

	g2, err := reopened.Group("ledger")
	if err != nil {
		t.Fatalf("Group: %v", err)
	}
	off, err := g2.Committed()
	if err != nil {
		t.Fatalf("Committed: %v", err)
	}
	if off != 10 {
		t.Fatalf("the group resumed at %d, want the 10 that was durable", off)
	}

	history, err := reopened.CommitHistory()
	if err != nil {
		t.Fatalf("CommitHistory: %v", err)
	}
	if len(history) != 1 || history[0].Offset != 10 {
		t.Fatalf("history is %+v, want only the durable commit", history)
	}

	// The commits log still works after recovery.
	if err := g2.Commit(75); err != nil {
		t.Fatalf("commit after recovery: %v", err)
	}
	if off, err := g2.Committed(); err != nil || off != 75 {
		t.Fatalf("after recovery the group is at %d (%v), want 75", off, err)
	}
}

// The commits log lives in the segment directory and must never be mistaken for
// a segment.
func TestTheCommitsFileIsNotASegment(t *testing.T) {
	if _, ok := ParseSegmentName(CommitsFile); ok {
		t.Fatalf("%q parses as a segment name", CommitsFile)
	}

	fs := sim.NewFS()
	l, _, err := Open(fs, tinySegments())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	appendN(t, l, 0, 200)
	g, err := l.Group("ledger")
	if err != nil {
		t.Fatalf("Group: %v", err)
	}
	if err := g.Commit(100); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	segments := len(l.Segments())
	next := l.Next()
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, _, err := Open(fs, tinySegments())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()

	if got := len(reopened.Segments()); got != segments {
		t.Fatalf("reopened with %d segments, want %d: the commits file was counted as one", got, segments)
	}
	if reopened.Next() != next {
		t.Fatalf("next offset after reopen is %d, want %d", reopened.Next(), next)
	}
}
