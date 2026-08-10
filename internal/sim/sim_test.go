package sim

import (
	"errors"
	"testing"

	"github.com/DamoDCoder/event-spine/internal/core"
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

func TestRunIsDeterministicAndSeedSensitive(t *testing.T) {
	w := Workload{Seed: 7, Commands: 400, Accounts: 16}

	first, err := Run(w)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for i := range 10 {
		again, err := Run(w)
		if err != nil {
			t.Fatalf("Run %d: %v", i, err)
		}
		if again.Chain != first.Chain {
			t.Fatalf("run %d chain %s, want %s", i, again.Chain, first.Chain)
		}
		if again.Projection != first.Projection {
			t.Fatalf("run %d projection %s, want %s", i, again.Projection, first.Projection)
		}
		if again.FinalTime != first.FinalTime {
			t.Fatalf("run %d ended at logical time %d, want %d", i, again.FinalTime, first.FinalTime)
		}
	}

	other, err := Run(Workload{Seed: 8, Commands: 400, Accounts: 16})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if other.Chain == first.Chain {
		t.Fatal("two seeds produced the same chain, so the seed is not reaching the workload")
	}
}

// The gate is worthless if the workload it runs is absorbed, which is the whole
// lesson of docs/decisions/m0-determinism-spike.md.
func TestRunStaysLive(t *testing.T) {
	for _, seed := range []int64{1, 2, 3, 500, 1000} {
		res, err := Run(Workload{Seed: seed, Commands: 500, Accounts: 16})
		if err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		if res.Absorbed {
			t.Fatalf("seed %d absorbed: unchanged for the last %d of %d steps",
				seed, res.StepsSinceChange, res.Steps)
		}
		if res.Steps == 0 {
			t.Fatalf("seed %d applied no events at all", seed)
		}
		if res.Rejected == 0 {
			t.Fatalf("seed %d never exercised the rejection path", seed)
		}
	}
}

// A transfer is decided as a unit. An underfunded one must leave both accounts
// exactly as they were, not debit the source and fail on the credit.
func TestUnderfundedTransferLeavesBothAccountsUntouched(t *testing.T) {
	l := newLedger(4)
	before := l.Digest()

	events, err := teller{l: l}.Decide(core.Command{
		Name:    "transfer",
		Key:     AccountID(0),
		Payload: payload(0, 0, 250),
	})
	if !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("got %v, want ErrInsufficientFunds", err)
	}
	if events != nil {
		t.Fatalf("a rejected transfer returned %d events", len(events))
	}
	if l.Digest() != before {
		t.Fatal("a rejected transfer moved the ledger")
	}
}

func TestFundedTransferMovesTheAmountAndNothingElse(t *testing.T) {
	l := newLedger(4)
	if err := l.Apply(core.Event{Key: AccountID(0), Schema: workloadSchema, Payload: payload(opCredit, 0, 300)}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	events, err := teller{l: l}.Decide(core.Command{
		Name:    "transfer",
		Key:     AccountID(0),
		Payload: payload(0, 0, 120),
	})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("got %d events, want a debit and a credit", len(events))
	}
	for _, e := range events {
		if err := l.Apply(e); err != nil {
			t.Fatalf("Apply: %v", err)
		}
	}

	if l.balances[0] != 180 {
		t.Fatalf("source holds %d, want 180", l.balances[0])
	}
	if l.balances[1] != 120 {
		t.Fatalf("destination holds %d, want 120", l.balances[1])
	}
	if l.balances[2] != 0 || l.balances[3] != 0 {
		t.Fatalf("an uninvolved account moved: %v", l.balances)
	}
}

// Anything decoded from a log is untrusted, including an account index that no
// encoder in this repository would produce.
func TestLedgerRejectsMalformedEvents(t *testing.T) {
	l := newLedger(2)
	cases := map[string]core.Event{
		"short payload":        {Key: "a", Schema: workloadSchema, Payload: []byte{1, 2}},
		"account out of range": {Key: "a", Schema: workloadSchema, Payload: payload(opCredit, 9, 1)},
		"unknown op":           {Key: "a", Schema: workloadSchema, Payload: payload(9, 0, 1)},
	}
	for name, e := range cases {
		t.Run(name, func(t *testing.T) {
			if err := l.Apply(e); err == nil {
				t.Fatal("a malformed event was applied")
			}
		})
	}
}

func TestRunValidatesItsConfiguration(t *testing.T) {
	if _, err := Run(Workload{Seed: 1, Commands: 0, Accounts: 4}); err == nil {
		t.Fatal("expected an error for zero commands")
	}
	if _, err := Run(Workload{Seed: 1, Commands: 10, Accounts: 1}); err == nil {
		t.Fatal("expected an error for a single-account ledger")
	}
}
