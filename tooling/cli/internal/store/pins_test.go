package store

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
)

// putResult publishes the task-owned entry of key, whose result is its own,
// and returns the SHA-256 of its result.json.
func putResult(t *testing.T, s *LocalStore, key string, n int) string {
	t.Helper()
	spec := taskSpec(key)
	spec.Result = &EntryResult{Status: "success", Error: &EntryError{Message: fmt.Sprintf("result %d", n)}}
	entry, err := s.IngestTaskEntry(t.TempDir(), spec)
	if err != nil {
		t.Fatalf("IngestTaskEntry(%s): %v", key, err)
	}
	return entry.ResultDigest
}

// pinnedObject is the result a pin keeps for the result.json whose digest is
// result, "sha256:<hex>".
func pinnedObject(root, result string) string {
	return filepath.Join(root, pinsDirName, pinObjectsDirName, strings.TrimPrefix(result, "sha256:")+".json")
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// TestPinnedResultsOutliveCollectionWhileTheirSessionIsRetained runs a
// collection whose budget evicts every entry: the entries go under the
// ordinary rules, pinned or not, and the result.json a retained session's pin
// names stays, hard-linked from its entry, so a reader of the session still
// reads it. Once the session directory is gone, collection deletes the pin
// and the result it kept.
func TestPinnedResultsOutliveCollectionWhileTheirSessionIsRetained(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "cached-task-by-reference", "retention-keeps-the-results-retained-sessions-reference")
	root := t.TempDir()
	s := NewLocalStore(root)
	now := time.Now()
	results := map[int]string{}
	for n := 1; n <= 3; n++ {
		results[n] = putResult(t, s, hashN(n), n)
		writeLastUsed(taskBlobDir(s, hashN(n)), now.Add(-48*time.Hour), 1)
	}
	want := publishedEntry(s, hashN(2)).Result
	session := filepath.Join(t.TempDir(), ".putnami", "sessions", "20261127-120000-abc123")
	if err := os.MkdirAll(session, 0o755); err != nil {
		t.Fatal(err)
	}
	pinned := PinnedResult{Key: hashN(2), Result: results[2]}
	if err := s.PinSession(session, []PinnedResult{pinned, pinned}); err != nil {
		t.Fatalf("PinSession: %v", err)
	}
	pins, _ := filepath.Glob(filepath.Join(root, pinsDirName, "*.json"))
	if len(pins) != 1 {
		t.Fatalf("pins = %v, want one for the session", pins)
	}
	object := pinnedObject(root, results[2])
	kept, err := os.Stat(object)
	if err != nil {
		t.Fatalf("the pinned result is not kept: %v", err)
	}
	source, err := os.Stat(filepath.Join(taskBlobDir(s, hashN(2)), "result.json"))
	if err != nil || !os.SameFile(kept, source) {
		t.Fatalf("the kept result is not the entry's result.json hard-linked (%v)", err)
	}

	if _, err := RunGC([]string{root}, GCOptions{MaxBytes: 1, Grace: 0, Now: now}); err != nil {
		t.Fatalf("RunGC: %v", err)
	}
	for n := 1; n <= 3; n++ {
		if publishedEntry(s, hashN(n)) != nil {
			t.Fatalf("entry %d survived a collection that evicts everything: a pin keeps no entry", n)
		}
	}
	if got := s.ReadReferencedResult(hashN(2), results[2]); got == nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("the pinned result reads as %+v once its entry is evicted, want %+v", got, want)
	}
	if s.ReadReferencedResult(hashN(1), results[1]) != nil {
		t.Fatal("a result no pin keeps reads once its entry is evicted")
	}

	if err := os.RemoveAll(session); err != nil {
		t.Fatal(err)
	}
	if _, err := RunGC([]string{root}, GCOptions{MaxBytes: 1, Grace: 0, Now: now}); err != nil {
		t.Fatalf("RunGC: %v", err)
	}
	if exists(object) || exists(pins[0]) {
		t.Fatal("the pin of a pruned session, or the result it kept, is still there")
	}
	if s.ReadReferencedResult(hashN(2), results[2]) != nil {
		t.Fatal("the result of a pruned session still reads")
	}
}

