package bench

import (
	"testing"
	"time"
)

// Samples are injected rather than timed. A test that measured real durations
// would be asserting things about this machine's scheduler, and the arithmetic
// under test is what turns a benchmark run into a published number.
func withSamples(ns ...int) *Histogram {
	h := NewHistogram(len(ns))
	for _, n := range ns {
		h.samples = append(h.samples, time.Duration(n))
	}
	return h
}

func TestSummarizePicksRealSamples(t *testing.T) {
	// 1..1000, shuffled by construction: the largest arrive first, so an
	// implementation that forgot to sort would report the wrong tail.
	ns := make([]int, 0, 1000)
	for i := 1000; i >= 1; i-- {
		ns = append(ns, i)
	}
	got := withSamples(ns...).Summarize()

	want := Summary{Samples: 1000, P50: 501, P99: 991, P999: 1000, Max: 1000}
	if got != want {
		t.Fatalf("summary %+v, want %+v", got, want)
	}
}

// A percentile is a sample that some operation actually took, never an
// interpolation between two of them.
func TestPercentilesAreNotInterpolated(t *testing.T) {
	h := withSamples(10, 20, 30, 40, 1_000_000)
	got := h.Summarize()

	for name, v := range map[string]float64{"p50": got.P50, "p99": got.P99, "p999": got.P999} {
		if v != 30 && v != 1_000_000 {
			t.Fatalf("%s is %v, which is not one of the samples", name, v)
		}
	}
	if got.Max != 1_000_000 {
		t.Fatalf("max is %v, want the largest sample", got.Max)
	}
}

// Too few samples to have a thousandth means the answer is the maximum. That is
// reported honestly alongside the sample count rather than hidden.
func TestFewSamplesReportTheMaximum(t *testing.T) {
	got := withSamples(5, 7, 9).Summarize()
	if got.P999 != got.Max {
		t.Fatalf("p999 over 3 samples is %v, want the maximum %v", got.P999, got.Max)
	}
	if got.Samples != 3 {
		t.Fatalf("reported %d samples, want 3", got.Samples)
	}
}

func TestAnEmptyHistogramSummarizesToZero(t *testing.T) {
	if got := NewHistogram(0).Summarize(); got != (Summary{}) {
		t.Fatalf("empty histogram summarized to %+v", got)
	}
}
