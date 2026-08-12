// Package bench measures wall-clock latency for benchmarks.
//
// It reads the real clock, which nothing in a production path may do. It lives
// under internal/devtools for exactly that reason: the determinism check exempts
// this directory, and a benchmark that reported logical time would be measuring
// the simulator rather than the disk.
//
// Nothing here may be imported by a production package. A latency number is an
// observation about one machine on one afternoon, and a decision made from one
// would not reproduce.
package bench

import (
	"slices"
	"time"
)

// Histogram collects per-operation latencies.
//
// Samples are kept in full rather than bucketed. A run is bounded by the
// benchmark's iteration count, exact percentiles are cheaper to explain than
// interpolated ones, and bucket boundaries chosen now would be the wrong ones
// as soon as the numbers moved.
type Histogram struct {
	samples []time.Duration
	started time.Time
}

// NewHistogram returns a histogram sized for n samples.
func NewHistogram(n int) *Histogram {
	return &Histogram{samples: make([]time.Duration, 0, n)}
}

// Start marks the beginning of an operation.
func (h *Histogram) Start() { h.started = time.Now() }

// Stop records the time since Start.
//
// The pair costs two clock reads, tens of nanoseconds, which is a visible share
// of an append that takes a microsecond. That is why throughput and latency are
// measured by separate benchmarks rather than by one instrumented loop: the
// number that has to be right about throughput is not paid for by the number
// that has to be right about the tail.
func (h *Histogram) Stop() { h.samples = append(h.samples, time.Since(h.started)) }

// Len returns how many samples were recorded.
func (h *Histogram) Len() int { return len(h.samples) }

// Summary is a latency distribution in nanoseconds, ready for
// testing.B.ReportMetric.
type Summary struct {
	Samples int
	P50     float64
	P99     float64
	P999    float64
	Max     float64
}

// Summarize returns the distribution. It sorts the samples in place.
//
// A percentile is the sample at the rank at or above it, never an interpolation
// between two samples: an interpolated p99 is a number no operation actually
// took, and the point of a tail measurement is to name a real one.
func (h *Histogram) Summarize() Summary {
	if len(h.samples) == 0 {
		return Summary{}
	}
	slices.Sort(h.samples)

	return Summary{
		Samples: len(h.samples),
		P50:     h.quantile(0.50),
		P99:     h.quantile(0.99),
		P999:    h.quantile(0.999),
		Max:     float64(h.samples[len(h.samples)-1].Nanoseconds()),
	}
}

// quantile returns the sample at q, in nanoseconds. The samples must be sorted.
//
// With fewer than 1/(1-q) samples the answer is the maximum, which is honest but
// not informative: a p999 over 100 samples is the slowest of 100, not the
// slowest thousandth. Summary reports the sample count alongside so a reader can
// tell the difference.
func (h *Histogram) quantile(q float64) float64 {
	rank := int(q * float64(len(h.samples)))
	if rank >= len(h.samples) {
		rank = len(h.samples) - 1
	}
	return float64(h.samples[rank].Nanoseconds())
}
