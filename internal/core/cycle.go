package core

import (
	"errors"
	"fmt"
)

// Projection is a pure fold over an event sequence.
//
// Same events, same projection, same digest — regardless of batching, restarts,
// or replay. Apply must be idempotent with respect to redelivery of an event
// the projection has already folded, because the delivery contract is
// at-least-once and duplicates die here rather than upstream.
type Projection interface {
	// Apply folds one event into the projection. It must not retain the
	// event's payload slice.
	Apply(Event) error

	// Digest fingerprints the projection. It must depend on every field
	// that a consumer can observe, must not depend on map iteration order,
	// and must not depend on a float comparison.
	Digest() Digest
}

// Decider turns a command into events, or rejects it.
//
// A decider reads the projection it was built against and returns events
// without timestamps: the cycle stamps those from the injected clock, so a
// decider cannot reach for a clock of its own.
type Decider interface {
	Decide(Command) ([]Event, error)
}

// ErrPoisoned is returned by every call on a cycle whose projection failed
// mid-batch.
//
// A failed Apply may have left the projection holding some of a command's
// events and not others, which is a state no fold can be resumed from honestly.
// The cycle refuses further work rather than continuing on a projection whose
// digest no longer describes any sequence of events.
var ErrPoisoned = errors.New("core: cycle is poisoned by a failed apply")

// Cycle runs command → validate → event → projection.
//
// It owns no goroutines and performs no I/O. Appending events to a durable log
// is the caller's job and happens outside this type, so the same cycle drives a
// live run, a replay, and a simulation without knowing which it is in.
type Cycle struct {
	deps  Deps
	dec   Decider
	proj  Projection
	chain *Chain

	poison error
}

// NewCycle wires a cycle. It fails rather than defaulting a missing dependency,
// because a defaulted clock is how a wall-clock read gets into a fold.
func NewCycle(deps Deps, dec Decider, proj Projection) (*Cycle, error) {
	if err := deps.Validate(); err != nil {
		return nil, err
	}
	if dec == nil {
		return nil, errMissingDep("Decider")
	}
	if proj == nil {
		return nil, errMissingDep("Projection")
	}
	return &Cycle{deps: deps, dec: dec, proj: proj, chain: NewChain()}, nil
}

// Submit runs one command and returns the events it produced.
//
// A rejected command leaves the projection and the chain untouched: the decider
// is consulted, and nothing is applied unless every event it returned validates
// first. That ordering is what makes rejection free of side effects rather than
// merely usually free of them.
//
// The returned slice is the cycle's own. Callers append it to a log; they must
// not mutate it.
func (c *Cycle) Submit(cmd Command) ([]Event, error) {
	if c.poison != nil {
		return nil, c.poison
	}
	if err := cmd.Validate(); err != nil {
		return nil, err
	}

	events, err := c.dec.Decide(cmd)
	if err != nil {
		return nil, fmt.Errorf("decide %s: %w", cmd.Name, err)
	}
	if len(events) == 0 {
		return nil, nil
	}

	now := c.deps.Clock.Now()
	for i := range events {
		// Stamped, not defaulted: an event carrying a time a decider
		// chose would be a decider that found a clock somewhere.
		events[i].Time = now
		if events[i].Key == "" {
			events[i].Key = cmd.Key
		}
		if err := events[i].Validate(); err != nil {
			return nil, fmt.Errorf("event %d of %s: %w", i, cmd.Name, err)
		}
	}

	if err := c.apply(events); err != nil {
		return nil, err
	}
	return events, nil
}

// Fold applies events that already exist — a replay from the log, or a
// consumer group catching up. It performs no validation of command intent
// because there is no command: these are facts.
//
// Events are still validated, because anything decoded from disk is untrusted.
func (c *Cycle) Fold(events []Event) error {
	if c.poison != nil {
		return c.poison
	}
	for i := range events {
		if err := events[i].Validate(); err != nil {
			return fmt.Errorf("replayed event %d: %w", i, err)
		}
	}
	return c.apply(events)
}

// apply folds a validated batch and advances the chain one step per event.
func (c *Cycle) apply(events []Event) error {
	for i, e := range events {
		if err := c.proj.Apply(e); err != nil {
			c.poison = fmt.Errorf("%w: event %d: %w", ErrPoisoned, i, err)
			return c.poison
		}
		c.chain.Advance(e, c.proj.Digest())
	}
	return nil
}

// Chain returns the run's hash chain.
func (c *Cycle) Chain() *Chain { return c.chain }

// Digest returns the current projection digest. Compare chains, not this, when
// asking whether two runs agree: see Chain's documentation for why.
func (c *Cycle) Digest() Digest { return c.proj.Digest() }

// Deps returns the injected dependency set, so a decider built by a caller can
// share the cycle's randomness and identifiers rather than sourcing its own.
func (c *Cycle) Deps() Deps { return c.deps }
