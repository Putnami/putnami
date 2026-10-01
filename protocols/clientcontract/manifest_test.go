package clientcontract

import (
	"encoding/json"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
)

func TestGeneratedManifestStrictRoundTrip(t *testing.T) {
	spectest.Proves(t, "client-contract/first-party-generated-clients", "generated-inventory", "the-generated-manifest-inventories-contract-operations-and-file-hashes")
	manifest := &GeneratedClientManifestV1{
		ProtocolVersion: ProtocolVersion,
		GeneratedBy:     GeneratedBy,
		Language:        GeneratedLanguageGo,
		Service:         Service{ID: "catalog", Audience: "catalog"},
		Binding: GeneratedBinding{ImportPath: "example.dev/catalog/client", Clients: []GeneratedBindingClient{{
			Service: "CatalogService", ClientSymbol: "CatalogClient", BindingSymbol: "RegisterCatalogClient",
		}}},
		ContractSHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		Operations: []GeneratedOperation{{
			OperationID:  "catalog.get",
			Service:      "CatalogService",
			MethodSymbol: "Get",
			Stream:       StreamUnary,
			Transports:   []Transport{{Protocol: TransportRESTJSON, Path: "/catalog/{id}", Encoding: EncodingJSON}},
		}},
		Files: []GeneratedFile{{
			Path: "client.gen.go", SHA256: "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
		}},
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	parsed, diags := ParseAndValidateGeneratedManifest(data)
	if diag.HasErrors(diags) || parsed.ContractSHA256 != manifest.ContractSHA256 {
		t.Fatalf("manifest failed round trip: parsed=%+v diags=%v", parsed, diags)
	}
}

func TestGeneratedManifestRejectsUnsupportedMarkerAndUnsafeInventory(t *testing.T) {
	spectest.Proves(t, "client-contract/first-party-generated-clients", "generated-inventory", "unsupported-generator-markers-and-unsafe-generated-paths-are-rejected")
	cases := []string{
		`{"protocolVersion":1,"generatedBy":"handwritten","language":"go","service":{"id":"x","audience":"x"},"binding":{"importPath":"x/client","clients":[{"service":"X","clientSymbol":"Client","bindingSymbol":"Register"}]},"contractSha256":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","operations":[],"files":[]}`,
		`{"protocolVersion":1,"generatedBy":"@putnami/clientgen","language":"go","service":{"id":"x","audience":"x"},"binding":{"importPath":"x/client","clients":[{"service":"X","clientSymbol":"Client","bindingSymbol":"Register"}]},"contractSha256":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","operations":[],"files":[{"path":"../client.go","sha256":"abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"}]}`,
		`{"protocolVersion":1,"generatedBy":"@putnami/clientgen","language":"go","service":{"id":"x","audience":"x"},"binding":{"importPath":"x/client","clients":[{"service":"X","clientSymbol":"Client","bindingSymbol":"Register"}]},"contractSha256":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","operations":[],"files":[{"path":"client.putnami.json","sha256":"abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"}]}`,
		`{"protocolVersion":1,"generatedBy":"@putnami/clientgen","language":"go","service":{"id":"x","audience":"x"},"binding":{"importPath":"x/client","clients":[{"service":"X","clientSymbol":"Client","bindingSymbol":"Register"}]},"contractSha256":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","operations":[],"files":[{"path":"C:/client.go","sha256":"abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"}]}`,
		`{"protocolVersion":1,"generatedBy":"@putnami/clientgen","language":"go","service":{"id":"x","audience":"x"},"binding":{"importPath":"x/client","clients":[{"service":"X","clientSymbol":"Client","bindingSymbol":"Register"}]},"contractSha256":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","operations":[],"files":[{"path":"client\u0000.go","sha256":"abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"}]}`,
		`{"protocolVersion":1,"generatedBy":"@putnami/clientgen","language":"go","service":{"id":"x","audience":"x"},"binding":{"importPath":"x/client","clients":[{"service":"X","clientSymbol":"Client","bindingSymbol":"Register"}]},"contractSha256":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","operations":[{"operationId":"get","service":"Other","methodSymbol":"Get","stream":"unary","transports":[{"protocol":"rest-json","path":"/get","encoding":"json"}]}],"files":[]}`,
	}
	for _, input := range cases {
		if _, diags := ParseAndValidateGeneratedManifest([]byte(input)); !diag.HasErrors(diags) {
			t.Fatalf("invalid manifest was accepted: %s", input)
		}
	}
}

// An operation a target leaves out is named, never inventoried as generated:
// operations and omittedOperations together account for the contract, so a
// reader of committed bytes alone can tell an omission from a lost operation.
func TestGeneratedManifestNamesAnOmittedOperationApartFromTheGeneratedOnes(t *testing.T) {
	spectest.Proves(t, "client-contract/first-party-generated-clients", "generated-inventory", "an-operation-a-target-leaves-out-is-named-and-never-inventoried-as-generated")
	manifest := func(omitted string) string {
		return `{"protocolVersion":1,"generatedBy":"@putnami/clientgen","language":"go","service":{"id":"x","audience":"x"},"binding":{"importPath":"x/client","clients":[{"service":"X","clientSymbol":"Client","bindingSymbol":"Register"}]},"contractSha256":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","operations":[{"operationId":"get","service":"X","methodSymbol":"Get","stream":"unary","transports":[{"protocol":"rest-json","path":"/get","encoding":"json"}]}],` + omitted + `"files":[]}`
	}
	parsed, diags := ParseAndValidateGeneratedManifest([]byte(manifest(`"omittedOperations":["deploy","purge"],`)))
	if diag.HasErrors(diags) || len(parsed.OmittedOperations) != 2 || parsed.OmittedOperations[0] != "deploy" {
		t.Fatalf("a manifest naming two omitted operations: parsed=%+v diags=%v", parsed, diags)
	}
	for name, omitted := range map[string]string{
		"generated and omitted": `"omittedOperations":["get"],`,
		"out of order":          `"omittedOperations":["purge","deploy"],`,
		"repeated":              `"omittedOperations":["deploy","deploy"],`,
		"blank":                 `"omittedOperations":[" "],`,
	} {
		if _, diags := ParseAndValidateGeneratedManifest([]byte(manifest(omitted))); !diag.HasErrors(diags) {
			t.Fatalf("%s: invalid omitted operations were accepted", name)
		}
	}
	sorted := &GeneratedClientManifestV1{OmittedOperations: []string{"purge", "deploy"}}
	SortGeneratedManifest(sorted)
	if sorted.OmittedOperations[0] != "deploy" {
		t.Fatalf("SortGeneratedManifest left omitted operations as %v", sorted.OmittedOperations)
	}
	// A manifest that omits nothing carries no key at all, so every existing
	// target keeps its bytes.
	encoded, err := json.Marshal(&GeneratedClientManifestV1{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "omittedOperations") {
		t.Fatalf("an empty omission list is serialized: %s", encoded)
	}
}
