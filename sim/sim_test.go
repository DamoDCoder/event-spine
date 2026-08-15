package sim

import (
	"testing"
)

// The published splitmix64 sequence for seed 0.
//
// These are pinned because a committed seed's value is entirely in its ability
// to reproduce a failure years later. A generator that quietly changes turns the
// whole corpus in seeds/ into a set of numbers that used to mean something.
func TestSourceMatchesPublishedSplitmix64Vectors(t *testing.T) {
	want := []uint64{
		0xe220a8397b1dcdaf,
		0x6e789e6aa1b965f4,
		0x06c45d188009454f,
		0xf88bb8a8724c81ec,
		0x1b39896a51a8749b,
	}
	s := NewSource(0)
	for i, w := range want {
		if got := s.Uint64(); got != w {
			t.Fatalf("draw %d: got 0x%016x, want 0x%016x", i, got, w)
		}
	}
}

func TestIntnStaysInRangeAndSpreads(t *testing.T) {
	const (
		buckets = 8
		draws   = 200000
	)
	counts := make([]int, buckets)
	s := NewSource(99)
	for range draws {
		v := s.Intn(buckets)
		if v < 0 || v >= buckets {
			t.Fatalf("Intn returned %d, outside [0, %d)", v, buckets)
		}
		counts[v]++
	}

	// A loose bound. The point is to catch a bound that is off by one or a
	// rejection loop that never terminates, not to certify the generator.
	expect := draws / buckets
	for i, c := range counts {
		if c < expect*8/10 || c > expect*12/10 {
			t.Fatalf("bucket %d holds %d draws, expected near %d", i, c, expect)
		}
	}
}

func TestIntnPanicsOnNonPositiveBound(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("Intn(0) returned instead of panicking")
		}
	}()
	NewSource(1).Intn(0)
}

func TestClockAdvancesOnlyForward(t *testing.T) {
	c := NewClock()
	if c.Now() != 0 {
		t.Fatalf("a new clock reads %d, want 0", c.Now())
	}
	c.Advance(500)
	if c.Now() != 500 {
		t.Fatalf("clock reads %d after advancing 500", c.Now())
	}
	// Reading time must not move it, or two reads within one fold would
	// disagree.
	first, second := c.Now(), c.Now()
	if first != second {
		t.Fatalf("reading the clock advanced it: %d then %d", first, second)
	}

	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("Advance(-1) returned instead of panicking")
			}
		}()
		c.Advance(-1)
	}()

	// Going backwards is legal, but only deliberately.
	c.Set(1)
	if c.Now() != 1 {
		t.Fatalf("Set left the clock at %d", c.Now())
	}
}

func TestTimersAreReadyByDeadlineAndCancellable(t *testing.T) {
	c := NewClock()
	early := c.Timer(10)
	late := c.Timer(100)

	if len(c.Due()) != 0 {
		t.Fatal("a timer was ready before its deadline")
	}
	c.Advance(10)
	if due := c.Due(); len(due) != 1 || due[0] != early {
		t.Fatalf("got %d due timers, want only the early one", len(due))
	}

	early.Cancel()
	c.Advance(100)
	if due := c.Due(); len(due) != 1 || due[0] != late {
		t.Fatalf("a cancelled timer is still firing: %d due", len(due))
	}
}

func TestSchedulerStreamIsSeparateFromTheWorkloadStream(t *testing.T) {
	// Both are built from the same seed. If they shared a stream, the
	// scheduler's first choice would track the workload's first draw.
	const seed = 12345
	sched := NewScheduler(seed)
	src := NewSource(seed)

	var same int
	for range 64 {
		if sched.Choose(1000) == src.Intn(1000) {
			same++
		}
	}
	if same > 4 {
		t.Fatalf("scheduler and workload agreed on %d of 64 draws; the streams are not independent", same)
	}

	if got := NewScheduler(seed).Choose(1); got != 0 {
		t.Fatalf("Choose(1) returned %d, want 0", got)
	}
}
