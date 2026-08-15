package ledger

import (
	"errors"
	"testing"

	"github.com/DamoDCoder/event-spine/core"
)

// The gate's own claim: one seed, one digest, and two seeds that differ.
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
