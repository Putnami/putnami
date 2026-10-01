package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

const testContractDigest = "1111111111111111111111111111111111111111111111111111111111111111"

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

func clientManifestBody(serviceID, digest string) string {
	return `{"protocolVersion":1,"generatedBy":"@putnami/clientgen","language":"ts",` +
		`"service":{"id":"` + serviceID + `","audience":"api://` + serviceID + `"},` +
		`"contractSha256":"` + digest + `"}`
}

func contractBody(serviceID string) string {
	return `{"openapi":"3.1.0","x-putnami-client":{"protocolVersion":1,` +
		`"service":{"id":"` + serviceID + `","audience":"api://` + serviceID + `"}},"paths":{}}`
}

// TestResolveContractBindings_ReadsTheCommittedTree pins both ends of the
// derivation: a client manifest committed at a project's root, and the provider
// identity the provider's committed contract sidecar declares.
func TestResolveContractBindings_ReadsTheCommittedTree(t *testing.T) {
	root := t.TempDir()
	writeContractFile(t, root, "services/catalog/schema/openapi.json", contractBody("catalog.items"))
	writeContractFile(t, root, "clients/ts/client.putnami.json", clientManifestBody("catalog.items", testContractDigest))

	provider := &Project{ID: "/services/catalog", Name: "catalog", Path: "services/catalog"}
	client := &Project{ID: "/clients/ts", Name: "ts", Path: "clients/ts"}
	projects := []*Project{provider, client}

	resolveContractBindings(root, projects)

	if provider.ContractServiceID != "catalog.items" {
		t.Errorf("provider ContractServiceID = %q, want catalog.items", provider.ContractServiceID)
	}
	// The path and digest come from the same read: the digest is the one
	// clientgen records as contractSha256, over the file's exact bytes.
	sum := sha256.Sum256([]byte(contractBody("catalog.items")))
	if provider.ContractPath != "services/catalog/schema/openapi.json" ||
		provider.ContractSHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("provider contract = %q sha256 %q, want services/catalog/schema/openapi.json sha256 %x",
			provider.ContractPath, provider.ContractSHA256, sum)
	}
	if client.ContractPath != "" || client.ContractSHA256 != "" {
		t.Errorf("client contract = %q sha256 %q, want none: it commits no contract", client.ContractPath, client.ContractSHA256)
	}
	if client.GeneratedClient == nil {
		t.Fatal("the committed client manifest produced no binding")
	}
	if client.GeneratedClient.ServiceID != "catalog.items" ||
		client.GeneratedClient.ContractSHA256 != testContractDigest ||
		client.GeneratedClient.Language != "ts" ||
		client.GeneratedClient.ManifestPath != "clients/ts/client.putnami.json" {
		t.Errorf("binding = %+v, want the manifest's own words", client.GeneratedClient)
	}

	ws := NewWorkspace(root, nil, projects)
	if got := ws.Graph.ContractProviderOf("/clients/ts"); got != "/services/catalog" {
		t.Errorf("ContractProviderOf = %q, want /services/catalog", got)
	}
}

// TestResolveContractBindings_IgnoresTheBuiltTree pins that only COMMITTED
// bytes are read. A contract that exists under .gen describes whatever build
// last ran here, so resolving from it would give a cold clone and a warm
// checkout two different graphs for one commit.
func TestResolveContractBindings_IgnoresTheBuiltTree(t *testing.T) {
	root := t.TempDir()
	writeContractFile(t, root, "services/catalog/.gen/schema/openapi.json", contractBody("catalog.items"))
	writeContractFile(t, root, "clients/ts/client.putnami.json", clientManifestBody("catalog.items", testContractDigest))

	provider := &Project{ID: "/services/catalog", Name: "catalog", Path: "services/catalog"}
	client := &Project{ID: "/clients/ts", Name: "ts", Path: "clients/ts"}
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

// TestResolveContractBindings_ReadsTheManifestAtTheProjectRootOnly pins that a
// manifest deeper in the tree is another project's binding. A provider that
// generates into its own tree is not a client of itself.
func TestResolveContractBindings_ReadsTheManifestAtTheProjectRootOnly(t *testing.T) {
	root := t.TempDir()
	writeContractFile(t, root, "services/catalog/schema/openapi.json", contractBody("catalog.items"))
	writeContractFile(t, root, "services/catalog/clients/ts/client.putnami.json",
		clientManifestBody("catalog.items", testContractDigest))

	provider := &Project{ID: "/services/catalog", Name: "catalog", Path: "services/catalog"}
	resolveContractBindings(root, []*Project{provider})

	if provider.GeneratedClient != nil {
		t.Errorf("provider took a nested manifest as its own binding: %+v", provider.GeneratedClient)
	}
}

// TestCheckedInGeneratedClientsCarryAContractEdge is this repository's own
// inventory of the relation: every project that commits a generated client
// manifest resolves to the provider its manifest names, on the checked-in
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
			t.Errorf("%s commits a generated client manifest whose service no provider declares", clientID)
			continue
		}
		// The loader's digest of the provider's committed contract is the
		// digest clientgen recorded when it generated the client, byte for
		// byte: the trace names the same value a regenerated manifest states.
		provider := ws.ProjectByID(providerID)
		client := ws.ProjectByID(clientID)
		if provider.ContractSHA256 != client.GeneratedClient.ContractSHA256 {
			t.Errorf("%s records contractSha256 %s, but %s's committed contract %s hashes to %s",
				clientID, client.GeneratedClient.ContractSHA256, providerID, provider.ContractPath, provider.ContractSHA256)
		}
	}
}

// TestResolveContractBindings_SkipsProviderScanWithoutAClient pins the cost
// rule: with no committed client manifest anywhere, no contract is read, so a
// workspace that generates no client pays nothing for the relation.
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
	writeContractFile(t, root, "clients/ts/client.putnami.json", clientManifestBody("catalog.items", testContractDigest))

	provider := &Project{ID: "/services/catalog", Name: "catalog", Path: "services/catalog"}
	client := &Project{ID: "/clients/ts", Name: "ts", Path: "clients/ts"}
	resolveContractBindings(root, []*Project{provider, client})

	if provider.ContractServiceID != "" || provider.ContractPath != "" || provider.ContractSHA256 != "" {
		t.Errorf("contract = %q at %q (sha256 %q), want none for an unmarked document",
			provider.ContractServiceID, provider.ContractPath, provider.ContractSHA256)
	}
}
