package harness

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/DamoDCoder/event-spine/sim"
)

// Shape is the size of the run: how big segments are, how far apart index
// entries sit, how large records get, and how many go into one call.
//
// These were constants for four milestones. The durability mode was a constant
// too, and when it stopped being one the first sweep found two bugs — one of
// them a panic reachable since compaction landed. There is no reason to think
// that axis was special, so the rest of the shape is drawn from the seed now.
//
// A zero field means the value every seed used before this existed, so the
// corpus keeps meaning what it meant.
type Shape struct {
	// SegmentBytes is when a segment rolls. Small values roll constantly,
	// which is where crashes during creation and directory syncs live.
	SegmentBytes int64

	// IndexInterval is the gap between sparse index entries. It bounds the
	// forward scan a lookup performs, so a large value with small records
	// makes every read walk a long way.
	IndexInterval int64

	// MaxPayload bounds a record's payload. Records larger than a segment
	// are the interesting end: a segment that cannot hold one record has to
	// hold it anyway.
	MaxPayload int

	// MaxBatch bounds how many records one Append call carries, which is
	// how much a single write puts at risk.
	MaxBatch int

	// SyncRecords is batch mode's record count between syncs.
	SyncRecords int
}

// Shape defaults, which are the values every seed recorded before the shape was
// part of a configuration.
const (
	DefaultSegmentBytes  int64 = 4 << 10
	DefaultIndexInterval int64 = 256
	DefaultMaxPayload          = 200
	DefaultMaxBatch            = 4
	DefaultSyncRecords         = 16
)

func (s Shape) withDefaults() Shape {
	if s.SegmentBytes <= 0 {
		s.SegmentBytes = DefaultSegmentBytes
	}
	if s.IndexInterval <= 0 {
		s.IndexInterval = DefaultIndexInterval
	}
	if s.MaxPayload <= 0 {
		s.MaxPayload = DefaultMaxPayload
	}
	if s.MaxBatch <= 0 {
		s.MaxBatch = DefaultMaxBatch
	}
	if s.SyncRecords <= 0 {
		s.SyncRecords = DefaultSyncRecords
	}
	return s
}

// IsDefault reports whether the shape is the one a seed file may omit.
func (s Shape) IsDefault() bool { return s == Shape{}.withDefaults() || s == Shape{} }

// String renders the shape as a corpus entry stores it.
func (s Shape) String() string {
	d := s.withDefaults()
	return fmt.Sprintf("seg=%d index=%d payload=%d batch=%d syncrecords=%d",
		d.SegmentBytes, d.IndexInterval, d.MaxPayload, d.MaxBatch, d.SyncRecords)
}

// ParseShape reads a shape back from a seed file.
func ParseShape(s string) (Shape, error) {
	var shape Shape
	for field := range strings.FieldsSeq(strings.TrimSpace(s)) {
		key, value, ok := strings.Cut(field, "=")
		if !ok {
			return shape, fmt.Errorf("harness: %q is not key=value", field)
		}
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return shape, fmt.Errorf("harness: %q has a bad value: %w", field, err)
		}

		switch key {
		case "seg":
			shape.SegmentBytes = n
		case "index":
			shape.IndexInterval = n
		case "payload":
			shape.MaxPayload = int(n)
		case "batch":
			shape.MaxBatch = int(n)
		case "syncrecords":
			shape.SyncRecords = int(n)
		default:
			return shape, fmt.Errorf("harness: %q is not part of a shape", key)
		}
	}
	return shape, nil
}

// shapeFor draws a run's shape from its seed.
//
// The values are chosen from short lists rather than from a range, so a sweep
// revisits the same interesting sizes often enough to find what lives at them
// instead of scattering thinly across everything.
func shapeFor(src *sim.Source) Shape {
	segments := []int64{512, 4 << 10, 64 << 10}
	intervals := []int64{64, 256, 4 << 10}
	payloads := []int{16, 200, 2000}
	batches := []int{1, 4, 32}
	syncs := []int{1, 16, 256}

	return Shape{
		SegmentBytes:  segments[src.Intn(len(segments))],
		IndexInterval: intervals[src.Intn(len(intervals))],
		MaxPayload:    payloads[src.Intn(len(payloads))],
		MaxBatch:      batches[src.Intn(len(batches))],
		SyncRecords:   syncs[src.Intn(len(syncs))],
	}
}
