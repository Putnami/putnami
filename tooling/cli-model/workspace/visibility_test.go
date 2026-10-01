package workspace

import (
	"reflect"
	"testing"

	"go.putnami.dev/protocol/features/spectest"

	wsproto "go.putnami.dev/protocol/workspace"
)

// scoped builds one project in the scope whose config paths are given,
// shallowest first — the shape discovery records on Project.Scope.
func scoped(id, name, projectPath string, scopeConfigs ...string) *Project {
	return &Project{
		ID: id, Name: name, Path: projectPath,
		Scope: ScopeContribution{ConfigPaths: scopeConfigs},
	}
}

func imports(p *Project, source wsproto.DependencySource, dependencies ...string) *Project {
	p.Dependencies = append(p.Dependencies, dependencies...)
	if p.DependencySources == nil {
		p.DependencySources = map[string]wsproto.DependencySource{}
	}
	for _, dependency := range dependencies {
		p.DependencySources[dependency] = source
	}
	return p
}

func TestScopeKeyOfIsTheNearestScopeConfigDirectory(t *testing.T) {
	cases := []struct {
		name    string
		project *Project
		want    string
	}{
		{"no scope config is the workspace root scope", scoped("/a", "a", "a"), ""},
		{"one scope config", scoped("/go/framework/api", "api", "go/framework/api", "go/putnami.json"), "go"},
		{
			"the nearest of a nested chain wins",
			scoped("/go/framework/api", "api", "go/framework/api", "go/putnami.json", "go/framework/putnami.json"),
			"go/framework",
		},
		{
			"a root-level scope config is the root scope",
			scoped("/a", "a", "a", "putnami.json"),
			"",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ScopeKeyOf(tc.project); got != tc.want {
				t.Fatalf("ScopeKeyOf = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestVisibilityViolationsAllowsAnImportInsideOneScope(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "import-visibility", "an-import-inside-one-scope-is-allowed")
	ws := NewWorkspace("/ws", nil, []*Project{
		imports(scoped("/go/framework/api", "api", "go/framework/api", "go/putnami.json"),
			wsproto.DependencySourceGoModule, "logger"),
		scoped("/go/framework/logger", "logger", "go/framework/logger", "go/putnami.json"),
	})

	if got := VisibilityViolations(ws); len(got) != 0 {
		t.Fatalf("violations = %v, want none inside one scope", got)
	}
}

func TestVisibilityViolationsRefusesACrossScopeImport(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "import-visibility", "a-cross-scope-import-of-a-scope-private-project-is-reported")
	ws := NewWorkspace("/ws", nil, []*Project{
		imports(scoped("/tooling/cli", "@putnami/cli", "tooling/cli", "tooling/putnami.json"),
			wsproto.DependencySourceGoModule, "go.putnami.dev/logger"),
		scoped("/go/framework/logger", "go.putnami.dev/logger", "go/framework/logger", "go/putnami.json"),
	})

	want := []VisibilityViolation{{
		Importer:      "/tooling/cli",
		ImporterScope: "tooling",
		Imported:      "/go/framework/logger",
		ImportedScope: "go",
		Source:        wsproto.DependencySourceGoModule,
	}}
	if got := VisibilityViolations(ws); !reflect.DeepEqual(got, want) {
		t.Fatalf("violations = %+v, want %+v", got, want)
	}
}

func TestVisibilityViolationsAllowsACrossScopeImportOfAPublicProject(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "import-visibility", "a-public-project-a-generated-client-and-a-contract-edge-are-allowed")
	logger := scoped("/go/framework/logger", "go.putnami.dev/logger", "go/framework/logger", "go/putnami.json")
	logger.Visibility = wsproto.VisibilityPublic
	ws := NewWorkspace("/ws", nil, []*Project{
		imports(scoped("/tooling/cli", "@putnami/cli", "tooling/cli", "tooling/putnami.json"),
			wsproto.DependencySourceGoModule, "go.putnami.dev/logger"),
		logger,
	})

	if got := VisibilityViolations(ws); len(got) != 0 {
		t.Fatalf("violations = %v, want none: the imported project is public", got)
	}
}

func TestVisibilityViolationsIgnoresADeclaredEdgeWithNoImport(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "import-visibility", "an-edge-carries-the-manifest-family-it-came-from")
	ws := NewWorkspace("/ws", nil, []*Project{
		{
			ID: "/tooling/cli", Name: "@putnami/cli", Path: "tooling/cli",
			Scope:        ScopeContribution{ConfigPaths: []string{"tooling/putnami.json"}},
			Dependencies: []string{"go.putnami.dev/logger"},
		},
		scoped("/go/framework/logger", "go.putnami.dev/logger", "go/framework/logger", "go/putnami.json"),
	})

	if got := VisibilityViolations(ws); len(got) != 0 {
		t.Fatalf("violations = %v, want none: a declared edge is not an import", got)
	}
	if source := ws.Graph.EdgeSource("/tooling/cli", "/go/framework/logger"); source != wsproto.DependencySourceDeclared {
		t.Fatalf("EdgeSource = %q, want %q", source, wsproto.DependencySourceDeclared)
	}
}

func TestVisibilityViolationsAllowsAContractEdgeAcrossScopes(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "import-visibility", "a-public-project-a-generated-client-and-a-contract-edge-are-allowed")
	provider := scoped("/go/samples/catalog", "catalog", "go/samples/catalog", "go/putnami.json")
	provider.ContractServiceID = "catalog.items"
	client := scoped("/tooling/clients/catalog", "catalog-client", "tooling/clients/catalog", "tooling/putnami.json")
	client.GeneratedClient = &GeneratedClientBinding{
		ServiceID:      "catalog.items",
		ContractSHA256: "2222222222222222222222222222222222222222222222222222222222222222",
		Language:       "go",
		ManifestPath:   "tooling/clients/catalog/client.putnami.json",
	}
	ws := NewWorkspace("/ws", nil, []*Project{provider, client})

	if got := VisibilityViolations(ws); len(got) != 0 {
		t.Fatalf("violations = %v, want none: a contract is a service's public surface", got)
	}
	if source := ws.Graph.EdgeSource("/tooling/clients/catalog", "/go/samples/catalog"); source != wsproto.DependencySourceContract {
		t.Fatalf("EdgeSource = %q, want %q", source, wsproto.DependencySourceContract)
	}
}

