package sim

import (
	"fmt"
	"io"
	"sort"

	"github.com/DamoDCoder/event-spine/internal/core"
)

// FS is a simulated filesystem: fast, deterministic, and crashable at any
// instant.
//
// It models three things a naive in-memory map does not, each because the real
// filesystem does and the log will eventually depend on the difference:
//
//   - Durability is separate from visibility. Appending changes what a reader
//     sees; only Sync changes what survives a crash.
//   - A directory entry is durable separately from the file it names. A file
//     synced into a directory that was not synced does not survive a crash,
//     which is why Rename-then-crash is the compaction hazard it is.
//   - Unlink is not deletion. An open handle keeps working after Remove or
//     Rename, because it holds the inode rather than the name.
//
// The model is a belief about how filesystems behave, and a wrong belief here
// produces a green crash matrix and real data loss. TestFSDifferential runs the
// same operations against this and against a real disk to keep the belief
// honest.
type FS struct {
	live    map[string]*inode
	durable map[string]*inode
}

// NewFS returns an empty simulated filesystem.
func NewFS() *FS {
	return &FS{
		live:    map[string]*inode{},
		durable: map[string]*inode{},
	}
}

// inode is a file's content, independent of any name it currently has.
type inode struct {
	data    []byte
	durable []byte
}

// Create makes a new empty file, refusing to replace an existing one.
func (f *FS) Create(name string) (core.File, error) {
	if err := core.ValidateName(name); err != nil {
		return nil, err
	}
	if _, taken := f.live[name]; taken {
		return nil, fmt.Errorf("sim: create %s: %w", name, core.ErrExist)
	}
	n := &inode{}
	f.live[name] = n
	return &simFile{fs: f, node: n, name: name}, nil
}

// Open opens an existing file for reading and appending.
func (f *FS) Open(name string) (core.File, error) {
	if err := core.ValidateName(name); err != nil {
		return nil, err
	}
	n, ok := f.live[name]
	if !ok {
		return nil, fmt.Errorf("sim: open %s: %w", name, core.ErrNotExist)
	}
	return &simFile{fs: f, node: n, name: name}, nil
}

// Remove deletes a directory entry. Handles already open on the file keep
// working, as they do on a real filesystem.
func (f *FS) Remove(name string) error {
	if err := core.ValidateName(name); err != nil {
		return err
	}
	if _, ok := f.live[name]; !ok {
		return fmt.Errorf("sim: remove %s: %w", name, core.ErrNotExist)
	}
	delete(f.live, name)
	return nil
}

// Rename atomically replaces newName with oldName.
func (f *FS) Rename(oldName, newName string) error {
	if err := core.ValidateName(oldName); err != nil {
		return err
	}
	if err := core.ValidateName(newName); err != nil {
		return err
	}
	n, ok := f.live[oldName]
	if !ok {
		return fmt.Errorf("sim: rename %s: %w", oldName, core.ErrNotExist)
	}
	if oldName == newName {
		return nil
	}
	delete(f.live, oldName)
	f.live[newName] = n
	return nil
}

// List returns the file names, sorted.
func (f *FS) List() ([]string, error) {
	names := make([]string, 0, len(f.live))
	for name := range f.live {
		names = append(names, name)
	}
	// The map above is ranged, which is normally forbidden. It is safe only
	// because the result is sorted before anyone sees it, and this sort is
	// the reason the interface promises sorted output at all.
	sort.Strings(names)
	return names, nil
}

// Sync makes the directory's current entries durable. It does not make file
// contents durable: fsync on a directory never has.
func (f *FS) Sync() error {
	f.durable = make(map[string]*inode, len(f.live))
	for name, n := range f.live {
		f.durable[name] = n
	}
	return nil
}

// CrashExtend is a power cut where a file's length outlived its contents.
//
// ext4 journals metadata and writes data separately, so a crash can leave a
// file *longer* than the bytes that reached the disk, with the gap reading back
// as zeros. Crash models the other case — the file reverts to what was synced —
// and for four milestones that was the only case the simulator could produce.
//
// scripts/powercut.sh found the difference the hard way. A sealed segment
// carrying acknowledged records and an unsynced tail came back from real ext4
// as a file with a hole of zeros in the middle, which recovery refused; the
// simulation of the same run passed, because its crash had shortened the file
// instead of zero-filling it and there was no damaged tail to refuse.
//
// A zero byte is not a valid record length, so this lands squarely on the
// framing checks rather than sneaking past them.
func (f *FS) CrashExtend() {
	live := make(map[string]*inode, len(f.durable))
	for name, n := range f.durable {
		grown := append([]byte(nil), n.durable...)
		if extra := len(n.data) - len(n.durable); extra > 0 {
			grown = append(grown, make([]byte, extra)...)
		}
		n.data = grown
		live[name] = n
	}
	f.live = live
}

