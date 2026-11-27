package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/tooling/cli/internal/fileindex"
)

// plantInputDigest replaces the recorded digest of project's patterns with
// value, under the same witness: a record that answers a wrong value.
func plantInputDigest(t *testing.T, record *fileindex.Record, project string, patterns []string, value string) {
	t.Helper()
	key, _ := recordKey(project+"|"+strings.Join(patterns, ",")+"||", patterns, nil)
	_, parts, ok := record.Answer(InputDigestTable, key, record.Hold)
	if !ok {
		t.Fatal("the digest was not recorded")
	}
	if !record.Put(InputDigestTable, key, []byte(value+"\x001\x00memoized"), parts) {
		t.Fatal("the record refused the planted digest")
	}
}

// A write of the run named by the physical path of a file the record
// witnessed through a link reaches the stats the run checked it with
// (ADR 0113 §1): a project directory that is a link, then a link inside the
// project to a directory another project writes.
func TestAWriteByThePhysicalPathMovesADigestReadThroughALink(t *testing.T) {
	cases := []struct {
		name     string
		layout   func(root string) (project, written, invalidated string)
		patterns []string
	}{
		{"a linked project directory", func(root string) (string, string, string) {
			real := filepath.Join(root, "real-p")
			writeRecordTree(t, real, map[string]string{"src/a.go": "a"})
			must(t, os.Symlink("real-p", filepath.Join(root, "p")))
			written := filepath.Join(real, "src", "a.go")
			return filepath.Join(root, "p"), written, written
		}, []string{"src/**/*.go"}},
		{"a link inside the project", func(root string) (string, string, string) {
			project := filepath.Join(root, "app")
			writeRecordTree(t, project, map[string]string{"src/a.go": "a"})
			writeRecordTree(t, root, map[string]string{"lib/dist/index.js": "v1"})
			must(t, os.Symlink(filepath.Join(root, "lib"), filepath.Join(project, "deps")))
			return project, filepath.Join(root, "lib", "dist", "index.js"), filepath.Join(root, "lib", "dist")
		}, []string{"deps/dist/**/*.js"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			must(t, err)
			project, written, invalidated := c.layout(root)
			record := useRecordAt(t, root)
			if _, err := NewCacheManager(nil).HashFiles(project, c.patterns, nil, ProjectConfigScope{}); err != nil {
				t.Fatal(err)
			}
			must(t, record.Save())

			next := useRecordAt(t, root)
			cm := NewCacheManager(nil)
			if _, err := cm.HashFiles(project, c.patterns, nil, ProjectConfigScope{}); err != nil {
				t.Fatal(err)
			}
			if next.Answered() != 1 {
				t.Fatalf("answered %d, want the recorded digest answered once", next.Answered())
			}
			time.Sleep(20 * time.Millisecond)
			must(t, os.WriteFile(written, []byte("version 2"), 0o644))
			cm.InvalidateFileHashesUnder([]string{invalidated})
			got, err := cm.HashFiles(project, c.patterns, nil, ProjectConfigScope{})
			if err != nil {
				t.Fatal(err)
			}
			want, err := hashFiles(project, c.patterns, nil, ProjectConfigScope{})
			if err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Fatalf("a false hit within the run: answered %s, a fresh collection gives %s", got, want)
			}
		})
	}
}

