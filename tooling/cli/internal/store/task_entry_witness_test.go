package store

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"go.putnami.dev/tooling/cli/internal/fileindex"
)

// witnessOf looks key up through a new handle on root, as the held run's
// lookup does, and returns the witness of the entry it was served.
func witnessOf(t *testing.T, root, key string) EntryWitness {
	t.Helper()
	entry, err := NewLocalStore(root).LookupTaskEntry(key)
	if err != nil || entry == nil {
		t.Fatalf("lookup of %s = %v, %v; want the published entry", key, entry, err)
	}
	return entry.Witness
}

// resultOnlyWitnessOf is witnessOf for key's result-only entry.
func resultOnlyWitnessOf(t *testing.T, root, key string) EntryWitness {
	t.Helper()
	entry := NewLocalStore(root).LookupResultOnlyTaskEntry(key)
	if entry == nil {
		t.Fatalf("result-only lookup of %s missed; want the published entry", key)
	}
	return entry.Witness
}

// TestServeWitnessedServesOnlyTheEntriesTheHeldRunWasServed pins CLI ADR 0116
// §6a's store half: an answer reports a task served from the local cache only
// while the store holds the very entry the held run's hit read. An untouched
// entry is served again and its last use stamped as a hit's lookup stamps it;
// an entry evicted (renamed aside as a collection does, or wiped by a clean),
// or published again at its address with the same bytes, moved, and then no
// entry of the set is stamped. A result-only entry moves also when a
// task-owned entry, which a lookup serves first, appears at its key. A lookup
// of files younger than the file index's margin witnesses nothing.
func TestServeWitnessedServesOnlyTheEntriesTheHeldRunWasServed(t *testing.T) {
	if _, ok := fileindex.Lstat(os.Args[0]); !ok {
		t.Skip("the file index does not decode this platform's stat: no entry is witnessed")
	}
	t.Parallel()
	stores := map[string]string{}
	for _, name := range []string{"untouched", "evicted", "cleaned", "rewritten", "result-only", "result-only shadowed"} {
		root := t.TempDir()
		stores[name] = root
		s := NewLocalStore(root)
		if name == "result-only" || name == "result-only shadowed" {
			if _, err := s.PublishResultOnlyTaskEntry(resultOnlyHit(resultOnlyKeyA)); err != nil {
				t.Fatal(err)
			}
			continue
		}
		putWithFile(t, s, hashA, "main.js", "bundle a")
		putWithFile(t, s, hashB, "main.js", "bundle b")
	}

	// A lookup of files written less than the margin ago witnesses nothing,
	// and nothing is served under it.
	fresh := witnessOf(t, stores["untouched"], hashA)
	if fresh.Sound() {
		t.Fatalf("a lookup of an entry written just now witnessed %+v, want nothing", fresh)
	}
	if moved := NewLocalStore(stores["untouched"]).ServeWitnessed([]EntryWitness{fresh}); moved != 0 {
		t.Fatalf("an unsound witness served as %d, want refused as 0", moved)
	}
	if fresh := resultOnlyWitnessOf(t, stores["result-only"], resultOnlyKeyA); fresh.Sound() {
		t.Fatalf("a result-only lookup of a record written just now witnessed %+v, want nothing", fresh)
	}

	time.Sleep(fileindex.Margin + 100*time.Millisecond)

	t.Run("untouched", func(t *testing.T) {
		root := stores["untouched"]
		witnesses := []EntryWitness{witnessOf(t, root, hashA), witnessOf(t, root, hashB)}
		for i, w := range witnesses {
			if !w.Sound() {
				t.Fatalf("witness %d of a settled entry: %+v, want sound", i, w)
			}
		}
		before := time.Now()
		answer := NewLocalStore(root)
		if moved := answer.ServeWitnessed(witnesses); moved != -1 {
			t.Fatalf("untouched entries: moved %d, want -1", moved)
		}
		// The stamp is a hit's: this handle's generation, bumped once, and the
		// time of the answer.
		for _, key := range []string{hashA, hashB} {
			used, gen := readLastUsed(taskBlobDir(answer, key))
			if used.Before(before) || gen != answer.gen || gen == 0 {
				t.Errorf("%s: last use %v gen %d, want at or after %v with the answer's generation %d", key[:4], used, gen, before, answer.gen)
			}
		}
		// The stamp is not part of the witness: a later answer still holds.
		if moved := NewLocalStore(root).ServeWitnessed(witnesses); moved != -1 {
			t.Fatalf("after an answer's own stamp: moved %d, want -1", moved)
		}
	})

	t.Run("evicted", func(t *testing.T) {
		root := stores["evicted"]
		witnesses := []EntryWitness{witnessOf(t, root, hashA), witnessOf(t, root, hashB)}
		stampedA, _ := readLastUsed(taskBlobDir(NewLocalStore(root), hashA))
		// A collection renames its victim into its trash, then removes it.
		trash := filepath.Join(root, "tmp", gcTrashPrefix+"witness")
		if err := os.MkdirAll(trash, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(taskBlobDir(NewLocalStore(root), hashB), filepath.Join(trash, "0")); err != nil {
			t.Fatal(err)
		}
		if moved := NewLocalStore(root).ServeWitnessed(witnesses); moved != 1 {
			t.Fatalf("one entry evicted: moved %d, want 1", moved)
		}
		if used, _ := readLastUsed(taskBlobDir(NewLocalStore(root), hashA)); !used.Equal(stampedA) {
			t.Errorf("an answer refused for a moved entry stamped another one: last use %v, was %v", used, stampedA)
		}
	})

	t.Run("cleaned", func(t *testing.T) {
		root := stores["cleaned"]
		witnesses := []EntryWitness{witnessOf(t, root, hashA), witnessOf(t, root, hashB)}
		if _, _, err := CleanStore(root); err != nil {
			t.Fatal(err)
		}
		if moved := NewLocalStore(root).ServeWitnessed(witnesses); moved != 0 {
			t.Fatalf("a cleaned store: moved %d, want 0", moved)
		}
	})

	t.Run("rewritten", func(t *testing.T) {
		root := stores["rewritten"]
		witnesses := []EntryWitness{witnessOf(t, root, hashA), witnessOf(t, root, hashB)}
		// Another run publishes the same key again once it was evicted: the
		// same bytes, at the same address, in files of its own.
		if err := os.RemoveAll(taskBlobDir(NewLocalStore(root), hashA)); err != nil {
			t.Fatal(err)
		}
		putWithFile(t, NewLocalStore(root), hashA, "main.js", "bundle a")
		if again := publishedEntry(NewLocalStore(root), hashA); again == nil {
			t.Fatal("the entry was not published again")
		}
		if moved := NewLocalStore(root).ServeWitnessed(witnesses); moved != 0 {
			t.Fatalf("one entry published again: moved %d, want 0", moved)
		}
	})

	t.Run("result-only", func(t *testing.T) {
		root := stores["result-only"]
		w := resultOnlyWitnessOf(t, root, resultOnlyKeyA)
		if !w.Sound() {
			t.Fatalf("witness of a settled result-only record: %+v, want sound", w)
		}
		before := time.Now()
		answer := NewLocalStore(root)
		if moved := answer.ServeWitnessed([]EntryWitness{w}); moved != -1 {
			t.Fatalf("an untouched result-only entry: moved %d, want -1", moved)
		}
		if used, gen := readLastUsed(answer.blobDir(ResultOnlyTaskEntryAddress(resultOnlyKeyA))); used.Before(before) || gen != answer.gen {
			t.Errorf("result-only last use %v gen %d, want at or after %v with the answer's generation %d", used, gen, before, answer.gen)
		}
		if err := os.RemoveAll(answer.blobDir(ResultOnlyTaskEntryAddress(resultOnlyKeyA))); err != nil {
			t.Fatal(err)
		}
		if moved := NewLocalStore(root).ServeWitnessed([]EntryWitness{w}); moved != 0 {
			t.Fatalf("an evicted result-only entry: moved %d, want 0", moved)
		}
	})

	t.Run("result-only shadowed", func(t *testing.T) {
		root := stores["result-only shadowed"]
		w := resultOnlyWitnessOf(t, root, resultOnlyKeyA)
		putWithFile(t, NewLocalStore(root), resultOnlyKeyA, "main.js", "built")
		if moved := NewLocalStore(root).ServeWitnessed([]EntryWitness{w}); moved != 0 {
			t.Fatalf("a task-owned entry published at the result-only key: moved %d, want 0", moved)
		}
	})
}

// TestServeWitnessedChecksALargeSetOverTheProcessors pins the parallel path:
// a set above the floor is checked in ranges, every entry is stamped when all
// hold, and an entry that moved anywhere in the set refuses the whole set.
func TestServeWitnessedChecksALargeSetOverTheProcessors(t *testing.T) {
	if _, ok := fileindex.Lstat(os.Args[0]); !ok {
		t.Skip("the file index does not decode this platform's stat: no entry is witnessed")
	}
	t.Parallel()
	root := t.TempDir()
	s := NewLocalStore(root)
	const n = 3 * witnessParallelFloor
	keys := make([]string, n)
	for i := range keys {
		keys[i] = TaskEntryAddress(strconv.Itoa(i))
		putWithFile(t, s, keys[i], "main.js", keys[i])
	}
	time.Sleep(fileindex.Margin + 100*time.Millisecond)
	witnesses := make([]EntryWitness, n)
	for i, key := range keys {
		witnesses[i] = witnessOf(t, root, key)
	}
	before := time.Now()
	answer := NewLocalStore(root)
	if moved := answer.ServeWitnessed(witnesses); moved != -1 {
		t.Fatalf("untouched entries: moved %d, want -1", moved)
	}
	for _, key := range keys {
		if used, _ := readLastUsed(taskBlobDir(answer, key)); used.Before(before) {
			t.Fatalf("%s: last use %v, want stamped at or after %v", key[:8], used, before)
		}
	}
	last := n - 1
	if err := os.RemoveAll(taskBlobDir(answer, keys[last])); err != nil {
		t.Fatal(err)
	}
	if moved := NewLocalStore(root).ServeWitnessed(witnesses); moved != last {
		t.Fatalf("the last entry evicted: moved %d, want %d", moved, last)
	}
}
