package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
)

func writeContractFile(t *testing.T, root, rel, body string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func contractBody(serviceID string) string {
	return `{"openapi":"3.1.0","x-putnami-client":{"protocolVersion":1,` +
		`"service":{"id":"` + serviceID + `","audience":"api://` + serviceID + `"}},"paths":{}}`
}

func contractDigest(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

// clientProject is a project whose putnami.json declares `generatedFrom`.
func clientProject(id, path string, from wsproto.GeneratedFrom) *Project {
	return &Project{ID: id, Name: filepath.Base(path), Path: path, Config: &wsproto.ProjectConfig{GeneratedFrom: &from}}
}

// TestResolveContractBindings_ReadsGeneratedFrom pins both ends of the
// derivation: the `generatedFrom` a client's putnami.json declares, and the
// provider identity, path and digest the provider's committed contract states.
func TestResolveContractBindings_ReadsGeneratedFrom(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "a-client-binds-from-generated-from",
		"the-binding-reads-generated-from-at-load")
	root := t.TempDir()
	body := contractBody("catalog.items")
	writeContractFile(t, root, "services/catalog/schema/openapi.json", body)

	provider := &Project{ID: "/services/catalog", Name: "catalog", Path: "services/catalog"}
	client := clientProject("/clients/go", "clients/go", wsproto.GeneratedFrom{
		Service: "catalog.items", Language: "go", PackageName: "itemsclient", ClientName: "ItemsClient",
		OmitOperations: []string{"b", "a", "b"},
	})
	projects := []*Project{provider, client}

	resolveContractBindings(root, projects)

	if provider.ContractServiceID != "catalog.items" ||
		provider.ContractPath != "services/catalog/schema/openapi.json" ||
		provider.ContractSHA256 != contractDigest(body) {
		t.Errorf("provider contract = %q at %q sha256 %q, want catalog.items at services/catalog/schema/openapi.json",
			provider.ContractServiceID, provider.ContractPath, provider.ContractSHA256)
	}
	want := GeneratedClientBinding{
		ServiceID:      "catalog.items",
		Language:       "go",
		Options:        `{"packageName":"itemsclient","clientName":"ItemsClient","omitOperations":["a","b"]}`,
		ProviderID:     "/services/catalog",
		ContractPath:   "services/catalog/schema/openapi.json",
		ContractSHA256: contractDigest(body),
	}
	if client.GeneratedClient == nil || *client.GeneratedClient != want {
		t.Errorf("binding = %+v, want %+v", client.GeneratedClient, want)
	}
	if provider.GeneratedClient != nil {
		t.Errorf("provider took a binding it does not declare: %+v", provider.GeneratedClient)
	}

	ws := NewWorkspace(root, nil, projects)
	if got := ws.Graph.ContractProviderOf("/clients/go"); got != "/services/catalog" {
		t.Errorf("ContractProviderOf = %q, want /services/catalog", got)
	}
}

// TestResolveContractBindings_AnUnresolvedServiceFoldsAFixedMarker pins that a
// service no project declares, and one two projects declare, both bind with
// the fixed marker and no provider: the key never depends on project order.
func TestResolveContractBindings_AnUnresolvedServiceFoldsAFixedMarker(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "a-client-binds-from-generated-from",
		"an-unresolved-service-folds-a-fixed-marker")
	root := t.TempDir()
	writeContractFile(t, root, "services/a/schema/openapi.json", contractBody("shared"))
	writeContractFile(t, root, "services/b/schema/openapi.json", contractBody("shared"))

	missing := clientProject("/clients/missing", "clients/missing", wsproto.GeneratedFrom{Service: "nobody", Language: "ts"})
	ambiguous := clientProject("/clients/shared", "clients/shared", wsproto.GeneratedFrom{Service: "shared", Language: "ts"})
	projects := []*Project{
		{ID: "/services/a", Name: "a", Path: "services/a"},
		{ID: "/services/b", Name: "b", Path: "services/b"},
		missing, ambiguous,
	}

	resolveContractBindings(root, projects)

	for _, client := range []*Project{missing, ambiguous} {
		binding := client.GeneratedClient
		if binding == nil {
			t.Fatalf("%s: no binding", client.ID)
		}
		if binding.ContractSHA256 != UnresolvedContractDigest || binding.ProviderID != "" || binding.ContractPath != "" {
			t.Errorf("%s: binding = %+v, want the unresolved marker and no provider", client.ID, binding)
		}
	}
}

