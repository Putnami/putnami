package workspace

import (
	"path/filepath"
	"reflect"
	"testing"
)

// contractWorkspace is a provider, the generated client of its contract, and a
// consumer that imports the client: the shape a contract edge exists for. The
// client was generated at the provider's committed contract, so both carry
// contractDigest.
func contractWorkspace(contractDigest string) *Workspace {
	projects := []*Project{
		{
			ID: "/services/catalog", Name: "catalog", Path: "services/catalog",
			ContractServiceID: "catalog.items",
			ContractPath:      "services/catalog/schema/openapi.json",
			ContractSHA256:    contractDigest,
		},
		{
			ID: "/clients/catalog-ts", Name: "catalog-ts", Path: "clients/catalog-ts",
			GeneratedClient: &GeneratedClientBinding{
				ServiceID:      "catalog.items",
				ContractSHA256: contractDigest,
				Language:       "ts",
				ManifestPath:   "clients/catalog-ts/client.putnami.json",
			},
		},
		{
			ID: "/apps/storefront", Name: "storefront", Path: "apps/storefront",
			Dependencies: []string{"catalog-ts"},
		},
	}
	return NewWorkspace("/ws", nil, projects)
}

const contractEdgeDigest = "1111111111111111111111111111111111111111111111111111111111111111"

// TestBuildGraph_ContractEdgeResolvesServiceToProvider pins the derivation
// rule: a project's committed client manifest names a service, and the provider
// is the project whose committed contract declares that same service.
func TestBuildGraph_ContractEdgeResolvesServiceToProvider(t *testing.T) {
	g := contractWorkspace(contractEdgeDigest).Graph

	if got := g.ContractClientsOf("/services/catalog"); !reflect.DeepEqual(got, []string{"/clients/catalog-ts"}) {
		t.Errorf("ContractClientsOf(provider) = %v, want [/clients/catalog-ts]", got)
	}
	if got := g.ContractProviderOf("/clients/catalog-ts"); got != "/services/catalog" {
		t.Errorf("ContractProviderOf(client) = %q, want /services/catalog", got)
	}
}

// TestBuildGraph_ContractEdgeIsNotADependencyEdge pins that the contract edge
// stays out of every family a dependent's inputs travel on. A generated client
// reads its contract, never its provider's sources, so an edge in deps would
// fold the provider's cache key into the client's and miss the whole point.
func TestBuildGraph_ContractEdgeIsNotADependencyEdge(t *testing.T) {
	g := contractWorkspace(contractEdgeDigest).Graph

	if got := g.DependenciesOf("/clients/catalog-ts"); len(got) != 0 {
		t.Errorf("DependenciesOf(client) = %v, want none", got)
	}
	if got := g.DependentsOf("/services/catalog"); len(got) != 0 {
		t.Errorf("DependentsOf(provider) = %v, want none", got)
	}
	if got := g.ImpactDependentsOf("/services/catalog"); len(got) != 0 {
		t.Errorf("ImpactDependentsOf(provider) = %v, want none", got)
	}
	if path := g.DependencyPath("/clients/catalog-ts", "/services/catalog"); path != nil {
		t.Errorf("DependencyPath(client, provider) = %v, want none", path)
	}
}

// TestBuildGraph_ContractEdgeRefusesAnAmbiguousService pins that a service two
// projects both declare resolves to neither: an edge that depended on project
// order would name a different provider from one run to the next.
func TestBuildGraph_ContractEdgeRefusesAnAmbiguousService(t *testing.T) {
	projects := []*Project{
		{ID: "/services/a", Name: "a", Path: "services/a", ContractServiceID: "catalog.items"},
		{ID: "/services/b", Name: "b", Path: "services/b", ContractServiceID: "catalog.items"},
		{
			ID: "/clients/ts", Name: "ts", Path: "clients/ts",
			GeneratedClient: &GeneratedClientBinding{
				ServiceID: "catalog.items", ContractSHA256: contractEdgeDigest, Language: "ts",
			},
		},
	}
	g := BuildGraph(projects)

	if got := g.ContractProviderOf("/clients/ts"); got != "" {
		t.Errorf("ContractProviderOf(client) = %q, want no provider for an ambiguous service", got)
	}
	if got := g.ContractClientsOf("/services/a"); len(got) != 0 {
		t.Errorf("ContractClientsOf(/services/a) = %v, want none", got)
	}
	if got := g.ContractClientsOf("/services/b"); len(got) != 0 {
		t.Errorf("ContractClientsOf(/services/b) = %v, want none", got)
	}
}

