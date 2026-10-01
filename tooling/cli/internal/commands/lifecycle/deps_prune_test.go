package lifecycle

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	extproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// declaredEdgeWorkspace is a project that declares a dependency in its putnami.json
// with no import behind it, beside a generated client whose provider is gone.
// One fixture therefore covers the class this command repairs and the class it
// refuses.
func declaredEdgeWorkspace(t *testing.T) (root string, ws *workspace.Workspace) {
	t.Helper()
	root = t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "tools", "cli"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "tools", "cli", "putnami.json"),
		[]byte("{\n  \"name\": \"acme/tool\",\n  \"tags\": [\"cli\"],\n  \"dependencies\": [\"acme/core\", \"acme/kept\"]\n}\n"),
		0o600); err != nil {
		t.Fatal(err)
	}

	core := &workspace.Project{ID: "/libs/core", Name: "acme/core", Path: "libs/core"}
	kept := &workspace.Project{ID: "/libs/kept", Name: "acme/kept", Path: "libs/kept"}
	tool := &workspace.Project{
		ID: "/tools/cli", Name: "acme/tool", Path: "tools/cli",
		Config: &wsproto.ProjectConfig{
			Name:         "acme/tool",
			Dependencies: []string{"acme/core", "acme/kept"},
		},
		Dependencies: []string{"acme/core", "acme/kept"},
		// `acme/kept` is imported; `acme/core` is not.
		DependencySources: map[string]wsproto.DependencySource{
			"acme/kept": wsproto.DependencySourceGoModule,
		},
	}
	orphan := &workspace.Project{
		ID: "/clients/orders", Name: "acme/orders-client", Path: "clients/orders",
		GeneratedClient: &workspace.GeneratedClientBinding{
			ServiceID: "orders", ManifestPath: "clients/orders/client.putnami.json",
		},
	}
	return root, workspace.NewWorkspace(root, nil, []*workspace.Project{core, kept, tool, orphan})
}

func readToolConfig(t *testing.T, root string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "tools", "cli", "putnami.json"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// TestDepsPruneDryRunListsEveryClassAndChangesNothing.
func TestDepsPruneDryRunListsEveryClassAndChangesNothing(t *testing.T) {
	root, ws := declaredEdgeWorkspace(t)
	before := readToolConfig(t, root)

	var out bytes.Buffer
	if err := prunePlan(context.Background(), root, ws, "", true, &out, LifecycleEnv{}); err != nil {
		t.Fatalf("a dry run must not fail on a finding it would refuse: %v", err)
	}
	listing := out.String()
	if !strings.Contains(listing, "prune acme/tool → acme/core") {
		t.Errorf("dry run did not list the putnami.json entry: %q", listing)
	}
	if !strings.Contains(listing, "keep") || !strings.Contains(listing, "client.putnami.json") {
		t.Errorf("dry run did not list the orphan manifest as kept: %q", listing)
	}
	if strings.Contains(listing, "acme/kept") {
		t.Errorf("dry run listed an edge a real import backs: %q", listing)
	}
	if after := readToolConfig(t, root); after != before {
		t.Errorf("a dry run rewrote putnami.json:\n%s", after)
	}
}

// TestDepsPruneRemovesTheDeclaredEntryAndRefusesTheOrphanManifest: the repair
// this command owns is applied, the one it does not is reported as a failure
// with the path to delete — a run that cannot repair everything never reports
// success.
func TestDepsPruneRemovesTheDeclaredEntryAndRefusesTheOrphanManifest(t *testing.T) {
	root, ws := declaredEdgeWorkspace(t)

	var out bytes.Buffer
	err := prunePlan(context.Background(), root, ws, "", false, &out, LifecycleEnv{})
	if err == nil {
		t.Fatal("prune reported success while an orphan client manifest was left behind")
	}
	if !strings.Contains(err.Error(), "clients/orders/client.putnami.json") {
		t.Errorf("the refusal does not name the file to delete: %v", err)
	}

	after := readToolConfig(t, root)
	if strings.Contains(after, "acme/core") {
		t.Errorf("the unimported entry survived the prune:\n%s", after)
	}
	if !strings.Contains(after, "acme/kept") {
		t.Errorf("the imported entry was removed:\n%s", after)
	}
	if !strings.Contains(after, `"tags"`) {
		t.Errorf("an unrelated member of putnami.json was lost:\n%s", after)
	}
}

// TestDepsPruneNarrowsToTheSelectedProject.
func TestDepsPruneNarrowsToTheSelectedProject(t *testing.T) {
	root, ws := declaredEdgeWorkspace(t)
	var out bytes.Buffer
	if err := prunePlan(context.Background(), root, ws, "acme/tool", true, &out, LifecycleEnv{}); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if strings.Contains(out.String(), "orders") {
		t.Errorf("a selected project still reported another project's finding: %q", out.String())
	}
}

// TestDepsPruneOnACleanWorkspaceSaysSo.
func TestDepsPruneOnACleanWorkspaceSaysSo(t *testing.T) {
	root := t.TempDir()
	ws := workspace.NewWorkspace(root, nil, []*workspace.Project{
		{ID: "/libs/core", Name: "acme/core", Path: "libs/core"},
	})
	var out bytes.Buffer
	if err := prunePlan(context.Background(), root, ws, "", false, &out, LifecycleEnv{}); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if !strings.Contains(out.String(), "No declared dependency edge") {
		t.Errorf("output = %q", out.String())
	}
}

// A package.json dependency no import backs is kept: this command has no
// writer for package.json. The dry run and the failed run name the file and
// the edit to make by hand, and never send the user back to deps prune.
func TestDepsPruneNamesTheHandEditForAManifestItKeeps(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "declared-edge-check",
		"prune-names-the-hand-edit-for-a-manifest-it-keeps")
	root := t.TempDir()
	manifest := filepath.Join(root, "apps", "web", "package.json")
	writeRawFile(t, manifest, `{"name":"@acme/web","dependencies":{"@acme/ui":"workspace:*"}}`)
	before, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	declared := map[string]wsproto.DependencySource{"@acme/ui": wsproto.DependencySourceDeclared}
	ws := workspace.NewWorkspace(root, nil, []*workspace.Project{
		{ID: "/libs/ui", Name: "@acme/ui", Path: "libs/ui"},
		{ID: "/apps/web", Name: "@acme/web", Path: "apps/web", Dependencies: []string{"@acme/ui"}, DependencySources: declared},
		// A manifest this command cannot name.
		{ID: "/apps/other", Name: "acme/other", Path: "apps/other", Dependencies: []string{"@acme/ui"}, DependencySources: declared},
	})
	const handEdit = "remove the dependency on @acme/ui from apps/web/package.json, then run `putnami deps install`"
	const unnamedEdit = "remove the dependency on @acme/ui from acme/other's language manifest by hand"

	var out bytes.Buffer
	if err := prunePlan(context.Background(), root, ws, "", true, &out, LifecycleEnv{}); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	for _, want := range []string{
		"keep  @acme/web → @acme/ui [manifest] in apps/web/package.json — " + handEdit,
		"keep  acme/other → @acme/ui [manifest] in its own language manifest — " + unnamedEdit,
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("dry run = %q, want the line %q", out.String(), want)
		}
	}

	out.Reset()
	err = prunePlan(context.Background(), root, ws, "", false, &out, LifecycleEnv{})
	if err == nil {
		t.Fatal("prune reported success while a package.json dependency was left behind")
	}
	for _, want := range []string{handEdit, unnamedEdit} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to name %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "putnami deps prune") {
		t.Errorf("error = %q, want no advice to run the command that just kept the finding", err)
	}
	if after, _ := os.ReadFile(manifest); !bytes.Equal(after, before) {
		t.Errorf("prune rewrote package.json:\n%s", after)
	}
}

