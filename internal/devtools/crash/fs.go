// Package crash runs a log workload against a filesystem that dies at a chosen
// operation, then checks what survived.
//
// It lives under internal/devtools rather than internal/sim because the log's
// own tests import the simulator, and a simulator that imported the log back
// would be an import cycle. The dependency only runs one way: this package
// knows about both.
package crash

import (
	"errors"
	"fmt"

	"github.com/DamoDCoder/event-spine/internal/core"
	"github.com/DamoDCoder/event-spine/internal/sim"
)

// ErrCrashed is returned by every filesystem operation after the crash.
//
// A power cut does not return an error to the process, it ends the process. The
// error exists because a test binary has to keep running: it is how the
// workload learns to stop pretending it is still alive, and any code that
// treats it as a retryable failure is testing something that cannot happen.
var ErrCrashed = errors.New("crash: the machine stopped here")

// FS wraps the simulated filesystem and crashes it after a chosen number of
// operations.
//
// Operations are counted rather than timed, which is what makes a crash point
// an integer the corpus can store: seed 41 crashing at operation 137 is a
// complete description of a failure.
type FS struct {
	fs *sim.FS

	// at is the operation to crash after. Zero means never, which is how
	// the counting run measures how many points there are.
	at int

	ops     int
	crashed bool
}

// NewFS returns a filesystem that crashes after the at-th operation, or never
// when at is zero.
func NewFS(at int) *FS { return &FS{fs: sim.NewFS(), at: at} }

// Ops returns how many operations have been performed.
func (f *FS) Ops() int { return f.ops }

// Crashed reports whether the crash point was reached.
func (f *FS) Crashed() bool { return f.crashed }

// Simulated returns the underlying filesystem, which outlives the crash the way
// a disk outlives the machine that was writing to it.
func (f *FS) Simulated() *sim.FS { return f.fs }

// step counts one operation and reports whether the caller may proceed.
func (f *FS) step() error {
	if f.crashed {
		return ErrCrashed
	}
	f.ops++
	if f.at > 0 && f.ops >= f.at {
		// The crash lands before the operation is performed, so the
		// operation is one the disk never saw. Crashing after it would
		// make the point "the operation succeeded and then the machine
		// died", which is the next point along.
		f.fs.Crash()
		f.crashed = true
		return ErrCrashed
	}
	return nil
}

func (f *FS) Create(name string) (core.File, error) {
	if err := f.step(); err != nil {
		return nil, err
	}
	file, err := f.fs.Create(name)
	if err != nil {
		return nil, err
	}
	return &file_{File: file, fs: f}, nil
}

func (f *FS) Open(name string) (core.File, error) {
	if err := f.step(); err != nil {
		return nil, err
	}
	file, err := f.fs.Open(name)
	if err != nil {
		return nil, err
	}
	return &file_{File: file, fs: f}, nil
}

func (f *FS) Remove(name string) error {
	if err := f.step(); err != nil {
		return err
	}
	return f.fs.Remove(name)
}

func (f *FS) Rename(oldName, newName string) error {
	if err := f.step(); err != nil {
		return err
	}
	return f.fs.Rename(oldName, newName)
}

// List does not count as an operation. Reading a directory changes nothing, so
// a crash between two listings is indistinguishable from a crash before both.
func (f *FS) List() ([]string, error) {
	if f.crashed {
		return nil, ErrCrashed
	}
	return f.fs.List()
}

func (f *FS) Sync() error {
	if err := f.step(); err != nil {
		return err
	}
	return f.fs.Sync()
}

// file_ counts the operations a file handle performs. Reads are not counted,
// for the same reason listings are not.
type file_ struct {
	core.File
	fs *FS
}

func (f *file_) Append(p []byte) (int, error) {
	if err := f.fs.step(); err != nil {
		return 0, err
	}
	return f.File.Append(p)
}

func (f *file_) Sync() error {
	if err := f.fs.step(); err != nil {
		return err
	}
	return f.File.Sync()
}

func (f *file_) Truncate(size int64) error {
	if err := f.fs.step(); err != nil {
		return err
	}
	return f.File.Truncate(size)
}

func (f *file_) ReadAt(p []byte, off int64) (int, error) {
	if f.fs.crashed {
		return 0, ErrCrashed
	}
	return f.File.ReadAt(p, off)
}

func (f *file_) Close() error {
	if f.fs.crashed {
		// A closed handle after a crash is not an error worth
		// propagating: the process is over and the file is gone with it.
		return nil
	}
	return f.File.Close()
}

// recovered returns a filesystem over the same disk, without a crash point,
// which is what the machine sees when it boots again.
func (f *FS) recovered() core.FS { return &FS{fs: f.fs} }

// assert is a small helper: the matrix reports the first invariant a crash
// point broke, and every check reads better as a sentence than as an if.
func assert(ok bool, format string, args ...any) error {
	if ok {
		return nil
	}
	return fmt.Errorf(format, args...)
}
