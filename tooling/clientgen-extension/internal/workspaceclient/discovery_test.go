package workspaceclient

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

func TestDiscoveryUsesTheWorkspaceIndex(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "strict-provider-discovery", "only-indexed-projects-are-provider-authorities")
	root := t.TempDir()
	writeWorkspaceFile(t, root, ".putnami/workspace-index.json", `{
  "version": 4,
  "projects": [{"path":"."}]
}`)
	writeWorkspaceFile(t, root, "putnami.json", `{"name":"root-provider"}`)
	writeWorkspaceFile(t, root, "clientgen.json", `{"targets":[]}`)
	writeWorkspaceFile(t, root, "schema/openapi.json", markedOpenAPI("root"))
	writeWorkspaceFile(t, root, "unindexed/putnami.json", `{"name":"unindexed"}`)
	writeWorkspaceFile(t, root, "unindexed/clientgen.json", `{"targets":[]}`)
	writeWorkspaceFile(t, root, "unindexed/schema/openapi.json", markedOpenAPI("unindexed"))

	providers, findings := discoverCommitted(indexedView(root))
	if len(findings) != 0 {
		t.Fatalf("discovery findings = %+v", findings)
	}
	if len(providers) != 1 || providers[0].rel != "." || providers[0].document == nil || providers[0].document.Service.ID != "root" {
		t.Fatalf("discovery did not keep to the indexed root project: %+v", providers)
	}
}

// A provider with no app commits its generation contract as clientgen.json at
// its root, and discovery reads it. A first-party provider needs none: its
// committed contract alone makes it a provider.
func TestDiscoveryReadsTheCommittedGenerationContract(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "strict-provider-discovery", "the-check-reads-the-committed-contract-and-the-committed-generation-contract")
	root := t.TempDir()
	writeWorkspaceFile(t, root, ".putnami/workspace-index.json", `{
  "version": 4,
  "projects": [{"path":"providers/slice"}]
}`)
	writeWorkspaceFile(t, root, "providers/slice/putnami.json", `{"name":"slice"}`)
	writeWorkspaceFile(t, root, "providers/slice/schema/openapi.json", string(catalogContractForFixture(t)))

	providers, findings := discoverCommitted(indexedView(root))
	if len(findings) != 0 || len(providers) != 1 || providers[0].config != nil || providers[0].classification != ClassificationFirstParty {
		t.Fatalf("a first-party provider without clientgen.json: providers=%+v findings=%+v", providers, findings)
	}

	writeWorkspaceFile(t, root, "providers/slice/clientgen.json", `{"targets":["go"],"go":{"output":"go"}}`)
	providers, findings = discoverCommitted(indexedView(root))
	if len(findings) != 0 {
		t.Errorf("discovery with a committed contract reported %+v", findings)
	}
	if len(providers) != 1 || providers[0].config == nil || !reflect.DeepEqual(providers[0].config.Targets, []string{"go"}) ||
		providers[0].config.Go.Output != "go" {
		t.Fatalf("the committed clientgen.json was not read: %+v", providers)
	}
}

// The OpenAPI document pairs with the generation contract's origin, as both
// language emitters pair it. With the committed clientgen.json, discovery reads
// the committed schema/openapi.json even when an earlier build left a different
// .gen/schema/openapi.json, which no cache key covers in a project whose build
// does not write the contract, and reports the contract missing when the
// project commits none.
func TestDiscoveryPairsTheOpenAPIDocumentWithTheContractOrigin(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "strict-provider-discovery", "a-committed-generation-contract-reads-only-the-committed-sidecar")
	root := t.TempDir()
	writeWorkspaceFile(t, root, ".putnami/workspace-index.json", `{
  "version": 4,
  "projects": [{"path":"providers/slice"}]
}`)
	writeWorkspaceFile(t, root, "providers/slice/putnami.json", `{"name":"slice"}`)
	writeWorkspaceFile(t, root, "providers/slice/clientgen.json", `{"targets":[]}`)
	writeWorkspaceFile(t, root, "providers/slice/schema/openapi.json", markedOpenAPI("committed"))
	writeWorkspaceFile(t, root, "providers/slice/.gen/schema/openapi.json", markedOpenAPI("leftover"))

	providers, findings := discoverCommitted(indexedView(root))
	if len(findings) != 0 {
		t.Fatalf("discovery findings = %+v", findings)
	}
	if len(providers) != 1 || providers[0].specPath != "providers/slice/schema/openapi.json" ||
		providers[0].document == nil || providers[0].document.Service.ID != "committed" {
		t.Fatalf("a committed contract did not read the committed document: %+v", providers)
	}

	if err := os.Remove(filepath.Join(root, "providers", "slice", "schema", "openapi.json")); err != nil {
		t.Fatal(err)
	}
	_, findings = discoverCommitted(indexedView(root))
	assertFindingCodeList(t, findings, "clientgen.missing-contract")
}

