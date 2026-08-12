package main

import (
	"os"
	"path/filepath"
	"testing"
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
	write("0007.md", "---\nseed: 7\npoint: 3\nfound: 2026-08-12\n---\n\nA torn tail.\n")
	write("0002.md", "---\nseed: 2\npoint: 41\nfixed_in: abc1234\n---\n\nAnother one.\n")

	got, err := loadCorpus(dir)
	if err != nil {
		t.Fatalf("loadCorpus: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("loaded %d entries, want 2: %+v", len(got), got)
	}
	// Sorted by seed, so a corpus run reports in one order.
	if got[0].seed != 2 || got[0].point != 41 {
		t.Fatalf("first entry is %+v, want seed 2 point 41", got[0])
	}
	if got[1].seed != 7 || got[1].point != 3 {
		t.Fatalf("second entry is %+v, want seed 7 point 3", got[1])
	}
}

// A seed file that says nothing useful is an error rather than a silent skip.
func TestACorpusEntryNeedsBothIntegers(t *testing.T) {
	for name, body := range map[string]string{
		"no seed":      "---\npoint: 3\n---\n",
		"no point":     "---\nseed: 7\n---\n",
		"seed is text": "---\nseed: soon\npoint: 3\n---\n",
	} {
		if _, err := parseCorpusEntry("0001.md", body); err == nil {
			t.Errorf("%s: parsed without error", name)
		}
	}
}
