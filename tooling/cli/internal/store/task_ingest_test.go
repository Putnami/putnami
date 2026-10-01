package store

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	proto "go.putnami.dev/protocol/extension"
)

// TestIngestTaskEntry_CapturesExactlyTheDeclaration is the happy path across all
// three roots and both kinds: the entry records the declaration (id, kind, root,
// resolved relative path) and its payload is laid out by output id, so a restore
// never has to guess which task's bytes it is holding.
func TestIngestTaskEntry_CapturesExactlyTheDeclaration(t *testing.T) {
	s := NewLocalStore(t.TempDir())
	staging := t.TempDir()

	dist := dirOutput("dist", "dist")
	lock := DeclaredEntryOutput{ID: "lock", Kind: proto.OutputKindFile, Root: proto.OutputRootWorkspace, Path: "bun.lock"}
	report := DeclaredEntryOutput{ID: "report", Kind: proto.OutputKindFile, Root: proto.OutputRootCommandOutput, Path: "report.json"}

	stage(t, staging, dist, "main.js", "built")
	stage(t, staging, dist, "nested/chunk.js", "chunk")
	stage(t, staging, lock, "", "lockfile")
	stage(t, staging, report, "", "{}")

	// Declaration order is deliberately not id order: the descriptor must be
	// canonical regardless.
	entry, err := s.IngestTaskEntry(staging, taskSpec("declaration-key", report, dist, lock))
	if err != nil {
		t.Fatalf("IngestTaskEntry: %v", err)
	}

	gotIDs := make([]string, 0, len(entry.Outputs))
	for _, out := range entry.Outputs {
		gotIDs = append(gotIDs, out.ID)
		if !out.Present() {
			t.Errorf("output %q recorded as %q, want present", out.ID, out.State)
		}
	}
	if want := []string{"dist", "lock", "report"}; !reflect.DeepEqual(gotIDs, want) {
		t.Errorf("recorded output ids = %v, want %v (sorted)", gotIDs, want)
	}

	distRecord, _ := entry.Output("dist")
	if distRecord.Kind != proto.OutputKindDirectory || distRecord.Root != proto.OutputRootProject || distRecord.Path != "dist" {
		t.Errorf("dist recorded as %+v", distRecord)
	}
	if distRecord.Files != 2 {
		t.Errorf("dist file count = %d, want 2", distRecord.Files)
	}
	lockRecord, _ := entry.Output("lock")
	if lockRecord.Root != proto.OutputRootWorkspace || lockRecord.Path != "bun.lock" {
		t.Errorf("lock recorded as %+v", lockRecord)
	}

	// Payload layout: files/<id> for a file output, files/<id>/<rel> for a
	// directory output.
	assertBlobFile(t, entry, "dist/main.js", "built")
	assertBlobFile(t, entry, "dist/nested/chunk.js", "chunk")
	assertBlobFile(t, entry, "lock", "lockfile")
	assertBlobFile(t, entry, "report", "{}")

	// The CAS manifest mirrors the same paths, so protocols/cache needs no change.
	if entry.Manifest == nil || len(entry.Manifest.Files) != 4 {
		t.Fatalf("manifest = %+v, want 4 files", entry.Manifest)
	}
	want := []string{"dist/main.js", "dist/nested/chunk.js", "lock", "report"}
	got := make([]string, 0, len(entry.Manifest.Files))
	for _, f := range entry.Manifest.Files {
		got = append(got, f.Path)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("manifest paths = %v, want %v", got, want)
	}
}

