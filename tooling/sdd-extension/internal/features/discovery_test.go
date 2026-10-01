package features

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	featureproto "go.putnami.dev/protocol/features"
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