// Overwrite replaces bytes already on the disk, which no caller of core.FS can
// do.
//
// This is bit rot, and it is deliberately not reachable through the interface:
// the log only ever appends, and an interface that cannot overwrite cannot
// corrupt a sealed segment by accident. A fault injector simulating a bad cable
// has to reach past the interface to do it, which is the right amount of
// friction.
//
// It changes the durable bytes as well as the live ones, because a flipped bit
// is on the platter. A crash that healed corruption would be a simulator making
// the disk more trustworthy than disks are.
func (f *FS) Overwrite(name string, off int64, data []byte) error {
	n, ok := f.live[name]
	if !ok {
		return fmt.Errorf("sim: overwrite %s: %w", name, core.ErrNotExist)
	}
	if off < 0 || off+int64(len(data)) > int64(len(n.data)) {
		return fmt.Errorf("sim: overwrite %s at %d: past the end of a %d byte file", name, off, len(n.data))
	}

	copy(n.data[off:], data)
	if off+int64(len(data)) <= int64(len(n.durable)) {
		copy(n.durable[off:], data)
	}
	return nil
}

// Clone returns an independent copy of the filesystem, live and durable bytes
// both.
//
// It exists for the replay tools, which have to inspect a running log without
// touching it. Opening a log mutates: recovery truncates a torn tail, a lookup
// caches a segment handle, and asking for a consumer group creates the commits
// file if it is missing. Every one of those changes what the run does next, so
// the tools inspect a copy and leave the original alone. Observation that
// changes the thing observed is not observation.
func (f *FS) Clone() *FS {
	clone := &FS{
		live:    make(map[string]*inode, len(f.live)),
		durable: make(map[string]*inode, len(f.durable)),
	}

	// Names are copied through one shared mapping, so two names for one
	// inode in the original are two names for one inode in the copy.
	copied := map[*inode]*inode{}
	for _, names := range []map[string]*inode{f.live, f.durable} {
		for _, n := range names {
			if _, done := copied[n]; done {
				continue
			}
			copied[n] = &inode{
				data:    append([]byte(nil), n.data...),
				durable: append([]byte(nil), n.durable...),
			}
		}
	}
	for name, n := range f.live {
		clone.live[name] = copied[n]
	}
	for name, n := range f.durable {
		clone.durable[name] = copied[n]
	}
	return clone
}

// Crash discards everything that was not durable, as a power cut would.
//
// Files whose directory entry was never synced disappear entirely. Files whose
// entry was synced come back holding only the bytes that were synced, which is
// the torn tail recovery exists to truncate.
//
// This is the optimistic half of what a real filesystem does. See CrashExtend
// for the other half, and for what it cost to learn the difference.
func (f *FS) Crash() {
	live := make(map[string]*inode, len(f.durable))
	for name, n := range f.durable {
		n.data = append([]byte(nil), n.durable...)
		live[name] = n
	}
	f.live = live
}

// simFile is a handle on an inode. It holds the inode rather than the name, so
// it survives a Remove or a Rename of the name it was opened under.
type simFile struct {
	fs     *FS
	node   *inode
	name   string
	closed bool
}

func (f *simFile) Append(p []byte) (int, error) {
	if f.closed {
		return 0, fmt.Errorf("sim: append %s: %w", f.name, core.ErrClosed)
	}
	f.node.data = append(f.node.data, p...)
	return len(p), nil
}

func (f *simFile) ReadAt(p []byte, off int64) (int, error) {
	if f.closed {
		return 0, fmt.Errorf("sim: read %s: %w", f.name, core.ErrClosed)
	}
	if off < 0 {
		return 0, fmt.Errorf("sim: read %s: negative offset %d", f.name, off)
	}
	if off >= int64(len(f.node.data)) {
		// Reading nothing at the end of a file is not an error, which is
		// the one case where io.ReaderAt's contract surprises people.
		if len(p) == 0 {
			return 0, nil
		}
		return 0, io.EOF
	}
	n := copy(p, f.node.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (f *simFile) Truncate(size int64) error {
	if f.closed {
		return fmt.Errorf("sim: truncate %s: %w", f.name, core.ErrClosed)
	}
	if size < 0 {
		return fmt.Errorf("sim: truncate %s: negative size %d", f.name, size)
	}
	if size > int64(len(f.node.data)) {
		return fmt.Errorf("sim: truncate %s to %d: file is %d bytes; growing is not supported",
			f.name, size, len(f.node.data))
	}
	f.node.data = f.node.data[:size]
	return nil
}

func (f *simFile) Size() (int64, error) {
	if f.closed {
		return 0, fmt.Errorf("sim: size %s: %w", f.name, core.ErrClosed)
	}
	return int64(len(f.node.data)), nil
}

func (f *simFile) Sync() error {
	if f.closed {
		return fmt.Errorf("sim: sync %s: %w", f.name, core.ErrClosed)
	}
	f.node.durable = append([]byte(nil), f.node.data...)
	return nil
}

func (f *simFile) Close() error {
	f.closed = true
	return nil
}