func TestDiscoveryIgnoresUnindexedFixturesAndRequiresExplicitThirdParty(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "strict-provider-discovery", "unmarked-contracts-require-explicit-third-party-mode")
	root := t.TempDir()
	writeWorkspaceFile(t, root, ".putnami/workspace-index.json", `{
  "version": 4,
  "projects": [{"path":"providers/external"}]
}`)
	writeWorkspaceFile(t, root, "providers/external/putnami.json", `{"name":"external"}`)
	writeWorkspaceFile(t, root, "providers/external/clientgen.json", `{"targets":["ts"]}`)
	writeWorkspaceFile(t, root, "providers/external/schema/openapi.json", `{"openapi":"3.1.0","paths":{}}`)
	writeWorkspaceFile(t, root, ".context/fixture/putnami.json", `{"name":"fixture"}`)
	writeWorkspaceFile(t, root, ".context/fixture/clientgen.json", `{"thirdParty":true,"targets":["ts"]}`)
	writeWorkspaceFile(t, root, ".context/fixture/schema/openapi.json", `{"openapi":"3.1.0","paths":{}}`)

	providers, findings := discoverCommitted(indexedView(root))
	if len(providers) != 1 || providers[0].rel != "providers/external" || providers[0].classification == ClassificationThirdParty {
		t.Fatalf("unmarked provider was implicitly classified or fixture leaked into graph: %+v", providers)
	}
	assertFindingCodeList(t, findings, "clientgen.missing-first-party-marker")

	writeWorkspaceFile(t, root, "providers/external/clientgen.json", `{"thirdParty":true,"targets":["ts"]}`)
	providers, findings = discoverCommitted(indexedView(root))
	if len(findings) != 0 || len(providers) != 1 || providers[0].classification != ClassificationThirdParty {
		t.Fatalf("explicit third-party config providers=%+v findings=%+v", providers, findings)
	}
}

func markedOpenAPI(serviceID string) string {
	return `{
  "openapi": "3.1.0",
  "x-putnami-client": {
    "protocolVersion": 1,
    "service": {"id": "` + serviceID + `", "audience": "` + serviceID + `"},
    "credentials": {}
  },
  "paths": {}
}`
}

// TestTheCheckReadsTheCommittedContractOnAColdTree is the cold-tree half of
// discovery: with nothing under .gen, a provider is still found through its
// committed sidecar, a client manifest an older generator left is no input,
// and a stale build's contract does not outrank the sidecar.
func TestTheCheckReadsTheCommittedContractOnAColdTree(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "strict-provider-discovery", "the-check-reads-the-committed-contract-and-the-committed-generation-contract")
	root := t.TempDir()
	writeWorkspaceFile(t, root, ".putnami/workspace-index.json", `{"version":4,"projects":[{"path":"services/catalog"}]}`)
	writeWorkspaceFile(t, root, "services/catalog/putnami.json", `{"name":"catalog","extensions":["@putnami/clientgen"]}`)
	writeWorkspaceFile(t, root, "services/catalog/schema/openapi.json", markedOpenAPI("committed"))
	writeWorkspaceFile(t, root, "services/catalog/clients/go/client.putnami.json", `{"protocolVersion":1,"language":"go"}`)

	providers, findings := discoverCommitted(indexedView(root))
	if len(findings) != 0 {
		t.Fatalf("cold discovery findings = %+v", findings)
	}
	if len(providers) != 1 || providers[0].specPath != "services/catalog/schema/openapi.json" || providers[0].document.Service.ID != "committed" {
		t.Fatalf("the committed sidecar was not the provider's contract: %+v", providers)
	}
	if providers[0].config != nil {
		t.Fatalf("a leftover client manifest configured the provider: %+v", providers[0].config)
	}

	writeWorkspaceFile(t, root, "services/catalog/.gen/schema/openapi.json", markedOpenAPI("built"))
	providers, _ = discoverCommitted(indexedView(root))
	if providers[0].document.Service.ID != "committed" {
		t.Fatalf("the check preferred a built contract over the committed sidecar: %+v", providers[0].specPath)
	}
}