// A link on the way that lies on another trusted local file system than the
// record is witnessed by its own stat: the digest is still recorded.
func TestALinkOnAnotherDeviceIsWitnessed(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	must(t, err)
	shm, err := os.MkdirTemp("/dev/shm", "putnami-record-")
	if err != nil {
		t.Skip("no /dev/shm")
	}
	t.Cleanup(func() { _ = os.RemoveAll(shm) })
	if same, ok := sameDevice(root, shm); !ok || same {
		t.Skip("/dev/shm is not another device")
	}
	writeRecordTree(t, filepath.Join(root, "p"), map[string]string{"src/a.go": "a"})
	must(t, os.Symlink(root, filepath.Join(shm, "w")))
	project := filepath.Join(shm, "w", "p")
	patterns := []string{"src/**/*.go"}

	record := useRecordAt(t, root)
	if _, err := NewCacheManager(nil).HashFiles(project, patterns, nil, ProjectConfigScope{}); err != nil {
		t.Fatal(err)
	}
	must(t, record.Save())
	next := useRecordAt(t, root)
	if _, err := NewCacheManager(nil).HashFiles(project, patterns, nil, ProjectConfigScope{}); err != nil {
		t.Fatal(err)
	}
	if next.Answered() != 1 {
		t.Fatalf("answered %d: a digest reached through a link on another device was not recorded", next.Answered())
	}
}

func sameDevice(a, b string) (same, ok bool) {
	sa, okA := fileindex.LstatAny(a)
	sb, okB := fileindex.LstatAny(b)
	return sa.Dev == sb.Dev, okA && okB
}

