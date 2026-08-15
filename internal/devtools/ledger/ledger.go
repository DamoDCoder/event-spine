// Package ledger is the workload the determinism gate runs.
//
// It lives here rather than in sim because it is a fixture, not a dependency. A
// project adopting the spine wants sim's clock, filesystem, and seeded source
// for its own tests; it has no use for this repository's toy ledger, and a
// public package should not hand it one.
package ledger

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/DamoDCoder/event-spine/core"
	"github.com/DamoDCoder/event-spine/sim"
)

// The workload is a ledger: accounts, credits, debits, and transfers.
//
// It is synthetic, and it is chosen rather than arbitrary. A transfer produces
// two events from one command and must be rejected atomically when the source
// cannot cover it, which exercises the property that a rejected command leaves
// state untouched — the property most likely to be broken by a plausible
// refactor. Balances keep moving, so the projection never reaches the absorbing
// state that made the M0 spike's hash comparison vacuous.
const (
	// workloadSchema versions the event payload below.
	workloadSchema = 1

	// payloadBytes is op, account index, and amount.
	payloadBytes = 1 + 2 + 4

	opCredit byte = 1
	opDebit  byte = 2
)

// ErrInsufficientFunds is the rejection a debit or transfer earns when the
// source account cannot cover it.
var ErrInsufficientFunds = errors.New("sim: insufficient funds")

// AccountID returns the stable key for account i, zero-padded so keys sort
// lexically in index order.
func AccountID(i int) string { return fmt.Sprintf("acct-%04d", i) }

// ledger is the projection: one balance per account, held in a slice so
// iteration order is positional and never depends on a map.
type ledger struct {
	balances []int64
	applied  int64
}

func newLedger(accounts int) *ledger {
	return &ledger{balances: make([]int64, accounts)}
}

// Apply folds one event. It is total over valid payloads: an event naming an
// account that does not exist is a decoding failure, not a silent no-op, since
// anything read back from a log is untrusted.
func (l *ledger) Apply(e core.Event) error {
	if len(e.Payload) != payloadBytes {
		return fmt.Errorf("sim: payload is %d bytes, want %d", len(e.Payload), payloadBytes)
	}
	op := e.Payload[0]
	idx := int(binary.LittleEndian.Uint16(e.Payload[1:3]))
	amount := int64(binary.LittleEndian.Uint32(e.Payload[3:7]))

	if idx < 0 || idx >= len(l.balances) {
		return fmt.Errorf("sim: account index %d is outside the ledger", idx)
	}
	switch op {
	case opCredit:
		l.balances[idx] += amount
	case opDebit:
		l.balances[idx] -= amount
	default:
		return fmt.Errorf("sim: unknown op %d", op)
	}
	l.applied++
	return nil
}

// Digest fingerprints every balance in index order. Integers only: a float
// average here would be a determinism bug waiting for a different architecture.
func (l *ledger) Digest() core.Digest {
	buf := make([]byte, 0, len(l.balances)*8+8)
	for _, b := range l.balances {
		buf = binary.LittleEndian.AppendUint64(buf, uint64(b))
	}
	buf = binary.LittleEndian.AppendUint64(buf, uint64(l.applied))

	c := core.NewChain()
	c.Advance(core.Event{Key: "ledger", Schema: workloadSchema, Payload: buf}, core.Digest{})
	return c.Digest()
}

// teller decides ledger commands against the balances the projection holds.
type teller struct{ l *ledger }

