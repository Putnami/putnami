package store

import (
	"fmt"
	"sync"
	"testing"
)

// TestIngestTaskEntry_ConcurrentDifferentKeys publishes many distinct entries at
// once and asserts they all land — the shared lock must not serialize them to
// death or drop any.
func TestIngestTaskEntry_ConcurrentDifferentKeys(t *testing.T) {
	root := t.TempDir()
	const writers = 16
	keys := make([]string, writers)
	var wg sync.WaitGroup
	for i := range writers {
		keys[i] = fmt.Sprintf("%064x", i+1)
		staging := t.TempDir()
		out := dirOutput("dist", "dist")
		stage(t, staging, out, "main.js", fmt.Sprintf("c%d", i))
		wg.Go(func() {
			s := NewLocalStore(root)
			if _, err := s.IngestTaskEntry(staging, taskSpec(keys[i], out)); err != nil {
				t.Errorf("IngestTaskEntry %d: %v", i, err)
			}
		})
	}
	wg.Wait()

	s := NewLocalStore(root)
	for i, key := range keys {
		entry, err := s.LookupTaskEntry(key)
		if err != nil || entry == nil {
			t.Errorf("entry %d (%s) missing: err=%v", i, key, err)
			continue
		}
		if got, want := readDist(t, entry, "main.js"), fmt.Sprintf("c%d", i); got != want {
			t.Errorf("entry %d holds %q, want %q", i, got, want)
		}
	}
}

// TestIngestTaskEntry_FirstWriterWins verifies an existing entry is never
// clobbered by a later publish of the same key: the first writer's result and
// files are authoritative.
func TestIngestTaskEntry_FirstWriterWins(t *testing.T) {
	s := NewLocalStore(t.TempDir())
	const key = "first-writer-key"

	for _, who := range []string{"first", "second"} {
		staging := t.TempDir()
		out := dirOutput("dist", "dist")
		stage(t, staging, out, "main.js", who)
		spec := taskSpec(key, out)
		spec.Result = &EntryResult{Status: "success", Data: map[string]any{"who": who}}
		if _, err := s.IngestTaskEntry(staging, spec); err != nil {
			t.Fatalf("%s ingest: %v", who, err)
		}
	}

	entry, err := s.LookupTaskEntry(key)
	if err != nil || entry == nil {
		t.Fatalf("LookupTaskEntry: %v (entry=%v)", err, entry)
	}
	if entry.Result.Data["who"] != "first" {
		t.Errorf("first-writer-wins violated: result = %v, want first", entry.Result.Data["who"])
	}
	if got := readDist(t, entry, "main.js"); got != "first" {
		t.Errorf("files clobbered: %q, want first", got)
	}
}