// TestAGenerationContractWithoutAContractIsAFinding: a provider that commits
// clientgen.json but no contract sidecar names clients nothing can generate. It
// is reported, never skipped as a project that generates nothing. A project
// with neither is no provider and no finding.
func TestAGenerationContractWithoutAContractIsAFinding(t *testing.T) {
	root := t.TempDir()
	writeWorkspaceFile(t, root, ".putnami/workspace-index.json",
		`{"version":4,"projects":[{"path":"services/catalog"},{"path":"services/orders"}]}`)
	writeWorkspaceFile(t, root, "services/catalog/putnami.json", `{"name":"catalog","extensions":["@putnami/clientgen"]}`)
	writeWorkspaceFile(t, root, "services/catalog/clientgen.json", `{"targets":["go"],"go":{"output":"client"}}`)
	writeWorkspaceFile(t, root, "services/orders/putnami.json", `{"name":"orders"}`)

	providers, findings := discoverCommitted(indexedView(root))
	if len(providers) != 0 {
		t.Fatalf("a provider without a contract was admitted: %+v", providers)
	}
	assertFindingCodeList(t, findings, "clientgen.missing-contract")
	if !strings.Contains(findings[0].Message, "schema/openapi.json") || findings[0].Path != "services/catalog" {
		t.Fatalf("missing-contract finding = %+v, want it to name the project and schema/openapi.json", findings[0])
	}
}

// An operation an external authority owns is not a provider operation: no
// target generates it, so the provider's inventory leaves it out and nothing
// reports it missing. A malformed marker is a finding that names the route,
// and an unmarked operation without metadata stays one.
func TestDiscoveryLeavesOperationsAnExternalAuthorityOwnsOutOfTheProvider(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "strict-provider-discovery", "an-operation-an-external-authority-owns-is-not-a-provider-operation")
	document := func(externalMarker string) string {
		return `{
  "openapi": "3.0.3",
  "x-putnami-client": {
    "protocolVersion": 1,
    "service": {"id": "oci-server", "audience": "oci-server"},
    "credentials": {}
  },
  "paths": {
    "/v2/_putnami/capabilities": {"get": {
      "operationId": "getV2PutnamiCapabilities",
      "responses": {"204": {"description": "empty"}},
      "x-putnami-client": {
        "stream": "unary",
        "transports": [{"protocol": "rest-json", "path": "/v2/_putnami/capabilities", "encoding": "json"}],
        "security": {"alternatives": [{"allOf": []}]},
        "errors": [],
        "idempotency": {"kind": "safe"}
      }
    }},
    "/v2/{name}/manifests/{reference}": {"get": {
      "operationId": "getV2_Name_Manifests_Reference",
      "responses": {"200": {"description": "manifest"}}` + externalMarker + `
    }}
  }
}`
	}
	root := t.TempDir()
	writeWorkspaceFile(t, root, ".putnami/workspace-index.json", `{"version": 4, "projects": [{"path":"."}]}`)
	writeWorkspaceFile(t, root, "putnami.json", `{"name":"oci-server"}`)
	writeWorkspaceFile(t, root, "clientgen.json", `{"targets":[]}`)

	writeWorkspaceFile(t, root, "schema/openapi.json", document(`,
      "x-putnami-external-contract": "OCI Distribution Specification v1.1"`))
	providers, findings := discoverCommitted(indexedView(root))
	if len(findings) != 0 {
		t.Fatalf("discovery findings = %+v", findings)
	}
	if len(providers) != 1 || len(providers[0].operations) != 1 || providers[0].operations[0].OperationID != "getV2PutnamiCapabilities" {
		t.Fatalf("provider operations = %+v, want only the first-party one", providers)
	}

	writeWorkspaceFile(t, root, "schema/openapi.json", document(`,
      "x-putnami-external-contract": " "`))
	_, findings = discoverCommitted(indexedView(root))
	if len(findings) != 1 || findings[0].Code != "clientgen.invalid-operation-contract" ||
		!strings.Contains(findings[0].Message, "GET /v2/{name}/manifests/{reference}") || findings[0].ServiceID != "oci-server" {
		t.Fatalf("findings = %+v, want one invalid-operation-contract naming the route and the service", findings)
	}

	writeWorkspaceFile(t, root, "schema/openapi.json", document(``))
	_, findings = discoverCommitted(indexedView(root))
	assertFindingCodeList(t, findings, "clientgen.missing-operation-contract")
}