// TestASessionPinIsReplacedAndRemoved pins one session twice, then with no
// result: the store holds one pin, naming the last results, then none, and
// collection keeps only the results a pin names.
func TestASessionPinIsReplacedAndRemoved(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "cached-task-by-reference", "retention-keeps-the-results-retained-sessions-reference")
	root := t.TempDir()
	s := NewLocalStore(root)
	results := map[int]string{}
	for n := 1; n <= 3; n++ {
		results[n] = putResult(t, s, hashN(n), n)
	}
	session := t.TempDir()
	if err := s.PinSession(session, []PinnedResult{{hashN(1), results[1]}}); err != nil {
		t.Fatal(err)
	}
	if err := s.PinSession(session, []PinnedResult{{hashN(3), results[3]}, {hashN(2), results[2]}}); err != nil {
		t.Fatal(err)
	}
	live, complete, _ := pinnedResults(root, false)
	if !complete || len(live) != 2 || !live[strings.TrimPrefix(results[2], "sha256:")] || !live[strings.TrimPrefix(results[3], "sha256:")] {
		t.Fatalf("pinned = %v (%v), want the results of the last pin only", live, complete)
	}
	if !sweepPinnedResults(root, false) || exists(pinnedObject(root, results[1])) || !exists(pinnedObject(root, results[2])) || !exists(pinnedObject(root, results[3])) {
		t.Fatal("collection kept a result no pin names, or deleted one a pin names")
	}
	if err := s.PinSession(session, nil); err != nil {
		t.Fatal(err)
	}
	if pins, _ := filepath.Glob(filepath.Join(root, pinsDirName, "*.json")); len(pins) != 0 {
		t.Fatalf("pins = %v after the session referenced nothing, want none", pins)
	}
	sweepPinnedResults(root, false)
	if objects, _ := os.ReadDir(filepath.Join(root, pinsDirName, pinObjectsDirName)); len(objects) != 0 {
		t.Fatalf("kept results = %v once no pin names one, want none", objects)
	}
}

