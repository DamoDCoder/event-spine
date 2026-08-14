package sim

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/DamoDCoder/event-spine/core"
	"github.com/DamoDCoder/event-spine/runtime"
)

// The simulated filesystem is a belief about how a real one behaves, and a
// wrong belief produces a green crash matrix and real data loss. This test runs
// the same seeded operation sequence against both and compares every observable
// result, so the belief fails here rather than in production.
//
// It runs against a real directory on a real disk. It is the test that should
// fail first when the model drifts, which is why it exists before the log that
// will depend on it rather than after.

// outcome is the comparable shape of an operation's result. Error messages
// differ between implementations by design, so only the classification is
// compared.
type outcome struct {
	op    string
	class string
	n     int
	data  string
	names string
	size  int64
}

func (o outcome) String() string {
	return fmt.Sprintf("op=%s class=%s n=%d size=%d data=%q names=%q",
		o.op, o.class, o.n, o.size, o.data, o.names)
}

// classify reduces an error to the vocabulary core.FS promises. An error
// outside that vocabulary is reported as "other" without its message, because
// the two implementations are entitled to word things differently — but they
// are not entitled to disagree about which case they are in.
func classify(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, io.EOF):
		return "eof"
	case errors.Is(err, core.ErrNotExist):
		return "notexist"
	case errors.Is(err, core.ErrExist):
		return "exist"
	case errors.Is(err, core.ErrClosed):
		return "closed"
	case errors.Is(err, core.ErrInvalidName):
		return "invalidname"
	case errors.Is(err, core.ErrShortWrite):
		return "shortwrite"
	default:
		return "other"
	}
}

// fsUnderTest pairs a filesystem with the handles opened on it, so the same
// script drives both.
type fsUnderTest struct {
	fs      core.FS
	handles []core.File
}

func (u *fsUnderTest) handle(i int) core.File {
	if len(u.handles) == 0 {
		return nil
	}
	return u.handles[i%len(u.handles)]
}

// script is one operation, expressed so it can be replayed against either
// implementation.
type step struct {
	kind    string
	name    string
	newName string
	payload []byte
	off     int64
	size    int64
	handle  int
}

// buildScript draws a sequence of operations from the seeded source. The draws
// are grouped here so the sequence a seed produces cannot change by accident.
func buildScript(seed int64, steps int) []step {
	src := NewSource(seed)
	names := []string{"00000000000000000000.log", "00000000000000000042.log", "offsets.log", "compacted.tmp"}

	// Names that must be rejected identically by both implementations. They
	// are in the script rather than in a separate test because a name from a
	// directory listing is untrusted input, and the boundary that rejects it
	// is the one being compared.
	bad := []string{"", ".", "..", "a/b", `a\b`, strings.Repeat("x", core.MaxNameLen+1)}

	pick := func() string {
		if src.Intn(16) == 0 {
			return bad[src.Intn(len(bad))]
		}
		return names[src.Intn(len(names))]
	}

	out := make([]step, 0, steps)
	for range steps {
		s := step{
			name:    pick(),
			newName: pick(),
			off:     int64(src.Intn(64)),
			size:    int64(src.Intn(64)),
			handle:  src.Intn(8),
		}
		payload := make([]byte, src.Intn(24))
		for i := range payload {
			payload[i] = byte(src.Intn(256))
		}
		s.payload = payload

		switch src.Intn(14) {
		case 0, 1:
			s.kind = "create"
		case 2, 3:
			s.kind = "open"
		case 4, 5, 6:
			s.kind = "append"
		case 7:
			s.kind = "readat"
		case 8:
			s.kind = "truncate"
		case 9:
			s.kind = "remove"
		case 10:
			s.kind = "rename"
		case 11:
			s.kind = "close"
		case 12:
			s.kind = "sync"
		default:
			s.kind = "list"
		}
		out = append(out, s)
	}
	return out
}

// apply runs one step and returns everything observable about it.
func apply(u *fsUnderTest, s step) outcome {
	o := outcome{op: s.kind}

	switch s.kind {
	case "create":
		f, err := u.fs.Create(s.name)
		o.class = classify(err)
		if err == nil {
			u.handles = append(u.handles, f)
		}

	case "open":
		f, err := u.fs.Open(s.name)
		o.class = classify(err)
		if err == nil {
			u.handles = append(u.handles, f)
		}

	case "append":
		h := u.handle(s.handle)
		if h == nil {
			o.class = "nohandle"
			break
		}
		n, err := h.Append(s.payload)
		o.class, o.n = classify(err), n
		if size, serr := h.Size(); serr == nil {
			o.size = size
		}

	case "readat":
		h := u.handle(s.handle)
		if h == nil {
			o.class = "nohandle"
			break
		}
		buf := make([]byte, 16)
		n, err := h.ReadAt(buf, s.off)
		o.class, o.n, o.data = classify(err), n, string(buf[:n])

	case "truncate":
		h := u.handle(s.handle)
		if h == nil {
			o.class = "nohandle"
			break
		}
		o.class = classify(h.Truncate(s.size))
		if size, serr := h.Size(); serr == nil {
			o.size = size
		}

	case "remove":
		o.class = classify(u.fs.Remove(s.name))

	case "rename":
		o.class = classify(u.fs.Rename(s.name, s.newName))

	case "close":
		h := u.handle(s.handle)
		if h == nil {
			o.class = "nohandle"
			break
		}
		o.class = classify(h.Close())

	case "sync":
		h := u.handle(s.handle)
		if h == nil {
			o.class = classify(u.fs.Sync())
			break
		}
		fileErr := h.Sync()
		dirErr := u.fs.Sync()
		o.class = classify(errors.Join(fileErr, dirErr))

	case "list":
		names, err := u.fs.List()
		o.class, o.names = classify(err), strings.Join(names, ",")
	}

	// Every step ends with the directory listing, so a divergence in what
	// the filesystem contains is caught at the step that caused it rather
	// than at the next list.
	if o.names == "" {
		if names, err := u.fs.List(); err == nil {
			o.names = strings.Join(names, ",")
		}
	}
	return o
}

