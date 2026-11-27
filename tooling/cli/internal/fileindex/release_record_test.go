package fileindex

import (
	"os"
	"path/filepath"
	"testing"
)

// still answers a recheck with w: the witness has not moved.
func still(w ReleaseWitness) func() (ReleaseWitness, bool) {
	return func() (ReleaseWitness, bool) { return w, true }
}

func witnessOf(b byte) ReleaseWitness {
	var w ReleaseWitness
	w[0] = b
	return w
}

func releaseRecordPathFor(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), ".putnami", ReleaseRecordFilename)
}

func TestARecordedValueIsAnsweredUnderItsWitnessOnly(t *testing.T) {
	t.Parallel()
	path := releaseRecordPathFor(t)
	first := OpenReleaseRecord(path, "id")
	first.Store("/w/api", "api/v{version}", witnessOf(1), []byte("baseline"), still(witnessOf(1)))
	if err := first.Save(); err != nil {
		t.Fatal(err)
	}

	next := OpenReleaseRecord(path, "id")
	if value, ok := next.Lookup("/w/api", "api/v{version}", witnessOf(1)); !ok || string(value) != "baseline" {
		t.Fatalf("Lookup = %q, %v; want the recorded value", value, ok)
	}
	for _, miss := range []struct{ dir, pattern string }{{"/w/web", "api/v{version}"}, {"/w/api", "v{version}"}} {
		if _, ok := next.Lookup(miss.dir, miss.pattern, witnessOf(1)); ok {
			t.Errorf("Lookup(%s, %s) answered a value recorded for another key", miss.dir, miss.pattern)
		}
	}
	if _, ok := next.Lookup("/w/api", "api/v{version}", witnessOf(2)); ok {
		t.Error("Lookup answered under another witness")
	}
	if next.Hits() != 1 {
		t.Errorf("Hits = %d, want 1", next.Hits())
	}
}

func TestAnotherIdentityOrADamagedFileLoadsEmpty(t *testing.T) {
	t.Parallel()
	path := releaseRecordPathFor(t)
	r := OpenReleaseRecord(path, "id")
	r.Store("/w", "p", witnessOf(1), []byte("v"), still(witnessOf(1)))
	if err := r.Save(); err != nil {
		t.Fatal(err)
	}
	if _, ok := OpenReleaseRecord(path, "other").Lookup("/w", "p", witnessOf(1)); ok {
		t.Error("a record written by another program answered")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := range data {
		damaged := append([]byte(nil), data...)
		damaged[i] ^= 0x01
		if _, err := decodeReleaseRecord(damaged); err == nil {
			t.Fatalf("a file damaged at byte %d decoded", i)
		}
	}
	if _, err := decodeReleaseRecord(data[:len(data)-1]); err == nil {
		t.Error("a truncated file decoded")
	}
}

func TestNoIdentityAndReadOnlyRecordsWriteNothing(t *testing.T) {
	t.Parallel()
	for name, open := range map[string]func(string) *ReleaseRecord{
		"no identity": func(path string) *ReleaseRecord { return OpenReleaseRecord(path, ReleaseRecordIdentity("")) },
		"read-only":   func(path string) *ReleaseRecord { return OpenReleaseRecordReadOnly(path, "id") },
	} {
		path := releaseRecordPathFor(t)
		r := open(path)
		r.Store("/w", "p", witnessOf(1), []byte("v"), still(witnessOf(1)))
		if err := r.Save(); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s: the record was written (%v)", name, err)
		}
	}
	var none *ReleaseRecord
	if _, ok := none.Lookup("/w", "p", witnessOf(1)); ok || none.Hits() != 0 || none.Save() != nil {
		t.Error("a nil record answered or saved")
	}
	if ReleaseRecordIdentity("exe") == ReleaseRecordIdentity("other") || ReleaseRecordIdentity("exe") == "" {
		t.Error("Identity does not name the program")
	}
}

