package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
)

// Command kafkacompare measures the owned log against Kafka on one workload.
//
// It is its own module so the spine itself has no third-party dependencies: a
// consumer importing the log should not inherit a Kafka client. The protocol it
// follows is docs/decisions/m5-comparison-protocol.md.
func main() {
	if err := benchCompare(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "kafkacompare: %v\n", err)
		os.Exit(1)
	}
}

// benchCompare runs the identical workload through both systems, in every
// declared mode, and prints the table the M5 result quotes.
func benchCompare(args []string) error {
	fs := flag.NewFlagSet("bench compare", flag.ContinueOnError)
	var (
		broker  = fs.String("broker", "localhost:9092", "Kafka bootstrap broker")
		dir     = fs.String("dir", os.TempDir(), "where the spine's log directories are created")
		records = fs.Int("records", 20_000, "records per configuration")
		batch   = fs.Int("batch", 256, "records handed to each Append call")
		seed    = fs.Int64("seed", 1, "seed for the record stream both systems receive")
		sizes   = fs.String("sizes", "64,1024", "record sizes to measure, in bytes")
		only    = fs.String("only", "", "run only this backend: spine or kafka")
		repeat  = fs.Int("repeat", 3, "runs per configuration; the median is reported")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}

	widths, err := parseSizes(*sizes)
	if err != nil {
		return err
	}

	fmt.Printf("# spine against kafka, %d records per configuration, %d per append call\n", *records, *batch)
	fmt.Printf("# %d runs per configuration, median reported\n", *repeat)
	fmt.Printf("# sync rows hand over one record per call on both sides, so \"fsync per record\" is literal;\n")
	fmt.Printf("# batch and os rows hand over %d. Rows compare backends, not modes.\n", *batch)
	fmt.Printf("# protocol: docs/decisions/m5-comparison-protocol.md\n")
	fmt.Printf("# the spine is in-process; kafka is a broker across a socket. See the protocol.\n\n")
	fmt.Printf("%-7s %-6s %6s %12s %14s %10s %10s %11s %11s %12s\n",
		"backend", "mode", "bytes", "records/sec", "ns/record",
		"p50-ns", "p99-ns", "p999-ns", "max-ns", "read/sec")

	var failures int
	for _, size := range widths {
		for _, mode := range Modes {
			workload := Workload{
				Seed:      *seed,
				Records:   *records,
				Size:      size,
				BatchSize: *batch,
			}

			for _, backend := range []string{"spine", "kafka"} {
				if *only != "" && *only != backend {
					continue
				}
				results := make([]Result, 0, *repeat)
				var failed error
				for run := range *repeat {
					result, err := runOne(backend, *broker, *dir, workload, mode, run)
					if err != nil {
						failed = err
						break
					}
					results = append(results, result)
				}
				if failed != nil {
					failures++
					fmt.Printf("%-7s %-6s %6d  FAILED: %v\n", backend, mode, size, failed)
					continue
				}
				printResult(median(results))
			}
		}
	}

	if failures > 0 {
		return fmt.Errorf("bench compare: %d configurations failed", failures)
	}
	return nil
}

// runOne opens a backend, measures it, and closes it. A fresh backend per
// configuration, so no run inherits another's data.
func runOne(backend, broker, dir string, w Workload, mode Mode, run int) (Result, error) {
	var (
		b   Backend
		err error
	)

	switch backend {
	case "spine":
		b, err = OpenSpine(dir, mode)
	case "kafka":
		// A fresh topic per run as well as per configuration: appending
		// to one that already holds records would measure a log of a
		// different size.
		topic := fmt.Sprintf("spine-compare-%s-%d-%d", mode, w.Size, run)
		b, err = OpenKafka(broker, mode, topic)
	default:
		return Result{}, fmt.Errorf("unknown backend %q", backend)
	}
	if err != nil {
		return Result{}, err
	}
	defer b.Close()

	result := Run(b, w, mode)
	return result, result.Err
}

// median reports the middle run by append throughput.
//
// The median rather than the best, because the best of three is a number
// selected for being flattering, and rather than the mean because one 300
// millisecond outlier from a broker deciding to do something else moves a mean
// and does not move a median.
func median(results []Result) Result {
	sort.Slice(results, func(i, j int) bool {
		return results[i].RecordsPerSecond() < results[j].RecordsPerSecond()
	})
	return results[len(results)/2]
}

func printResult(r Result) {
	fmt.Printf("%-7s %-6s %6d %12.0f %14.0f %10.0f %10.0f %11.0f %11.0f %12.0f\n",
		r.Backend, r.Mode, r.Size,
		r.RecordsPerSecond(), r.PerRecordNanos(),
		r.Latency.P50, r.Latency.P99, r.Latency.P999, r.Latency.Max,
		r.ReadRecordsPerSecond())
}

func parseSizes(list string) ([]int, error) {
	var sizes []int
	for field := range strings.SplitSeq(list, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		var size int
		if _, err := fmt.Sscanf(field, "%d", &size); err != nil || size <= 0 {
			return nil, fmt.Errorf("bench: %q is not a record size", field)
		}
		sizes = append(sizes, size)
	}
	if len(sizes) == 0 {
		return nil, fmt.Errorf("bench: no record sizes given")
	}
	return sizes, nil
}
