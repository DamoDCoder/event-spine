package log

import (
	"errors"
	"fmt"
	"testing"

	"github.com/DamoDCoder/event-spine/internal/core"
	"github.com/DamoDCoder/event-spine/internal/devtools/bench"
	"github.com/DamoDCoder/event-spine/internal/runtime"
)

// Benchmarks run against a real disk, never the simulated filesystem. The
// simulator's job is to make failure reproducible, and a throughput number
// taken from a map of byte slices would measure this package's encoding and
// call it durability.

// Record sizes to publish against, from docs/log-design.md. Small enough that a
// run does not write tens of gigabytes, spread widely enough that per-record
// overhead and per-byte cost separate.
var benchSizes = []int{64, 1024}

// The three durability modes, which is the axis the M2 claim is stated on.
var benchModes = []struct {
	name string
	cfg  Config
}{
	{"sync", Config{Durability: Sync}},
	{"batch", Config{Durability: Batch, SyncRecords: 1024}},
	{"os", Config{Durability: OS}},
}

// benchEvent returns an event whose encoded record is close to size bytes.
func benchEvent(size int) core.Event {
	e := core.Event{Key: "acct-0001", Time: 1, Schema: 1}
	payload := size - HeaderLen - len(e.Key)
	if payload < 0 {
		payload = 0
	}
	e.Payload = make([]byte, payload)
	return e
}

func benchLog(b *testing.B, cfg Config) *Log {
	b.Helper()
	fs, err := runtime.NewFS(b.TempDir())
	if err != nil {
		b.Fatalf("real filesystem: %v", err)
	}
	l, _, err := Open(fs, cfg)
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	return l
}

// BenchmarkLogAppend is the throughput number. It is deliberately
// uninstrumented: the loop does nothing but append, so the only thing between
// the caller and the disk is the code being measured.
func BenchmarkLogAppend(b *testing.B) {
	for _, mode := range benchModes {
		for _, size := range benchSizes {
			b.Run(fmt.Sprintf("%s/%dB", mode.name, size), func(b *testing.B) {
				l := benchLog(b, mode.cfg)
				e := benchEvent(size)
				b.SetBytes(int64(RecordLen(e)))
				b.ReportAllocs()

				for b.Loop() {
					if _, err := l.Append(e); err != nil {
						b.Fatalf("Append: %v", err)
					}
				}

				b.StopTimer()
				closeLog(b, l)
			})
		}
	}
}

// BenchmarkLogAppendBatch measures the same appends handed over in one call,
// which is where the durability decision is amortised. The gap between this and
// BenchmarkLogAppend in sync mode is the cost of an fsync, stated as a number
// rather than as a warning in a comment.
func BenchmarkLogAppendBatch(b *testing.B) {
	const batchSize = 256

	for _, mode := range benchModes {
		b.Run(mode.name, func(b *testing.B) {
			l := benchLog(b, mode.cfg)
			batch := make([]core.Event, batchSize)
			for i := range batch {
				batch[i] = benchEvent(benchSizes[0])
			}
			b.SetBytes(int64(batchSize * RecordLen(batch[0])))
			b.ReportAllocs()

			for b.Loop() {
				if _, err := l.Append(batch...); err != nil {
					b.Fatalf("Append: %v", err)
				}
			}

			b.StopTimer()
			closeLog(b, l)
		})
	}
}

// BenchmarkLogAppendLatency reports the distribution rather than the mean.
//
// The M2 claim is about p99, and a mean hides exactly the events a p99 claim is
// made of: the fsync that waited on a flush, the append that crossed a segment
// boundary and paid for a file creation. Two clock reads per operation are
// added here and nowhere else, which is why the throughput benchmark above
// exists separately.
func BenchmarkLogAppendLatency(b *testing.B) {
	for _, mode := range benchModes {
		for _, size := range benchSizes {
			b.Run(fmt.Sprintf("%s/%dB", mode.name, size), func(b *testing.B) {
				l := benchLog(b, mode.cfg)
				e := benchEvent(size)
				h := bench.NewHistogram(b.N)
				b.SetBytes(int64(RecordLen(e)))

				for b.Loop() {
					h.Start()
					_, err := l.Append(e)
					h.Stop()
					if err != nil {
						b.Fatalf("Append: %v", err)
					}
				}

				b.StopTimer()
				report(b, h.Summarize())
				closeLog(b, l)
			})
		}
	}
}

// BenchmarkLogRead reads back from a warm log: every segment it touches is
// already open, so this is the sparse index and the record decode, without the
// scan a cold segment pays for.
//
// Offsets are visited on a stride coprime with the record count, so the walk
// covers every offset exactly once without ever reading two neighbours in a
// row. A sequential walk would measure the operating system's readahead.
func BenchmarkLogRead(b *testing.B) {
	const records = 200_000
	const stride = 7919 // prime, and coprime with records

	for _, size := range benchSizes {
		b.Run(fmt.Sprintf("%dB", size), func(b *testing.B) {
			l := benchLog(b, Config{Durability: OS})
			fillBench(b, l, records, size)

			// Warm every segment, so the cold-open cost is measured by
			// BenchmarkLogOpenCold and not smeared across these reads.
			for _, base := range l.Segments() {
				if _, err := l.Read(base); err != nil {
					b.Fatalf("warm segment %d: %v", base, err)
				}
			}
			b.ReportAllocs()
			b.ResetTimer()

			off := Offset(0)
			for b.Loop() {
				if _, err := l.Read(off); err != nil {
					b.Fatalf("Read %d: %v", off, err)
				}
				off = (off + stride) % records
			}

			b.StopTimer()
			closeLog(b, l)
		})
	}
}