func TestVisibilityViolationsAllowsACrossScopeImportOfAGeneratedClient(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "import-visibility", "a-public-project-a-generated-client-and-a-contract-edge-are-allowed")
	if got := VisibilityViolations(clientWorkspace("", true)); len(got) != 0 {
		t.Fatalf("violations = %v, want none: a generated client is the service's published way in", got)
	}
}

func TestVisibilityViolationsKeepsAGeneratedClientThatDeclaresScope(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "import-visibility", "a-cross-scope-import-of-a-scope-private-project-is-reported")
	if got := VisibilityViolations(clientWorkspace(wsproto.VisibilityScope, true)); len(got) != 1 {
		t.Fatalf("violations = %v, want the import refused: the client declared scope", got)
	}
}

func TestVisibilityViolationsOpensNothingForAClientManifestNoProviderBacks(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "import-visibility", "a-cross-scope-import-of-a-scope-private-project-is-reported")
	if got := VisibilityViolations(clientWorkspace("", false)); len(got) != 1 {
		t.Fatalf("violations = %v, want the import refused: no provider backs the manifest", got)
	}
}

// clientWorkspace is a generated client under its provider, so its scope is
// the provider's own directory, imported by a consumer in another scope.
// withProvider decides whether a project commits the contract the client names.
func clientWorkspace(visibility wsproto.Visibility, withProvider bool) *Workspace {
	client := scoped("/identity/api/clients/go", "identity-client", "identity/api/clients/go",
		"identity/putnami.json", "identity/api/putnami.json")
	client.Visibility = visibility
	client.GeneratedClient = &GeneratedClientBinding{
		ServiceID:      "identity.api",
		ContractSHA256: "3333333333333333333333333333333333333333333333333333333333333333",
		Language:       "go",
		ManifestPath:   "identity/api/clients/go/client.putnami.json",
	}
	provider := scoped("/identity/api", "identity-api", "identity/api", "identity/putnami.json")
	if withProvider {
		provider.ContractServiceID = "identity.api"
	}
	return NewWorkspace("/ws", nil, []*Project{
		imports(scoped("/surfaces/cli", "operator-cli", "surfaces/cli", "surfaces/putnami.json"),
			wsproto.DependencySourceGoModule, "identity-client"),
		client,
		provider,
	})
}

func TestVisibilityViolationsSeparatesNestedScopes(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "import-visibility", "a-cross-scope-import-of-a-scope-private-project-is-reported")
	// framework and samples are two scopes under one parent scope: the nearest
	// scope config decides, so an import between them crosses a boundary.
	ws := NewWorkspace("/ws", nil, []*Project{
		imports(scoped("/go/samples/task-api", "task-api", "go/samples/task-api",
			"go/putnami.json", "go/samples/putnami.json"),
			wsproto.DependencySourceGoModule, "go.putnami.dev/http"),
		scoped("/go/framework/http", "go.putnami.dev/http", "go/framework/http",
			"go/putnami.json", "go/framework/putnami.json"),
	})

	got := VisibilityViolations(ws)
	if len(got) != 1 {
		t.Fatalf("violations = %+v, want one", got)
	}
	if got[0].ImporterScope != "go/samples" || got[0].ImportedScope != "go/framework" {
		t.Fatalf("scopes = %q → %q, want go/samples → go/framework", got[0].ImporterScope, got[0].ImportedScope)
	}
}

