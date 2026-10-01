package workspaceclient

import (
	"testing"

	clientcontract "go.putnami.dev/protocol/clientcontract"
	"go.putnami.dev/protocol/features/spectest"
)

func TestConsumerEdgesUseActualManifestBindingSymbols(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "consumer-lineage", "each-imported-binding-reports-provider-operation-artifact-consumer-auth-and-transport")
	root := t.TempDir()
	writeSourceIndex(t, root, "consumers/go", "consumers/ts")
	writeWorkspaceFile(t, root, "consumers/go/putnami.json", `{"name":"go-consumer"}`)
	writeWorkspaceFile(t, root, "consumers/go/main.go", `package main
import catalog "example.dev/catalog/client"
func register(registry any) { catalog.RegisterCatalogClient(registry) }
`)
	writeWorkspaceFile(t, root, "consumers/ts/putnami.json", `{"name":"ts-consumer"}`)
	writeWorkspaceFile(t, root, "consumers/ts/main.ts", `
import { registerCatalogClient as bind } from "@example/catalog-client";
bind(registry);
`)
	providerItem := provider{
		rel: "services/catalog", classification: ClassificationFirstParty,
		document: &clientcontract.DocumentV1{Service: clientcontract.Service{ID: "catalog", Audience: "catalog"}},
		security: map[string]clientcontract.Security{"catalog.watch": {Alternatives: []clientcontract.SecurityAlternative{
			{AllOf: []clientcontract.SecurityRequirement{{Profile: "service"}, {Profile: "tenant"}}},
		}}},
	}
	operation := clientcontract.GeneratedOperation{
		OperationID: "catalog.watch", Service: "Catalog", MethodSymbol: "Watch",
		Stream:     clientcontract.StreamServer,
		Transports: []clientcontract.Transport{{Protocol: clientcontract.TransportSSE, Path: "/catalog/watch", Encoding: clientcontract.EncodingJSON}},
	}
	report := Report{Providers: []ProviderReport{{
		Project: "services/catalog", Classification: ClassificationFirstParty,
		Targets: []TargetReport{
			{Language: clientcontract.GeneratedLanguageGo, Manifest: "services/catalog/clients/go/client.putnami.json",
				Binding: &clientcontract.GeneratedBinding{ImportPath: "example.dev/catalog/client", Clients: []clientcontract.GeneratedBindingClient{{
					Service: "Catalog", ClientSymbol: "CatalogClient", BindingSymbol: "RegisterCatalogClient",
				}}}, GeneratedOperations: []clientcontract.GeneratedOperation{operation}},
			{Language: clientcontract.GeneratedLanguageTypeScript, Manifest: "services/catalog/clients/ts/client.putnami.json",
				Binding: &clientcontract.GeneratedBinding{ImportPath: "@example/catalog-client", Clients: []clientcontract.GeneratedBindingClient{{
					Service: "Catalog", ClientSymbol: "CatalogClient", BindingSymbol: "registerCatalogClient",
				}}}, GeneratedOperations: []clientcontract.GeneratedOperation{operation}},
		},
	}}}
	edges := scanConsumerEdgesFromWorkspace(t, root, []provider{providerItem}, report)
	if len(edges) != 2 {
		t.Fatalf("consumer edges = %+v, want Go and TypeScript binding edges", edges)
	}
	if edges[0].BindingSymbol == "" || edges[1].BindingSymbol == "" {
		t.Fatalf("binding symbols were not retained: %+v", edges)
	}
	for _, edge := range edges {
		if len(edge.AuthProfiles) != 2 || edge.AuthProfiles[0] != "service" || edge.AuthProfiles[1] != "tenant" {
			t.Errorf("auth profiles = %v, want [service tenant]", edge.AuthProfiles)
		}
		if edge.MethodSymbol != "Watch" || edge.GeneratedArtifact == "" {
			t.Errorf("operation-to-artifact lineage incomplete: %+v", edge)
		}
	}
}

func TestBindingDetectionIgnoresCommentsAndRequiresImportedSymbol(t *testing.T) {
	tsComment := `// import { registerCatalogClient } from "@example/catalog-client";
// registerCatalogClient(registry);
`
	if tsUsesBinding(tsComment, tsCodeMask(tsComment), "@example/catalog-client", "registerCatalogClient") {
		t.Fatal("comment-only TypeScript binding must not produce a consumer edge")
	}
	tsString := `const fixture = 'import { registerCatalogClient } from "@example/catalog-client"; registerCatalogClient(registry)'`
	if tsUsesBinding(tsString, tsCodeMask(tsString), "@example/catalog-client", "registerCatalogClient") {
		t.Fatal("string-literal TypeScript binding must not produce a consumer edge")
	}
	goComment := []byte(`package main
// import catalog "example.dev/catalog/client"
// catalog.RegisterCatalogClient(registry)
`)
	if goUsesBinding("main.go", goComment, "example.dev/catalog/client", "RegisterCatalogClient") {
		t.Fatal("comment-only Go binding must not produce a consumer edge")
	}
}