// A discard drops the digests the record answered earlier in the run: the
// run collects them again.
func TestADiscardDropsTheAnswersTheRunAlreadyGave(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	must(t, err)
	p1, p2 := filepath.Join(root, "p1"), filepath.Join(root, "p2")
	writeRecordTree(t, p1, map[string]string{"src/a.go": "a"})
	writeRecordTree(t, p2, map[string]string{"src/b.go": "b"})
	patterns := []string{"src/**/*.go"}
	record := useRecordAt(t, root)
	cm := NewCacheManager(nil)
	want1, err := cm.HashFiles(p1, patterns, nil, ProjectConfigScope{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cm.HashFiles(p2, patterns, nil, ProjectConfigScope{}); err != nil {
		t.Fatal(err)
	}
	plantInputDigest(t, record, p1, patterns, "planted1")
	plantInputDigest(t, record, p2, patterns, "planted2")
	must(t, record.Save())

	next := useRecordAt(t, root)
	cm = NewCacheManager(nil)
	if first, _ := cm.HashFiles(p1, patterns, nil, ProjectConfigScope{}); first != "planted1" {
		t.Fatalf("the unsampled answer is %q, want the planted one", first)
	}
	next.SetSampleRate(1)
	if _, err := cm.HashFiles(p2, patterns, nil, ProjectConfigScope{}); err != nil {
		t.Fatal(err)
	}
	if next.Discarded() == "" {
		t.Fatal("the sampled planted answer did not discard the record")
	}
	if again, _ := cm.HashFiles(p1, patterns, nil, ProjectConfigScope{}); again != want1 {
		t.Fatalf("after the discard the run still serves %q; a collection gives %s", again, want1)
	}
}

// A sampled answer whose collection now fails discards the record and
// returns the error a run without the record meets.
func TestASampledAnswerThatNoLongerCollectsDiscardsTheRecord(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	must(t, err)
	p := filepath.Join(root, "p")
	writeRecordTree(t, p, map[string]string{"src/a.go": "a"})
	patterns := []string{"src/**/*.go"}
	record := useRecordAt(t, root)
	if _, err := NewCacheManager(nil).HashFiles(p, patterns, nil, ProjectConfigScope{}); err != nil {
		t.Fatal(err)
	}
	must(t, record.Save())
	original := memoizedHashFiles
	t.Cleanup(func() { memoizedHashFiles = original })
	memoizedHashFiles = func(CandidateSource, string, []string, InputExpander, ProjectConfigScope) (string, *selectionWitness, error) {
		return "", nil, os.ErrPermission
	}
	next := useRecordAt(t, root)
	next.SetSampleRate(1)
	got, err := NewCacheManager(nil).HashFiles(p, patterns, nil, ProjectConfigScope{})
	if err == nil || next.Discarded() == "" {
		t.Fatalf("got %q, %v, discarded %q: want the collection's error and a discarded record", got, err, next.Discarded())
	}
}

// A write of the run between an answer and its sampled collection is not a
// false hit: the run uses the collection and keeps the record.
func TestAWriteBetweenAnAnswerAndItsSampleKeepsTheRecord(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	must(t, err)
	p := filepath.Join(root, "p")
	writeRecordTree(t, p, map[string]string{"src/a.go": "a"})
	patterns := []string{"src/**/*.go"}
	record := useRecordAt(t, root)
	if _, err := NewCacheManager(nil).HashFiles(p, patterns, nil, ProjectConfigScope{}); err != nil {
		t.Fatal(err)
	}
	must(t, record.Save())

	next := useRecordAt(t, root)
	next.SetSampleRate(1)
	cm := NewCacheManager(nil)
	original := memoizedHashFiles
	t.Cleanup(func() { memoizedHashFiles = original })
	written := filepath.Join(p, "src", "a.go")
	memoizedHashFiles = func(cut CandidateSource, dir string, patterns []string, expander InputExpander, scope ProjectConfigScope) (string, *selectionWitness, error) {
		memoizedHashFiles = original
		must(t, os.WriteFile(written, []byte("version 2"), 0o644))
		cm.InvalidateFileHashesUnder([]string{written})
		return original(cut, dir, patterns, expander, scope)
	}
	got, err := cm.HashFiles(p, patterns, nil, ProjectConfigScope{})
	if err != nil {
		t.Fatal(err)
	}
	want, err := hashFiles(p, patterns, nil, ProjectConfigScope{})
	if err != nil {
		t.Fatal(err)
	}
	if got != want || next.Discarded() != "" {
		t.Fatalf("got %s (fresh %s), discarded %q: want the collection and the record kept", got, want, next.Discarded())
	}
}

// A sampled answer that differs while a write of the run came between, even
// one elsewhere, is not trusted again: neither later in the run nor by the
// next run.
func TestAnAnswerAWriteExcusedIsDropped(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	must(t, err)
	p := filepath.Join(root, "p")
	writeRecordTree(t, p, map[string]string{"src/a.go": "a"})
	writeRecordTree(t, root, map[string]string{"other/out/x.txt": "x"})
	patterns := []string{"src/**/*.go"}
	record := useRecordAt(t, root)
	want, err := NewCacheManager(nil).HashFiles(p, patterns, nil, ProjectConfigScope{})
	must(t, err)
	plantInputDigest(t, record, p, patterns, "planted")
	must(t, record.Save())

	next := useRecordAt(t, root)
	next.SetSampleRate(1)
	cm := NewCacheManager(nil)
	original := memoizedHashFiles
	t.Cleanup(func() { memoizedHashFiles = original })
	unrelated := filepath.Join(root, "other", "out", "x.txt")
	memoizedHashFiles = func(cut CandidateSource, dir string, patterns []string, expander InputExpander, scope ProjectConfigScope) (string, *selectionWitness, error) {
		memoizedHashFiles = original
		must(t, os.WriteFile(unrelated, []byte("y"), 0o644))
		cm.InvalidateFileHashesUnder([]string{unrelated})
		return original(cut, dir, patterns, expander, scope)
	}
	if got, err := cm.HashFiles(p, patterns, nil, ProjectConfigScope{}); err != nil || got != want {
		t.Fatalf("got %q, %v; want the collection %s", got, err, want)
	}
	next.SetSampleRate(-1)
	if again, _ := cm.HashFiles(p, patterns, nil, ProjectConfigScope{}); again != want {
		t.Fatalf("later in the run the record answers %q; a collection gives %s", again, want)
	}
	must(t, next.Save())

	useRecordAt(t, root)
	if third, _ := NewCacheManager(nil).HashFiles(p, patterns, nil, ProjectConfigScope{}); third != want {
		t.Fatalf("the next run answers %q; a collection gives %s", third, want)
	}
}