func TestVisibilityViolationsIgnoresAnImplicitScopeOrderingEdge(t *testing.T) {
	scope := scoped("/tooling", "tooling-scope", "tooling")
	scope.ActivatedScope = true
	scope.ScopeIncludes = []string{"/tooling/cli"}
	child := scoped("/tooling/cli", "@putnami/cli", "tooling/cli", "tooling/putnami.json")
	ws := NewWorkspace("/ws", nil, []*Project{scope, child})

	if got := VisibilityViolations(ws); len(got) != 0 {
		t.Fatalf("violations = %v, want none: an implicit scope edge orders and reads nothing", got)
	}
	if source := ws.Graph.EdgeSource("/tooling/cli", "/tooling"); source != wsproto.DependencySourceDeclared {
		t.Fatalf("EdgeSource = %q, want %q", source, wsproto.DependencySourceDeclared)
	}
}

func TestVisibilityViolationsAreDeterministic(t *testing.T) {
	logger := scoped("/go/framework/logger", "go.putnami.dev/logger", "go/framework/logger", "go/putnami.json")
	http := scoped("/go/framework/http", "go.putnami.dev/http", "go/framework/http", "go/putnami.json")
	ws := NewWorkspace("/ws", nil, []*Project{
		imports(scoped("/tooling/cli", "@putnami/cli", "tooling/cli", "tooling/putnami.json"),
			wsproto.DependencySourceGoModule, "go.putnami.dev/logger", "go.putnami.dev/http"),
		logger, http,
	})

	first := VisibilityViolations(ws)
	if len(first) != 2 {
		t.Fatalf("violations = %+v, want two", first)
	}
	if first[0].Imported != "/go/framework/http" || first[1].Imported != "/go/framework/logger" {
		t.Fatalf("violations are not sorted by imported project: %+v", first)
	}
	for range 5 {
		if !reflect.DeepEqual(VisibilityViolations(ws), first) {
			t.Fatal("two evaluations over one workspace disagree")
		}
	}
}

func TestEdgeSourceIsEmptyWithoutAnEdge(t *testing.T) {
	ws := NewWorkspace("/ws", nil, []*Project{
		scoped("/a", "a", "a"),
		scoped("/b", "b", "b"),
	})
	if source := ws.Graph.EdgeSource("/a", "/b"); source != "" {
		t.Fatalf("EdgeSource = %q, want empty: there is no edge", source)
	}
}

func TestEdgeSourceKeepsTheImportWhenAnEdgeIsAlsoDeclared(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "import-visibility", "an-edge-carries-the-manifest-family-it-came-from")
	// The authored putnami.json declares the edge and the module imports it.
	// One edge, and the build really reads it.
	importer := scoped("/tooling/cli", "@putnami/cli", "tooling/cli", "tooling/putnami.json")
	importer.Dependencies = []string{"go.putnami.dev/logger"}
	importer.DependencySources = map[string]wsproto.DependencySource{
		"go.putnami.dev/logger": wsproto.DependencySourceGoModule,
	}
	ws := NewWorkspace("/ws", nil, []*Project{
		importer,
		scoped("/go/framework/logger", "go.putnami.dev/logger", "go/framework/logger", "go/putnami.json"),
	})

	if source := ws.Graph.EdgeSource("/tooling/cli", "/go/framework/logger"); source != wsproto.DependencySourceGoModule {
		t.Fatalf("EdgeSource = %q, want %q", source, wsproto.DependencySourceGoModule)
	}
	if got := ws.Graph.DependenciesOf("/tooling/cli"); !reflect.DeepEqual(got, []string{"/go/framework/logger"}) {
		t.Fatalf("DependenciesOf = %v, want exactly one edge", got)
	}
}

func TestEffectiveVisibilityDefaultsToScope(t *testing.T) {
	if got := EffectiveVisibility(nil); got != wsproto.VisibilityScope {
		t.Fatalf("EffectiveVisibility(nil) = %q, want %q", got, wsproto.VisibilityScope)
	}
	if got := EffectiveVisibility(&Project{}); got != wsproto.VisibilityScope {
		t.Fatalf("EffectiveVisibility(undeclared) = %q, want %q", got, wsproto.VisibilityScope)
	}
	if got := EffectiveVisibility(&Project{Visibility: wsproto.VisibilityPublic}); got != wsproto.VisibilityPublic {
		t.Fatalf("EffectiveVisibility(public) = %q, want %q", got, wsproto.VisibilityPublic)
	}
}