// TestIngestTaskEntry_RefusesPartialCapture is the completeness invariant: a
// required declared output the task did not produce is an ingest ERROR and
// nothing is published. A smaller entry would be indistinguishable from a
// complete one on every future hit.
func TestIngestTaskEntry_RefusesPartialCapture(t *testing.T) {
	shapes := map[string]func(t *testing.T, staging string, out DeclaredEntryOutput){
		"absent": func(*testing.T, string, DeclaredEntryOutput) {},
		"empty directory": func(t *testing.T, staging string, out DeclaredEntryOutput) {
			if err := os.MkdirAll(TaskStagingPath(staging, out), 0o755); err != nil {
				t.Fatal(err)
			}
		},
		"only throwaway artifacts": func(t *testing.T, staging string, out DeclaredEntryOutput) {
			stage(t, staging, out, "lcov.info.1.tmp", "shard")
		},
	}

	for name, prepare := range shapes {
		t.Run(name, func(t *testing.T) {
			s := NewLocalStore(t.TempDir())
			staging := t.TempDir()
			key := "incomplete-" + name

			required := dirOutput("dist", "dist")
			sibling := dirOutput("gen", ".gen")
			stage(t, staging, sibling, "types.d.ts", "types")
			prepare(t, staging, required)

			_, err := s.IngestTaskEntry(staging, taskSpec(key, required, sibling))
			if !errors.Is(err, ErrIncompleteCapture) {
				t.Fatalf("err = %v, want ErrIncompleteCapture", err)
			}
			if got, err := s.LookupTaskEntry(key); err != nil || got != nil {
				t.Errorf("a rejected ingest published an entry: %v (err=%v)", got, err)
			}

			// Positive control: staging the required output makes the very same
			// spec succeed, so the rejection is about completeness and nothing else.
			stage(t, staging, required, "main.js", "built")
			if _, err := s.IngestTaskEntry(staging, taskSpec(key, required, sibling)); err != nil {
				t.Fatalf("complete ingest rejected: %v", err)
			}
			if got, _ := s.LookupTaskEntry(key); got == nil {
				t.Error("complete ingest published nothing")
			}
		})
	}
}

// TestIngestTaskEntry_OptionalEmptyIsAllowedAndRecorded pins the other half:
// exactly the same missing output is fine when the declaration marks it
// optionalEmpty, and the entry says so explicitly instead of just carrying less.
func TestIngestTaskEntry_OptionalEmptyIsAllowedAndRecorded(t *testing.T) {
	s := NewLocalStore(t.TempDir())
	staging := t.TempDir()

	required := dirOutput("dist", "dist")
	optional := dirOutput("coverage", "coverage")
	optional.Optional = true
	stage(t, staging, required, "main.js", "built")

	entry, err := s.IngestTaskEntry(staging, taskSpec("optional-empty-key", required, optional))
	if err != nil {
		t.Fatalf("IngestTaskEntry: %v", err)
	}
	out, ok := entry.Output("coverage")
	if !ok {
		t.Fatal("optional-empty output is missing from the declared-output manifest")
	}
	if out.State != TaskOutputEmpty || out.Files != 0 || out.Size != 0 {
		t.Errorf("optional-empty output recorded as %+v", out)
	}
	if !out.Optional {
		t.Error("the record lost the declaration's optional flag")
	}

	// An optional output that DID produce bytes is recorded present, so
	// "optional" is not a synonym for "empty".
	stage(t, staging, optional, "lcov.info", "coverage")
	entry, err = s.IngestTaskEntry(staging, taskSpec("optional-present-key", required, optional))
	if err != nil {
		t.Fatalf("IngestTaskEntry: %v", err)
	}
	if out, _ := entry.Output("coverage"); !out.Present() {
		t.Errorf("optional output with bytes recorded as %+v", out)
	}
}

// TestIngestTaskEntry_CapturesAnOwnedCommandOutputFile pins the other side of
// the deleted guard: a declared command-output FILE is captured like any
// other output. The store used to refuse one exact path there — the package
// channel index, which several tasks read-merge-wrote — and that refusal would
// now reject the declaration that gave the path an owner.
func TestIngestTaskEntry_CapturesAnOwnedCommandOutputFile(t *testing.T) {
	s := NewLocalStore(t.TempDir())
	staging := t.TempDir()

	manifest := DeclaredEntryOutput{
		ID: "publication-manifest", Kind: proto.OutputKindFile,
		Root: proto.OutputRootCommandOutput, Path: "metadata.json",
	}
	stage(t, staging, manifest, "", `{"channels":["archives"]}`)

	entry, err := s.IngestTaskEntry(staging, taskSpec("owned-manifest-key", manifest))
	if err != nil {
		t.Fatalf("IngestTaskEntry: %v", err)
	}
	if out, _ := entry.Output("publication-manifest"); !out.Present() {
		t.Fatalf("owned command-output file recorded as %+v", out)
	}
}

