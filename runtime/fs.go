// Package runtime implements core's injected dependencies against the real
// machine.
//
// This is the only package outside cmd and the simulator where time, os, and
// net may appear, which is what scripts/check-determinism.sh exempts it for.
// Everything here is an adapter: it holds no logic that a test would want to
// exercise, because logic here is logic the simulator cannot reach.
package runtime

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/DamoDCoder/event-spine/core"
)

// FS is a real directory on a real disk.
//
// One caveat worth stating where someone will read it: Sync means something
// different on each platform. On darwin, os.File.Sync issues F_FULLFSYNC, which
// does flush the drive's own write cache, so a sync there is stricter — and
// several milliseconds slower — than the fsync a Linux run performs. That
// difference is visible in bench/log.txt and is a property of the platform, not
// of this code. The authoritative test run is the Linux container for exactly
// this kind of reason.
type FS struct {
	dir string
}

// NewFS returns a filesystem rooted at dir, creating it if it is absent.
func NewFS(dir string) (*FS, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("runtime: create %s: %w", dir, err)
	}
	return &FS{dir: dir}, nil
}

// Dir returns the directory the filesystem is rooted at.
func (f *FS) Dir() string { return f.dir }

func (f *FS) path(name string) (string, error) {
	if err := core.ValidateName(name); err != nil {
		return "", err
	}
	return filepath.Join(f.dir, name), nil
}

// Create makes a new empty file, refusing to replace an existing one.
func (f *FS) Create(name string) (core.File, error) {
	p, err := f.path(name)
	if err != nil {
		return nil, err
	}
	h, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE|os.O_EXCL|os.O_APPEND, 0o644)
	if err != nil {
		return nil, translate(name, "create", err)
	}
	return &file{h: h, name: name}, nil
}

// Open opens an existing file for reading and appending.
func (f *FS) Open(name string) (core.File, error) {
	p, err := f.path(name)
	if err != nil {
		return nil, err
	}
	h, err := os.OpenFile(p, os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return nil, translate(name, "open", err)
	}
	return &file{h: h, name: name}, nil
}

// Remove deletes a file.
func (f *FS) Remove(name string) error {
	p, err := f.path(name)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil {
		return translate(name, "remove", err)
	}
	return nil
}

// Rename atomically replaces newName with oldName.
func (f *FS) Rename(oldName, newName string) error {
	from, err := f.path(oldName)
	if err != nil {
		return err
	}
	to, err := f.path(newName)
	if err != nil {
		return err
	}
	if err := os.Rename(from, to); err != nil {
		return translate(oldName, "rename", err)
	}
	return nil
}

// List returns the regular file names in the directory, sorted.
func (f *FS) List() ([]string, error) {
	entries, err := os.ReadDir(f.dir)
	if err != nil {
		return nil, fmt.Errorf("runtime: list %s: %w", f.dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		names = append(names, e.Name())
	}
	// ReadDir already sorts, but the interface promises sorted output and a
	// promise that depends on another package's documented side effect is a
	// promise waiting to be broken.
	sort.Strings(names)
	return names, nil
}

// Sync flushes the directory, making creations, renames, and removals durable.
func (f *FS) Sync() error {
	h, err := os.Open(f.dir)
	if err != nil {
		return fmt.Errorf("runtime: open %s for sync: %w", f.dir, err)
	}
	defer h.Close()
	if err := h.Sync(); err != nil {
		return fmt.Errorf("runtime: sync %s: %w", f.dir, err)
	}
	return nil
}

// file is a real file opened with O_APPEND, so a write cannot land anywhere but
// the end no matter what the caller believes the offset is.
type file struct {
	mu     sync.Mutex
	h      *os.File
	name   string
	closed bool
}

func (f *file) Append(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return 0, fmt.Errorf("runtime: append %s: %w", f.name, core.ErrClosed)
	}
	n, err := f.h.Write(p)
	if err != nil {
		return n, fmt.Errorf("runtime: append %s: %w", f.name, err)
	}
	if n != len(p) {
		return n, fmt.Errorf("runtime: append %s: %w: stored %d of %d bytes", f.name, core.ErrShortWrite, n, len(p))
	}
	return n, nil
}

func (f *file) ReadAt(p []byte, off int64) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return 0, fmt.Errorf("runtime: read %s: %w", f.name, core.ErrClosed)
	}
	if off < 0 {
		return 0, fmt.Errorf("runtime: read %s: negative offset %d", f.name, off)
	}
	// The io.EOF from a short read is passed through unwrapped: callers
	// compare against io.EOF, and wrapping it would break every one of them
	// that uses == rather than errors.Is.
	return f.h.ReadAt(p, off)
}

func (f *file) Truncate(size int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return fmt.Errorf("runtime: truncate %s: %w", f.name, core.ErrClosed)
	}
	if size < 0 {
		return fmt.Errorf("runtime: truncate %s: negative size %d", f.name, size)
	}
	info, err := f.h.Stat()
	if err != nil {
		return fmt.Errorf("runtime: truncate %s: %w", f.name, err)
	}
	if size > info.Size() {
		return fmt.Errorf("runtime: truncate %s to %d: file is %d bytes; growing is not supported",
			f.name, size, info.Size())
	}
	if err := f.h.Truncate(size); err != nil {
		return fmt.Errorf("runtime: truncate %s: %w", f.name, err)
	}
	return nil
}

func (f *file) Size() (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return 0, fmt.Errorf("runtime: size %s: %w", f.name, core.ErrClosed)
	}
	info, err := f.h.Stat()
	if err != nil {
		return 0, fmt.Errorf("runtime: size %s: %w", f.name, err)
	}
	return info.Size(), nil
}

func (f *file) Sync() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return fmt.Errorf("runtime: sync %s: %w", f.name, core.ErrClosed)
	}
	if err := f.h.Sync(); err != nil {
		return fmt.Errorf("runtime: sync %s: %w", f.name, err)
	}
	return nil
}

func (f *file) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil
	}
	f.closed = true
	if err := f.h.Close(); err != nil {
		return fmt.Errorf("runtime: close %s: %w", f.name, err)
	}
	return nil
}

// translate maps an operating system error onto the interface's vocabulary, so
// a caller cannot come to depend on the difference between what Linux said and
// what the simulator says.
func translate(name, op string, err error) error {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("runtime: %s %s: %w", op, name, core.ErrNotExist)
	case errors.Is(err, fs.ErrExist):
		return fmt.Errorf("runtime: %s %s: %w", op, name, core.ErrExist)
	default:
		return fmt.Errorf("runtime: %s %s: %w", op, name, err)
	}
}
