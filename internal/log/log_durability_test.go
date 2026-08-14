package log

import (
	"testing"

	"github.com/DamoDCoder/event-spine/internal/core"
	"github.com/DamoDCoder/event-spine/internal/sim"
)

// countingFS counts syncs, which is the only externally visible difference
// between the three durability modes. Everything else about them is identical,
// so a test that did not count would pass under any of them.
type countingFS struct {
	core.FS
	syncs int

	// writes counts calls to File.Append, which is one write(2) each. It is
	// the number the batching in Log.Append exists to reduce.
	writes int
}

func (c *countingFS) Create(name string) (core.File, error) {
	f, err := c.FS.Create(name)
	if err != nil {
		return nil, err
	}
	return &countingFile{File: f, fs: c}, nil
}

func (c *countingFS) Open(name string) (core.File, error) {
	f, err := c.FS.Open(name)
	if err != nil {
		return nil, err
	}
	return &countingFile{File: f, fs: c}, nil
}

type countingFile struct {
	core.File
	fs *countingFS
}

func (f *countingFile) Sync() error {
	f.fs.syncs++
	return f.File.Sync()
}

func (f *countingFile) Append(p []byte) (int, error) {
	f.fs.writes++
	return f.File.Append(p)
}

// A segment large enough that nothing here rolls, since a roll syncs on its own
// and would be counted as if a durability mode had asked for it.
func noRoll() Options { return Options{MaxBytes: 1 << 20} }

func TestDurabilityModesSyncWhenTheySay(t *testing.T) {
	const n = 50

	cases := []struct {
		name string
		cfg  Config
		want int
	}{
		{
			name: "sync-every-record",
			cfg:  Config{Segment: noRoll(), Durability: Sync},
			want: n,
		},
		{
			name: "batch-every-ten",
			cfg:  Config{Segment: noRoll(), Durability: Batch, SyncRecords: 10},
			want: n / 10,
		},
		{
			name: "os-never",
			cfg:  Config{Segment: noRoll(), Durability: OS},
			want: 0,
		},
		{
			// The default, chosen by leaving the zero value alone.
			name: "default-is-batch-at-the-documented-size",
			cfg:  Config{Segment: noRoll()},
			want: 0, // n is well below DefaultSyncRecords
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := &countingFS{FS: sim.NewFS()}
			l, _, err := Open(fs, tc.cfg)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer l.Close()

			appendN(t, l, 0, n)

			if fs.syncs != tc.want {
				t.Fatalf("%d appends in %s mode synced %d times, want %d",
					n, tc.cfg.Durability, fs.syncs, tc.want)
			}
		})
	}
}

// The durability decision is made once per Append call, not once per record, so
// a batch of a hundred costs one fsync rather than ten. Stating it as a test
// because the alternative reading is just as plausible from the mode's name.
func TestBatchDurabilitySyncsOncePerAppendCall(t *testing.T) {
	fs := &countingFS{FS: sim.NewFS()}
	l, _, err := Open(fs, Config{Segment: noRoll(), Durability: Batch, SyncRecords: 10})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer l.Close()

	batch := make([]core.Event, 100)
	for i := range batch {
		batch[i] = event(i)
	}
	if _, err := l.Append(batch...); err != nil {
		t.Fatalf("Append: %v", err)
	}

	if fs.syncs != 1 {
		t.Fatalf("one batch of %d synced %d times, want 1", len(batch), fs.syncs)
	}
}

