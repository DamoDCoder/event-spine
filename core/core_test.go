package core

import (
	"encoding/binary"
	"errors"
	"fmt"
	"testing"
)

// --------------------------------------------------------------- test doubles

// stepClock advances by a fixed logical step on every read, so a test can tell
// which clock read produced which timestamp.
type stepClock struct {
	now  Time
	step Duration
}

func (c *stepClock) Now() Time {
	n := c.now
	c.now += Time(c.step)
	return n
}

func (c *stepClock) Timer(d Duration) Timer { return &fixedTimer{due: c.now + Time(d)} }

type fixedTimer struct {
	due       Time
	cancelled bool
}

func (t *fixedTimer) Deadline() Time      { return t.due }
func (t *fixedTimer) Ready(now Time) bool { return !t.cancelled && now >= t.due }
func (t *fixedTimer) Cancel()             { t.cancelled = true }

// countingSource is a stand-in sequence. It is not a good generator and does
// not need to be: these tests assert that draws are reproducible, not that they
// are well distributed.
type countingSource struct{ n uint64 }

func (s *countingSource) Uint64() uint64 { s.n++; return s.n }

func (s *countingSource) Intn(n int) int {
	if n <= 0 {
		panic("core: Intn requires n > 0")
	}
	return int(s.Uint64() % uint64(n))
}

type seqIDs struct{ n int }

func (g *seqIDs) NextID(kind string) string { g.n++; return fmt.Sprintf("%s-%04d", kind, g.n) }

// firstReady always takes the lowest-indexed alternative. Deterministic and
// deliberately boring; M3 replaces it with a seeded chooser.
type firstReady struct{}

func (firstReady) Choose(int) int { return 0 }

func testDeps() Deps {
	return Deps{
		Clock: &stepClock{step: 1000},
		Rand:  &countingSource{},
		IDs:   &seqIDs{},
		Sched: firstReady{},
	}
}

// ------------------------------------------------------------------- fixtures

// tally is a projection whose digest depends on every observable field. It
// counts per key, which makes it order-insensitive on purpose: the chain still
// has to notice a reordering even though the terminal state does not.
type tally struct {
	keys   []string
	counts map[string]int64
	failOn string
}

func newTally() *tally { return &tally{counts: map[string]int64{}} }

func (t *tally) Apply(e Event) error {
	if t.failOn != "" && e.Key == t.failOn {
		return errors.New("tally: refusing key " + e.Key)
	}
	if _, seen := t.counts[e.Key]; !seen {
		t.keys = append(t.keys, e.Key)
	}
	// Read the payload without appending to it: appending can write into
	// the caller's backing array, which is exactly the aliasing a
	// projection is forbidden from doing.
	var amount uint16
	if len(e.Payload) >= 2 {
		amount = binary.LittleEndian.Uint16(e.Payload[:2])
	} else if len(e.Payload) == 1 {
		amount = uint16(e.Payload[0])
	}
	t.counts[e.Key] += int64(amount)
	return nil
}

// Digest sorts nothing because keys are appended in first-seen order, never
// ranged from the map. The map is lookup only.
func (t *tally) Digest() Digest {
	var buf []byte
	for _, k := range t.keys {
		buf = append(buf, k...)
		buf = binary.LittleEndian.AppendUint64(buf, uint64(t.counts[k]))
	}
	c := NewChain()
	c.Advance(Event{Key: "digest", Schema: 1, Payload: buf}, Digest{})
	return c.Digest()
}

// amounts turns a command into one event per byte of its payload.
type amounts struct{ reject bool }

func (a amounts) Decide(cmd Command) ([]Event, error) {
	if a.reject {
		return nil, errors.New("amounts: rejected")
	}
	out := make([]Event, 0, len(cmd.Payload))
	for _, b := range cmd.Payload {
		out = append(out, Event{
			Schema: 1,
			// Deliberately set: the cycle must overwrite it.
			Time:    999999,
			Payload: []byte{b, 0},
		})
	}
	return out, nil
}

func mustCycle(t *testing.T, dec Decider, proj Projection) *Cycle {
	t.Helper()
	c, err := NewCycle(testDeps(), dec, proj)
	if err != nil {
		t.Fatalf("NewCycle: %v", err)
	}
	return c
}

// ---------------------------------------------------------------------- tests

