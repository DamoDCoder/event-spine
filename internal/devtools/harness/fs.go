package harness

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
var ErrCrashed = errors.New("harness: the machine stopped here")

// errInjected is what a deliberate disk failure looks like to the log. It
// deliberately does not wrap anything the log matches on, because a log that
// only handled failures it recognised would be a log that handles none.
var errInjected = errors.New("harness: injected disk failure")

// FS is the simulated filesystem with faults scheduled against it.
//
// Operations are counted rather than timed, which is what makes a fault
// addressable by an integer: seed 41 with `crash@137` is a complete description
// of a failure, and a complete description is what a corpus file can hold.
type FS struct {
	fs *sim.FS

	// byOp is every disk fault, keyed by the operation it fires on. More
	// than one fault may share an operation, so a minimizer removing one
	// does not silently reschedule another.
	byOp map[int][]Fault

	ops     int
	crashed bool

	// lastWritten is the file a bit flip lands on: the one most recently
	// appended to, which is where a real bit rot would be most quickly
	// noticed and is the only choice that does not need a second integer
	// in the fault.
	lastWritten string

	// fired records which faults actually took effect. A run whose faults
	// were all scheduled past the end of the workload is a run with no
	// faults, and the difference matters to a minimizer.
	fired []Fault

	// signature is a running hash of every operation the run performed: its
	// index, its kind, and the file it touched.
	//
	// A fault's position is an ordinal in this stream, so the stream is what
	// a stored seed's meaning depends on. Counting operations catches a
	// stream that got longer; it does not catch one the same length in a
	// different order, which is exactly what reordering two syncs would
	// produce. The signature catches both.
	signature uint64

	// trace records what happened, for the devtools rather than the checks.
	// It is only kept when someone asks, because a sweep of thousands of
	// seeds has no use for it.
	trace   []Op
	tracing bool

	// atStep is the workload step currently running, so an operation can be
	// mapped back to the action that caused it. The workload sets it; the
	// filesystem only reports it.
	atStep int
}

// AtStep tells the filesystem which workload step is running.
func (f *FS) AtStep(step int) { f.atStep = step }

// Trace turns operation recording on. A sweep of thousands of seeds has no use
// for the trace, so it is off unless a tool asks.
func (f *FS) Tracing(on bool) { f.tracing = on }

// Op is one filesystem operation, as the replay tools show it.
type Op struct {
	// Index is the operation's ordinal, which is what a fault's position
	// names.
	Index int

	// Kind is the operation performed, and Name the file it touched.
	Kind string
	Name string

	// Step is the workload step the operation belongs to, which is how a
	// scrub maps an operation back to the action that caused it.
	Step int

	// Fault is the fault that fired here, if any.
	Fault *Fault
}

// NewFS returns a filesystem with the disk faults from cfg scheduled against
// it. Clock faults are the workload's business and are ignored here.
func NewFS(faults []Fault) *FS {
	byOp := map[int][]Fault{}
	for _, f := range faults {
		if f.Kind == ClockBack {
			continue
		}
		byOp[f.At] = append(byOp[f.At], f)
	}
	return &FS{fs: sim.NewFS(), byOp: byOp}
}

// Ops returns how many operations have been performed.
func (f *FS) Ops() int { return f.ops }

// Crashed reports whether a crash fault fired.
func (f *FS) Crashed() bool { return f.crashed }

// Fired returns the faults that actually took effect.
//
// A fault scheduled past the end of a run is a fault that did nothing, and a
// minimizer that could not tell the difference would keep it forever.
func (f *FS) Fired() []Fault { return f.fired }

// hasFired reports whether a fault of this kind took effect.
func (f *FS) hasFired(kind Kind) bool {
	for _, fault := range f.fired {
		if fault.Kind == kind {
			return true
		}
	}
	return false
}

// Simulated returns the underlying filesystem, which outlives the crash the way
// a disk outlives the machine that was writing to it.
func (f *FS) Simulated() *sim.FS { return f.fs }

// recovered returns a filesystem over the same disk with no faults scheduled,
// which is what the machine sees when it boots again.
func (f *FS) recovered() core.FS { return &FS{fs: f.fs, byOp: map[int][]Fault{}} }

// op counts one operation, records it, and applies whatever is scheduled
// against it.
//
// It returns the error the operation should fail with, or nil to proceed.
func (f *FS) op(kind, name string) error {
	if f.crashed {
		return ErrCrashed
	}
	f.ops++
	f.note(kind, name)

	for _, fault := range f.byOp[f.ops] {
		switch fault.Kind {
		case Crash:
			// The crash lands before the operation, so the
			// operation is one the disk never saw. Crashing after
			// it would make the point "the operation succeeded and
			// then the machine died", which is the next point
			// along.
			f.fs.Crash()
			f.crashed = true
			f.record(fault)
			return ErrCrashed

		case WriteError, SyncError:
			f.record(fault)
			return fmt.Errorf("%w at operation %d", errInjected, f.ops)

		case BitFlip:
			// A flip is not a failure of the operation it rides
			// on: the disk accepts the write and quietly holds
			// something else. Errors are ignored because a flip
			// that cannot be applied is a fault that did not fire.
			if f.flip(fault.Arg) {
				f.record(fault)
			}
		}
	}
	return nil
}