// The interval is measured against the injected clock, and only when a caller
// appends. Nothing here starts a timer, so a log that is idle never syncs on
// its own — which is what makes the sync points reproducible.
func TestBatchDurabilityHonoursTheInjectedClock(t *testing.T) {
	const interval = core.Duration(100)

	fs := &countingFS{FS: sim.NewFS()}
	clock := sim.NewClock()
	l, _, err := Open(fs, Config{
		Segment:      noRoll(),
		Durability:   Batch,
		SyncInterval: interval,
		Clock:        clock,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer l.Close()

	// Time does not move, so no append reaches the deadline.
	appendN(t, l, 0, 20)
	if fs.syncs != 0 {
		t.Fatalf("a stopped clock produced %d syncs", fs.syncs)
	}

	clock.Advance(core.Duration(interval - 1))
	appendN(t, l, 20, 1)
	if fs.syncs != 0 {
		t.Fatalf("an append one tick short of the interval synced %d times", fs.syncs)
	}

	clock.Advance(1)
	appendN(t, l, 21, 1)
	if fs.syncs != 1 {
		t.Fatalf("the append at the deadline synced %d times, want 1", fs.syncs)
	}

	// The deadline moved with the sync, so the next append does not
	// immediately trip it again.
	appendN(t, l, 22, 1)
	if fs.syncs != 1 {
		t.Fatalf("the interval did not reset: %d syncs", fs.syncs)
	}
}

// Sync says it makes everything appended so far durable. After a roll in os
// mode it did not: roll skips syncing the outgoing segment in that mode, and
// Sync only ever touched the active one, so a crash left a hole in the middle
// of the log where a sealed segment's records should have been.
//
// Found by scripts/powercut.sh against real ext4, which is the first thing to
// exercise this: the simulation workload only ever ran in batch mode, where
// roll syncs the outgoing segment and the gap cannot open.
func TestSyncCoversSealedSegmentsInOSMode(t *testing.T) {
	fs := sim.NewFS()

	// Segments small enough that this rolls several times.
	l, _, err := Open(fs, Config{Segment: Options{MaxBytes: 512}, Durability: OS})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	const n = 400
	appendN(t, l, 0, n)
	if len(l.Segments()) < 3 {
		t.Fatalf("the log rolled %d times; the gap needs a sealed segment", len(l.Segments())-1)
	}

	// The caller is told everything so far is durable.
	if err := l.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if err := fs.Sync(); err != nil {
		t.Fatalf("FS.Sync: %v", err)
	}
	fs.Crash()

	reopened, _, err := Open(fs, Config{Segment: Options{MaxBytes: 512}, Durability: OS})
	if err != nil {
		t.Fatalf("reopen after the crash: %v", err)
	}
	defer reopened.Close()

	if reopened.Next() < Offset(n) {
		t.Fatalf("the log recovered to %d, below the %d acknowledged by Sync", reopened.Next(), n)
	}
	for i := range n {
		if _, err := reopened.Read(Offset(i)); err != nil {
			t.Fatalf("record %d was acknowledged durable and is gone: %v", i, err)
		}
	}
}

// A sealed segment must be durable the moment it is sealed.
//
// Recovery refuses a sealed segment with a damaged tail outright, on the premise
// that "nothing has appended to it since it was sealed and synced". Deferring
// that sync in os mode broke the premise: a crash between the roll and the next
// Sync leaves a sealed segment holding acknowledged records and an unsynced
// tail, and refusing the file takes the acknowledged prefix with it.
//
// Found by scripts/powercut.sh, on a run where the previous fix for this same
// area had already passed twenty cuts.
func TestASealedSegmentIsDurableWhenItIsSealed(t *testing.T) {
	fs := sim.NewFS()
	cfg := Config{Segment: Options{MaxBytes: 512}, Durability: OS}

	l, _, err := Open(fs, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// Acknowledged: everything to here is durable.
	appendN(t, l, 0, 40)
	if err := l.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if err := fs.Sync(); err != nil {
		t.Fatalf("FS.Sync: %v", err)
	}
	acked := int(l.Next())

	// More records, crossing a roll, and no Sync after it. The machine dies
	// with a sealed segment whose tail never reached the disk.
	appendN(t, l, acked, 200)
	if len(l.Segments()) < 3 {
		t.Fatalf("the log rolled %d times, which is not enough to seal one", len(l.Segments())-1)
	}
	// The cut that leaves the file longer than its contents, which is what
	// ext4 did and what the simulator could not produce until now.
	fs.CrashExtend()

	reopened, _, err := Open(fs, cfg)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()

	for i := range acked {
		if _, err := reopened.Read(Offset(i)); err != nil {
			t.Fatalf("record %d was acknowledged durable and is unreadable: %v", i, err)
		}
	}
}

// A duration with no clock to measure it against would silently never fire.
func TestAnIntervalWithoutAClockIsRejected(t *testing.T) {
	_, _, err := Open(sim.NewFS(), Config{SyncInterval: 100})
	if err == nil {
		t.Fatal("a SyncInterval with no Clock was accepted")
	}
}