func TestNewCycleRejectsMissingDependencies(t *testing.T) {
	full := testDeps()
	cases := map[string]Deps{
		"clock":      {Rand: full.Rand, IDs: full.IDs, Sched: full.Sched},
		"rand":       {Clock: full.Clock, IDs: full.IDs, Sched: full.Sched},
		"ids":        {Clock: full.Clock, Rand: full.Rand, Sched: full.Sched},
		"sched":      {Clock: full.Clock, Rand: full.Rand, IDs: full.IDs},
		"everything": {},
	}
	for name, deps := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := NewCycle(deps, amounts{}, newTally()); err == nil {
				t.Fatal("expected a missing-dependency error, got nil")
			}
		})
	}

	if _, err := NewCycle(full, nil, newTally()); err == nil {
		t.Fatal("expected an error for a nil decider")
	}
	if _, err := NewCycle(full, amounts{}, nil); err == nil {
		t.Fatal("expected an error for a nil projection")
	}
}

func TestCycleStampsTimeFromTheInjectedClock(t *testing.T) {
	c := mustCycle(t, amounts{}, newTally())

	events, err := c.Submit(Command{Name: "credit", Key: "a", Payload: []byte{1, 2, 3}})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("got %d events, want 3", len(events))
	}
	// One clock read per command, not one per event: every event of a
	// command shares the instant the command was decided.
	for i, e := range events {
		if e.Time != 0 {
			t.Fatalf("event %d has time %d, want the first clock read (0)", i, e.Time)
		}
		if e.Key != "a" {
			t.Fatalf("event %d has key %q, want the command's key", i, e.Key)
		}
	}

	next, err := c.Submit(Command{Name: "credit", Key: "a", Payload: []byte{4}})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if next[0].Time != 1000 {
		t.Fatalf("second command stamped %d, want 1000", next[0].Time)
	}
}

func TestRejectedCommandLeavesStateUntouched(t *testing.T) {
	proj := newTally()
	c := mustCycle(t, amounts{reject: true}, proj)

	before := c.Chain().Digest()
	beforeProj := c.Digest()

	if _, err := c.Submit(Command{Name: "credit", Key: "a", Payload: []byte{1}}); err == nil {
		t.Fatal("expected the decider's rejection")
	}
	if got := c.Chain().Digest(); got != before {
		t.Fatal("a rejected command advanced the chain")
	}
	if got := c.Digest(); got != beforeProj {
		t.Fatal("a rejected command mutated the projection")
	}
	if steps := c.Chain().Steps(); steps != 0 {
		t.Fatalf("chain took %d steps on a rejected command", steps)
	}
}

func TestInvalidCommandIsRejectedBeforeTheDecider(t *testing.T) {
	c := mustCycle(t, amounts{}, newTally())
	if _, err := c.Submit(Command{Name: "", Key: "a"}); !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("got %v, want ErrInvalidCommand", err)
	}
	if steps := c.Chain().Steps(); steps != 0 {
		t.Fatalf("chain took %d steps on an invalid command", steps)
	}
}

func TestFailedApplyPoisonsTheCycle(t *testing.T) {
	proj := newTally()
	proj.failOn = "bad"
	c := mustCycle(t, amounts{}, proj)

	if _, err := c.Submit(Command{Name: "credit", Key: "bad", Payload: []byte{1}}); !errors.Is(err, ErrPoisoned) {
		t.Fatalf("got %v, want ErrPoisoned", err)
	}
	if _, err := c.Submit(Command{Name: "credit", Key: "good", Payload: []byte{1}}); !errors.Is(err, ErrPoisoned) {
		t.Fatalf("a poisoned cycle accepted more work: %v", err)
	}
	if err := c.Fold([]Event{{Key: "good", Schema: 1, Payload: []byte{1, 0}}}); !errors.Is(err, ErrPoisoned) {
		t.Fatalf("a poisoned cycle accepted a fold: %v", err)
	}
}

// The property the whole repository rests on: the same commands produce the
// same chain, every time, in any order of construction.
func TestSameCommandsProduceTheSameChain(t *testing.T) {
	run := func() Digest {
		c := mustCycle(t, amounts{}, newTally())
		for i := 0; i < 200; i++ {
			key := fmt.Sprintf("key-%d", i%7)
			if _, err := c.Submit(Command{Name: "credit", Key: key, Payload: []byte{byte(i), byte(i * 3)}}); err != nil {
				t.Fatalf("Submit: %v", err)
			}
		}
		return c.Chain().Digest()
	}

	first := run()
	for i := 0; i < 20; i++ {
		if got := run(); got != first {
			t.Fatalf("run %d produced %s, want %s", i, got, first)
		}
	}
}

