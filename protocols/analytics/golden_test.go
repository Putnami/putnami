package analytics

import (
	"os"
	"path/filepath"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

// TestGoldenBatchConformsToVocabulary proves the single importable fixture is a
// conforming batch, so a consumer that needs a reference payload can import
// GoldenBatchJSON instead of vendoring a copy that nothing validates.
func TestGoldenBatchConformsToVocabulary(t *testing.T) {
	batch, diags := ParseAndValidateBatch(GoldenBatchJSON)
	if diag.HasErrors(diags) {
		t.Fatalf("golden batch failed validation: %v", diags)
	}
	if batch == nil {
		t.Fatal("golden batch decoded to nil")
	}
	if len(batch.Events) != 2 {
		t.Fatalf("golden batch carries %d events, want 2 (one page view, one action)", len(batch.Events))
	}
	if batch.Events[0].Name != EventPageView || batch.Events[1].Name != EventAction {
		t.Errorf("golden batch carries %q then %q, want %q then %q",
			batch.Events[0].Name, batch.Events[1].Name, EventPageView, EventAction)
	}
	if batch.Events[0].Page == nil || batch.Events[0].Page.UTM == nil {
		t.Error("the golden page view must populate page.utm — it is the reference for the campaign columns")
	}
	if len(batch.Events[1].Props) != 3 {
		t.Errorf("the golden action carries %d props, want 3 (string, number, boolean)", len(batch.Events[1].Props))
	}
}

// TestGoldenBatchMatchesItsFixtureFile pins the embed to the file on disk. The
// TypeScript side reads the fixture through a relative path while Go consumers
// read the embedded bytes; if the two could drift, "both runtimes agree on the
// golden" would be a claim about two different documents.
func TestGoldenBatchMatchesItsFixtureFile(t *testing.T) {
	onDisk, err := os.ReadFile(filepath.Join("fixtures", "equivalence", "batch.golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk) != string(GoldenBatchJSON) {
		t.Error("GoldenBatchJSON does not match fixtures/equivalence/batch.golden.json")
	}
}
