package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/DamoDCoder/event-spine/core"
	"github.com/DamoDCoder/event-spine/log"
	"github.com/DamoDCoder/event-spine/runtime"
)

// powercut is the half of a power-loss test that runs inside the machine being
// cut. scripts/powercut.sh is the other half: it owns the block device, takes
// the snapshot, and decides when the power goes.
//
// Everything this repository claims about durability has until now rested on
// sim.FS modelling a disk correctly — see the final section of
// docs/decisions/m2-filesystem-model.md. This is the first test where a real
// kernel, a real filesystem, and a real block device decide what survives.
func powercut(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("powercut: write or verify")
	}
	switch args[0] {
	case "write":
		return powercutWrite(args[1:])
	case "verify":
		return powercutVerify(args[1:])
	default:
		return fmt.Errorf("powercut: unknown mode %q", args[0])
	}
}

// powercutEvent is the record at an index, derived from the index alone.
//
// No seed and no generator state: the verifier runs in a different process,
// after the machine it was written on stopped, and it has to rebuild the
// expected record from the offset and nothing else.
func powercutEvent(i int) core.Event {
	payload := make([]byte, 64)
	for j := range payload {
		payload[j] = byte(i + j)
	}
	return core.Event{
		Key:     fmt.Sprintf("acct-%04d", i%64),
		Time:    core.Time(i),
		Schema:  1,
		Payload: payload,
	}
}

func powercutWrite(args []string) error {
	fs := flag.NewFlagSet("powercut write", flag.ContinueOnError)
	var (
		dir   = fs.String("dir", "", "directory on the filesystem that will lose power")
		acked = fs.String("acked", "", "file to record durable counts in, on a filesystem that will not")
		batch = fs.Int("batch", 64, "records per append call")

		// Small segments so the run rolls often. The cut has to be able
		// to land during a segment creation and its directory sync,
		// which is where seeds/0001.md lived: with 64 MiB segments a
		// short run never rolls and never tests it. Writes are also
		// larger than a page, so a partial write-back can leave a torn
		// record rather than always ending on a write boundary.
		segment  = fs.Int64("segment-bytes", 64<<10, "segment size, small so the run rolls often")
		syncFreq = fs.Int("sync-every", 4, "append calls between syncs")

		// The negative control. It records counts as durable without
		// ever syncing, so a harness with teeth must fail the run: if
		// unsynced data survives a cut, the cut is not a power cut and
		// every passing run above it proved nothing.
		neverSync = fs.Bool("never-sync", false, "claim durability without syncing, to prove the cut has teeth")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dir == "" || *acked == "" {
		return fmt.Errorf("powercut write: --dir and --acked are required")
	}

	disk, err := runtime.NewFS(*dir)
	if err != nil {
		return err
	}

	// OS mode, so the only thing that ever reaches the disk durably is a
	// Sync this program asked for. Any other mode would let the log sync on
	// its own schedule and blur what the test is asserting.
	l, _, err := log.Open(disk, log.Config{
		Durability: log.OS,
		Segment:    log.Options{MaxBytes: *segment},
	})
	if err != nil {
		return err
	}
	defer l.Close()

	record, err := os.OpenFile(*acked, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("powercut: open %s: %w", *acked, err)
	}
	defer record.Close()

	events := make([]core.Event, *batch)
	written := 0

	// Until the power goes. The script kills this process; returning on its
	// own would mean the cut never happened.
	for call := 0; ; call++ {
		for i := range events {
			events[i] = powercutEvent(written + i)
		}
		offsets, err := l.Append(events...)
		if err != nil {
			return fmt.Errorf("powercut: append at %d: %w", written, err)
		}
		written += len(offsets)

		if call%*syncFreq != 0 {
			continue
		}
		if !*neverSync {
			if err := l.Sync(); err != nil {
				return fmt.Errorf("powercut: sync at %d: %w", written, err)
			}
		}

		// The claim being tested, written down only after Sync returned:
		// these records are durable. The record of it has to outlive the
		// cut, so it is fsynced onto a filesystem that is not being cut.
		if _, err := fmt.Fprintf(record, "%d\n", written); err != nil {
			return err
		}
		if err := record.Sync(); err != nil {
			return err
		}
	}
}

func powercutVerify(args []string) error {
	fs := flag.NewFlagSet("powercut verify", flag.ContinueOnError)
	var (
		dir     = fs.String("dir", "", "directory holding the log as it survived")
		acked   = fs.String("acked", "", "the durable counts the writer recorded")
		segment = fs.Int64("segment-bytes", 64<<10, "segment size the writer used")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dir == "" || *acked == "" {
		return fmt.Errorf("powercut verify: --dir and --acked are required")
	}

	durable, err := lastAcked(*acked)
	if err != nil {
		return err
	}

	disk, err := runtime.NewFS(*dir)
	if err != nil {
		return err
	}
	l, recovery, err := log.Open(disk, log.Config{
		Durability: log.OS,
		Segment:    log.Options{MaxBytes: *segment},
	})
	if err != nil {
		return fmt.Errorf("powercut: the log did not reopen after the cut: %w", err)
	}
	defer l.Close()

	if recovery.Corrupt != nil {
		return fmt.Errorf("powercut: recovery reported corruption: %w", recovery.Corrupt)
	}

	// The whole test in one comparison: every record the writer was told
	// was durable has to still be here, byte for byte.
	if int(l.Next()) < durable {
		return fmt.Errorf("powercut: %d records were acknowledged durable, the log recovered to %d across %d segments",
			durable, l.Next(), len(l.Segments()))
	}

	for i := range durable {
		got, err := l.Read(log.Offset(i))
		if err != nil {
			return fmt.Errorf("powercut: durable record %d was lost: %w", i, err)
		}
		want := powercutEvent(i)
		if got.Event.Key != want.Key || got.Event.Time != want.Time {
			return fmt.Errorf("powercut: record %d came back as %+v, want %+v", i, got.Event, want)
		}
		if !bytes.Equal(got.Event.Payload, want.Payload) {
			return fmt.Errorf("powercut: record %d has the wrong payload", i)
		}
	}

	fmt.Printf("survived: %d records acknowledged durable, %d recovered, %d segments, %d torn bytes discarded\n",
		durable, l.Next(), len(l.Segments()), recovery.Discarded)
	return nil
}

// lastAcked reads the highest durable count the writer managed to record.
func lastAcked(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("powercut: open %s: %w", path, err)
	}
	defer f.Close()

	var last int
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		n, err := strconv.Atoi(line)
		if err != nil {
			// A half-written final line is the writer being killed
			// mid-print, which is expected: the count before it is
			// the one that was acknowledged.
			break
		}
		last = n
	}
	return last, scanner.Err()
}
