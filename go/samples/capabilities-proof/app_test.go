package capproof

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	protocaps "go.putnami.dev/protocol/capabilities"
	protofeatures "go.putnami.dev/protocol/features"
)

const proofProject = "go.putnami.dev/examples/capabilities-proof"

func proofBinding() string {
	binding, err := protocaps.ComputeSourceBinding([]protocaps.SourceBindingFile{{Path: "app.go", Mode: protocaps.SourceModeRegular, Digest: protocaps.SourceDigest([]byte("capabilities-proof"))}})
	if err != nil {
		panic(err)
	}
	return binding
}

func describeArtifacts(t *testing.T) ([]byte, []byte) {
	t.Helper()
	out := t.TempDir()
	stamp, err := json.Marshal(map[string]any{"name": proofProject, "version": "9.9.9", "capabilityPackages": []map[string]any{{
		"package": proofProject, "version": "0.1.0", "sourceRoot": ".", "evidencePath": "putnami.json", "sourceBinding": proofBinding(),
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "version.json"), stamp, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := BuildApp().Describe(out, nil); err != nil {
		t.Fatalf("Describe: %v", err)
	}
	manifest, err := os.ReadFile(filepath.Join(out, "schema", protocaps.ManifestFilename))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(out, "schema", "feature-evidence", "go-framework.json")); !os.IsNotExist(err) {
		t.Fatalf("deprecated feature evidence was emitted: %v", err)
	}
	graph, err := os.ReadFile(filepath.Join(out, filepath.FromSlash(protofeatures.DesignGraphArtifact)))
	if err != nil {
		t.Fatal(err)
	}
	return manifest, graph
}

func TestCapabilityManifestGolden(t *testing.T) {
	manifest, _ := describeArtifacts(t)
	goldens := map[string][]byte{"capabilities.golden.json": manifest}
	if os.Getenv("PUTNAMI_UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll("testdata", 0o750); err != nil {
			t.Fatal(err)
		}
		for name, data := range goldens {
			if err := os.WriteFile(filepath.Join("testdata", name), data, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return
	}
	for name, got := range goldens {
		want, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("%s mismatch\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
		}
	}
}

func TestCapabilityAndDesignGraphAreValidCanonicalAndDeterministic(t *testing.T) {
	manifest, graphBytes := describeArtifacts(t)
	document, diags := protocaps.ParseAndValidateManifestDocument(manifest)
	if document == nil || document.V2 == nil || len(diags) != 0 {
		t.Fatalf("manifest: %#v %v", document, diags)
	}
	if len(document.V2.PackageVersions) != 0 || bytes.Contains(manifest, []byte(`"packageVersions"`)) || bytes.Contains(manifest, []byte(`"version"`)) {
		t.Fatalf("manifest contains resolved package versions: %s", manifest)
	}
	canonicalManifest, err := protocaps.MarshalManifestV2(document.V2)
	if err != nil || !bytes.Equal(manifest, canonicalManifest) {
		t.Fatalf("manifest canonical: %v", err)
	}
	counts := []int{len(document.V2.ConfigDefinitions), len(document.V2.Schemas), len(document.V2.Discoverers), len(document.V2.Migrations), len(document.V2.InfraRequirements), len(document.V2.HealthContributors), len(document.V2.LifecycleHooks), len(document.V2.Packages), len(document.V2.RequiredCapabilities)}
	for index, count := range counts {
		if count == 0 {
			t.Errorf("representative collection %d is empty", index)
		}
	}
	var openAPIArtifacts []protocaps.ArtifactLocation
	for _, schema := range document.V2.Schemas {
		if schema.Identity.Key == "proofOpenAPI" {
			openAPIArtifacts = schema.Provenance.Artifacts
		}
	}
	if len(openAPIArtifacts) != 1 || openAPIArtifacts[0].Root != protocaps.LocationRootProject || openAPIArtifacts[0].Path != "schema/openapi.json" {
		t.Fatalf("native schema artifact provenance = %#v", openAPIArtifacts)
	}
	graph, err := protofeatures.ParseDesignGraph(graphBytes)
	if err != nil {
		t.Fatalf("design graph: %v", err)
	}
	wantNodes := map[string]bool{
		"feature:capabilities/source-bound-manifest": true,
		"data.migration:sql:iam":                     true,
		"data.schema:default:public":                 true,
	}
	for _, node := range graph.Nodes {
		delete(wantNodes, node.ID)
	}
	if len(wantNodes) != 0 {
		t.Fatalf("native design graph is missing nodes: %#v", wantNodes)
	}
	canonicalGraph, err := protofeatures.MarshalDesignGraph(graph)
	if err != nil || !bytes.Equal(graphBytes, canonicalGraph) {
		t.Fatalf("design graph canonical: %v", err)
	}
	for i := 0; i < 3; i++ {
		nextManifest, nextGraph := describeArtifacts(t)
		if !bytes.Equal(manifest, nextManifest) || !bytes.Equal(graphBytes, nextGraph) {
			t.Fatalf("run %d is nondeterministic", i)
		}
	}
}