// TestBuildGraph_ContractEdgeRefusesAnUnknownProvider pins that a client whose
// service no workspace project declares has no edge at all, rather than an edge
// to whichever project happens to sit above it.
func TestBuildGraph_ContractEdgeRefusesAnUnknownProvider(t *testing.T) {
	projects := []*Project{
		{ID: "/services/catalog", Name: "catalog", Path: "services/catalog", ContractServiceID: "other.service"},
		{
			ID: "/services/catalog/clients/ts", Name: "ts", Path: "services/catalog/clients/ts",
			GeneratedClient: &GeneratedClientBinding{
				ServiceID: "catalog.items", ContractSHA256: contractEdgeDigest, Language: "ts",
			},
		},
	}
	g := BuildGraph(projects)

	if got := g.ContractProviderOf("/services/catalog/clients/ts"); got != "" {
		t.Errorf("ContractProviderOf(client) = %q, want no provider: nearness is not the rule", got)
	}
}

// TestChangeImpact_AMovedContractSelectsTheClientAndItsConsumers pins the
// propagation a change to the provider's committed contract performs: the
// client is selected in full over an edge named `contract`, which names the
// contract that changed and its digest, and the consumers that import the
// client follow on their own dependency edges.
func TestChangeImpact_AMovedContractSelectsTheClientAndItsConsumers(t *testing.T) {
	ws := contractWorkspace(contractEdgeDigest)

	result := TraceChangeImpact(ws, []string{"services/catalog/schema/openapi.json"}, ChangeImpactOptions{})

	want := []string{"/services/catalog", "/clients/catalog-ts", "/apps/storefront"}
	if got := ownerProjectIDs(result.Projects); !reflect.DeepEqual(got, want) {
		t.Fatalf("selected %v, want %v", got, want)
	}
	if got, want := result.Trace.Edges["/clients/catalog-ts"], (ImpactEdge{
		From: "/services/catalog", Kind: ImpactEdgeContract,
		Via: "services/catalog/schema/openapi.json", ContractSHA256: contractEdgeDigest,
	}); got != want {
		t.Errorf("edge into the client = %+v, want %+v", got, want)
	}
	if got, want := result.Trace.Edges["/apps/storefront"],
		(ImpactEdge{From: "/clients/catalog-ts", Kind: ImpactEdgeDependency}); got != want {
		t.Errorf("edge into the consumer = %+v, want %+v", got, want)
	}
	if scope := result.Trace.ScopeOf("/clients/catalog-ts"); scope != nil {
		t.Errorf("client task scope = %v, want full", scope)
	}
}

// TestChangeImpact_AnImplementationChangeLeavesTheClientsAlone pins the other
// half of the rule: a provider change that leaves its committed contract alone
// selects the provider and nothing through the contract edge. The client's
// identity folds only the contract digest, so there is nothing to re-run, and
// a change that would move the contract without committing it fails the
// provider's own drift check.
func TestChangeImpact_AnImplementationChangeLeavesTheClientsAlone(t *testing.T) {
	ws := contractWorkspace(contractEdgeDigest)

	for _, changed := range [][]string{
		{"services/catalog/src/items.ts"},
		{"services/catalog/src/items.ts", "services/catalog/schema/README.md"},
		// A path that only resembles the contract is not the contract.
		{"services/catalog/src/schema/openapi.json"},
	} {
		result := TraceChangeImpact(ws, changed, ChangeImpactOptions{})

		if got, want := ownerProjectIDs(result.Projects), []string{"/services/catalog"}; !reflect.DeepEqual(got, want) {
			t.Errorf("%v selected %v, want the provider alone", changed, got)
		}
		if edge, reached := result.Trace.Edges["/clients/catalog-ts"]; reached {
			t.Errorf("%v reached the client over %+v, want no contract edge", changed, edge)
		}
	}
}

// TestChangeImpact_AContractMatchesInEverySpellingOfTheChangedPath pins that
// the contract gate cleans a changed path the way path ownership does, so a
// `./`-prefixed or unclean entry names the same committed contract.
func TestChangeImpact_AContractMatchesInEverySpellingOfTheChangedPath(t *testing.T) {
	ws := contractWorkspace(contractEdgeDigest)

	for _, changed := range []string{
		"./services/catalog/schema/openapi.json",
		"services/catalog/schema/../schema/openapi.json",
		// The host spelling: a backslash path on Windows, which only the
		// Windows QA VM runs (D-W11); the slash form elsewhere.
		filepath.FromSlash("services/catalog/schema/openapi.json"),
	} {
		result := TraceChangeImpact(ws, []string{changed}, ChangeImpactOptions{})
		edge := result.Trace.Edges["/clients/catalog-ts"]
		if edge.Kind != ImpactEdgeContract || edge.Via != "services/catalog/schema/openapi.json" {
			t.Errorf("%s reached the client over %+v, want the contract edge naming the committed contract", changed, edge)
		}
	}
}