// goProbeAnswer is a Go provider's answer for a workspace whose api requires
// the workspace module lib and imports nothing of it.
type goProbeAnswer struct{}

func (goProbeAnswer) Name() string { return "@acme/go" }

func (goProbeAnswer) Probe(wsproto.ProbeRequest) (wsproto.ProbeResult, error) {
	return wsproto.ProbeResult{
		Version:   wsproto.ProbeProtocolVersion,
		Extension: "@acme/go",
		Projects: []wsproto.ProbeProject{
			{Path: "lib", SourceName: "example.com/lib", SourceFile: "lib/go.mod"},
			{
				Path: "api", SourceName: "example.com/api", SourceFile: "api/go.mod",
				Dependencies:      []string{"lib"},
				DependencySources: map[string]wsproto.DependencySource{"lib": wsproto.DependencySourceDeclared},
			},
		},
	}, nil
}

// A hand edit of go.mod after the provider view was recorded makes prune
// refuse and name the edited file, rather than prune from the previous tree.
// Before, prune worked from that view: it reported nothing after a require was
// added by hand, and it would drop a requirement a new import backs.
func TestDepsPruneRefusesAProviderViewTheTreeMovedPast(t *testing.T) {
	root := t.TempDir()
	writeRawFile(t, filepath.Join(root, wsproto.WorkspaceConfigFilename), `{"name":"go-ws","includes":["api","lib"]}`)
	writeRawFile(t, filepath.Join(root, "lib", "go.mod"), "module example.com/lib\n\ngo 1.22\n")
	writeRawFile(t, filepath.Join(root, "api", "go.mod"), "module example.com/api\n\ngo 1.22\n\nrequire example.com/lib v0.0.0\n")
	workspace.InvalidateLoadCache(root)
	t.Cleanup(func() { workspace.InvalidateLoadCache(root) })
	ws, err := workspace.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	scope, ok := workspace.NewProviderScope("@acme/go", &extproto.WorkspaceAdapter{Markers: []string{"go.mod"}, Inputs: []string{"go.mod"}})
	if !ok {
		t.Fatal("the fixture adapter was rejected")
	}
	if _, err := workspace.Synchronize(workspace.SyncRequest{
		Workspace: ws, Reason: wsproto.ProbeReasonLoad,
		Providers: []workspace.ProviderBinding{{Scope: scope, Provider: goProbeAnswer{}}},
	}); err != nil {
		t.Fatalf("record the provider view: %v", err)
	}
	workspace.InvalidateLoadCache(root)

	var out bytes.Buffer
	if err := DepsPrune(context.Background(), root, "", true, LifecycleEnv{Out: &out}); err != nil {
		t.Fatalf("prune over the recorded view: %v", err)
	}
	if !strings.Contains(out.String(), "prune example.com/api → example.com/lib") {
		t.Fatalf("prune over the recorded view = %q, want the unimported requirement listed", out.String())
	}

	writeRawFile(t, filepath.Join(root, "api", "go.mod"), "module example.com/api\n\ngo 1.22\n")
	workspace.InvalidateLoadCache(root)
	out.Reset()
	err = DepsPrune(context.Background(), root, "", true, LifecycleEnv{Out: &out})
	if err == nil {
		t.Fatalf("prune over a stale view succeeded:\n%s", out.String())
	}
	for _, want := range []string{"stale", "api/go.mod", "putnami install"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to name %q", err, want)
		}
	}
}