// The M0 finding, as a test. The tally projection is commutative, so two
// different orderings reach an identical terminal state. A terminal-digest
// check passes; the chain does not.
func TestChainDetectsReorderingThatTheTerminalDigestCannotSee(t *testing.T) {
	events := []Event{
		{Key: "a", Schema: 1, Time: 1, Payload: []byte{5, 0}},
		{Key: "b", Schema: 1, Time: 2, Payload: []byte{9, 0}},
	}
	swapped := []Event{events[1], events[0]}

	forward := mustCycle(t, amounts{}, newTally())
	if err := forward.Fold(events); err != nil {
		t.Fatalf("Fold: %v", err)
	}
	reverse := mustCycle(t, amounts{}, newTally())
	if err := reverse.Fold(swapped); err != nil {
		t.Fatalf("Fold: %v", err)
	}

	if forward.Chain().Digest() == reverse.Chain().Digest() {
		t.Fatal("the chain missed a reordering, which is the one thing it exists to catch")
	}
}

func TestAbsorbedRunIsReportedRatherThanPassing(t *testing.T) {
	proj := newTally()
	c := mustCycle(t, amounts{}, proj)

	// Zero-amount events fold to no change: the projection is absorbed
	// while the log keeps growing, which is the shape that made 40
	// divergent Signal Garden runs report one hash.
	live := []Event{{Key: "a", Schema: 1, Time: 1, Payload: []byte{7, 0}}}
	if err := c.Fold(live); err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if c.Chain().Absorbed(4) {
		t.Fatal("a run that just changed state was reported absorbed")
	}

	dead := make([]Event, 10)
	for i := range dead {
		dead[i] = Event{Key: "a", Schema: 1, Time: Time(i + 2), Payload: []byte{0, 0}}
	}
	if err := c.Fold(dead); err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if !c.Chain().Absorbed(4) {
		t.Fatalf("10 no-op events left StepsSinceChange at %d", c.Chain().StepsSinceChange())
	}
	if c.Chain().Absorbed(0) {
		t.Fatal("a non-positive window must not report absorption")
	}
}

func TestCanonicalEncodingIsInjective(t *testing.T) {
	// The classic length-prefix failure: "ab" + "" and "a" + "b" must not
	// encode alike.
	a := Event{Key: "ab", Schema: 1, Time: 1}
	b := Event{Key: "a", Schema: 1, Time: 1, Payload: []byte("b")}
	if string(a.AppendCanonical(nil)) == string(b.AppendCanonical(nil)) {
		t.Fatal("distinct events share a canonical encoding")
	}

	// Encoding appends to dst rather than replacing it.
	prefix := []byte("keep")
	got := a.AppendCanonical(prefix)
	if string(got[:len(prefix)]) != "keep" {
		t.Fatal("AppendCanonical overwrote its destination")
	}
}

func TestEventValidation(t *testing.T) {
	long := make([]byte, MaxKeyLen+1)
	cases := map[string]Event{
		"negative time": {Schema: 1, Time: -1},
		"zero schema":   {Schema: 0, Time: 1},
		"oversized key": {Schema: 1, Time: 1, Key: string(long)},
	}
	for name, e := range cases {
		t.Run(name, func(t *testing.T) {
			if err := e.Validate(); !errors.Is(err, ErrInvalidEvent) {
				t.Fatalf("got %v, want ErrInvalidEvent", err)
			}
		})
	}

	if err := (Event{Schema: 1, Time: 0, Key: "a"}).Validate(); err != nil {
		t.Fatalf("a valid event was rejected: %v", err)
	}
}

func TestFoldValidatesReplayedEvents(t *testing.T) {
	c := mustCycle(t, amounts{}, newTally())
	// Anything read back from disk is untrusted, including a schema of
	// zero that no encoder in this repository would ever write.
	err := c.Fold([]Event{{Key: "a", Schema: 0, Time: 1, Payload: []byte{1, 0}}})
	if !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("got %v, want ErrInvalidEvent", err)
	}
	if steps := c.Chain().Steps(); steps != 0 {
		t.Fatalf("an invalid replay advanced the chain %d steps", steps)
	}
}

func TestEmptyDecisionIsNotAStep(t *testing.T) {
	c := mustCycle(t, emptyDecider{}, newTally())
	events, err := c.Submit(Command{Name: "noop", Key: "a"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("got %d events, want none", len(events))
	}
	if steps := c.Chain().Steps(); steps != 0 {
		t.Fatalf("an empty decision advanced the chain %d steps", steps)
	}
}

type emptyDecider struct{}

func (emptyDecider) Decide(Command) ([]Event, error) { return nil, nil }
