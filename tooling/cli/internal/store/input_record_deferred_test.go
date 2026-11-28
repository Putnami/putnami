package store

import (
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/tooling/cli/internal/fileindex"
)

// countCollections counts the collections lookupFileHash makes from now on.
func countCollections(t *testing.T) *int {
	t.Helper()
	original := memoizedHashFiles
	t.Cleanup(func() { memoizedHashFiles = original })
	n := new(int)
	memoizedHashFiles = func(cut CandidateSource, dir string, patterns []string, expander InputExpander, scope ProjectConfigScope) (string, *selectionWitness, error) {
		*n++
		return original(cut, dir, patterns, expander, scope)
	}
	return n
}

// plantedRecord records the digest of p's sources, replaces it with a wrong
// one under the same witness, and opens the record again as the next run
// does, every answer sampled; it returns the record and the right digest.
func plantedRecord(t *testing.T, root, p string, patterns []string) (*fileindex.Record, string) {
	t.Helper()
	writeRecordTree(t, p, map[string]string{"src/a.go": "a"})
	record := useRecordAt(t, root)
	want, err := NewCacheManager(nil).HashFiles(p, patterns, nil, ProjectConfigScope{})
	must(t, err)
	plantInputDigest(t, record, p, patterns, "planted")
	must(t, record.Save())
	next := useRecordAt(t, root)
	next.SetSampleRate(1)
	return next, want
}

// In a machine worker a sampled answer is not collected while the run uses
// it: the run uses the answer, and the worker collects it once the run
// returned. One that differs then empties the record on disk, so the next
// run answers nothing from it and collects every digest (tooling/cli ADR
// 0113 §1).
func TestAWorkerChecksASampledAnswerOnceTheRunReturned(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	must(t, err)
	p := filepath.Join(root, "p")
	patterns := []string{"src/**/*.go"}
	next, want := plantedRecord(t, root, p, patterns)
	fileindex.DeferSamples(true)
	t.Cleanup(func() { fileindex.DeferSamples(false) })
	collections := countCollections(t)

	got, err := NewCacheManager(nil).HashFiles(p, patterns, nil, ProjectConfigScope{})
	if err != nil || got != "planted" || *collections != 0 || next.Discarded() != "" {
		t.Fatalf("got %q, %v after %d collections, discarded %q: want the answer used and no collection",
			got, err, *collections, next.Discarded())
	}
	must(t, next.Save())

	checked, reason := fileindex.TakeDeferredSamples()()
	if checked != 1 || *collections != 1 || !strings.Contains(reason, "which a collection gives as "+want) {
		t.Fatalf("checked %d with %d collections, reason %q: want the sample collected once and found to differ",
			checked, *collections, reason)
	}
	if told := fileindex.TakeDeferredDiscard(fileindex.RecordPath(root)); told != reason {
		t.Fatalf("the next run is told %q, want %q", told, reason)
	}
	third := useRecordAt(t, root)
	if again, err := NewCacheManager(nil).HashFiles(p, patterns, nil, ProjectConfigScope{}); err != nil || again != want || third.Answered() != 0 {
		t.Fatalf("the next run got %q, %v with %d answers: want an empty record and the collection %s", again, err, third.Answered(), want)
	}
}

// A process that is not a machine worker, in CI, in a hosted run or with the
// machine process off, never defers a sample: it collects a sampled answer
// before the run uses it, uses the collection, and discards the record then,
// as it always has.
func TestOutsideAWorkerASampledAnswerIsCollectedBeforeTheRunUsesIt(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	must(t, err)
	p := filepath.Join(root, "p")
	patterns := []string{"src/**/*.go"}
	next, want := plantedRecord(t, root, p, patterns)
	collections := countCollections(t)

	got, err := NewCacheManager(nil).HashFiles(p, patterns, nil, ProjectConfigScope{})
	if err != nil || got != want || *collections == 0 || next.Discarded() == "" {
		t.Fatalf("got %q, %v after %d collections, discarded %q: want the collection %s and the record discarded",
			got, err, *collections, next.Discarded(), want)
	}
	if checked, reason := fileindex.TakeDeferredSamples()(); checked != 0 || reason != "" {
		t.Fatalf("%d checks were deferred (%q), want none", checked, reason)
	}
}

// A sampled digest of input selectors is collected while the run uses it,
// even in a worker: its collection asks the run's extensions, which are gone
// once the run returned.
func TestAWorkerCollectsASampledDigestOfInputSelectorsAtOnce(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	must(t, err)
	p := filepath.Join(root, "p")
	writeRecordTree(t, p, map[string]string{"src/a.go": "a"})
	patterns := []string{"go:sources"}
	expander := basisExpander{identity: "ext"}
	record := useRecordAt(t, root)
	want, err := NewCacheManager(nil).HashFiles(p, patterns, expander, ProjectConfigScope{})
	must(t, err)
	key, ok := recordKey(p+"|go:sources|||expander\x00"+expander.Identity(), patterns, expander)
	if !ok {
		t.Fatal("the digest of input selectors has no record key")
	}
	_, parts, answered := record.Answer(InputDigestTable, key, record.Hold)
	if !answered || !record.Put(InputDigestTable, key, []byte("planted\x001\x00memoized"), parts) {
		t.Fatal("the digest of input selectors was not recorded")
	}
	must(t, record.Save())
	fileindex.DeferSamples(true)
	t.Cleanup(func() { fileindex.DeferSamples(false) })
	next := useRecordAt(t, root)
	next.SetSampleRate(1)

	got, err := NewCacheManager(nil).HashFiles(p, patterns, expander, ProjectConfigScope{})
	if err != nil || got != want || next.Discarded() == "" {
		t.Fatalf("got %q, %v, discarded %q: want the collection %s and the record discarded", got, err, next.Discarded(), want)
	}
	if checked, _ := fileindex.TakeDeferredSamples()(); checked != 0 {
		t.Fatalf("%d checks were deferred, want none", checked)
	}
}
