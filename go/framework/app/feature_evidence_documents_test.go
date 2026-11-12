package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	protofeatures "go.putnami.dev/protocol/features"
)

func writeAuthoredFile(t *testing.T, root, name, body string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func authoredDocument(id string) string {
	return `{"protocolVersion":1,"namespace":"billing","feature":{"id":"` + id + `","type":"feature","name":"Name","outcome":"Outcome","owner":"billing","target":"coded",` +
		`"requirements":[{"id":"implementation","stage":"coded","evidenceKinds":["capability"]}]}}`
}

func TestReadAuthoredFeatureManifestMergesFeatureDocuments(t *testing.T) {
	root := t.TempDir()
	writeAuthoredFile(t, root, "features/refund.json", authoredDocument("billing/refund"))
	writeAuthoredFile(t, root, "features/export.json", authoredDocument("billing/export"))
	writeAuthoredFile(t, root, "features/notes.md", "not a document")

	manifest, err := readAuthoredFeatureManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	if manifest == nil || manifest.Namespace != "billing" || len(manifest.Features) != 2 ||
		manifest.Features[0].ID != "billing/export" || manifest.Features[1].ID != "billing/refund" {
		t.Fatalf("manifest = %#v, want both documents in filename order", manifest)
	}
}

func TestReadAuthoredFeatureManifestRefusesBothForms(t *testing.T) {
	root := t.TempDir()
	writeAuthoredFile(t, root, protofeatures.ManifestFilename,
		`{"protocolVersion":1,"namespace":"billing","features":[]}`)
	writeAuthoredFile(t, root, "features/export.json", authoredDocument("billing/export"))

	if _, err := readAuthoredFeatureManifest(root); err == nil || !strings.Contains(err.Error(), protofeatures.ErrorCodeManifestFormConflict) {
		t.Fatalf("err = %v, want a form conflict", err)
	}
}

func TestReadAuthoredFeatureManifestNamesAnInvalidDocument(t *testing.T) {
	root := t.TempDir()
	writeAuthoredFile(t, root, "features/export.json", strings.Replace(authoredDocument("billing/export"), `"name":"Name"`, `"name":""`, 1))

	if _, err := readAuthoredFeatureManifest(root); err == nil || !strings.Contains(err.Error(), "features/export.json") ||
		!strings.Contains(err.Error(), "feature.name") {
		t.Fatalf("err = %v, want the document and the feature position", err)
	}
}

func TestReadAuthoredFeatureManifestSkipsANestedProjectNamedFeatures(t *testing.T) {
	root := t.TempDir()
	writeAuthoredFile(t, root, "features/putnami.json", `{"name":"features"}`)
	writeAuthoredFile(t, root, "features/package.json", `{}`)

	manifest, err := readAuthoredFeatureManifest(root)
	if err != nil || manifest != nil {
		t.Fatalf("manifest = %#v, err = %v, want no authored intent", manifest, err)
	}
}