// TestIngestTaskEntry_RejectsKindMismatch pins that a declared kind is a promise
// about the restore shape: staging that contradicts it fails at capture, where
// the diagnostic is local, instead of at restore on another machine.
func TestIngestTaskEntry_RejectsKindMismatch(t *testing.T) {
	t.Run("directory declared, file staged", func(t *testing.T) {
		s := NewLocalStore(t.TempDir())
		staging := t.TempDir()
		out := dirOutput("dist", "dist")
		p := TaskStagingPath(staging, out)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("not a tree"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := s.IngestTaskEntry(staging, taskSpec("kind-a", out)); err == nil {
			t.Fatal("a file staged for a directory output was accepted")
		}
	})

	t.Run("file declared, directory staged", func(t *testing.T) {
		s := NewLocalStore(t.TempDir())
		staging := t.TempDir()
		out := fileOutput("lcov", "lcov.info")
		if err := os.MkdirAll(TaskStagingPath(staging, out), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := s.IngestTaskEntry(staging, taskSpec("kind-b", out)); err == nil {
			t.Fatal("a directory staged for a file output was accepted")
		}
	})

	t.Run("positive control", func(t *testing.T) {
		s := NewLocalStore(t.TempDir())
		staging := t.TempDir()
		out := fileOutput("lcov", "lcov.info")
		stage(t, staging, out, "", "TN:")
		if _, err := s.IngestTaskEntry(staging, taskSpec("kind-c", out)); err != nil {
			t.Fatalf("matching kind rejected: %v", err)
		}
	})
}

// TestIngestTaskEntry_RejectsUnusableDeclarations pins the input validation that
// protects the blob layout: an output id doubles as its slot under files/, so it
// must be a single path segment, and paths must normalize.
func TestIngestTaskEntry_RejectsUnusableDeclarations(t *testing.T) {
	cases := map[string]DeclaredEntryOutput{
		"empty id":     {ID: "", Kind: proto.OutputKindFile, Path: "a"},
		"nested id":    {ID: "a/b", Kind: proto.OutputKindFile, Path: "a"},
		"escaping id":  {ID: "..", Kind: proto.OutputKindFile, Path: "a"},
		"unknown kind": {ID: "x", Kind: "socket", Path: "a"},
		"unknown root": {ID: "x", Kind: proto.OutputKindFile, Root: "home", Path: "a"},
		"glob path":    {ID: "x", Kind: proto.OutputKindDirectory, Path: "dist/**"},
		"escaping path": {
			ID: "x", Kind: proto.OutputKindDirectory, Path: "../outside",
		},
		"root itself": {ID: "x", Kind: proto.OutputKindDirectory, Path: "."},
	}
	for name, out := range cases {
		t.Run(name, func(t *testing.T) {
			s := NewLocalStore(t.TempDir())
			if _, err := s.IngestTaskEntry(t.TempDir(), taskSpec("bad-"+name, out)); err == nil {
				t.Errorf("%s accepted: %+v", name, out)
			}
		})
	}

	t.Run("duplicate ids", func(t *testing.T) {
		s := NewLocalStore(t.TempDir())
		staging := t.TempDir()
		a := fileOutput("same", "a.txt")
		b := fileOutput("same", "b.txt")
		stage(t, staging, a, "", "a")
		stage(t, staging, b, "", "b")
		if _, err := s.IngestTaskEntry(staging, taskSpec("dup-key", a, b)); err == nil {
			t.Error("two outputs sharing one id were accepted")
		}
	})
}

// TestIngestTaskEntry_DescriptorIsDeterministic pins that the entry is a
// function of the declaration and the bytes, not of authoring order: the same
// capture published under two keys yields byte-identical descriptors apart from
// the key itself.
func TestIngestTaskEntry_DescriptorIsDeterministic(t *testing.T) {
	s := NewLocalStore(t.TempDir())
	staging := t.TempDir()

	a := dirOutput("alpha", "alpha")
	b := dirOutput("zeta", "zeta")
	stage(t, staging, a, "one.txt", "1")
	stage(t, staging, b, "two.txt", "2")

	first, err := s.IngestTaskEntry(staging, taskSpec("order-key-1", a, b))
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.IngestTaskEntry(staging, taskSpec("order-key-2", b, a))
	if err != nil {
		t.Fatal(err)
	}

	firstRaw := strings.ReplaceAll(readDescriptor(t, s, first), "order-key-1", "KEY")
	secondRaw := strings.ReplaceAll(readDescriptor(t, s, second), "order-key-2", "KEY")
	if firstRaw != secondRaw {
		t.Errorf("descriptors differ with declaration order:\n%s\n---\n%s", firstRaw, secondRaw)
	}
}

// TestIngestTaskEntry_ExcludesThrowawayArtifacts keeps the legacy exclusion rule
// (coverage lcov shards) applying to declared capture, so a declared directory
// output does not smuggle multi-megabyte intermediates into the cache.
func TestIngestTaskEntry_ExcludesThrowawayArtifacts(t *testing.T) {
	s := NewLocalStore(t.TempDir())
	staging := t.TempDir()

	out := dirOutput("coverage", "coverage")
	stage(t, staging, out, "lcov.info", "TN:")
	stage(t, staging, out, "lcov.info.1.tmp", "shard")

	entry, err := s.IngestTaskEntry(staging, taskSpec("throwaway-key", out))
	if err != nil {
		t.Fatalf("IngestTaskEntry: %v", err)
	}
	record, _ := entry.Output("coverage")
	if record.Files != 1 {
		t.Errorf("captured %d files, want 1 (the shard must be excluded)", record.Files)
	}
	if _, err := os.Stat(filepath.Join(entry.FilesDir, "coverage", "lcov.info.1.tmp")); !os.IsNotExist(err) {
		t.Error("throwaway shard was captured into the entry")
	}
	assertBlobFile(t, entry, "coverage/lcov.info", "TN:")
}

// TestIngestTaskEntry_RejectsMissingKeyOrResult pins the two spec fields that
// cannot be defaulted: the address derives from the key, and a hit replays a
// result.
func TestIngestTaskEntry_RejectsMissingKeyOrResult(t *testing.T) {
	s := NewLocalStore(t.TempDir())
	if _, err := s.IngestTaskEntry(t.TempDir(), TaskEntrySpec{Result: &EntryResult{Status: "success"}}); err == nil {
		t.Error("ingest without a cache key was accepted")
	}
	if _, err := s.IngestTaskEntry(t.TempDir(), TaskEntrySpec{Key: "k"}); err == nil {
		t.Error("ingest without a result was accepted")
	}
	if _, err := s.IngestTaskEntry(t.TempDir(), taskSpec("no-outputs-key")); err != nil {
		t.Errorf("a task declaring no outputs must still be publishable: %v", err)
	}
}

// --- helpers ---------------------------------------------------------------

func assertBlobFile(t *testing.T, entry *TaskEntry, rel, want string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(entry.FilesDir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read blob file %s: %v", rel, err)
	}
	if string(data) != want {
		t.Errorf("blob file %s = %q, want %q", rel, data, want)
	}
}

func readDescriptor(t *testing.T, s *LocalStore, entry *TaskEntry) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(s.blobDir(entry.Address), entryDescriptorFilename))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
