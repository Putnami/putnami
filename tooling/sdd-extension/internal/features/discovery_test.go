package features

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	featureproto "go.putnami.dev/protocol/features"
	"go.putnami.dev/protocol/features/spectest"
	workspaceproto "go.putnami.dev/protocol/workspace"
	workspace "go.putnami.dev/tooling/sdd/extension/internal/wsview"
)

func TestDiscoverAuthoredManifestsReadsOnlyExactAuthorityRoots(t *testing.T) {
	root := t.TempDir()
	writeDiscoveryFixture(t, filepath.Join(root, featureproto.ManifestFilename), modeledManifestJSON("workspace/journey", "workspace"))
	writeDiscoveryFixture(t, filepath.Join(root, "tooling", featureproto.ManifestFilename), modeledManifestJSON("tooling/cli", "tooling"))
	writeDiscoveryFixture(t, filepath.Join(root, "tooling", "spec", featureproto.ManifestFilename), modeledManifestJSON("spec/invented", "spec"))
	writeDiscoveryFixture(t, filepath.Join(root, "tooling", "feature-spec.json"), modeledManifestJSON("spec/named", "spec"))
	writeDiscoveryFixture(t, filepath.Join(root, "undeclared", featureproto.ManifestFilename), modeledManifestJSON("undeclared/invented", "undeclared"))

	ws := workspace.NewWorkspace(root, &workspaceproto.Config{Name: "fixture"}, []*workspace.Project{
		{Name: "tooling", Path: "tooling"},
	})
	result := DiscoverAuthoredManifests(ws)
	if len(result.Diagnostics) != 0 {
		t.Fatalf("diagnostics = %#v", result.Diagnostics)
	}
	if len(result.Manifests) != 2 {
		t.Fatalf("manifests = %#v, want only workspace and exact project roots", result.Manifests)
	}
	if got := result.Manifests[0].Path + "," + result.Manifests[1].Path; got != "putnami.features.json,tooling/putnami.features.json" {
		t.Fatalf("manifest paths = %q", got)
	}
	for _, source := range result.Manifests {
		for _, feature := range source.Manifest.Features {
			if strings.Contains(feature.ID, "invented") || feature.ID == "spec/named" {
				t.Fatalf("non-root artifact minted feature authority: %#v", feature)
			}
		}
	}
}

func TestDiscoverAuthoredManifestsSkipsInvalidDocumentWithoutHidingHealthyRoot(t *testing.T) {
	root := t.TempDir()
	writeDiscoveryFixture(t, filepath.Join(root, "healthy", featureproto.ManifestFilename), modeledManifestJSON("healthy/catalog", "healthy"))
	writeDiscoveryFixture(t, filepath.Join(root, "invalid", featureproto.ManifestFilename),
		`{"protocolVersion":1,"namespace":"invalid","features":[{"id":"invalid/catalog","type":"feature","name":"","outcome":"Outcome","owner":"team","target":"modeled"}]}`)

	ws := workspace.NewWorkspace(root, &workspaceproto.Config{Name: "fixture"}, []*workspace.Project{
		{Name: "invalid", Path: "invalid"},
		{Name: "healthy", Path: "healthy"},
	})
	result := DiscoverAuthoredManifests(ws)
	if len(result.Manifests) != 1 || result.Manifests[0].Path != "healthy/putnami.features.json" {
		t.Fatalf("manifests = %#v, want the healthy exact-root manifest", result.Manifests)
	}
	if len(result.Diagnostics) == 0 || !strings.HasPrefix(result.Diagnostics[0].Field, "invalid/putnami.features.json#") {
		t.Fatalf("diagnostics = %#v, want exact invalid manifest provenance", result.Diagnostics)
	}
}

