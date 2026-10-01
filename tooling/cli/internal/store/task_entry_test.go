package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	proto "go.putnami.dev/protocol/extension"
)

// --- shared fixtures -------------------------------------------------------

// dirOutput / fileOutput build the two declared-output shapes the store knows.
func dirOutput(id, path string) DeclaredEntryOutput {
	return DeclaredEntryOutput{ID: id, Kind: proto.OutputKindDirectory, Root: proto.OutputRootProject, Path: path}
}

func fileOutput(id, path string) DeclaredEntryOutput {
	return DeclaredEntryOutput{ID: id, Kind: proto.OutputKindFile, Root: proto.OutputRootProject, Path: path}
}

// stage writes content at an output's staging location; rel is appended for
// directory outputs and must be empty for file outputs.
func stage(t *testing.T, stagingRoot string, out DeclaredEntryOutput, rel, content string) {
	t.Helper()
	p := TaskStagingPath(stagingRoot, out)
	if rel != "" {
		p = filepath.Join(p, filepath.FromSlash(rel))
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func taskSpec(key string, outputs ...DeclaredEntryOutput) TaskEntrySpec {
	return TaskEntrySpec{
		Key:      key,
		Result:   &EntryResult{Status: "success"},
		Metadata: &EntryMetadata{Extension: "ext", Task: "build~transpile", Project: "pkg"},
		Outputs:  outputs,
	}
}

// ingestOne publishes a one-directory-output entry, the minimal fixture most
// tests need.
func ingestOne(t *testing.T, s *LocalStore, key, content string) *TaskEntry {
	t.Helper()
	staging := t.TempDir()
	out := dirOutput("dist", "dist")
	stage(t, staging, out, "main.js", content)
	entry, err := s.IngestTaskEntry(staging, taskSpec(key, out))
	if err != nil {
		t.Fatalf("IngestTaskEntry: %v", err)
	}
	return entry
}

// --- entry-format version and migration semantics --------------------------

// TestTaskEntry_LegacyEntryReadsAsMiss is the invariant the whole format version
// exists for: an entry written by any CLI up to B3 carries a correct cache key
// v5 and a legacy payload, and the task-owned model must refuse it rather than
// reinterpret its files/ tree.
//
// The positive control in the same test — the identical key served after a
// task-owned ingest — is what proves the miss comes from the FORMAT and not from
// a lookup that never finds anything.
func TestTaskEntry_LegacyEntryReadsAsMiss(t *testing.T) {
	root := t.TempDir()
	s := NewLocalStore(root)
	key := "v5keyaaaa0000000000000000000000000000000000000000000000000000000"

	if err := s.Put(key, entryWith(makeSource(t, "legacy"))); err != nil {
		t.Fatalf("legacy Put: %v", err)
	}

	entry, err := s.LookupTaskEntry(key)
	if err != nil {
		t.Fatalf("LookupTaskEntry: %v", err)
	}
	if entry != nil {
		t.Fatalf("legacy entry served through the task-owned lookup: %+v", entry.Outputs)
	}

	// Coexistence: the legacy reader still serves the legacy entry unchanged,
	// which is what keeps the scheduler working until B4b.
	legacy, err := s.Get(key)
	if err != nil || legacy == nil {
		t.Fatalf("legacy Get after task-owned lookup: entry=%v err=%v", legacy, err)
	}

	// Positive control: same key, task-owned entry → hit.
	ingestOne(t, s, key, "declared")
	entry, err = s.LookupTaskEntry(key)
	if err != nil || entry == nil {
		t.Fatalf("task-owned entry not served: entry=%v err=%v", entry, err)
	}
	if entry.Format != CurrentEntryFormat {
		t.Errorf("entry format = %d, want %d", entry.Format, CurrentEntryFormat)
	}
}

// TestTaskEntry_AddressBindsTheFormatVersion pins the migration mechanism: the
// two models occupy disjoint addresses derived from the same key, so neither can
// see the other's payload — including CLI binaries already built from an older
// worktree, which cannot be taught a new guard.
func TestTaskEntry_AddressBindsTheFormatVersion(t *testing.T) {
	key := "some-cache-key"

	address := TaskEntryAddress(key)
	if address == key {
		t.Fatal("task-owned address equals the raw key: the formats share an address")
	}
	if address != TaskEntryAddress(key) {
		t.Error("TaskEntryAddress is not deterministic")
	}
	if TaskEntryAddress("other-key") == address {
		t.Error("distinct keys collide on one address")
	}
	if len(address) != 64 {
		t.Errorf("address length = %d, want a 64-char sha256 hex digest", len(address))
	}
}

// TestTaskEntry_LegacyReaderRefusesTaskOwnedBlob is the reverse direction:
// reaching a task-owned blob through the legacy reader must be a miss, so a
// declared payload is never restored as an inferred capture.
func TestTaskEntry_LegacyReaderRefusesTaskOwnedBlob(t *testing.T) {
	s := NewLocalStore(t.TempDir())
	key := "reverse-direction-key"

	entry := ingestOne(t, s, key, "declared")

	legacy, err := s.Get(entry.Address)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if legacy != nil {
		t.Errorf("legacy Get served a task-owned blob: %+v", legacy.Metadata)
	}

	// Positive control: a legacy blob at its own address is still served.
	if err := s.Put("legacy-key", entryWith(makeSource(t, "legacy"))); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Get("legacy-key"); err != nil || got == nil {
		t.Fatalf("legacy blob no longer readable: got=%v err=%v", got, err)
	}
}

// TestTaskEntry_ForeignFormatReadsAsMiss mutates a published descriptor's format
// version in place and asserts the entry disappears from the lookup, then
// restores it and asserts it comes back. That pair is what proves the version
// field is actually consulted rather than merely written.
func TestTaskEntry_ForeignFormatReadsAsMiss(t *testing.T) {
	s := NewLocalStore(t.TempDir())
	key := "format-check-key"
	entry := ingestOne(t, s, key, "declared")

	descriptorPath := filepath.Join(s.blobDir(entry.Address), entryDescriptorFilename)
	original, err := os.ReadFile(descriptorPath)
	if err != nil {
		t.Fatal(err)
	}

	for _, format := range []int{EntryFormatLegacy, CurrentEntryFormat + 1, 0} {
		mutated := editDescriptor(t, original,
			fmt.Sprintf(`"entryFormat": %d`, CurrentEntryFormat),
			fmt.Sprintf(`"entryFormat": %d`, format))
		if err := os.WriteFile(descriptorPath, mutated, 0o644); err != nil {
			t.Fatal(err)
		}
		got, err := s.LookupTaskEntry(key)
		if err != nil {
			t.Fatalf("format %d: LookupTaskEntry: %v", format, err)
		}
		if got != nil {
			t.Errorf("format %d served as a hit; a format this build does not read must be a miss", format)
		}
	}

	if err := os.WriteFile(descriptorPath, original, 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := s.LookupTaskEntry(key); err != nil || got == nil {
		t.Fatalf("restored descriptor did not read back: got=%v err=%v", got, err)
	}
}

// TestTaskEntry_KeyMismatchReadsAsMiss pins that the recorded key is verified,
// not just the derived address: an entry that claims another key is a miss.
func TestTaskEntry_KeyMismatchReadsAsMiss(t *testing.T) {
	s := NewLocalStore(t.TempDir())
	key := "key-binding"
	entry := ingestOne(t, s, key, "declared")

	descriptorPath := filepath.Join(s.blobDir(entry.Address), entryDescriptorFilename)
	original, err := os.ReadFile(descriptorPath)
	if err != nil {
		t.Fatal(err)
	}
	mutated := editDescriptor(t, original, `"key": "`+key+`"`, `"key": "a-different-key"`)
	if err := os.WriteFile(descriptorPath, mutated, 0o644); err != nil {
		t.Fatal(err)
	}

	if got, err := s.LookupTaskEntry(key); err != nil || got != nil {
		t.Fatalf("entry recording another key was served: got=%v err=%v", got, err)
	}
	if err := os.WriteFile(descriptorPath, original, 0o644); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.LookupTaskEntry(key); got == nil {
		t.Fatal("positive control failed: restored descriptor is still a miss")
	}
}

// TestTaskEntry_EmptyStateIsNeverInferred pins the explicit optional-empty
// representation. The state is a required field: a descriptor with it removed is
// NOT read as "present" (nor as "empty") — it is not interpretable at all, so it
// misses. Emptiness can therefore never be the consequence of something being
// absent from the entry.
func TestTaskEntry_EmptyStateIsNeverInferred(t *testing.T) {
	s := NewLocalStore(t.TempDir())
	key := "explicit-empty"
	staging := t.TempDir()
	present := dirOutput("dist", "dist")
	missing := dirOutput("coverage", "coverage")
	missing.Optional = true
	stage(t, staging, present, "main.js", "built")

	entry, err := s.IngestTaskEntry(staging, taskSpec(key, present, missing))
	if err != nil {
		t.Fatalf("IngestTaskEntry: %v", err)
	}

	descriptorPath := filepath.Join(s.blobDir(entry.Address), entryDescriptorFilename)
	raw, err := os.ReadFile(descriptorPath)
	if err != nil {
		t.Fatal(err)
	}
	// The empty output is spelled out on disk rather than omitted.
	if !strings.Contains(string(raw), `"state": "`+TaskOutputEmpty+`"`) {
		t.Errorf("descriptor does not record the empty state explicitly:\n%s", raw)
	}
	if out, ok := entry.Output("coverage"); !ok || out.State != TaskOutputEmpty || out.Present() {
		t.Errorf("optional-empty output recorded as %+v", out)
	}
	if out, ok := entry.Output("dist"); !ok || !out.Present() {
		t.Errorf("present output recorded as %+v", out)
	}

	// Mutation: strip the state field. A reader must not fall back to a default.
	stripped := stripDescriptorField(t, raw, "state")
	if err := os.WriteFile(descriptorPath, stripped, 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := s.LookupTaskEntry(key); err != nil || got != nil {
		t.Fatalf("descriptor without an explicit state was interpreted: got=%v err=%v", got, err)
	}
	if err := os.WriteFile(descriptorPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.LookupTaskEntry(key); got == nil {
		t.Fatal("positive control failed: restored descriptor is still a miss")
	}
}

// TestTaskEntry_EmptyRequiresOptional pins that an entry cannot claim a REQUIRED
// output is legitimately empty — the only way to be empty is to have been
// declared optionalEmpty.
func TestTaskEntry_EmptyRequiresOptional(t *testing.T) {
	err := validateRecordedOutputs([]TaskEntryOutput{{
		ID: "dist", Kind: proto.OutputKindDirectory, Root: proto.OutputRootProject,
		Path: "dist", Optional: false, State: TaskOutputEmpty,
	}})
	if err == nil {
		t.Fatal("a required output recorded empty was accepted")
	}
	// Positive control: the same record with Optional set is valid.
	if err := validateRecordedOutputs([]TaskEntryOutput{{
		ID: "dist", Kind: proto.OutputKindDirectory, Root: proto.OutputRootProject,
		Path: "dist", Optional: true, State: TaskOutputEmpty,
	}}); err != nil {
		t.Fatalf("optional empty output rejected: %v", err)
	}
}

// TestTaskEntry_RecordedOutputVocabulariesAreClosed pins that kind, root and
// state are validated on read, since each of them decides a materialize.
func TestTaskEntry_RecordedOutputVocabulariesAreClosed(t *testing.T) {
	valid := TaskEntryOutput{
		ID: "dist", Kind: proto.OutputKindDirectory, Root: proto.OutputRootProject,
		Path: "dist", State: TaskOutputPresent, Files: 1, Size: 3,
	}
	if err := validateRecordedOutputs([]TaskEntryOutput{valid}); err != nil {
		t.Fatalf("valid record rejected: %v", err)
	}

	mutations := map[string]func(o *TaskEntryOutput){
		"unknown kind":  func(o *TaskEntryOutput) { o.Kind = "socket" },
		"unknown root":  func(o *TaskEntryOutput) { o.Root = "home" },
		"unknown state": func(o *TaskEntryOutput) { o.State = "maybe" },
		"escaping path": func(o *TaskEntryOutput) { o.Path = "../outside" },
		"glob path":     func(o *TaskEntryOutput) { o.Path = "dist/**" },
		"empty id":      func(o *TaskEntryOutput) { o.ID = "" },
		"nested id":     func(o *TaskEntryOutput) { o.ID = "a/b" },
		"dotdot id":     func(o *TaskEntryOutput) { o.ID = ".." },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			broken := valid
			mutate(&broken)
			if err := validateRecordedOutputs([]TaskEntryOutput{broken}); err == nil {
				t.Errorf("%s accepted: %+v", name, broken)
			}
		})
	}

	t.Run("duplicate id", func(t *testing.T) {
		if err := validateRecordedOutputs([]TaskEntryOutput{valid, valid}); err == nil {
			t.Error("duplicate output ids accepted")
		}
	})
}

// --- lease coalescing ------------------------------------------------------

// TestTaskEntry_LeaseCoalescesOnTheDerivedAddress pins that the existing lease
// primitive is EXTENDED rather than forked: exactly one claimant wins, waiting
// on a published entry returns immediately, and a legacy claim on the same cache
// key does not block a task-owned one (they publish different artifacts).
func TestTaskEntry_LeaseCoalescesOnTheDerivedAddress(t *testing.T) {
	s := NewLocalStore(t.TempDir())
	key := "leased-key"

	winner, release := s.TryClaimTaskEntry(key)
	if !winner {
		t.Fatal("first claimant lost")
	}
	if again, _ := s.TryClaimTaskEntry(key); again {
		t.Error("second claimant also won while a lease was live")
	}
	// A legacy claim on the same cache key is a different artifact: it must not
	// be blocked by the task-owned lease.
	legacyWinner, legacyRelease := s.TryClaim(key)
	if !legacyWinner {
		t.Error("legacy claim blocked by a task-owned lease on the same key")
	}
	legacyRelease()

	release()
	ingestOne(t, s, key, "declared")
	if err := s.WaitForTaskEntry(key, 2*time.Second); err != nil {
		t.Errorf("WaitForTaskEntry after publish: %v", err)
	}
	if claimed, _ := s.TryClaimTaskEntry(key); claimed {
		t.Error("claim won for an already published entry")
	}
}

// editDescriptor rewrites the SERIALIZED descriptor, replacing old with new
// once. Mutating the bytes rather than a re-marshaled document is what makes
// the assertion honest: the test hands the reader exactly the file another CLI
// could have written, including a field the typed struct would silently restore.
//
// A replacement that matches nothing is fatal, so a change in the descriptor's
// serialization can never turn one of these mutations into a no-op and leave the
// assertion around it vacuous.
func editDescriptor(t *testing.T, raw []byte, old, new string) []byte {
	t.Helper()
	if !bytes.Contains(raw, []byte(old)) {
		t.Fatalf("descriptor does not contain %q, so the mutation would be a no-op:\n%s", old, raw)
	}
	return bytes.Replace(raw, []byte(old), []byte(new), 1)
}

// stripDescriptorField removes every line declaring field, which is how a test
// asks "what does the reader do when this is simply absent?". Struct field order
// puts every removable field before another one, so the result stays parseable
// JSON — the point is that it parses and is still refused.
func stripDescriptorField(t *testing.T, raw []byte, field string) []byte {
	t.Helper()
	re := regexp.MustCompile(`(?m)^[\t ]*"` + field + `": [^\n]*\n`)
	if !re.Match(raw) {
		t.Fatalf("descriptor declares no %q field, so stripping it would be a no-op:\n%s", field, raw)
	}
	stripped := re.ReplaceAll(raw, nil)
	if !json.Valid(stripped) {
		t.Fatalf("stripping %q produced invalid JSON, which is not the case under test:\n%s", field, stripped)
	}
	return stripped
}
