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

// A duration with no clock to measure it against would silently never fire.
func TestAnIntervalWithoutAClockIsRejected(t *testing.T) {
	_, _, err := Open(sim.NewFS(), Config{SyncInterval: 100})
	if err == nil {
		t.Fatal("a SyncInterval with no Clock was accepted")
	}
}
