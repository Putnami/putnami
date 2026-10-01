package features

import (
	"os"
	"path/filepath"
	"testing"

	featureproto "go.putnami.dev/protocol/features"
	workspaceproto "go.putnami.dev/protocol/workspace"
	workspace "go.putnami.dev/tooling/sdd/extension/internal/wsview"
)

func TestDiscoverSpecsReadsOnlyCanonicalSpecDirectories(t *testing.T) {
	root := t.TempDir()
	writeDiscoveryFixture(t, filepath.Join(root, "specs", "workspace.json"), specJSON("workspace/journey"))
	writeDiscoveryFixture(t, filepath.Join(root, "app", "specs", "renamed-file.json"), specJSON("billing/checkout"))
	// Not canonical: a nested directory under specs/, a non-JSON sibling, a
	// specs/ directory at a path that is not a project root, and a spec parked
	// next to the code it describes.
	writeDiscoveryFixture(t, filepath.Join(root, "app", "specs", "nested", "deep.json"), specJSON("billing/nested"))
	writeDiscoveryFixture(t, filepath.Join(root, "app", "specs", "notes.md"), "# not a spec")
	writeDiscoveryFixture(t, filepath.Join(root, "app", "internal", "specs", "internal.json"), specJSON("billing/internal"))
	writeDiscoveryFixture(t, filepath.Join(root, "app", "spec.json"), specJSON("billing/loose"))

	ws := workspace.NewWorkspace(root, &workspaceproto.Config{Name: "fixture"}, []*workspace.Project{
		{Name: "billing-app", Path: "app"},
	})
	result := DiscoverSpecs(ws, nil)
	if len(result.Diagnostics) != 0 {
		t.Fatalf("diagnostics = %#v", result.Diagnostics)
	}
	if len(result.Specs) != 2 {
		t.Fatalf("specs = %#v, want the workspace and exact project roots only", result.Specs)
	}
	if result.Specs[0].Path != "app/specs/renamed-file.json" || result.Specs[0].Project != "billing-app" || result.Specs[0].Root != "app" {
		t.Fatalf("project spec provenance = %#v", result.Specs[0])
	}
	// The filename says "renamed-file"; identity stays the authored feature.
	if result.Specs[0].Spec.Feature != "billing/checkout" {
		t.Fatalf("feature = %q, want the authored identity rather than the filename", result.Specs[0].Spec.Feature)
	}
	if result.Specs[1].Path != "specs/workspace.json" || result.Specs[1].Project != "" || result.Specs[1].Root != "" {
		t.Fatalf("workspace spec provenance = %#v", result.Specs[1])
	}
}

func TestDiscoverSpecsReportsUnparsableDocumentWithoutHidingHealthyOnes(t *testing.T) {
	root := t.TempDir()
	writeDiscoveryFixture(t, filepath.Join(root, "specs", "healthy.json"), specJSON("billing/checkout"))
	writeDiscoveryFixture(t, filepath.Join(root, "specs", "broken.json"),
		`{"protocolVersion":1,"feature":"billing/refunds","outcomes":["x"],"requirements":[],"status":"approved"}`)

	ws := workspace.NewWorkspace(root, &workspaceproto.Config{Name: "fixture"}, nil)
	result := DiscoverSpecs(ws, nil)
	if len(result.Specs) != 1 || result.Specs[0].Path != "specs/healthy.json" {
		t.Fatalf("specs = %#v, want only the healthy document", result.Specs)
	}
	if len(result.Diagnostics) != 1 {
		t.Fatalf("diagnostics = %#v, want exactly one parse finding", result.Diagnostics)
	}
	if result.Diagnostics[0].Code != featureproto.ErrorCodeUnknownField {
		t.Fatalf("diagnostic code = %q", result.Diagnostics[0].Code)
	}
	if result.Diagnostics[0].Field != "specs/broken.json#status" {
		t.Fatalf("diagnostic field = %q, want exact document provenance", result.Diagnostics[0].Field)
	}
}

// TestDiscoverSpecsProjectsProtocolSourcesWithoutProvenance pins the boundary:
// the protocol validator receives the path and document only, because owning
// project is a workspace fact the wire contract does not model.
func TestDiscoverSpecsProjectsProtocolSources(t *testing.T) {
	root := t.TempDir()
	writeDiscoveryFixture(t, filepath.Join(root, "app", "specs", "one.json"), specJSON("billing/checkout"))
	ws := workspace.NewWorkspace(root, &workspaceproto.Config{Name: "fixture"}, []*workspace.Project{
		{Name: "billing-app", Path: "app"},
	})
	sources := DiscoverSpecs(ws, nil).Sources()
	if len(sources) != 1 || sources[0].Path != "app/specs/one.json" || sources[0].Spec == nil {
		t.Fatalf("sources = %#v", sources)
	}
	findings := featureproto.ValidateSpecRepository(sources, []string{"billing/checkout"})
	if len(findings) != 0 {
		t.Fatalf("repository findings = %#v, want a clean canonical location", findings)
	}
}

func TestResolveDecisionRecordsAnswersOnlyForContainedRecords(t *testing.T) {
	root := t.TempDir()
	writeDiscoveryFixture(t, filepath.Join(root, "app", "doc", "adr", "0001-present.md"), "# present")
	if err := os.MkdirAll(filepath.Join(root, "app", "doc", "adr", "0003-directory.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	outsideRecord := filepath.Join(outside, "0004-outside.md")
	writeDiscoveryFixture(t, outsideRecord, "# outside")
	if err := os.Symlink(outsideRecord, filepath.Join(root, "app", "doc", "adr", "0004-leaf-link.md")); err != nil {
		t.Skipf("symlinks are unavailable: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "app", "escape")); err != nil {
		t.Skipf("symlinks are unavailable: %v", err)
	}

	ws := workspace.NewWorkspace(root, &workspaceproto.Config{Name: "fixture"}, []*workspace.Project{
		{Name: "billing-app", Path: "app"},
	})
	resolved := ResolveDecisionRecords(ws, []string{
		"app/doc/adr/0001-present.md",
		"app/doc/adr/0002-absent.md",
		"app/doc/adr/0003-directory.md",
		"app/doc/adr/0004-leaf-link.md",
		"app/escape/0004-outside.md",
		"../outside/doc/adr/0005-escaping.md",
	})
	want := map[string]bool{
		"app/doc/adr/0001-present.md":         true,
		"app/doc/adr/0002-absent.md":          false,
		"app/doc/adr/0003-directory.md":       false,
		"app/doc/adr/0004-leaf-link.md":       false,
		"app/escape/0004-outside.md":          false,
		"../outside/doc/adr/0005-escaping.md": false,
	}
	for link, expected := range want {
		if resolved[link] != expected {
			t.Errorf("resolved[%q] = %v, want %v", link, resolved[link], expected)
		}
	}
}

func specJSON(feature string) string {
	return `{"protocolVersion":1,"feature":"` + feature + `","outcomes":["An outcome"],"requirements":[]}`
}
