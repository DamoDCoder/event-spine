package core

import (
	"crypto/sha256"
	"encoding/hex"
)

// Digest is a projection or chain fingerprint.
type Digest [sha256.Size]byte

// String returns the hex encoding, which is what verification output prints and
// what a seed's failure note records.
func (d Digest) String() string { return hex.EncodeToString(d[:]) }

// chainDomain separates this hash from every other SHA-256 in the project, so a
// projection digest can never be mistaken for a chain digest by an equality
// check that happens to compile.
var chainDomain = []byte("event-spine/chain/v1\x00")

// Chain is a running hash over every step of a fold, not just its result.
//
// The M0 spike found the reason this exists. Comparing terminal projection
// hashes across 40 genuinely divergent runs reported them identical, because the
// projection had reached an absorbing state and every history folded to the same
// place. A terminal hash is strongest where the system is least interesting and
// blind where divergence is cheapest to introduce. See
// docs/decisions/m0-determinism-spike.md.
//
// Chaining costs one hash per event and makes the first divergent step visible
// in a single comparable value.
type Chain struct {
	digest     Digest
	steps      int64
	lastChange int64
	last       Digest
	seen       bool

	// scratch is reused across Advance calls so folding a million events
	// does not allocate a million encoding buffers.
	scratch []byte
}

// NewChain returns a chain positioned before the first event.
func NewChain() *Chain {
	return &Chain{digest: sha256.Sum256(chainDomain)}
}

// Advance folds one step into the chain: the event that was applied, and the
// projection digest that resulted from applying it.
//
// Both halves matter. The event alone would miss a projection that applies an
// event wrongly; the projection alone would miss two different events that
// happen to land on the same state, which is exactly the absorbing case.
func (c *Chain) Advance(e Event, proj Digest) {
	c.scratch = c.scratch[:0]
	c.scratch = append(c.scratch, c.digest[:]...)
	c.scratch = e.AppendCanonical(c.scratch)
	c.scratch = append(c.scratch, proj[:]...)
	c.digest = sha256.Sum256(c.scratch)

	c.steps++
	if !c.seen || proj != c.last {
		c.lastChange = c.steps
		c.last = proj
		c.seen = true
	}
}

// Digest returns the chain value. Two runs agree if and only if they applied
// the same events in the same order to the same projection behaviour.
func (c *Chain) Digest() Digest { return c.digest }

// Steps returns how many events have been folded.
func (c *Chain) Steps() int64 { return c.steps }

// StepsSinceChange returns how many steps have passed without the projection
// changing. A run that ends with a large value here proves less than its step
// count suggests.
func (c *Chain) StepsSinceChange() int64 { return c.steps - c.lastChange }

// Absorbed reports whether the projection has been unchanged for at least
// window steps.
//
// Verification treats an absorbed run as inconclusive rather than passing: once
// the projection stops responding to events, agreement between two runs is
// evidence about the absorbing state, not about determinism. The window is a
// caller's judgement because how long a projection may legitimately sit still
// depends on the domain, not on the substrate.
func (c *Chain) Absorbed(window int64) bool {
	if window <= 0 || c.steps == 0 {
		return false
	}
	return c.StepsSinceChange() >= window
}
