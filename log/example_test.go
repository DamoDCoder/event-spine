package log_test

import (
	"errors"
	"fmt"

	"github.com/DamoDCoder/event-spine/core"
	"github.com/DamoDCoder/event-spine/log"
	"github.com/DamoDCoder/event-spine/sim"
)

// The shape a consumer writes: open, append, commit what has been processed,
// and resume from that commit after a restart.
//
// It runs against the simulated filesystem so it needs no disk. A real program
// swaps in runtime.NewFS and changes nothing else, which is the point of the
// filesystem being injected.
func Example() {
	fs := sim.NewFS()

	l, recovery, err := log.Open(fs, log.Config{})
	if err != nil {
		panic(err)
	}
	defer l.Close()

	// Recovery is a decision, not a verdict: a torn tail has been truncated
	// already, and corruption is reported for the caller to judge.
	if recovery.Corrupt != nil {
		fmt.Println("the disk returned bytes that were wrong:", recovery.Corrupt)
	}

	if _, err := l.Append(
		core.Event{Key: "acct-1", Schema: 1, Payload: []byte("opened")},
		core.Event{Key: "acct-1", Schema: 1, Payload: []byte("deposit 500")},
		core.Event{Key: "acct-2", Schema: 1, Payload: []byte("opened")},
	); err != nil {
		panic(err)
	}

	group, err := l.Group("projections")
	if err != nil {
		panic(err)
	}

	// A group's reader starts where the group last committed, which for a
	// group that has never committed is the beginning.
	reader, err := group.Reader()
	if err != nil {
		panic(err)
	}

	var processed log.Offset
	for {
		record, err := reader.Next()
		if errors.Is(err, log.ErrEndOfLog) {
			break
		}
		if err != nil {
			panic(err)
		}
		fmt.Printf("%d %s %s\n", record.Offset, record.Event.Key, record.Event.Payload)
		processed = record.Offset + 1
	}

	// Committing is explicit and separate from reading. Until this returns,
	// a crash redelivers everything above the last commit — which is what
	// makes delivery at-least-once and why a projection must be idempotent.
	if err := group.Commit(processed); err != nil {
		panic(err)
	}

	// Output:
	// 0 acct-1 opened
	// 1 acct-1 deposit 500
	// 2 acct-2 opened
}

// Restoring a projection: load the snapshot, replay what it did not fold in.
func ExampleLog_Restore() {
	fs := sim.NewFS()
	l, _, err := log.Open(fs, log.Config{})
	if err != nil {
		panic(err)
	}
	defer l.Close()

	for i := range 5 {
		if _, err := l.Append(core.Event{
			Key:     "counter",
			Schema:  1,
			Payload: fmt.Appendf(nil, "%d", i),
		}); err != nil {
			panic(err)
		}
	}

	// A projection folded up to offset 3, saved with the offset it reached.
	if err := l.Snapshot(3, []byte("total=3")); err != nil {
		panic(err)
	}

	snapshot, reader, err := l.Restore()
	if err != nil {
		panic(err)
	}
	fmt.Printf("state %q, replaying from %d\n", snapshot.State, snapshot.Offset)

	for {
		record, err := reader.Next()
		if errors.Is(err, log.ErrEndOfLog) {
			break
		}
		if err != nil {
			panic(err)
		}
		fmt.Printf("replay %d %s\n", record.Offset, record.Event.Payload)
	}

	// Output:
	// state "total=3", replaying from 3
	// replay 3 3
	// replay 4 4
}

// A crash loses everything that was not synced, and the log comes back holding
// what was. This is the shape of a consumer's own durability test.
func ExampleLog_Sync() {
	fs := sim.NewFS()
	l, _, err := log.Open(fs, log.Config{Durability: log.OS})
	if err != nil {
		panic(err)
	}

	if _, err := l.Append(core.Event{Key: "acct-1", Schema: 1, Payload: []byte("durable")}); err != nil {
		panic(err)
	}
	if err := l.Sync(); err != nil {
		panic(err)
	}
	if err := fs.Sync(); err != nil {
		panic(err)
	}

	// Appended after the sync, so the power cut takes it.
	if _, err := l.Append(core.Event{Key: "acct-1", Schema: 1, Payload: []byte("lost")}); err != nil {
		panic(err)
	}
	fs.Crash()
	l.Close()

	reopened, _, err := log.Open(fs, log.Config{Durability: log.OS})
	if err != nil {
		panic(err)
	}
	defer reopened.Close()

	fmt.Println("records that survived:", reopened.Next())

	// Output:
	// records that survived: 1
}
