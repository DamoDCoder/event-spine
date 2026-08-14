package compare

import (
	"bytes"
	"testing"
)

// Both systems must receive the same records, or the comparison is measuring
// two different workloads and reporting the difference as a difference between
// the systems.
func TestBothBackendsGetTheSameRecords(t *testing.T) {
	w := Workload{Seed: 7, Records: 500, Size: 128, BatchSize: 16}

	first, second := w.Events(), w.Events()
	if len(first) != len(second) {
		t.Fatalf("the workload generated %d records and then %d", len(first), len(second))
	}
	for i := range first {
		if first[i].Key != second[i].Key || first[i].Time != second[i].Time {
			t.Fatalf("record %d differs between generations: %+v and %+v", i, first[i], second[i])
		}
		if !bytes.Equal(first[i].Payload, second[i].Payload) {
			t.Fatalf("record %d has a different payload between generations", i)
		}
	}

	// A different seed must produce a different stream, or the seed is
	// decoration.
	other := Workload{Seed: 8, Records: 500, Size: 128, BatchSize: 16}.Events()
	same := true
	for i := range first {
		if !bytes.Equal(first[i].Payload, other[i].Payload) {
			same = false
			break
		}
	}
	if same {
		t.Fatal("seeds 7 and 8 produced identical payloads")
	}
}

// The sync rows exist to make "fsync before acknowledging every record" true on
// both sides. The spine decides durability once per Append call, so a batched
// sync-mode call would be one fsync against Kafka's many — which is exactly the
// unfairness the first full run published before it was caught.
func TestSyncModeHandsOverOneRecordAtATime(t *testing.T) {
	w := Workload{Seed: 1, Records: 100, Size: 64, BatchSize: 256}

	if got := w.BatchFor(Sync); got != 1 {
		t.Fatalf("sync mode batches %d records per call, want 1", got)
	}
	for _, mode := range []Mode{Batch, OS} {
		if got := w.BatchFor(mode); got != w.BatchSize {
			t.Fatalf("%s mode batches %d records per call, want %d", mode, got, w.BatchSize)
		}
	}
}

// The spine half of the comparison runs without a broker, so the harness itself
// stays testable on a machine with nothing installed.
func TestTheSpineBackendRoundTrips(t *testing.T) {
	for _, mode := range Modes {
		spine, err := OpenSpine(t.TempDir(), mode)
		if err != nil {
			t.Fatalf("%s: OpenSpine: %v", mode, err)
		}

		w := Workload{Seed: 3, Records: 400, Size: 96, BatchSize: 32}
		result := Run(spine, w, mode)
		spine.Close()

		if result.Err != nil {
			t.Fatalf("%s: %v", mode, result.Err)
		}
		if result.Records != w.Records || result.ReadCount != w.Records {
			t.Fatalf("%s: appended %d and read back %d, want %d each",
				mode, result.Records, result.ReadCount, w.Records)
		}
		if result.RecordsPerSecond() <= 0 || result.ReadRecordsPerSecond() <= 0 {
			t.Fatalf("%s: reported %v records/sec and %v read/sec",
				mode, result.RecordsPerSecond(), result.ReadRecordsPerSecond())
		}
		if result.Latency.Samples == 0 {
			t.Fatalf("%s: no latency samples", mode)
		}
	}
}