func TestAnUnchangedRunWritesNothingUntilItsEntriesAge(t *testing.T) {
	t.Parallel()
	path := releaseRecordPathFor(t)
	r := OpenReleaseRecord(path, "id")
	r.Store("/w", "p", witnessOf(1), []byte("v"), still(witnessOf(1)))
	if err := r.Save(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	confirm := func() {
		r := OpenReleaseRecord(path, "id")
		if _, ok := r.Lookup("/w", "p", witnessOf(1)); !ok {
			t.Fatal("the entry was dropped")
		}
		r.Store("/w", "p", witnessOf(1), []byte("v"), still(witnessOf(1))) // the same value confirms
		if err := r.Save(); err != nil {
			t.Fatal(err)
		}
	}
	confirm()
	if after, err := os.Stat(path); err != nil || !after.ModTime().Equal(info.ModTime()) || !os.SameFile(info, after) {
		t.Fatalf("an unchanged run rewrote the record (%v)", err)
	}

	// Other runs record other entries: after half of keptRuns, a run that
	// confirms the first entry saves to keep it.
	for i := range keptRuns / 2 {
		other := OpenReleaseRecord(path, "id")
		other.Store("/w", "p", witnessOf(byte(2+i)), []byte("v"), still(witnessOf(byte(2+i))))
		if err := other.Save(); err != nil {
			t.Fatal(err)
		}
	}
	before, _ := os.Stat(path)
	confirm()
	if after, _ := os.Stat(path); os.SameFile(before, after) {
		t.Fatal("a run that confirmed an aging entry did not save")
	}
	// An entry no run confirms is dropped after keptRuns saves.
	for i := range keptRuns + 1 {
		other := OpenReleaseRecord(path, "id")
		other.Store("/w", "q", witnessOf(byte(100+i)), []byte("v"), still(witnessOf(byte(100+i))))
		if err := other.Save(); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := OpenReleaseRecord(path, "id").Lookup("/w", "p", witnessOf(1)); ok {
		t.Error("an entry unconfirmed for more than keptRuns saves was kept")
	}
}

func TestConcurrentRunsMergeTheirEntries(t *testing.T) {
	t.Parallel()
	path := releaseRecordPathFor(t)
	a, b := OpenReleaseRecord(path, "id"), OpenReleaseRecord(path, "id")
	a.Store("/w/a", "p", witnessOf(1), []byte("a"), still(witnessOf(1)))
	b.Store("/w/b", "p", witnessOf(1), []byte("b"), still(witnessOf(1)))
	if err := a.Save(); err != nil {
		t.Fatal(err)
	}
	if err := b.Save(); err != nil {
		t.Fatal(err)
	}
	merged := OpenReleaseRecord(path, "id")
	for _, dir := range []string{"/w/a", "/w/b"} {
		if _, ok := merged.Lookup(dir, "p", witnessOf(1)); !ok {
			t.Errorf("the entry of %s was lost in the merge", dir)
		}
	}
}

func TestTheRecordKeepsItsBound(t *testing.T) {
	path := releaseRecordPathFor(t)
	previous := maxReleaseRecordEntries
	maxReleaseRecordEntries = 2
	t.Cleanup(func() { maxReleaseRecordEntries = previous })
	old := OpenReleaseRecord(path, "id")
	old.Store("/w/old", "p", witnessOf(1), []byte("v"), still(witnessOf(1)))
	if err := old.Save(); err != nil {
		t.Fatal(err)
	}
	r := OpenReleaseRecord(path, "id")
	for i := range 3 {
		r.Store("/w", "p", witnessOf(byte(10+i)), []byte("v"), still(witnessOf(byte(10+i))))
	}
	r.Store("/w", "p", witnessOf(20), make([]byte, maxReleaseRecordValue+1), still(witnessOf(20))) // too large to record
	if err := r.Save(); err != nil {
		t.Fatal(err)
	}
	loaded := OpenReleaseRecord(path, "id")
	if _, ok := loaded.Lookup("/w/old", "p", witnessOf(1)); ok {
		t.Error("an entry the run neither recorded nor confirmed survived the bound")
	}
	if _, ok := loaded.Lookup("/w", "p", witnessOf(20)); ok {
		t.Error("a value above maxReleaseRecordValue was recorded")
	}
	kept := 0
	for i := range 3 {
		if _, ok := loaded.Lookup("/w", "p", witnessOf(byte(10+i))); ok {
			kept++
		}
	}
	if kept != maxReleaseRecordEntries {
		t.Errorf("%d of the run's 3 entries kept, want the bound, %d", kept, maxReleaseRecordEntries)
	}
}

func TestUseSetsTheCurrentRecord(t *testing.T) {
	r := OpenReleaseRecord(releaseRecordPathFor(t), "id")
	restore := UseReleaseRecord(r)
	if CurrentReleaseRecord() != r {
		t.Fatal("Current is not the record in use")
	}
	restore()
	if CurrentReleaseRecord() == r {
		t.Fatal("restore kept the record")
	}
	if ReleaseRecordPath("/w") != filepath.Join("/w", ".putnami", ReleaseRecordFilename) {
		t.Errorf("Path = %s", ReleaseRecordPath("/w"))
	}
}

// An entry whose witness does not hold again when the record saves, or whose
// recheck cannot take it, is not written.
func TestAnEntryWhoseWitnessMovedBeforeTheSaveIsNotWritten(t *testing.T) {
	t.Parallel()
	path := releaseRecordPathFor(t)
	r := OpenReleaseRecord(path, "id")
	calls := 0
	moved := func() (ReleaseWitness, bool) { calls++; return witnessOf(9), true }
	r.Store("/w/a", "p", witnessOf(1), []byte("a"), moved)
	r.Store("/w/b", "p", witnessOf(1), []byte("b"), moved)
	r.Store("/w/c", "p", witnessOf(2), []byte("c"), func() (ReleaseWitness, bool) { return ReleaseWitness{}, false })
	r.Store("/w/d", "p", witnessOf(3), []byte("d"), nil)
	r.Store("/w/e", "p", witnessOf(4), []byte("e"), still(witnessOf(4)))
	if err := r.Save(); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Errorf("the recheck of one witness ran %d times, want once", calls)
	}
	loaded := OpenReleaseRecord(path, "id")
	for dir, w := range map[string]ReleaseWitness{"/w/a": witnessOf(1), "/w/b": witnessOf(1), "/w/c": witnessOf(2), "/w/d": witnessOf(3)} {
		if _, ok := loaded.Lookup(dir, "p", w); ok {
			t.Errorf("%s was written under a witness that did not hold again", dir)
		}
	}
	if _, ok := loaded.Lookup("/w/e", "p", witnessOf(4)); !ok {
		t.Error("an entry whose witness held again was not written")
	}
}