// Decide turns a command into events, or rejects it.
//
// A transfer is decided as a unit: if the source cannot cover the amount,
// nothing is returned and nothing is applied. Returning the debit and letting
// the credit fail later would leave the projection holding half a transfer,
// which is the exact shape of bug the poisoned-cycle path exists to refuse.
func (t teller) Decide(cmd core.Command) ([]core.Event, error) {
	if len(cmd.Payload) != payloadBytes {
		return nil, fmt.Errorf("sim: command payload is %d bytes, want %d", len(cmd.Payload), payloadBytes)
	}
	// A command shares the event payload layout, but its first byte is
	// unused: the operation is the command's name, and a command that
	// disagreed with its own payload about which one it was would be a
	// second source of truth.
	idx := int(binary.LittleEndian.Uint16(cmd.Payload[1:3]))
	amount := int64(binary.LittleEndian.Uint32(cmd.Payload[3:7]))

	if idx < 0 || idx >= len(t.l.balances) {
		return nil, fmt.Errorf("sim: account index %d is outside the ledger", idx)
	}

	switch cmd.Name {
	case "credit":
		return []core.Event{{Key: AccountID(idx), Schema: workloadSchema, Payload: payload(opCredit, idx, amount)}}, nil

	case "debit":
		if t.l.balances[idx] < amount {
			return nil, fmt.Errorf("%w: %s holds %d, needs %d", ErrInsufficientFunds, AccountID(idx), t.l.balances[idx], amount)
		}
		return []core.Event{{Key: AccountID(idx), Schema: workloadSchema, Payload: payload(opDebit, idx, amount)}}, nil

	case "transfer":
		// The destination is the next account, so a transfer never
		// needs a second index in the payload and never targets its
		// own source.
		dst := (idx + 1) % len(t.l.balances)
		if t.l.balances[idx] < amount {
			return nil, fmt.Errorf("%w: %s holds %d, needs %d", ErrInsufficientFunds, AccountID(idx), t.l.balances[idx], amount)
		}
		return []core.Event{
			{Key: AccountID(idx), Schema: workloadSchema, Payload: payload(opDebit, idx, amount)},
			{Key: AccountID(dst), Schema: workloadSchema, Payload: payload(opCredit, dst, amount)},
		}, nil

	default:
		return nil, fmt.Errorf("sim: unknown command %q", cmd.Name)
	}
}

func payload(op byte, idx int, amount int64) []byte {
	p := make([]byte, payloadBytes)
	p[0] = op
	binary.LittleEndian.PutUint16(p[1:3], uint16(idx))
	binary.LittleEndian.PutUint32(p[3:7], uint32(amount))
	return p
}

// Workload describes one run of the ledger.
type Workload struct {
	Seed     int64
	Commands int
	Accounts int
}

// Result is what a run leaves behind. Chain is the value two runs compare;
// everything else explains it when they disagree.
type Result struct {
	Seed             int64
	Chain            core.Digest
	Projection       core.Digest
	Steps            int64
	Commands         int64
	Rejected         int64
	StepsSinceChange int64
	Absorbed         bool
	FinalTime        core.Time
}

// absorbedWindow is how many consecutive steps the ledger may leave every
// balance unchanged before a run stops counting as evidence. A ledger that
// applies an event always moves a balance, so any value above zero here would
// do; the margin is for the workload growing a no-op event later.
const absorbedWindow = 32

// Run executes the workload and returns its result.
//
// Every decision — which command, which account, how much, how far the clock
// moves — is drawn from the seeded source in a fixed order. Change that order
// and every committed seed reproduces a different run, which is why the draws
// are grouped here rather than scattered through helpers.
func Run(w Workload) (Result, error) {
	if w.Commands < 1 {
		return Result{}, fmt.Errorf("ledger: commands must be at least 1, got %d", w.Commands)
	}
	if w.Accounts < 2 {
		return Result{}, fmt.Errorf("ledger: accounts must be at least 2, got %d", w.Accounts)
	}

	deps, clock := sim.Deps(w.Seed)
	l := newLedger(w.Accounts)
	cycle, err := core.NewCycle(deps, teller{l: l}, l)
	if err != nil {
		return Result{}, err
	}

	src := deps.Rand
	res := Result{Seed: w.Seed}

	for i := 0; i < w.Commands; i++ {
		name := commandName(src.Intn(10))
		idx := src.Intn(w.Accounts)
		amount := int64(src.Intn(500) + 1)

		// Time advances before the command is decided, so every event
		// of a command carries the instant that command was accepted.
		clock.Advance(core.Duration(src.Intn(1000) + 1))

		res.Commands++
		_, err := cycle.Submit(core.Command{
			Name:    name,
			Key:     AccountID(idx),
			Payload: payload(0, idx, amount),
		})
		switch {
		case err == nil:
		case errors.Is(err, ErrInsufficientFunds):
			res.Rejected++
		default:
			return Result{}, fmt.Errorf("seed %d, command %d (%s): %w", w.Seed, i, name, err)
		}
	}

	chain := cycle.Chain()
	res.Chain = chain.Digest()
	res.Projection = cycle.Digest()
	res.Steps = chain.Steps()
	res.StepsSinceChange = chain.StepsSinceChange()
	res.Absorbed = chain.Absorbed(absorbedWindow)
	res.FinalTime = clock.Now()
	return res, nil
}

// commandName weights the mix so credits outnumber withdrawals. A workload that
// drains every account spends most of its run rejecting, which measures the
// rejection path and nothing else.
func commandName(roll int) string {
	switch {
	case roll < 5:
		return "credit"
	case roll < 8:
		return "debit"
	default:
		return "transfer"
	}
}
