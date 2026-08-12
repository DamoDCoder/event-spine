package harness

import "testing"

// A replay has to be the same execution the checks ran, or a debugger shows a
// run that never happened.
func TestAReplayMatchesTheRunItDebugs(t *testing.T) {
	cfg := Config{Seed: 8, Steps: 3, Faults: []Fault{{Kind: BitFlip, At: 5, Arg: 458021}}}

	trace := Replay(cfg)
	direct := Run(cfg)

	switch {
	case trace.Failure == nil && direct != nil:
		t.Fatalf("Replay found nothing where Run found %v", direct)
	case trace.Failure != nil && direct == nil:
		t.Fatalf("Replay found %v where Run found nothing", trace.Failure)
	case trace.Failure != nil && trace.Failure.Error() != direct.Error():
		t.Fatalf("Replay reported %v, Run reported %v", trace.Failure, direct)
	}
}

// The signature is what makes seed drift visible, so it has to change when the
// operation stream does and not otherwise.
func TestTheSignatureIdentifiesTheOperationStream(t *testing.T) {
	first, firstSig, err := CleanOps(Config{Seed: 3})
	if err != nil {
		t.Fatalf("CleanOps: %v", err)
	}
	again, againSig, err := CleanOps(Config{Seed: 3})
	if err != nil {
		t.Fatalf("CleanOps: %v", err)
	}
	if first != again || firstSig != againSig {
		t.Fatalf("the same seed gave %d ops %016x and then %d ops %016x", first, firstSig, again, againSig)
	}

	// A different seed does different work, so the two must not collide.
	_, otherSig, err := CleanOps(Config{Seed: 4})
	if err != nil {
		t.Fatalf("CleanOps: %v", err)
	}
	if otherSig == firstSig {
		t.Fatalf("seeds 3 and 4 share signature %016x", firstSig)
	}

	// A shorter run is a prefix of the same stream and must still be told
	// apart, since a seed records the steps it was minimized to.
	_, shortSig, err := CleanOps(Config{Seed: 3, Steps: 5})
	if err != nil {
		t.Fatalf("CleanOps: %v", err)
	}
	if shortSig == firstSig {
		t.Fatalf("a 5 step run and a %d step run share signature %016x", DefaultSteps, firstSig)
	}
}

// The counterfactual is the tool's main claim: run the seed with its faults and
// without them, and name the step where they part.
func TestDiffNamesTheStepWhereTheRunsPart(t *testing.T) {
	// A crash ends the faulted run, so the two agree until it stops.
	cfg := Config{Seed: 3, Faults: []Fault{{Kind: Crash, At: 12}}}

	faulted, clean, div := Diff(cfg, Without(cfg))
	if div.Step < 0 {
		t.Fatal("a crashed run and a clean one were reported identical")
	}
	if len(faulted.States) >= len(clean.States) {
		t.Fatalf("the crashed run completed %d steps and the clean one %d",
			len(faulted.States), len(clean.States))
	}
	if !div.Truncated {
		t.Fatalf("a run that stopped early was reported as a disagreement at step %d", div.Step)
	}

	// A seed against itself has nothing to report, which is the case a
	// diff must not invent a divergence for.
	_, _, same := Diff(Without(cfg), Without(cfg))
	if same.Step != -1 {
		t.Fatalf("a run diffed against itself diverged at step %d", same.Step)
	}
}

// Scrubbing is only useful if the state it shows changes with the log.
func TestTheScrubShowsTheLogGrowing(t *testing.T) {
	trace := Replay(Config{Seed: 3, Steps: 20})
	if len(trace.States) == 0 {
		t.Fatal("the replay recorded no states")
	}

	var seenRecords, seenDigests int
	previous := trace.States[0]
	for _, s := range trace.States[1:] {
		if s.Records != previous.Records {
			seenRecords++
		}
		if s.Digest != previous.Digest {
			seenDigests++
		}
		if s.Tail < previous.Tail {
			t.Fatalf("step %d reports a tail of %d, below the %d before it", s.Step, s.Tail, previous.Tail)
		}
		previous = s
	}
	if seenRecords == 0 || seenDigests == 0 {
		t.Fatalf("across %d steps the record count changed %d times and the digest %d: the scrub shows nothing",
			len(trace.States), seenRecords, seenDigests)
	}
}