// TestResolveContractBindings_IgnoresTheBuiltTree pins that only COMMITTED
// bytes are read. A contract that exists under .gen describes whatever build
// last ran here, so resolving from it would give a cold clone and a warm
// checkout two different graphs for one commit.
func TestResolveContractBindings_IgnoresTheBuiltTree(t *testing.T) {
	root := t.TempDir()
	writeContractFile(t, root, "services/catalog/.gen/schema/openapi.json", contractBody("catalog.items"))

	provider := &Project{ID: "/services/catalog", Name: "catalog", Path: "services/catalog"}
	client := clientProject("/clients/ts", "clients/ts", wsproto.GeneratedFrom{Service: "catalog.items", Language: "ts"})
	projects := []*Project{provider, client}

	resolveContractBindings(root, projects)

	if provider.ContractServiceID != "" || provider.ContractPath != "" || provider.ContractSHA256 != "" {
		t.Errorf("provider resolved service %q at %q (sha256 %q) from a built contract",
			provider.ContractServiceID, provider.ContractPath, provider.ContractSHA256)
	}
	ws := NewWorkspace(root, nil, projects)
	if got := ws.Graph.ContractProviderOf("/clients/ts"); got != "" {
		t.Errorf("ContractProviderOf = %q, want no edge from a built-only contract", got)
	}
}

// TestCheckedInGeneratedClientsCarryAContractEdge is this repository's own
// inventory of the relation: every project whose putnami.json declares
// `generatedFrom` resolves to the provider its service names, on the checked-in
// workspace rather than a fixture. The cross-language samples are the ones that
// have it — a Go client of a TypeScript provider, a TypeScript client of a Go
// provider — and neither language's module graph can express it.
func TestCheckedInGeneratedClientsCarryAContractEdge(t *testing.T) {
	ws := loadCheckedInWorkspace(t)

	want := map[string]string{
		"/typescript/samples/10-service-to-service/clients/go": "/typescript/samples/10-service-to-service",
		"/typescript/samples/10-service-to-service/clients/ts": "/typescript/samples/10-service-to-service",
		"/go/samples/service-to-service/clients/ts":            "/go/samples/service-to-service",
	}
	got := make(map[string]string)
	for _, project := range ws.Projects {
		if project == nil || project.GeneratedClient == nil {
			continue
		}
		got[project.ID] = ws.Graph.ContractProviderOf(project.ID)
	}
	for clientID, providerID := range want {
		if got[clientID] != providerID {
			t.Errorf("contract provider of %s = %q, want %s", clientID, got[clientID], providerID)
		}
	}
	for clientID, providerID := range got {
		if providerID == "" {
			t.Errorf("%s declares a generatedFrom service no provider declares", clientID)
			continue
		}
		// The binding folds the loader's digest of the provider's committed
		// contract, the value every key of the client asks for.
		provider := ws.ProjectByID(providerID)
		client := ws.ProjectByID(clientID)
		if provider.ContractSHA256 != client.GeneratedClient.ContractSHA256 ||
			provider.ContractPath != client.GeneratedClient.ContractPath {
			t.Errorf("%s binds %s at %s, but %s's committed contract %s hashes to %s", clientID,
				client.GeneratedClient.ContractSHA256, client.GeneratedClient.ContractPath,
				providerID, provider.ContractPath, provider.ContractSHA256)
		}
	}
}

// TestResolveContractBindings_SkipsProviderScanWithoutAClient pins the cost
// rule: with no `generatedFrom` anywhere, no contract is read, so a workspace
// that generates no client pays nothing for the relation.
func TestResolveContractBindings_SkipsProviderScanWithoutAClient(t *testing.T) {
	root := t.TempDir()
	writeContractFile(t, root, "services/catalog/schema/openapi.json", contractBody("catalog.items"))

	provider := &Project{ID: "/services/catalog", Name: "catalog", Path: "services/catalog"}
	resolveContractBindings(root, []*Project{provider})

	if provider.ContractServiceID != "" || provider.ContractPath != "" || provider.ContractSHA256 != "" {
		t.Errorf("contract = %q at %q (sha256 %q), want unresolved when no client asks",
			provider.ContractServiceID, provider.ContractPath, provider.ContractSHA256)
	}
}

