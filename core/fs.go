package core

import (
	"errors"
	"fmt"
	"strings"
)

// The filesystem interface the log is built on.
//
// It is deliberately smaller than a real filesystem. Every method here is one
// the segmented log in docs/log-design.md needs, and nothing here exists to be
// general: a simulated implementation has to reproduce the observable behaviour
// of a real one exactly, and every method added is another place the two can
// drift apart without anyone noticing until a crash matrix passes green while
// real disks lose data.

// Filesystem errors. Both implementations return these, so callers classify on
// them rather than on whatever the operating system said.
var (
	// ErrNotExist is returned when a named file is absent.
	ErrNotExist = errors.New("core: file does not exist")

	// ErrExist is returned by Create when the name is taken. Create refuses
	// rather than truncating, because a log segment silently emptied by a
	// re-create is data loss that leaves no trace.
	ErrExist = errors.New("core: file already exists")

	// ErrClosed is returned by any operation on a closed file.
	ErrClosed = errors.New("core: file is closed")

	// ErrInvalidName is returned for a name that is not a single path
	// element. Names arrive from callers and from directory listings, and
	// both are untrusted.
	ErrInvalidName = errors.New("core: invalid file name")

	// ErrShortWrite is returned when an append stored fewer bytes than it
	// was given. A partial append that returned nil would be a torn record
	// nobody knows about.
	ErrShortWrite = errors.New("core: short write")
)

// MaxNameLen bounds a file name. The limit is well under any filesystem's, and
// exists so an absurd name fails at the boundary rather than three layers down.
const MaxNameLen = 255

// FS is a flat directory of append-only files.
//
// There are no subdirectories. A log lives in one directory, partitions are
// separate logs, and a filesystem that cannot nest is a filesystem with fewer
// behaviours to model incorrectly.
type FS interface {
	// Create makes a new empty file. It returns ErrExist if the name is
	// taken.
	Create(name string) (File, error)

	// Open opens an existing file for reading and appending. It returns
	// ErrNotExist if the name is absent.
	Open(name string) (File, error)

	// Remove deletes a file. It returns ErrNotExist if the name is absent.
	Remove(name string) error

	// Rename atomically replaces newName with oldName. This is the
	// primitive compaction relies on: a crash either leaves the old file in
	// place or the new one, never a half-written mixture.
	Rename(oldName, newName string) error

	// List returns the file names in the directory, sorted lexically.
	// Sorted, always: a caller that ranged an unsorted listing would build
	// its segment list in whatever order the filesystem felt like.
	List() ([]string, error)

	// Sync flushes the directory itself, making creations, renames, and
	// removals durable. Syncing a file does not make its directory entry
	// durable, and a segment that survives a crash under a name that does
	// not is a segment nobody will find.
	Sync() error
}

// File is an append-only file with random reads.
//
// There is no seek and no write-at-offset. The log only ever appends, and an
// interface that cannot overwrite is an interface that cannot corrupt a sealed
// segment by accident.
type File interface {
	// Append writes p at the end of the file and returns the number of
	// bytes written. It returns ErrShortWrite, wrapped with any underlying
	// cause, when it stores fewer than len(p).
	//
	// Append does not imply durability. Data is durable after Sync
	// returns, and not before.
	Append(p []byte) (int, error)

	// ReadAt fills p from off. It returns io.EOF when it reads fewer than
	// len(p) bytes because the file ended, along with the count it did
	// read, matching io.ReaderAt.
	ReadAt(p []byte, off int64) (int, error)

	// Truncate shortens the file to size. This is recovery's last step,
	// discarding a torn tail. Growing a file is not supported: a caller
	// asking for it has confused truncation with allocation.
	Truncate(size int64) error

	// Size returns the current length in bytes.
	Size() (int64, error)

	// Sync makes everything appended so far durable.
	Sync() error

	// Close releases the file. Close does not sync: a caller that wants
	// durability asks for it, and a Close that synced silently would hide
	// how expensive it is.
	Close() error
}

// ValidateName checks that name is a single path element.
//
// It rejects separators, the current and parent directory, and anything a
// directory listing could return that a caller might then feed back in. Names
// read from disk are untrusted in the same way decoded records are.
func ValidateName(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("%w: name is empty", ErrInvalidName)
	case len(name) > MaxNameLen:
		return fmt.Errorf("%w: name is %d bytes, limit is %d", ErrInvalidName, len(name), MaxNameLen)
	case name == "." || name == "..":
		return fmt.Errorf("%w: %q is a directory reference", ErrInvalidName, name)
	case strings.ContainsAny(name, `/\`):
		return fmt.Errorf("%w: %q contains a path separator", ErrInvalidName, name)
	case strings.ContainsRune(name, 0):
		return fmt.Errorf("%w: name contains a null byte", ErrInvalidName)
	}
	return nil
}