func TestDiscoverAuthoredManifestsReadsOneDocumentPerFeature(t *testing.T) {
	root := t.TempDir()
	writeDiscoveryFixture(t, filepath.Join(root, "tooling", "features", "run.json"), modeledDocumentJSON("tooling/run", "tooling"))
	writeDiscoveryFixture(t, filepath.Join(root, "tooling", "features", "init.json"), modeledDocumentJSON("tooling/init", "tooling"))
	writeDiscoveryFixture(t, filepath.Join(root, "tooling", "features", "notes.md"), "not a feature document")
	writeDiscoveryFixture(t, filepath.Join(root, "tooling", "features", "nested", "deep.json"), modeledDocumentJSON("tooling/deep", "tooling"))
	writeDiscoveryFixture(t, filepath.Join(root, "broken", "features", "bad.json"),
		`{"protocolVersion":1,"namespace":"broken","feature":{"id":"broken/bad","type":"feature","name":"","outcome":"Outcome","owner":"team","target":"modeled"}}`)

	ws := workspace.NewWorkspace(root, &workspaceproto.Config{Name: "fixture"}, []*workspace.Project{
		{Name: "tooling", Path: "tooling"},
		{Name: "broken", Path: "broken"},
	})
	result := DiscoverAuthoredManifests(ws)
	var paths []string
	for _, source := range result.Manifests {
		if !source.Document || len(source.Manifest.Features) != 1 {
			t.Fatalf("source %s is not a one-feature document: %#v", source.Path, source)
		}
		paths = append(paths, source.Path)
	}
	if got := strings.Join(paths, ","); got != "tooling/features/init.json,tooling/features/run.json" {
		t.Fatalf("manifest paths = %q, want the valid direct JSON children of each features/", got)
	}
	if len(result.Diagnostics) == 0 || !strings.HasPrefix(result.Diagnostics[0].Field, "broken/features/bad.json#feature.name") {
		t.Fatalf("diagnostics = %#v, want the invalid name positioned in its document", result.Diagnostics)
	}
}

func TestDiscoverAuthoredManifestsRefusesARootThatHoldsBothForms(t *testing.T) {
	spectest.Proves(t, "features/feature-contracts", "one-file-per-feature", "a-root-that-holds-both-forms-is-refused")
	root := t.TempDir()
	writeDiscoveryFixture(t, filepath.Join(root, "tooling", featureproto.ManifestFilename), modeledManifestJSON("tooling/run", "tooling"))
	writeDiscoveryFixture(t, filepath.Join(root, "tooling", "features", "init.json"), modeledDocumentJSON("tooling/init", "tooling"))
	writeDiscoveryFixture(t, filepath.Join(root, "healthy", "features", "catalog.json"), modeledDocumentJSON("healthy/catalog", "healthy"))

	ws := workspace.NewWorkspace(root, &workspaceproto.Config{Name: "fixture"}, []*workspace.Project{
		{Name: "tooling", Path: "tooling"},
		{Name: "healthy", Path: "healthy"},
	})
	result := DiscoverAuthoredManifests(ws)
	if len(result.Manifests) != 1 || result.Manifests[0].Path != "healthy/features/catalog.json" {
		t.Fatalf("manifests = %#v, want only the root that holds one form", result.Manifests)
	}
	if len(result.Diagnostics) != 1 || result.Diagnostics[0].Code != featureproto.ErrorCodeManifestFormConflict ||
		result.Diagnostics[0].Field != "tooling/putnami.features.json" {
		t.Fatalf("diagnostics = %#v, want one form conflict at tooling", result.Diagnostics)
	}
}

func TestDiscoverAuthoredManifestsReadsAProjectNamedFeaturesAsAProject(t *testing.T) {
	root := t.TempDir()
	writeDiscoveryFixture(t, filepath.Join(root, "protocols", "features", featureproto.ManifestFilename), modeledManifestJSON("features/contracts", "features"))
	writeDiscoveryFixture(t, filepath.Join(root, "protocols", "features", "package.json"), `{"name":"features"}`)

	ws := workspace.NewWorkspace(root, &workspaceproto.Config{Name: "fixture"}, []*workspace.Project{
		{Name: "protocols", Path: "protocols"},
		{Name: "features", Path: "protocols/features"},
	})
	result := DiscoverAuthoredManifests(ws)
	if len(result.Diagnostics) != 0 {
		t.Fatalf("diagnostics = %#v", result.Diagnostics)
	}
	if len(result.Manifests) != 1 || result.Manifests[0].Path != "protocols/features/putnami.features.json" || result.Manifests[0].Document {
		t.Fatalf("manifests = %#v, want the nested project's own manifest only", result.Manifests)
	}
}

func modeledDocumentJSON(id, namespace string) string {
	return `{"protocolVersion":1,"namespace":"` + namespace + `","feature":{"id":"` + id + `","type":"feature","name":"Name","outcome":"Outcome","owner":"team","target":"modeled"}}`
}

func modeledManifestJSON(id, namespace string) string {
	return `{"protocolVersion":1,"namespace":"` + namespace + `","features":[{"id":"` + id + `","type":"feature","name":"Name","outcome":"Outcome","owner":"team","target":"modeled"}]}`
}

func writeDiscoveryFixture(t *testing.T, filename, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
