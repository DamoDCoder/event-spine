package crash

import "testing"

// The matrix is the M2 durability claim's evidence, so it runs in the ordinary
// test suite as well as under `task sim:crash-matrix`. A handful of seeds here,
// the full sweep there.
func TestTheMatrixFindsNoLossAcrossEverySeededCrashPoint(t *testing.T) {
	var total Report

	for seed := int64(1); seed <= 8; seed++ {
		report, err := Matrix(seed, 0)
		if err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		if report.Points == 0 {
			t.Fatalf("seed %d exercised no crash points", seed)
		}
		for _, f := range report.Failures {
			t.Errorf("seed %d point %d: %v", seed, f.Point, f.Err)
		}

		total.Points += report.Points
		total.Compactions += report.Compactions
		total.Dropped += report.Dropped
		total.Snapshots += report.Snapshots
	}

	// A matrix that stopped compacting or snapshotting would report zero
	// failures and mean nothing by it. These are the assertions that keep
	// "no failures" from being a statement about a workload that no longer
	// does anything interesting.
	switch {
	case total.Compactions == 0:
		t.Error("no seed compacted anything, so the rename path was never crashed into")
	case total.Dropped == 0:
		t.Error("compaction ran but dropped no records, so no gaps were ever created")
	case total.Snapshots == 0:
		t.Error("no seed took a snapshot")
	}
	t.Logf("%d crash points, %d compactions dropping %d records, %d snapshots",
		total.Points, total.Compactions, total.Dropped, total.Snapshots)
}

// A crash point is only a reproduction if the run that produced it is
// reproducible. Two runs of one seed must perform the same operations in the
// same order, or a corpus entry names a point that no longer means what it did.
func TestTheWorkloadIsReproducible(t *testing.T) {
	for seed := int64(1); seed <= 4; seed++ {
		first := NewFS(0)
		if _, err := runWorkload(first, seed); err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		second := NewFS(0)
		if _, err := runWorkload(second, seed); err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		if first.Ops() != second.Ops() {
			t.Fatalf("seed %d performed %d operations and then %d", seed, first.Ops(), second.Ops())
		}
	}
}

// The harness has to be able to fail. A matrix that passes because it never
// crashes anything is the most comfortable kind of broken.
func TestTheCrashPointActuallyCrashes(t *testing.T) {
	fs := NewFS(3)
	if _, err := runWorkload(fs, 1); err == nil {
		t.Fatal("a workload with a crash point at operation 3 ran to completion")
	}
	if !fs.Crashed() {
		t.Fatal("the filesystem reported no crash")
	}
	if fs.Ops() != 3 {
		t.Fatalf("the crash landed after %d operations, want 3", fs.Ops())
	}
}