// BenchmarkLogScan is the same records read through a cursor rather than by
// offset. The difference between this and BenchmarkLogRead is the sparse index
// search a random read repeats for every record, which a consumer streaming the
// log has no reason to pay.
func BenchmarkLogScan(b *testing.B) {
	const records = 200_000

	for _, size := range benchSizes {
		b.Run(fmt.Sprintf("%dB", size), func(b *testing.B) {
			l := benchLog(b, Config{Durability: OS})
			fillBench(b, l, records, size)

			r, err := l.Reader(0)
			if err != nil {
				b.Fatalf("Reader: %v", err)
			}
			b.SetBytes(int64(RecordLen(benchEvent(size))))
			b.ReportAllocs()
			b.ResetTimer()

			for b.Loop() {
				rec, err := r.Next()
				if errors.Is(err, ErrEndOfLog) {
					// Restarting mid-run costs one index search
					// per 200,000 records, which is below the
					// noise this measures.
					if err := r.Seek(0); err != nil {
						b.Fatalf("Seek: %v", err)
					}
					continue
				}
				if err != nil {
					b.Fatalf("Next: %v", err)
				}
				_ = rec
			}

			b.StopTimer()
			closeLog(b, l)
		})
	}
}

// BenchmarkLogOpen is the recovery number: the full scan an open performs over
// the active segment, which is the price of keeping no index file on disk.
//
// It is reported against record count rather than segment count because only
// the active segment is scanned. A log with a thousand sealed segments opens no
// more slowly than a log with one.
func BenchmarkLogOpen(b *testing.B) {
	for _, records := range []int{10_000, 100_000} {
		b.Run(fmt.Sprintf("%d-records", records), func(b *testing.B) {
			fs, err := runtime.NewFS(b.TempDir())
			if err != nil {
				b.Fatalf("real filesystem: %v", err)
			}
			// One segment large enough to hold the whole run, so the
			// scan measured is over every record written.
			cfg := Config{Segment: Options{MaxBytes: 1 << 30}, Durability: OS}
			l, _, err := Open(fs, cfg)
			if err != nil {
				b.Fatalf("Open: %v", err)
			}
			fillBench(b, l, records, benchSizes[0])
			if err := l.Sync(); err != nil {
				b.Fatalf("Sync: %v", err)
			}
			closeLog(b, l)

			h := bench.NewHistogram(b.N)
			b.ReportAllocs()

			for b.Loop() {
				h.Start()
				reopened, _, err := Open(fs, cfg)
				h.Stop()
				if err != nil {
					b.Fatalf("reopen: %v", err)
				}
				if reopened.Next() != Offset(records) {
					b.Fatalf("recovered to offset %d, want %d", reopened.Next(), records)
				}
				closeLog(b, reopened)
			}

			b.StopTimer()
			report(b, h.Summarize())
		})
	}
}

// BenchmarkLogReadCold is the cost the in-memory index decision defers: the
// first read into a sealed segment scans the whole file to rebuild its index.
//
// This is the number that decides whether the index belongs on disk after all.
// It is measured per segment opened, not per record read, because that is the
// unit the cost is paid in.
func BenchmarkLogReadCold(b *testing.B) {
	const segmentBytes = 4 << 20

	fs, err := runtime.NewFS(b.TempDir())
	if err != nil {
		b.Fatalf("real filesystem: %v", err)
	}
	cfg := Config{Segment: Options{MaxBytes: segmentBytes}, Durability: OS}
	l, _, err := Open(fs, cfg)
	if err != nil {
		b.Fatalf("Open: %v", err)
	}

	// Enough records to fill several segments, so every iteration can open
	// a sealed one it has not opened before.
	records := 8 * segmentBytes / benchSizes[0]
	fillBench(b, l, records, benchSizes[0])
	if err := l.Sync(); err != nil {
		b.Fatalf("Sync: %v", err)
	}
	bases := l.Segments()
	closeLog(b, l)

	h := bench.NewHistogram(b.N)
	b.ReportAllocs()

	for i := 0; b.Loop(); i++ {
		// A fresh log every iteration, so no segment is ever read from
		// the cache a previous iteration filled.
		b.StopTimer()
		cold, _, err := Open(fs, cfg)
		if err != nil {
			b.Fatalf("Open: %v", err)
		}
		base := bases[i%(len(bases)-1)] // never the active segment
		b.StartTimer()

		h.Start()
		_, err = cold.Read(base)
		h.Stop()
		if err != nil {
			b.Fatalf("cold read of segment %d: %v", base, err)
		}

		b.StopTimer()
		closeLog(b, cold)
		b.StartTimer()
	}

	b.StopTimer()
	report(b, h.Summarize())
}

// report publishes a latency distribution as benchmark metrics, so it lands in
// the committed results file beside ns/op instead of in a log line nobody diffs.
func report(b *testing.B, s bench.Summary) {
	b.Helper()
	b.ReportMetric(s.P50, "p50-ns")
	b.ReportMetric(s.P99, "p99-ns")
	b.ReportMetric(s.P999, "p999-ns")
	b.ReportMetric(s.Max, "max-ns")
	b.ReportMetric(float64(s.Samples), "samples")
}

// fillBench writes n records outside the measured region.
func fillBench(b *testing.B, l *Log, n, size int) {
	b.Helper()
	b.StopTimer()
	e := benchEvent(size)
	for range n {
		if _, err := l.Append(e); err != nil {
			b.Fatalf("fill: %v", err)
		}
	}
	b.StartTimer()
}

func closeLog(b *testing.B, l *Log) {
	b.Helper()
	if err := l.Close(); err != nil {
		b.Fatalf("Close: %v", err)
	}
}
