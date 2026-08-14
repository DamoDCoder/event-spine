// Package sim supplies the deterministic implementations of core's injected
// dependencies, and the synthetic workload the determinism gate runs.
//
// This package is exempt from the import rule in scripts/check-determinism.sh
// because it is the thing doing the injecting. That exemption is not a licence:
// nothing here reads a wall clock or an unseeded generator either. It exists so
// the fault injection and scheduling that arrive at M3 have somewhere to live.
package sim

import (
	"fmt"
	"math/bits"

	"github.com/DamoDCoder/event-spine/core"
)

// Clock is virtual time. It advances only when something advances it, so a
// simulated hour costs whatever the arithmetic costs.
type Clock struct {
	now    core.Time
	timers []*timer
}

// NewClock returns a clock at logical instant zero.
func NewClock() *Clock { return &Clock{} }

// Now returns the current logical instant. Reading time does not advance it:
// a fold that reads the clock twice must see the same answer both times, or
// "same events, same projection" stops being true.
func (c *Clock) Now() core.Time { return c.now }

// Advance moves logical time forward. It panics on a negative duration rather
// than silently moving backwards, because a clock that goes backwards is one of
// the failure modes docs/log-design.md asks the simulator to inject deliberately
// — and a deliberate injection goes through Set, not through arithmetic nobody
// meant to write.
func (c *Clock) Advance(d core.Duration) {
	if d < 0 {
		panic(fmt.Sprintf("sim: Advance(%d) would move the clock backwards; use Set", d))
	}
	c.now += core.Time(d)
}

// Set moves logical time to an arbitrary instant, backwards included. This is
// the deliberate injection of a clock that jumps.
func (c *Clock) Set(t core.Time) { c.now = t }

// Timer returns a timer due d from now.
func (c *Clock) Timer(d core.Duration) core.Timer {
	t := &timer{due: c.now + core.Time(d)}
	c.timers = append(c.timers, t)
	return t
}

// Due returns the timers that are ready as of now, in the order they were
// created. Order is by creation rather than by deadline so that two timers due
// at the same instant resolve the same way on every run; where that tie should
// be broken by the seed instead, the caller hands the slice to a Scheduler.
func (c *Clock) Due() []core.Timer {
	var out []core.Timer
	for _, t := range c.timers {
		if t.Ready(c.now) {
			out = append(out, t)
		}
	}
	return out
}

type timer struct {
	due       core.Time
	cancelled bool
}

func (t *timer) Deadline() core.Time      { return t.due }
func (t *timer) Ready(now core.Time) bool { return !t.cancelled && now >= t.due }
func (t *timer) Cancel()                  { t.cancelled = true }

// Source is a seeded splitmix64 generator.
//
// It is written out rather than taken from math/rand for two reasons. The
// generator's output must not change when the standard library's does, since a
// committed seed that stops reproducing its failure is worse than no seed. And
// the arithmetic is integer-only, so the sequence is identical on every
// architecture — which the M0 spike confirmed matters in practice.
type Source struct{ state uint64 }

// NewSource returns a generator seeded with seed.
func NewSource(seed int64) *Source { return &Source{state: uint64(seed)} }

// Uint64 returns the next value in the sequence.
func (s *Source) Uint64() uint64 {
	s.state += 0x9e3779b97f4a7c15
	z := s.state
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

// Intn returns a value in [0, n).
//
// The bound is applied by Lemire's multiply-and-reject method rather than by a
// modulo, because modulo bias would make some seeds subtly more likely to
// exercise some branches — a distortion that is invisible until a fault the
// sweep never reaches turns up in production.
func (s *Source) Intn(n int) int {
	if n <= 0 {
		panic(fmt.Sprintf("sim: Intn(%d) requires n > 0", n))
	}
	bound := uint64(n)
	hi, lo := bits.Mul64(s.Uint64(), bound)
	if lo < bound {
		// The rejection threshold is (2^64 - bound) mod bound.
		threshold := -bound % bound
		for lo < threshold {
			hi, lo = bits.Mul64(s.Uint64(), bound)
		}
	}
	return int(hi)
}

// IDGen produces a sequential identifier per kind.
//
// The counters live in a map, which is safe here because the map is only ever
// looked up and written, never ranged. Ranging it would put identifier
// assignment at the mercy of Go's map ordering.
type IDGen struct{ counters map[string]int64 }

// NewIDGen returns a generator with every counter at zero.
func NewIDGen() *IDGen { return &IDGen{counters: map[string]int64{}} }

// NextID returns the next identifier for kind, zero-padded so identifiers sort
// lexically in the order they were issued.
func (g *IDGen) NextID(kind string) string {
	g.counters[kind]++
	return fmt.Sprintf("%s-%012d", kind, g.counters[kind])
}

// Scheduler chooses between simultaneously ready alternatives from its own
// seeded stream.
//
// The stream is separate from the workload's randomness on purpose. If domain
// draws and scheduling draws shared a generator, changing how many events a
// command produces would silently change every later interleaving, and a
// minimized seed would stop reproducing its failure the moment the domain
// changed.
type Scheduler struct{ src *Source }

// NewScheduler returns a scheduler seeded independently of the workload.
func NewScheduler(seed int64) *Scheduler {
	// The offset keeps the scheduler's stream from being the workload's
	// stream shifted by nothing when both are built from the same seed.
	return &Scheduler{src: NewSource(seed ^ 0x5deece66d)}
}

// Choose returns the index of the alternative to run.
func (s *Scheduler) Choose(ready int) int {
	if ready <= 1 {
		return 0
	}
	return s.src.Intn(ready)
}

// Deps returns a fully wired deterministic dependency set for a run.
func Deps(seed int64) (core.Deps, *Clock) {
	clock := NewClock()
	return core.Deps{
		Clock: clock,
		Rand:  NewSource(seed),
		IDs:   NewIDGen(),
		Sched: NewScheduler(seed),
	}, clock
}
