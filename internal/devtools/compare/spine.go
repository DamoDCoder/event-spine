package compare

import (
	"errors"
	"fmt"
	"os"

	"github.com/DamoDCoder/event-spine/internal/core"
	"github.com/DamoDCoder/event-spine/internal/log"
	"github.com/DamoDCoder/event-spine/internal/runtime"
)

// Spine is the owned log behind the comparison interface.
//
// It runs against a real directory on a real disk, never the simulated
// filesystem: the simulator exists to make failure reproducible, and a
// throughput number taken from a map of byte slices would measure this
// repository's encoding and call it a log.
type Spine struct {
	log *log.Log
	dir string
}

// OpenSpine creates a log in a fresh directory under root.
func OpenSpine(root string, mode Mode) (*Spine, error) {
	dir, err := os.MkdirTemp(root, "spine-compare-")
	if err != nil {
		return nil, fmt.Errorf("compare: temporary directory: %w", err)
	}

	fs, err := runtime.NewFS(dir)
	if err != nil {
		return nil, err
	}

	cfg := log.Config{}
	switch mode {
	case Sync:
		cfg.Durability = log.Sync
	case Batch:
		cfg.Durability = log.Batch
		cfg.SyncRecords = FlushEvery
	case OS:
		cfg.Durability = log.OS
	default:
		return nil, fmt.Errorf("compare: unknown mode %q", mode)
	}

	l, _, err := log.Open(fs, cfg)
	if err != nil {
		return nil, err
	}
	return &Spine{log: l, dir: dir}, nil
}

func (s *Spine) Name() string { return "spine" }

func (s *Spine) Append(events []core.Event) error {
	_, err := s.log.Append(events...)
	return err
}

// ReadAll walks the log from the beginning with a cursor, which is how a
// consumer streams it.
func (s *Spine) ReadAll(want int) (int, error) {
	r, err := s.log.Reader(s.log.First())
	if err != nil {
		return 0, err
	}

	var seen int
	for seen < want {
		_, err := r.Next()
		if errors.Is(err, log.ErrEndOfLog) {
			break
		}
		if err != nil {
			return seen, err
		}
		seen++
	}
	return seen, nil
}

// Close closes the log and removes its directory. The comparison measures a
// fresh log every time, so leaving several gigabytes behind would be measuring
// the disk filling up.
func (s *Spine) Close() error {
	err := s.log.Close()
	if rmErr := os.RemoveAll(s.dir); err == nil {
		err = rmErr
	}
	return err
}
