package fileindex

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// withClock makes every record opened in the test read clock as its
// filesystem clock, on the device of the test's directory, and treat the
// directory as local.
func withRecordClock(t *testing.T, clock *int64) {
	t.Helper()
	previousClock, previousLocal := recordClock, recordLocal
	recordClock = func(dir string) (uint64, int64, error) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return 0, 0, err
		}
		st, ok := LstatAny(dir)
		if !ok {
			t.Fatalf("stat %s", dir)
		}
		return st.Dev, *clock, nil
	}
	recordLocal = func(string) bool { return true }
	t.Cleanup(func() { recordClock, recordLocal = previousClock, previousLocal })
}

// partOf stats path for a witness.
func recordPartOf(t *testing.T, path string) RecordPart {
	t.Helper()
	st, ok := LstatAny(path)
	if !ok {
		t.Fatalf("stat %s", path)
	}
	return RecordPart{Path: path, Stat: st}
}

func recordHoldsNow(parts []RecordPart) bool {
	for _, p := range parts {
		st, ok := LstatAny(p.Path)
		if !ok || st != p.Stat {
			return false
		}
	}
	return true
}

func recordFixture(t *testing.T) (root, recordPath, recordFile string) {
	t.Helper()
	root = t.TempDir()
	recordFile = filepath.Join(root, "a.txt")
	if err := os.WriteFile(recordFile, []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root, RecordPath(root), recordFile
}

func TestARecordedValueIsAnsweredWhileItsPartsHold(t *testing.T) {
	clock := time.Now().Add(time.Hour).UnixNano()
	withRecordClock(t, &clock)
	_, path, recordFile := recordFixture(t)

	r := OpenRecord(path, "id")
	if !r.Put("digests", "k", []byte("v"), []RecordPart{recordPartOf(t, recordFile)}) {
		t.Fatal("a part older than the clock by the margin was refused")
	}
	if err := r.Save(); err != nil {
		t.Fatal(err)
	}

	next := OpenRecord(path, "id")
	value, parts, ok := next.Answer("digests", "k", recordHoldsNow)
	if !ok || string(value) != "v" || len(parts) != 1 || parts[0].Path != recordFile {
		t.Fatalf("Answer = %q, %v, %v; want the recorded value and part", value, parts, ok)
	}
	if _, _, ok := next.Answer("other", "k", recordHoldsNow); ok {
		t.Fatal("a table answered a recordKey another table recorded")
	}

	if err := os.WriteFile(recordFile, []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := OpenRecord(path, "id").Answer("digests", "k", recordHoldsNow); ok {
		t.Fatal("a value whose part moved was answered")
	}
}

func TestAPartWrittenWithinTheMarginIsNotRecorded(t *testing.T) {
	_, path, recordFile := recordFixture(t)
	st, _ := LstatAny(recordFile)
	clock := st.ChangeNano + int64(Margin) // not older by more than the margin
	withRecordClock(t, &clock)

	r := OpenRecord(path, "id")
	if r.Put("digests", "k", []byte("v"), []RecordPart{recordPartOf(t, recordFile)}) {
		t.Fatal("a part written within the margin was recorded")
	}
	if r.Put("digests", "k", []byte("v"), nil) {
		t.Fatal("a value with no part was recorded")
	}
	if r.Put("digests", "k", []byte("v"), []RecordPart{{Path: "a.txt", Stat: st}}) {
		t.Fatal("a relative part was recorded")
	}
}

func TestARecordOfAnotherProgramOrDamagedLoadsEmpty(t *testing.T) {
	clock := time.Now().Add(time.Hour).UnixNano()
	withRecordClock(t, &clock)
	_, path, recordFile := recordFixture(t)
	r := OpenRecord(path, "id")
	r.Put("digests", "k", []byte("v"), []RecordPart{recordPartOf(t, recordFile)})
	if err := r.Save(); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := OpenRecord(path, "other").Answer("digests", "k", recordHoldsNow); ok {
		t.Fatal("a record another program wrote answered")
	}
	if _, _, ok := OpenRecord(path, "").Answer("digests", "k", recordHoldsNow); ok {
		t.Fatal("a process with no identity answered")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, damage := range []func([]byte) []byte{
		func(b []byte) []byte { b[len(b)/2] ^= 0xff; return b },
		func(b []byte) []byte { return b[:len(b)-1] },
		func(b []byte) []byte { return append(b, 0) },
	} {
		if err := os.WriteFile(path, damage(append([]byte(nil), data...)), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, _, ok := OpenRecord(path, "id").Answer("digests", "k", recordHoldsNow); ok {
			t.Fatal("a damaged record answered")
		}
	}
}

func TestARecordAheadOfTheClockDiscardsItself(t *testing.T) {
	clock := time.Now().Add(time.Hour).UnixNano()
	withRecordClock(t, &clock)
	_, path, recordFile := recordFixture(t)
	r := OpenRecord(path, "id")
	r.Put("digests", "k", []byte("v"), []RecordPart{recordPartOf(t, recordFile)})
	if err := r.Save(); err != nil {
		t.Fatal(err)
	}
	clock -= int64(time.Minute)
	if _, _, ok := OpenRecord(path, "id").Answer("digests", "k", recordHoldsNow); ok {
		t.Fatal("a record whose stamp lies ahead of the clock answered")
	}
}

func TestAClockThatWentBackDuringTheRunRecordsNothing(t *testing.T) {
	clock := time.Now().Add(time.Hour).UnixNano()
	withRecordClock(t, &clock)
	_, path, recordFile := recordFixture(t)
	r := OpenRecord(path, "id")
	r.Put("digests", "k", []byte("v"), []RecordPart{recordPartOf(t, recordFile)})
	clock -= int64(time.Minute)
	if err := r.Save(); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := OpenRecord(path, "id").Answer("digests", "k", recordHoldsNow); ok {
		t.Fatal("a value recorded before the clock went back was kept")
	}
}

func TestConcurrentRunsMergeAndDropWhatTheyFoundOutOfDate(t *testing.T) {
	clock := time.Now().Add(time.Hour).UnixNano()
	withRecordClock(t, &clock)
	root, path, recordFile := recordFixture(t)
	other := filepath.Join(root, "b.txt")
	if err := os.WriteFile(other, []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}

	first, second := OpenRecord(path, "id"), OpenRecord(path, "id")
	first.Put("digests", "a", []byte("1"), []RecordPart{recordPartOf(t, recordFile)})
	second.Put("digests", "b", []byte("2"), []RecordPart{recordPartOf(t, other)})
	if err := first.Save(); err != nil {
		t.Fatal(err)
	}
	if err := second.Save(); err != nil {
		t.Fatal(err)
	}
	merged := OpenRecord(path, "id")
	for k, want := range map[string]string{"a": "1", "b": "2"} {
		if value, _, ok := merged.Answer("digests", k, recordHoldsNow); !ok || string(value) != want {
			t.Fatalf("after two runs merged, %s = %q, %v; want %q", k, value, ok, want)
		}
	}

	if err := os.WriteFile(recordFile, []byte("moved"), 0o644); err != nil {
		t.Fatal(err)
	}
	finding := OpenRecord(path, "id")
	if _, _, ok := finding.Answer("digests", "a", recordHoldsNow); ok {
		t.Fatal("a value whose part moved was answered")
	}
	if err := finding.Save(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(recordFile, []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	after := OpenRecord(path, "id")
	if _, _, ok := after.Answer("digests", "a", func([]RecordPart) bool { return true }); ok {
		t.Fatal("a value a run found out of date stayed in the file")
	}
	if _, _, ok := after.Answer("digests", "b", recordHoldsNow); !ok {
		t.Fatal("a value no run found out of date was dropped")
	}
}

func TestAnUnchangedRunWritesNothing(t *testing.T) {
	clock := time.Now().Add(time.Hour).UnixNano()
	withRecordClock(t, &clock)
	_, path, recordFile := recordFixture(t)
	r := OpenRecord(path, "id")
	r.Put("digests", "k", []byte("v"), []RecordPart{recordPartOf(t, recordFile)})
	if err := r.Save(); err != nil {
		t.Fatal(err)
	}
	before, _ := LstatAny(path)
	confirming := OpenRecord(path, "id")
	if _, _, ok := confirming.Answer("digests", "k", recordHoldsNow); !ok {
		t.Fatal("the recorded value was not answered")
	}
	if err := confirming.Save(); err != nil {
		t.Fatal(err)
	}
	if after, _ := LstatAny(path); after != before {
		t.Fatal("a run that only confirmed entries rewrote the record")
	}
}

func TestAReadOnlyRecordAnswersAndWritesNothing(t *testing.T) {
	clock := time.Now().Add(time.Hour).UnixNano()
	withRecordClock(t, &clock)
	_, path, recordFile := recordFixture(t)
	r := OpenRecord(path, "id")
	r.Put("digests", "k", []byte("v"), []RecordPart{recordPartOf(t, recordFile)})
	if err := r.Save(); err != nil {
		t.Fatal(err)
	}
	readOnly := OpenRecordReadOnly(path, "id", clock)
	if _, _, ok := readOnly.Answer("digests", "k", recordHoldsNow); !ok {
		t.Fatal("a read-only record did not answer")
	}
	if readOnly.Put("digests", "other", []byte("v"), []RecordPart{recordPartOf(t, recordFile)}) {
		t.Fatal("a read-only record recorded")
	}
}