func TestFSDifferentialAgainstRealDisk(t *testing.T) {
	for _, seed := range []int64{1, 2, 3, 4, 5, 17, 99, 1000} {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			real, err := runtime.NewFS(t.TempDir())
			if err != nil {
				t.Fatalf("real filesystem: %v", err)
			}
			simulated := &fsUnderTest{fs: NewFS()}
			actual := &fsUnderTest{fs: real}

			for i, s := range buildScript(seed, 200) {
				want := apply(actual, s)
				got := apply(simulated, s)
				if want != got {
					t.Fatalf("step %d (%s %q -> %q) diverged:\n  real %s\n  sim  %s",
						i, s.kind, s.name, s.newName, want, got)
				}
			}
		})
	}
}

// A differential test is only worth its runtime if the script still reaches the
// cases that matter. This pins that: a refactor that quietly stops generating
// invalid names, or stops closing handles, turns the test above into a slow way
// of confirming that two implementations agree about nothing interesting.
func TestDifferentialScriptReachesEveryFailureMode(t *testing.T) {
	want := []string{
		"create/ok", "create/exist", "create/invalidname",
		"open/ok", "open/notexist", "open/invalidname",
		"append/ok", "append/closed",
		"readat/ok", "readat/eof", "readat/closed",
		"truncate/ok", "truncate/other", "truncate/closed",
		"remove/ok", "remove/notexist",
		"rename/ok", "rename/notexist",
		"close/ok", "sync/ok", "list/ok",
	}

	seen := map[string]int{}
	for _, seed := range []int64{1, 2, 3, 4, 5, 17, 99, 1000} {
		u := &fsUnderTest{fs: NewFS()}
		for _, s := range buildScript(seed, 200) {
			o := apply(u, s)
			seen[o.op+"/"+o.class]++
		}
	}

	for _, w := range want {
		if seen[w] == 0 {
			t.Errorf("the script never produced %s, so nothing compares it", w)
		}
	}
}

// Durability is the half the differential script cannot reach, because a real
// filesystem in one process cannot be power-cut. These assert the model's
// stated rules directly, and docs/decisions/m2-filesystem-model.md records
// which of them a real disk has actually been observed to obey.
func TestSimCrashDropsExactlyWhatWasNotDurable(t *testing.T) {
	fs := NewFS()

	f, err := fs.Create("segment.log")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := f.Append([]byte("durable")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := f.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if _, err := f.Append([]byte("torn")); err != nil {
		t.Fatalf("Append: %v", err)
	}

	// The file's bytes are synced but its directory entry is not, so the
	// whole file is still lost. This is the rule that makes crash-during-
	// compaction interesting, and it is the one a naive model gets wrong.
	fs.Crash()
	if names, err := fs.List(); err != nil || len(names) != 0 {
		t.Fatalf("a file whose directory entry was never synced survived a crash: %v, %v", names, err)
	}

	fs = NewFS()
	f, _ = fs.Create("segment.log")
	if _, err := f.Append([]byte("durable")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := f.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if err := fs.Sync(); err != nil {
		t.Fatalf("FS.Sync: %v", err)
	}
	if _, err := f.Append([]byte("torn")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	fs.Crash()

	reopened, err := fs.Open("segment.log")
	if err != nil {
		t.Fatalf("the file did not survive a crash after both syncs: %v", err)
	}
	size, err := reopened.Size()
	if err != nil {
		t.Fatalf("Size: %v", err)
	}
	if size != int64(len("durable")) {
		t.Fatalf("file is %d bytes after the crash, want %d", size, len("durable"))
	}
	buf := make([]byte, size)
	if _, err := reopened.ReadAt(buf, 0); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if !bytes.Equal(buf, []byte("durable")) {
		t.Fatalf("file holds %q after the crash, want %q", buf, "durable")
	}
}

// An open handle holds the inode, not the name. Both implementations must agree,
// because compaction renames a segment out from under readers on purpose.
func TestOpenHandleSurvivesRemoveAndRename(t *testing.T) {
	check := func(t *testing.T, fs core.FS) {
		t.Helper()
		f, err := fs.Create("a.log")
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if _, err := f.Append([]byte("payload")); err != nil {
			t.Fatalf("Append: %v", err)
		}
		if err := fs.Rename("a.log", "b.log"); err != nil {
			t.Fatalf("Rename: %v", err)
		}
		if _, err := f.Append([]byte("-more")); err != nil {
			t.Fatalf("append after rename: %v", err)
		}
		if err := fs.Remove("b.log"); err != nil {
			t.Fatalf("Remove: %v", err)
		}

		size, err := f.Size()
		if err != nil {
			t.Fatalf("size after remove: %v", err)
		}
		if size != int64(len("payload-more")) {
			t.Fatalf("handle reports %d bytes after its name was removed, want %d", size, len("payload-more"))
		}
		buf := make([]byte, size)
		if _, err := f.ReadAt(buf, 0); err != nil {
			t.Fatalf("read after remove: %v", err)
		}
		if string(buf) != "payload-more" {
			t.Fatalf("handle holds %q, want %q", buf, "payload-more")
		}
	}

	t.Run("simulated", func(t *testing.T) { check(t, NewFS()) })
	t.Run("real", func(t *testing.T) {
		fs, err := runtime.NewFS(t.TempDir())
		if err != nil {
			t.Fatalf("real filesystem: %v", err)
		}
		check(t, fs)
	})
}