// note folds one operation into the signature, and records it when tracing.
//
// FNV-1a rather than a cryptographic hash: this identifies a stream, it does
// not defend against one. The index is folded in along with the kind and the
// name, so two operations swapping places changes the result.
func (f *FS) note(kind, name string) {
	const (
		offset64 = 14695981039346656037
		prime64  = 1099511628211
	)
	if f.signature == 0 {
		f.signature = offset64
	}

	fold := func(b byte) {
		f.signature ^= uint64(b)
		f.signature *= prime64
	}
	fold(byte(f.ops))
	fold(byte(f.ops >> 8))
	for i := range len(kind) {
		fold(kind[i])
	}
	for i := range len(name) {
		fold(name[i])
	}

	if f.tracing {
		f.trace = append(f.trace, Op{Index: f.ops, Kind: kind, Name: name, Step: f.atStep})
	}
}

// record marks a fault as fired, and attaches it to the operation being traced
// so a replay shows where it landed rather than only that it happened.
func (f *FS) record(fault Fault) {
	f.fired = append(f.fired, fault)
	if f.tracing && len(f.trace) > 0 {
		f.trace[len(f.trace)-1].Fault = &fault
	}
}

// Signature returns the hash of the operation stream so far.
func (f *FS) Signature() uint64 { return f.signature }

// Trace returns the operations performed, when tracing was on.
func (f *FS) Trace() []Op { return f.trace }

// flip inverts one bit of the file most recently written to, and reports
// whether it found something to corrupt.
func (f *FS) flip(arg int64) bool {
	if f.lastWritten == "" {
		return false
	}

	file, err := f.fs.Open(f.lastWritten)
	if err != nil {
		return false
	}
	defer file.Close()

	size, err := file.Size()
	if err != nil || size == 0 {
		return false
	}

	// The argument picks a byte. Taken modulo the file's length so a
	// minimizer can shrink the argument without the fault sliding off the
	// end of a file that has since grown.
	at := arg % size
	if at < 0 {
		at += size
	}
	buf := make([]byte, 1)
	if _, err := file.ReadAt(buf, at); err != nil {
		return false
	}
	buf[0] ^= 1 << uint(arg%8)
	return f.fs.Overwrite(f.lastWritten, at, buf) == nil
}

// shortWrite returns the number of bytes a ShortWrite fault lets through, and
// whether one is scheduled for this operation.
func (f *FS) shortWrite(total int) (int, bool) {
	for _, fault := range f.byOp[f.ops] {
		if fault.Kind != ShortWrite {
			continue
		}
		// At least one byte and never the whole buffer: a short write
		// that wrote everything is not a short write, and one that
		// wrote nothing is a write error under another name. Both are
		// already in the catalogue.
		n := int(fault.Arg) % total
		if n <= 0 {
			n = total / 2
		}
		if n == 0 {
			return 0, false
		}
		f.fired = append(f.fired, fault)
		return n, true
	}
	return 0, false
}

func (f *FS) Create(name string) (core.File, error) {
	if err := f.op("create", name); err != nil {
		return nil, err
	}
	file, err := f.fs.Create(name)
	if err != nil {
		return nil, err
	}
	return &faultyFile{File: file, fs: f, name: name}, nil
}

func (f *FS) Open(name string) (core.File, error) {
	if err := f.op("open", name); err != nil {
		return nil, err
	}
	file, err := f.fs.Open(name)
	if err != nil {
		return nil, err
	}
	return &faultyFile{File: file, fs: f, name: name}, nil
}

func (f *FS) Remove(name string) error {
	if err := f.op("remove", name); err != nil {
		return err
	}
	return f.fs.Remove(name)
}

func (f *FS) Rename(oldName, newName string) error {
	if err := f.op("rename", oldName); err != nil {
		return err
	}
	return f.fs.Rename(oldName, newName)
}

// List does not count as an operation. Reading a directory changes nothing, so
// a fault between two listings is indistinguishable from one before both.
func (f *FS) List() ([]string, error) {
	if f.crashed {
		return nil, ErrCrashed
	}
	return f.fs.List()
}

func (f *FS) Sync() error {
	if err := f.op("syncdir", "."); err != nil {
		return err
	}
	return f.fs.Sync()
}

// faultyFile counts the operations a file handle performs. Reads are not
// counted, for the same reason listings are not.
type faultyFile struct {
	core.File
	fs   *FS
	name string
}

func (f *faultyFile) Append(p []byte) (int, error) {
	f.fs.lastWritten = f.name
	if err := f.fs.op("append", f.name); err != nil {
		return 0, err
	}
	if n, ok := f.fs.shortWrite(len(p)); ok {
		written, err := f.File.Append(p[:n])
		if err != nil {
			return written, err
		}
		return written, fmt.Errorf("%w: %d of %d bytes", errInjected, written, len(p))
	}
	return f.File.Append(p)
}

func (f *faultyFile) Sync() error {
	if err := f.fs.op("sync", f.name); err != nil {
		return err
	}
	return f.File.Sync()
}

func (f *faultyFile) Truncate(size int64) error {
	if err := f.fs.op("truncate", f.name); err != nil {
		return err
	}
	return f.File.Truncate(size)
}

func (f *faultyFile) ReadAt(p []byte, off int64) (int, error) {
	if f.fs.crashed {
		return 0, ErrCrashed
	}
	return f.File.ReadAt(p, off)
}

func (f *faultyFile) Close() error {
	if f.fs.crashed {
		// A closed handle after a crash is not an error worth
		// propagating: the process is over and the file is gone with
		// it.
		return nil
	}
	return f.File.Close()
}

// assert is a small helper: a run reports the first invariant it broke, and
// every check reads better as a sentence than as an if.
func assert(ok bool, format string, args ...any) error {
	if ok {
		return nil
	}
	return fmt.Errorf(format, args...)
}
