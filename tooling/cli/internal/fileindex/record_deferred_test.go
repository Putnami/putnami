package fileindex

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// deferSamplesInTest turns the deferral of sampled checks on for the test,
// as a machine worker does, and off again after it.
func deferSamplesInTest(t *testing.T) {
	t.Helper()
	DeferSamples(true)
	t.Cleanup(func() { DeferSamples(false) })
}

// twoRecordedValues records k1 under the stat of a.txt and k2 under the
// stat of b.txt, saves them, and opens the record again, as the next run
// does; it returns the record and the parts it answers k1 with.
func twoRecordedValues(t *testing.T) (next *Record, path, a string, parts []RecordPart) {
	t.Helper()
	clock := time.Now().Add(time.Hour).UnixNano()
	withRecordClock(t, &clock)
	root, path, a := recordFixture(t)
	b := filepath.Join(root, "b.txt")
	if err := os.WriteFile(b, []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := OpenRecord(path, "id")
	if !r.Put("digests", "k1", []byte("v1"), []RecordPart{recordPartOf(t, a)}) ||
		!r.Put("digests", "k2", []byte("v2"), []RecordPart{recordPartOf(t, b)}) {
		t.Fatal("the record refused a value")
	}
	if err := r.Save(); err != nil {
		t.Fatal(err)
	}
	next = OpenRecord(path, "id")
	_, parts, ok := next.Answer("digests", "k1", next.Hold)
	if !ok {
		t.Fatal("the recorded value was not answered")
	}
	return next, path, a, parts
}

// answers reports whether a record opened at path now answers key.
func answers(path, key string) bool {
	r := OpenRecord(path, "id")
	_, _, ok := r.Answer("digests", key, r.Hold)
	return ok
}

// A process that defers samples, a machine worker, queues the check of a
// sampled answer instead of running it: the run uses the answer. The check
// runs once the run returned, and an answer that differs under a witness
// that held throughout discards the record on disk, as a sampled answer that
// differs within a run discards it: the next run starts from an empty record,
// and is told why once (tooling/cli ADR 0113 §1).
func TestADeferredSampleThatDiffersDiscardsTheRecordOnDisk(t *testing.T) {
	deferSamplesInTest(t)
	next, path, _, parts := twoRecordedValues(t)
	index := &Index{}
	restoreIndex := Use(index)
	checked := false
	var checkedThrough *Index
	if !next.Defer(parts, func() string {
		checked, checkedThrough = true, Current()
		return "the value differs"
	}) {
		t.Fatal("a worker's record did not defer the check")
	}
	restoreIndex()
	if checked || next.Discarded() != "" {
		t.Fatalf("checked %v, discarded %q: the check ran before the run returned", checked, next.Discarded())
	}
	if _, _, ok := next.Answer("digests", "k2", next.Hold); !ok {
		t.Fatal("the run's record stopped answering before the check ran")
	}

	n, reason := TakeDeferredSamples()()
	if n != 1 || reason != "the value differs" || !checked {
		t.Fatalf("checked %d (%v), reason %q: want the queued check run and its difference", n, checked, reason)
	}
	if checkedThrough != index {
		t.Fatal("the check did not collect through the file index the run read files through")
	}
	if Current() != nil {
		t.Fatal("the check left its file index current")
	}
	if answers(path, "k1") || answers(path, "k2") {
		t.Fatal("the record on disk still answers after a deferred check differed")
	}
	if got := TakeDeferredDiscard(path); got != "the value differs" {
		t.Fatalf("the next run is told %q, want the difference", got)
	}
	if got := TakeDeferredDiscard(path); got != "" {
		t.Fatalf("a later run is told %q again", got)
	}
	if n, reason := TakeDeferredSamples()(); n != 0 || reason != "" {
		t.Fatalf("the queue ran %d checks again (%q)", n, reason)
	}
}

// A check whose answer's witness moved since the answer, before or while
// the value is collected again, is no evidence against the record: the file
// was written after the run read it, and the witness refuses that answer to
// the next run anyway. The record keeps the other answers.
func TestADeferredSampleWhoseWitnessMovedKeepsTheRecord(t *testing.T) {
	for name, move := range map[string]bool{"before the check": true, "during the check": false} {
		t.Run(name, func(t *testing.T) {
			deferSamplesInTest(t)
			next, path, a, parts := twoRecordedValues(t)
			edit := func() {
				if err := os.WriteFile(a, []byte("edited by the user"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			collected := false
			if !next.Defer(parts, func() string {
				collected = true
				if !move {
					edit()
				}
				return "the value differs"
			}) {
				t.Fatal("a worker's record did not defer the check")
			}
			if move {
				edit()
			}
			n, reason := TakeDeferredSamples()()
			if reason != "" || collected != !move || n != map[bool]int{true: 0, false: 1}[move] {
				t.Fatalf("checked %d (collected %v), reason %q: want no difference reported", n, collected, reason)
			}
			if !answers(path, "k2") || TakeDeferredDiscard(path) != "" {
				t.Fatal("a moved witness discarded the record")
			}
		})
	}
}

// A process that does not defer samples, which is every process but a
// machine worker, and a record that writes nothing, queue nothing: the
// caller checks the sampled answer before the run uses it.
func TestASampleIsCheckedAtOnceOutsideAWorker(t *testing.T) {
	next, path, _, parts := twoRecordedValues(t)
	check := func() string { t.Fatal("a check ran outside a worker's queue"); return "" }
	if next.Defer(parts, check) {
		t.Fatal("a process that does not defer samples queued a check")
	}
	deferSamplesInTest(t)
	readOnly := OpenRecordReadOnly(path, "id", time.Now().Add(time.Hour).UnixNano())
	if readOnly.Defer(parts, check) {
		t.Fatal("a record that writes nothing queued a check")
	}
	if n, reason := TakeDeferredSamples()(); n != 0 || reason != "" {
		t.Fatalf("the queue ran %d checks (%q), want none queued", n, reason)
	}
	DeferSamples(true)
	if !next.Defer(parts, check) {
		t.Fatal("a worker's record did not defer the check")
	}
	DeferSamples(false)
	if n, _ := TakeDeferredSamples()(); n != 0 {
		t.Fatal("turning the deferral off left a check queued")
	}
}
