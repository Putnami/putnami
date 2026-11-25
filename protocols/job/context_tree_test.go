package job

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
)

// TestTreeFixturesRoundTrip pins that every valid tree fixture parses,
// validates and keeps an empty listing as an answer: written again, `[]`
// stays `[]` and an absent listing stays absent.
func TestTreeFixturesRoundTrip(t *testing.T) {
	spectest.Proves(t, "job/execution-context", "tree-facts-are-answers", "an-empty-listing-stays-an-answer")
	for _, path := range []string{"fixtures/v2/valid/tree-facts.json", "fixtures/v2/valid/tree-unmanaged.json", "fixtures/v2/valid/tree-untagged.json",
		"fixtures/v2/valid/tree-candidates.json", "fixtures/v2/valid/tree-history-shallow.json"} {
		ctx, diags := ParseAndValidate(readFixture(t, path))
		if diag.HasErrors(diags) {
			t.Fatalf("%s: %v", path, diags)
		}
		if ctx.Tree == nil {
			t.Fatalf("%s: no tree", path)
		}
		data, err := json.Marshal(ctx)
		if err != nil {
			t.Fatal(err)
		}
		again, diags := ParseAndValidate(data)
		if diag.HasErrors(diags) {
			t.Fatalf("%s written again: %v", path, diags)
		}
		if (ctx.Tree.TrackedFiles == nil) != (again.Tree.TrackedFiles == nil) ||
			(ctx.Tree.IgnoredDirectories == nil) != (again.Tree.IgnoredDirectories == nil) ||
			(ctx.Tree.CandidateFiles == nil) != (again.Tree.CandidateFiles == nil) {
			t.Errorf("%s: a listing changed between absent and empty when written again", path)
		}
	}
	unmanaged, _ := ParseAndValidate(readFixture(t, "fixtures/v2/valid/tree-unmanaged.json"))
	if unmanaged.Tree.TrackedFiles == nil || len(unmanaged.Tree.TrackedFiles) != 0 {
		t.Errorf("trackedFiles = %#v, want the empty answer", unmanaged.Tree.TrackedFiles)
	}
	facts, _ := ParseAndValidate(readFixture(t, "fixtures/v2/valid/tree-facts.json"))
	if baseline := facts.Tree.ReleaseBaseline; baseline == nil || baseline.State != ReleaseBaselineTagged || baseline.Tag != "v1.2.0" || !baseline.Breaking {
		t.Errorf("releaseBaseline = %#v, want the tagged fixture baseline", baseline)
	}
}

// TestValidate_TreeIsVersion2Only pins that a version 1 document refuses the
// tree member, an empty one included.
func TestValidate_TreeIsVersion2Only(t *testing.T) {
	spectest.Proves(t, "job/execution-context", "version-1-stays-frozen", "a-version-1-document-refuses-the-version-2-members")
	v1 := &Context{WorkspaceRoot: "/ws", OutputPath: "/o", CacheRoot: "/c",
		Project: Project{Name: "p"}, Extension: Extension{Name: "e"}, Job: Job{Name: "j"},
		Tree: &Tree{Directory: ".", TrackedFiles: []string{}}}
	assertDiagnostic(t, Validate(v1), ErrorCodeUnexpectedField, "tree")
}

// TestTreeVocabulariesAreSorted pins the canonical order of the closed
// vocabularies, which a producer in another language iterates.
func TestTreeVocabulariesAreSorted(t *testing.T) {
	for name, vocabulary := range map[string][]string{"repository": RepositoryStates, "releaseBaseline": ReleaseBaselineStates} {
		for i := 1; i < len(vocabulary); i++ {
			if vocabulary[i-1] >= vocabulary[i] {
				t.Errorf("%s states are not sorted: %q precedes %q", name, vocabulary[i-1], vocabulary[i])
			}
		}
	}
}

// TestCandidateListingRoundTrip pins that a listing file reads back what was
// encoded, and that a file whose bytes do not match the digest, or whose cut is
// not sorted, is refused.
func TestCandidateListingRoundTrip(t *testing.T) {
	spectest.Proves(t, "job/execution-context", "candidate-cut-and-history-are-answers", "a-candidate-listing-file-matches-its-digest")
	files := []CandidateFile{{Path: "a/b.go", Mode: CandidateModeRegular}, {Path: "a/new.txt", Mode: CandidateModeUntracked}}
	data, digest, err := EncodeCandidateFiles(files)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "candidates.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ReadCandidateListing(&CandidateListing{File: path, Digest: digest})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, files) {
		t.Errorf("read %v, want %v", got, files)
	}
	if _, err := ReadCandidateListing(&CandidateListing{File: path, Digest: strings.Repeat("0", 64)}); err == nil {
		t.Error("a listing whose digest does not match was read")
	}
	unsorted, unsortedDigest, err := EncodeCandidateFiles([]CandidateFile{files[1], files[0]})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, unsorted, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadCandidateListing(&CandidateListing{File: path, Digest: unsortedDigest}); err == nil {
		t.Error("an unsorted listing was read")
	}
}

// TestTreeCandidateModesAreSorted pins the canonical order of the candidate
// modes.
func TestTreeCandidateModesAreSorted(t *testing.T) {
	for i := 1; i < len(CandidateModes); i++ {
		if CandidateModes[i-1] >= CandidateModes[i] {
			t.Errorf("candidate modes are not sorted: %q precedes %q", CandidateModes[i-1], CandidateModes[i])
		}
	}
}
