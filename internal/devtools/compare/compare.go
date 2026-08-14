// Package compare measures the owned log against Kafka on one workload.
//
// The protocol it implements — which knobs are declared equivalent, what is
// held identical, what is deliberately not compared, and what will not be
// claimed — is docs/decisions/m5-comparison-protocol.md, which was written and
// committed before this package existed. Reading the protocol first is the
// point of having written it first.
package compare

import (
	"fmt"

	"github.com/DamoDCoder/event-spine/internal/core"
	"github.com/DamoDCoder/event-spine/internal/devtools/bench"
	"github.com/DamoDCoder/event-spine/internal/log"
	"github.com/DamoDCoder/event-spine/internal/sim"
)

// Backend is the narrow interface both systems sit behind.
//
// docs/log-design.md writes it with Reader, Group, and Snapshot as well. This
// is the subset the comparison measures, defined where it is consumed rather
// than in a package that would exist only to hold it. What was not implemented
// over Kafka, and why, is recorded in the M5 result rather than hidden by a
// narrower interface.
type Backend interface {
	// Name identifies the backend in the published results.
	Name() string

	// Append writes a batch and returns when the write is acknowledged
	// under the durability mode the backend was opened with.
	Append(events []core.Event) error

	// ReadAll reads from the beginning until it has seen want records, and
	// returns how many it saw.
	ReadAll(want int) (int, error)

	Close() error
}

// Mode is a durability setting, paired across the two systems by the protocol.
type Mode string

const (
	// Sync fsyncs before acknowledging every record.
	Sync Mode = "sync"

	// Batch fsyncs once per FlushEvery records.
	Batch Mode = "batch"

	// OS never forces a flush and leaves it to the operating system.
	OS Mode = "os"
)

// Modes is every mode, in the order the results table prints them.
var Modes = []Mode{Sync, Batch, OS}

// FlushEvery is the N in "fsync once per N records", used by both systems in
// batch mode. It matches the spine's own default so the numbers here can be
// checked against bench/log.txt.
const FlushEvery = 1024

// Workload is the run both systems receive, byte for byte.
type Workload struct {
	// Seed generates the records, so both systems get the same stream.
	Seed int64

	// Records is how many to append, and Size the approximate encoded size
	// of each.
	Records int
	Size    int

	// BatchSize is how many records are handed over per Append call. The
	// API-level batch is held identical because it is the caller's choice
	// rather than the system's.
	BatchSize int
}

// Result is one backend in one mode.
type Result struct {
	Backend string
	Mode    Mode
	Size    int

	Records int
	Bytes   int64

	// AppendNanos is the wall time spent appending, and Latency the
	// distribution over individual Append calls.
	AppendNanos int64
	Latency     bench.Summary

	// ReadNanos is the wall time to read every record back from the start.
	ReadNanos int64
	ReadCount int

	Err error
}

// PerRecordNanos is the append cost per record, which is what the modes are
// compared on.
func (r Result) PerRecordNanos() float64 {
	if r.Records == 0 {
		return 0
	}
	return float64(r.AppendNanos) / float64(r.Records)
}

// RecordsPerSecond is the same number the other way up, for the report.
func (r Result) RecordsPerSecond() float64 {
	if r.AppendNanos == 0 {
		return 0
	}
	return float64(r.Records) / (float64(r.AppendNanos) / 1e9)
}

// ReadRecordsPerSecond is sequential read throughput.
func (r Result) ReadRecordsPerSecond() float64 {
	if r.ReadNanos == 0 {
		return 0
	}
	return float64(r.ReadCount) / (float64(r.ReadNanos) / 1e9)
}

// Events generates the workload's records.
//
// Both systems receive this same slice, so a difference in the numbers cannot
// be a difference in what was written.
func (w Workload) Events() []core.Event {
	src := sim.NewSource(w.Seed)
	events := make([]core.Event, w.Records)

	for i := range events {
		payload := w.Size - log.HeaderLen - len("acct-0000")
		if payload < 0 {
			payload = 0
		}
		body := make([]byte, payload)
		for j := range body {
			body[j] = byte(src.Intn(256))
		}
		events[i] = core.Event{
			Key:     fmt.Sprintf("acct-%04d", src.Intn(64)),
			Time:    core.Time(i),
			Schema:  1,
			Payload: body,
		}
	}
	return events
}

// BatchFor returns the API batch size to use for a mode.
//
// Sync mode uses one record per call and the others use the caller's batch.
// The protocol pairs spine `sync` with Kafka's flush.messages=1 on the grounds
// that "both fsync before acknowledging every record" — and that was only true
// of Kafka. The spine decides durability once per Append call, so a 256 record
// call in sync mode is one fsync, against Kafka's 256. The first full run was
// unfair to Kafka by the batch size in that row, which is the kind of error the
// protocol exists to catch and did, one layer later than intended.
//
// Handing over one record at a time in sync mode makes "every record" literally
// true on both sides. Rows are comparable across backends, not across modes.
func (w Workload) BatchFor(mode Mode) int {
	if mode == Sync {
		return 1
	}
	return w.BatchSize
}

// Run appends the workload through a backend and then reads it back.
//
// Latency is sampled per Append call, and the total is timed separately: the
// two clock reads per call are a visible share of an in-process append, which
// is why bench/log.txt keeps throughput and latency in separate benchmarks. The
// same caveat applies here and is reported with the numbers.
func Run(b Backend, w Workload, mode Mode) Result {
	events := w.Events()
	result := Result{Backend: b.Name(), Mode: mode, Size: w.Size, Records: len(events)}

	for _, e := range events {
		result.Bytes += int64(log.RecordLen(e))
	}

	size := w.BatchFor(mode)
	calls := (len(events) + size - 1) / size
	hist := bench.NewHistogram(calls)

	total := bench.NewHistogram(1)
	total.Start()
	for start := 0; start < len(events); start += size {
		end := min(start+size, len(events))

		hist.Start()
		err := b.Append(events[start:end])
		hist.Stop()
		if err != nil {
			result.Err = fmt.Errorf("append at record %d: %w", start, err)
			return result
		}
	}
	total.Stop()

	result.AppendNanos = int64(total.Summarize().Max)
	result.Latency = hist.Summarize()

	read := bench.NewHistogram(1)
	read.Start()
	n, err := b.ReadAll(len(events))
	read.Stop()
	if err != nil {
		result.Err = fmt.Errorf("read back: %w", err)
		return result
	}
	result.ReadNanos = int64(read.Summarize().Max)
	result.ReadCount = n

	return result
}