// TestAPinKeepsOnlyTheBytesItsSessionReferences pins a result its entry no
// longer holds: the entry was replaced since the session referenced it. The
// pin keeps nothing for it. A kept result whose bytes changed reads as the
// entry's result when that one matches, and as nothing otherwise.
func TestAPinKeepsOnlyTheBytesItsSessionReferences(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "cached-task-by-reference", "retention-keeps-the-results-retained-sessions-reference")
	root := t.TempDir()
	s := NewLocalStore(root)
	result := putResult(t, s, hashN(1), 1)
	replaced := "sha256:" + strings.Repeat("d", 64)
	if err := s.PinSession(t.TempDir(), []PinnedResult{{hashN(1), replaced}, {hashN(2), "sha256:" + strings.Repeat("e", 64)}, {hashN(1), "not-a-digest"}}); err != nil {
		t.Fatal(err)
	}
	if live, _, _ := pinnedResults(root, false); len(live) != 0 || exists(pinnedObject(root, replaced)) {
		t.Fatalf("a pin kept %v for results the store does not hold", live)
	}

	object := pinnedObject(root, result)
	if err := os.MkdirAll(filepath.Dir(object), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(object, []byte(`{"status":"failure"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := s.ReadReferencedResult(hashN(1), result); got == nil || got.Status != "success" {
		t.Fatalf("a changed kept result read as %+v, want the entry's", got)
	}
	if s.ReadReferencedResult(hashN(1), replaced) != nil {
		t.Fatal("a result neither a pin nor the entry holds reads")
	}
}

// TestCollectionNeverDeletesAResultBeingPinned holds the store's shared lock,
// as PinSession does while it keeps results: a collection that does not wait
// leaves every kept result in place, and one that waits deletes only once the
// lock is released.
func TestCollectionNeverDeletesAResultBeingPinned(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "cached-task-by-reference", "retention-keeps-the-results-retained-sessions-reference")
	root := t.TempDir()
	s := NewLocalStore(root)
	result := putResult(t, s, hashN(1), 1)
	session := t.TempDir()
	if err := s.PinSession(session, []PinnedResult{{hashN(1), result}}); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(session); err != nil {
		t.Fatal(err)
	}
	release := s.lockShared()
	if sweepPinnedResults(root, true) || !exists(pinnedObject(root, result)) {
		release()
		t.Fatal("a collection that does not wait deleted a result while the store's shared lock was held")
	}
	release()
	if !sweepPinnedResults(root, true) || exists(pinnedObject(root, result)) {
		t.Fatal("the result of a pruned session outlived a collection that took the lock")
	}
}

// TestReadTaskEntryDigestsItsResultAndStampsNothing reads an entry the way a
// reader of a recorded session does: the entry carries the SHA-256 of its
// result.json, and neither that read nor reading the result a reference names
// changes its recency.
func TestReadTaskEntryDigestsItsResultAndStampsNothing(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "cached-task-by-reference", "readers-rebuild-the-replayed-records")
	s := NewLocalStore(t.TempDir())
	key := hashN(4)
	putSized(t, s, key, 100)
	blobDir := taskBlobDir(s, key)
	old := time.Now().Add(-72 * time.Hour).Truncate(time.Second)
	writeLastUsed(blobDir, old, 7)

	entry := s.ReadTaskEntry(key)
	if entry == nil {
		t.Fatal("ReadTaskEntry missed a published entry")
	}
	data, err := os.ReadFile(filepath.Join(blobDir, "result.json"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	if want := "sha256:" + hex.EncodeToString(sum[:]); entry.ResultDigest != want {
		t.Fatalf("ResultDigest = %q, want %q", entry.ResultDigest, want)
	}
	if s.ReadReferencedResult(key, entry.ResultDigest) == nil {
		t.Fatal("ReadReferencedResult missed the entry's result")
	}
	if used, gen := readLastUsed(blobDir); !used.Equal(old) || gen != 7 {
		t.Fatalf("a read stamped recency %v (generation %d), want %v (7)", used, gen, old)
	}
	if s.ReadTaskEntry(hashN(5)) != nil {
		t.Fatal("ReadTaskEntry served a key nothing was published under")
	}
	if NewLocalStore(filepath.Join(t.TempDir(), "absent")).ReadTaskEntry(key) != nil {
		t.Fatal("a store that does not exist served an entry")
	}
}

// TestPinningASessionDeletesThePinsOfGoneSessions pins two sessions, removes
// the first one's directory, and pins a third: the gone session's pin leaves,
// the retained ones stay, so pins never outnumber the retained sessions by
// more than those pruned since the last run. A pin of version 1, which kept
// entries rather than results, leaves too; one of a later version stays, and
// collection then keeps every result: it may name any.
func TestPinningASessionDeletesThePinsOfGoneSessions(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "cached-task-by-reference", "retention-keeps-the-results-retained-sessions-reference")
	root := t.TempDir()
	s := NewLocalStore(root)
	first, second := putResult(t, s, hashN(1), 1), putResult(t, s, hashN(2), 2)
	gone, kept, latest := t.TempDir(), t.TempDir(), t.TempDir()
	for _, session := range []string{gone, kept} {
		if err := s.PinSession(session, []PinnedResult{{hashN(1), first}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.RemoveAll(gone); err != nil {
		t.Fatal(err)
	}
	pins := filepath.Join(root, pinsDirName)
	if err := os.WriteFile(filepath.Join(pins, "unknown.json"), []byte(`{"version":9}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pins, "entries.json"), []byte(`{"version":1,"session":"`+kept+`","entries":["blobs/00/0"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.PinSession(latest, []PinnedResult{{hashN(2), second}}); err != nil {
		t.Fatal(err)
	}
	for session, want := range map[string]bool{gone: false, kept: true, latest: true} {
		dir, _ := filepath.Abs(session)
		if exists(s.pinPath(dir)) != want {
			t.Errorf("pin of %s present = %v, want %v", session, !want, want)
		}
	}
	if !exists(filepath.Join(pins, "unknown.json")) || exists(filepath.Join(pins, "entries.json")) {
		t.Error("a pin of a later version was deleted, or one of version 1 kept")
	}

	if err := os.RemoveAll(kept); err != nil {
		t.Fatal(err)
	}
	sweepPinnedResults(root, false)
	if !exists(pinnedObject(root, first)) {
		t.Fatal("collection deleted a result while a pin it cannot read may name it")
	}
}

// TestTheIdlePassKeepsPinnedResults runs a collection far under its budget
// whose idle pass evicts a pinned entry: the entry goes like any other, and
// the result its retained session's pin names stays and reads.
func TestTheIdlePassKeepsPinnedResults(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "cached-task-by-reference", "retention-keeps-the-results-retained-sessions-reference")
	root := t.TempDir()
	s := NewLocalStore(root)
	now := time.Now()
	result := putResult(t, s, hashN(1), 1)
	writeGenerationFile(root, 200)
	writeLastUsed(taskBlobDir(s, hashN(1)), now.Add(-2*time.Hour), 50)
	if err := s.PinSession(t.TempDir(), []PinnedResult{{hashN(1), result}}); err != nil {
		t.Fatal(err)
	}
	res, err := RunGC([]string{root}, GCOptions{MaxBytes: 1 << 30, Grace: time.Hour, Now: now, MaxIdleGenerations: 100})
	if err != nil {
		t.Fatalf("RunGC: %v", err)
	}
	if res.IdleEvicted != 1 || publishedEntry(s, hashN(1)) != nil {
		t.Fatalf("idle-evicted %d, entry kept %v: a pin keeps no entry", res.IdleEvicted, publishedEntry(s, hashN(1)) != nil)
	}
	if s.ReadReferencedResult(hashN(1), result) == nil {
		t.Fatal("the idle pass deleted the result a retained session pins")
	}
}
