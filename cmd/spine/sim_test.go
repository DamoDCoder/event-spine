package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/DamoDCoder/event-spine/internal/devtools/harness"
)

// A corpus that parses nothing reports success, which is the failure mode worth
// guarding: the regression suite would go quiet exactly when it stopped running.
func TestLoadCorpusReadsEverySeedFile(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	write("README.md", "# Seed Corpus\n\nseed: not a number\n")
	write("notes.txt", "seed: 99\npoint: 1\n")
	write("0007.md", "---\nseed: 7\nsteps: 12\nfaults: bitflip@3:9 clockback@2\n---\n\nA flipped byte.\n")
	write("0002.md", "---\nseed: 2\npoint: 41\nfixed_in: abc1234\n---\n\nThe legacy shorthand.\n")

	got, err := loadCorpus(dir)
	if err != nil {
		t.Fatalf("loadCorpus: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("loaded %d entries, want 2: %+v", len(got), got)
	}

	// Sorted by seed, so a corpus run reports in one order.
	legacy, current := got[0], got[1]
	if legacy.cfg.Seed != 2 {
		t.Fatalf("first entry is seed %d, want 2", legacy.cfg.Seed)
	}
	// The pre-catalogue shorthand still replays: a seed nobody can run is
	// deleted in every sense that matters.
	if len(legacy.cfg.Faults) != 1 || legacy.cfg.Faults[0] != (harness.Fault{Kind: harness.Crash, At: 41}) {
		t.Fatalf("the legacy point became %s, want crash@41", harness.FormatFaults(legacy.cfg.Faults))
	}

	if current.cfg.Seed != 7 || current.cfg.Steps != 12 {
		t.Fatalf("second entry is seed %d steps %d, want 7 and 12", current.cfg.Seed, current.cfg.Steps)
	}
	if got := harness.FormatFaults(current.cfg.Faults); got != "bitflip@3:9 clockback@2" {
		t.Fatalf("faults parsed as %q", got)
	}
}

// A seed file that says nothing replayable is an error rather than a silent
// skip.
func TestACorpusEntryNeedsASeedAndFaults(t *testing.T) {
	for name, body := range map[string]string{
		"no seed":      "---\nfaults: crash@3\n---\n",
		"no faults":    "---\nseed: 7\n---\n",
		"seed is text": "---\nseed: soon\nfaults: crash@3\n---\n",
		"unknown kind": "---\nseed: 7\nfaults: meteor@3\n---\n",
		"bad position": "---\nseed: 7\nfaults: crash@soon\n---\n",
	} {
		if _, err := parseCorpusEntry("0001.md", body); err == nil {
			t.Errorf("%s: parsed without error", name)
		}
	}
}

// A sweep must never overwrite a seed. The corpus is append-only by policy, and
// policy that depends on remembering is policy that fails.
func TestWritingACorpusEntryRefusesToOverwrite(t *testing.T) {
	dir := t.TempDir()
	failure := harness.Failure{
		Config: harness.Config{Seed: 12, Steps: 40, Faults: []harness.Fault{{Kind: harness.Crash, At: 7}}},
		Err:    os.ErrClosed,
	}

	path, err := writeCorpusEntry(dir, failure)
	if err != nil {
		t.Fatalf("writeCorpusEntry: %v", err)
	}

	entries, err := loadCorpus(dir)
	if err != nil {
		t.Fatalf("the written entry does not load: %v", err)
	}
	if len(entries) != 1 || entries[0].cfg.Seed != 12 {
		t.Fatalf("the written entry loaded as %+v", entries)
	}
	if got := harness.FormatFaults(entries[0].cfg.Faults); got != "crash@7" {
		t.Fatalf("the written entry's faults are %q, want crash@7", got)
	}

	if _, err := writeCorpusEntry(dir, failure); err == nil {
		t.Fatalf("writing %s a second time was allowed", path)
	}
}