// TestResolveContractBindings_AContractWithoutAServiceIsNoContract pins that
// the path and digest are set exactly when the service identity is: a committed
// OpenAPI document with no first-party marker provides nothing a client can
// name, so no change to it can move a client either.
func TestResolveContractBindings_AContractWithoutAServiceIsNoContract(t *testing.T) {
	root := t.TempDir()
	writeContractFile(t, root, "services/catalog/schema/openapi.json", `{"openapi":"3.1.0","paths":{}}`)

	provider := &Project{ID: "/services/catalog", Name: "catalog", Path: "services/catalog"}
	client := clientProject("/clients/ts", "clients/ts", wsproto.GeneratedFrom{Service: "catalog.items", Language: "ts"})
	resolveContractBindings(root, []*Project{provider, client})

	if provider.ContractServiceID != "" || provider.ContractPath != "" || provider.ContractSHA256 != "" {
		t.Errorf("contract = %q at %q (sha256 %q), want none for an unmarked document",
			provider.ContractServiceID, provider.ContractPath, provider.ContractSHA256)
	}
}

// loadClientWorkspace writes a provider that commits a contract and source, a
// client of it with the given generatedFrom members, and loads the workspace.
func loadClientWorkspace(t *testing.T, root, contract, source, generatedFrom string) *Workspace {
	t.Helper()
	writeFileAt(t, filepath.Join(root, "putnami.workspace.json"), `{"includes":["services/catalog","clients/ts"]}`)
	writeFileAt(t, filepath.Join(root, "services", "catalog", "putnami.json"), `{"name":"catalog"}`)
	writeContractFile(t, root, "services/catalog/schema/openapi.json", contract)
	writeFileAt(t, filepath.Join(root, "services", "catalog", "src", "main.ts"), source)
	writeFileAt(t, filepath.Join(root, "clients", "ts", "putnami.json"),
		`{"name":"catalog-client","generatedFrom":{`+generatedFrom+`}}`)
	InvalidateLoadCache(root)
	t.Cleanup(func() { InvalidateLoadCache(root) })
	ws, err := Load(root)
	if err != nil {
		t.Fatalf("load workspace: %v", err)
	}
	return ws
}

// TestMetadataDigest_TheClientFoldsItsProvidersLoadTimeContract pins the
// client's provider-side identity at load: an edit to the provider's committed
// contract moves the client's digest, an edit to the provider's implementation
// does not, and a generation option moves it as the contract does.
func TestMetadataDigest_TheClientFoldsItsProvidersLoadTimeContract(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "a-client-binds-from-generated-from",
		"the-identity-folds-the-providers-load-time-contract-digest")
	spectest.Proves(t, "cli/workspace-discovery-selection", "a-client-binds-from-generated-from",
		"an-implementation-change-keeps-every-client-key")
	const from = `"service":"catalog.items","language":"ts"`
	clientDigest := func(ws *Workspace) string {
		t.Helper()
		client := ws.ProjectByID("/clients/ts")
		if client == nil || client.GeneratedClient == nil {
			t.Fatalf("the client did not bind: %+v", client)
		}
		return ws.MetadataDigestFor(client)
	}

	root := t.TempDir()
	base := clientDigest(loadClientWorkspace(t, root, contractBody("catalog.items"), "export {}\n", from))

	if got := clientDigest(loadClientWorkspace(t, root, contractBody("catalog.items"), "export const x = 1\n", from)); got != base {
		t.Errorf("a provider implementation change moved the client digest from %s to %s", base, got)
	}
	moved := `{"openapi":"3.1.0","x-putnami-client":{"protocolVersion":1,` +
		`"service":{"id":"catalog.items","audience":"api://catalog.items"}},"paths":{"/items":{}}}`
	if got := clientDigest(loadClientWorkspace(t, root, moved, "export {}\n", from)); got == base {
		t.Errorf("a contract change left the client digest at %s", base)
	}
	optioned := from + `,"packageName":"items"`
	if got := clientDigest(loadClientWorkspace(t, root, contractBody("catalog.items"), "export {}\n", optioned)); got == base {
		t.Errorf("a generation option change left the client digest at %s", base)
	}
}
