package runtime_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"

	"github.com/DamoDCoder/event-spine/internal/runtime"
)

// The end-to-end test against a real process and a real disk.
//
// docs/decisions/m1-deterministic-core.md named M2's riskiest assumption: the
// simulated filesystem models torn writes as the author believes they behave,
// and a wrong belief produces a green crash matrix and real data loss. This test
// is written before the log that will depend on the model, because a test added
// afterwards is a test written to agree with the code.
//
// What it proves: a process killed mid-record leaves a real partial record on a
// real disk, and everything appended before the kill is still there.
//
// What it cannot prove, stated plainly so nobody reads more into a green run
// than it earned: SIGKILL destroys a process, not a machine. The page cache
// survives, so bytes appended without fsync are still readable afterwards. The
// simulator's Crash models power loss, where they would not be. Validating that
// half needs a real power cut or a fault-injecting block device, and until one
// of those exists the durability rules in sim.FS are a belief this test does not
// reach. See docs/decisions/m2-filesystem-model.md.

const (
	childEnv  = "EVENT_SPINE_FS_CRASH_CHILD"
	childDir  = "EVENT_SPINE_FS_CRASH_DIR"
	recordLen = 16

	// wholeRecords is how many complete records the child writes before it
	// starts the one it will not finish.
	wholeRecords = 6

	// tornPrefix is how much of the final record reaches the file. A crash
	// mid-record is the normal outcome of a crash during an append, and the
	// point of the whole exercise is that recovery must find this and
	// truncate it.
	tornPrefix = 8
)

func record(i int) []byte {
	b := make([]byte, recordLen)
	binary.LittleEndian.PutUint64(b[:8], uint64(i))
	for j := 8; j < recordLen; j++ {
		b[j] = 0xAA
	}
	return b
}

// child appends records to a real file and then kills itself partway through
// one. The kill point is chosen rather than timed, so the test is not racing
// anything and cannot flake.
func child() {
	dir := os.Getenv(childDir)
	fs, err := runtime.NewFS(dir)
	if err != nil {
		os.Exit(2)
	}
	f, err := fs.Create("segment.log")
	if err != nil {
		os.Exit(3)
	}
	for i := range wholeRecords {
		if _, err := f.Append(record(i)); err != nil {
			os.Exit(4)
		}
	}
	if err := f.Sync(); err != nil {
		os.Exit(5)
	}
	if err := fs.Sync(); err != nil {
		os.Exit(6)
	}
	// The record that will never finish.
	if _, err := f.Append(record(wholeRecords)[:tornPrefix]); err != nil {
		os.Exit(7)
	}
	// No Close, no Sync, no deferred anything: this is a crash.
	syscall.Kill(os.Getpid(), syscall.SIGKILL)
	os.Exit(8) // unreachable unless the kill was somehow ignored
}

func TestRealProcessKilledMidRecordLeavesATornTail(t *testing.T) {
	if os.Getenv(childEnv) == "1" {
		child()
		return
	}

	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=TestRealProcessKilledMidRecordLeavesATornTail")
	cmd.Env = append(os.Environ(), childEnv+"=1", childDir+"="+dir)
	out, err := cmd.CombinedOutput()

	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("the child exited cleanly (%v); it was supposed to be killed\n%s", err, out)
	}
	status, ok := exit.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatalf("the child ended with %v, want death by SIGKILL\n%s", exit, out)
	}

	fs, err := runtime.NewFS(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	names, err := fs.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(names) != 1 || names[0] != "segment.log" {
		t.Fatalf("directory holds %v, want just segment.log", names)
	}

	f, err := fs.Open("segment.log")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()

	size, err := f.Size()
	if err != nil {
		t.Fatalf("Size: %v", err)
	}
	wantSize := int64(wholeRecords*recordLen + tornPrefix)
	if size != wantSize {
		t.Fatalf("file is %d bytes after the kill, want %d whole records plus a %d byte tail",
			size, wholeRecords, tornPrefix)
	}

	buf := make([]byte, size)
	if _, err := f.ReadAt(buf, 0); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	for i := range wholeRecords {
		got := buf[i*recordLen : (i+1)*recordLen]
		if !bytes.Equal(got, record(i)) {
			t.Fatalf("record %d reads %x, want %x", i, got, record(i))
		}
	}
	if tail := buf[wholeRecords*recordLen:]; !bytes.Equal(tail, record(wholeRecords)[:tornPrefix]) {
		t.Fatalf("torn tail reads %x, want the first %d bytes of record %d", tail, tornPrefix, wholeRecords)
	}
}