// TestChangeImpact_AProviderAtTheWorkspaceRootGatesItsContractEdge pins the
// root spelling: a provider whose directory is the workspace root commits its
// contract at `schema/openapi.json`, and that path alone opens the edge.
func TestChangeImpact_AProviderAtTheWorkspaceRootGatesItsContractEdge(t *testing.T) {
	ws := NewWorkspace("/ws", nil, []*Project{
		{
			ID: "/", Name: "catalog", Path: ".",
			ContractServiceID: "catalog.items",
			ContractPath:      "schema/openapi.json",
			ContractSHA256:    contractEdgeDigest,
		},
		{
			ID: "/clients/catalog-ts", Name: "catalog-ts", Path: "clients/catalog-ts",
			GeneratedClient: &GeneratedClientBinding{
				ServiceID: "catalog.items", ContractSHA256: contractEdgeDigest, Language: "ts",
			},
		},
	})

	moved := TraceChangeImpact(ws, []string{"schema/openapi.json"}, ChangeImpactOptions{})
	if got, want := moved.Trace.Edges["/clients/catalog-ts"],
		(ImpactEdge{From: "/", Kind: ImpactEdgeContract, Via: "schema/openapi.json", ContractSHA256: contractEdgeDigest}); got != want {
		t.Errorf("edge into the client = %+v, want %+v", got, want)
	}

	implementation := TraceChangeImpact(ws, []string{"src/items.ts"}, ChangeImpactOptions{})
	for _, p := range implementation.Projects {
		if p.ID == "/clients/catalog-ts" {
			t.Errorf("an implementation change of a root provider selected its client over %+v",
				implementation.Trace.Edges[p.ID])
		}
	}
}

// TestImpactPath_NamesTheContractHop pins that the path `why_impacted` answers
// from states the contract hop, so an operator reading why a client runs sees
// the relation rather than a dependency that does not exist.
func TestImpactPath_NamesTheContractHop(t *testing.T) {
	ws := contractWorkspace(contractEdgeDigest)

	got := ImpactPath(ws, "/services/catalog", "/apps/storefront")
	want := []ImpactStep{
		{Project: "/services/catalog"},
		{Project: "/clients/catalog-ts", Kind: ImpactEdgeContract},
		{Project: "/apps/storefront", Kind: ImpactEdgeDependency},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ImpactPath = %+v, want %+v", got, want)
	}
	// The reach asks about a change to the provider as a whole, which
	// includes its contract: the edge fires and names no changed file.
	if got, want := ImpactReach(ws, "/services/catalog").Trace.Edges["/clients/catalog-ts"],
		(ImpactEdge{From: "/services/catalog", Kind: ImpactEdgeContract}); got != want {
		t.Errorf("reach edge into the client = %+v, want %+v", got, want)
	}
}

// TestMetadataDigest_ContractIsTheClientsProviderSideIdentity pins the other
// half of the edge: the contract digest is in the client's identity, so a moved
// contract misses, and nothing about the provider is, so a provider change with
// a stable contract hits.
func TestMetadataDigest_ContractIsTheClientsProviderSideIdentity(t *testing.T) {
	first := contractWorkspace(contractEdgeDigest)
	moved := contractWorkspace("2222222222222222222222222222222222222222222222222222222222222222")

	before := first.MetadataDigestFor(first.ProjectByID("/clients/catalog-ts"))
	after := moved.MetadataDigestFor(moved.ProjectByID("/clients/catalog-ts"))
	if before == after {
		t.Errorf("a moved contractSha256 left the client digest at %s; its actions would serve a stale client", before)
	}

	// The provider's own identity says nothing about the contract it serves:
	// the digest belongs to the targets generated from it.
	if got, want := first.MetadataDigestFor(first.ProjectByID("/services/catalog")),
		moved.MetadataDigestFor(moved.ProjectByID("/services/catalog")); got != want {
		t.Errorf("provider digest %s != %s across a contract move", got, want)
	}
}

// TestMetadataDigest_NoClientManifestFoldsNothing pins that a project with no
// committed client manifest keeps the identity it had. The fold is guarded on
// presence, so adding the contract binding moved no key in any workspace that
// generates no client.
func TestMetadataDigest_NoClientManifestFoldsNothing(t *testing.T) {
	ws := contractWorkspace(contractEdgeDigest)
	project := ws.ProjectByID("/apps/storefront")

	const golden = "wsid1:20e1003d325718a4d67ac92e9e3ca4e7e021fbe27f146bdedd7fae862f78d498"
	if got := ws.MetadataDigestFor(project); got != golden {
		t.Errorf("digest of a project with no client manifest = %s, want %s", got, golden)
	}
}
