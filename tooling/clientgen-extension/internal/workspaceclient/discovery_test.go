package workspaceclient

import (
	"reflect"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

func TestDiscoveryUsesWorkspaceIndexAndPrefersBuiltContract(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "strict-provider-discovery", "only-indexed-projects-are-provider-authorities")
	spectest.Proves(t, clientgenFeature, "strict-provider-discovery", "built-contracts-win-over-stale-committed-copies")
	root := t.TempDir()
	writeWorkspaceFile(t, root, ".putnami/workspace-index.json", `{
  "version": 4,
  "projects": [{"path":"."}]
}`)
	writeWorkspaceFile(t, root, "putnami.json", `{"name":"root-provider"}`)
	writeWorkspaceFile(t, root, ".gen/clientgen/config.json", `{"targets":[]}`)
	writeWorkspaceFile(t, root, ".gen/schema/openapi.json", markedOpenAPI("built"))
	writeWorkspaceFile(t, root, "schema/openapi.json", markedOpenAPI("stale"))

	providers, findings := discover(root)
	if len(findings) != 0 {
		t.Fatalf("discovery findings = %+v", findings)
	}
	if len(providers) != 1 || providers[0].rel != "." || providers[0].document == nil || providers[0].document.Service.ID != "built" {
		t.Fatalf("root project or built contract was not selected: %+v", providers)
	}
	if providers[0].specPath != ".gen/schema/openapi.json" {
		t.Fatalf("specPath = %q, want built .gen artifact", providers[0].specPath)
	}
}

func TestDiscoveryIgnoresUnindexedFixturesAndRequiresExplicitThirdParty(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "strict-provider-discovery", "unmarked-contracts-require-explicit-third-party-mode")
	root := t.TempDir()
	writeWorkspaceFile(t, root, ".putnami/workspace-index.json", `{
  "version": 4,
  "projects": [{"path":"providers/external"}]
}`)
	writeWorkspaceFile(t, root, "providers/external/putnami.json", `{"name":"external"}`)
	writeWorkspaceFile(t, root, "providers/external/.gen/clientgen/config.json", `{"targets":["ts"]}`)
	writeWorkspaceFile(t, root, "providers/external/.gen/schema/openapi.json", `{"openapi":"3.1.0","paths":{}}`)
	writeWorkspaceFile(t, root, ".context/fixture/putnami.json", `{"name":"fixture"}`)
	writeWorkspaceFile(t, root, ".context/fixture/.gen/clientgen/config.json", `{"thirdParty":true,"targets":["ts"]}`)
	writeWorkspaceFile(t, root, ".context/fixture/.gen/schema/openapi.json", `{"openapi":"3.1.0","paths":{}}`)

	providers, findings := discover(root)
	if len(providers) != 1 || providers[0].rel != "providers/external" || providers[0].classification == ClassificationThirdParty {
		t.Fatalf("unmarked provider was implicitly classified or fixture leaked into graph: %+v", providers)
	}
	assertFindingCodeList(t, findings, "clientgen.missing-first-party-marker")

	writeWorkspaceFile(t, root, "providers/external/.gen/clientgen/config.json", `{"thirdParty":true,"targets":["ts"]}`)
	providers, findings = discover(root)
	if len(findings) != 0 || len(providers) != 1 || providers[0].classification != ClassificationThirdParty {
		t.Fatalf("explicit third-party config providers=%+v findings=%+v", providers, findings)
	}
}

// committedManifest is a generated client manifest's identity half, as the
// generator emits it: the marker, the provider service, and the contract
// digest. Discovery reads a target through the shared protocol decoder, which
// refuses bytes that name only one end of the relation, so a fixture that
// leaves any of them out is a file no first-party target is described by.
func committedManifest(language string) string {
	return `{"protocolVersion":1,"generatedBy":"@putnami/clientgen","language":"` + language + `",` +
		`"service":{"id":"catalog","audience":"catalog"},` +
		`"contractSha256":"1111111111111111111111111111111111111111111111111111111111111111"}`
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

// TestTheCheckReadsTheCommittedContractAndNamesTargetsFromCommittedManifests
// is the cold-tree half of discovery: with nothing under .gen, a provider is
// still found through its committed sidecar, and its targets through the
// manifests committed under it — the trap that once justified a nested build
// of every provider on every validate.
func TestTheCheckReadsTheCommittedContractAndNamesTargetsFromCommittedManifests(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "strict-provider-discovery", "the-check-reads-the-committed-contract-and-names-targets-from-committed-manifests")
	root := t.TempDir()
	writeWorkspaceFile(t, root, ".putnami/workspace-index.json", `{"version":4,"projects":[{"path":"services/catalog"}]}`)
	writeWorkspaceFile(t, root, "services/catalog/putnami.json", `{"name":"catalog","extensions":["@putnami/clientgen"]}`)
	writeWorkspaceFile(t, root, "services/catalog/schema/openapi.json", markedOpenAPI("committed"))
	writeWorkspaceFile(t, root, "services/catalog/clients/go/client.putnami.json", committedManifest("go"))
	writeWorkspaceFile(t, root, "services/catalog/clients/ts/client.putnami.json", committedManifest("ts"))
	writeWorkspaceFile(t, root, "services/catalog/node_modules/dep/client.putnami.json", committedManifest("ts"))

	providers, findings := discoverCommitted(root)
	if len(findings) != 0 {
		t.Fatalf("cold discovery findings = %+v", findings)
	}
	if len(providers) != 1 || providers[0].specPath != "services/catalog/schema/openapi.json" || providers[0].document.Service.ID != "committed" {
		t.Fatalf("the committed sidecar was not the provider's contract: %+v", providers)
	}
	config := providers[0].config
	if config == nil || !reflect.DeepEqual(config.Targets, []string{"go", "ts"}) || config.Go.Output != "clients/go" || config.TS.Output != "clients/ts" {
		t.Fatalf("targets synthesized from committed manifests = %+v", config)
	}

	// A stale build's contract does not outrank the committed sidecar for the
	// check, while sync keeps preferring what the build wrote.
	writeWorkspaceFile(t, root, "services/catalog/.gen/schema/openapi.json", markedOpenAPI("built"))
	providers, _ = discoverCommitted(root)
	if providers[0].document.Service.ID != "committed" {
		t.Fatalf("the check preferred a built contract over the committed sidecar: %+v", providers[0].specPath)
	}
	providers, _ = discover(root)
	if providers[0].document.Service.ID != "built" {
		t.Fatalf("sync no longer prefers the built contract: %+v", providers[0].specPath)
	}
}

// TestACommittedManifestThatNamesNoFirstPartyTargetIsAFinding pins the one
// decoder both readers share. A file at a target's path that does not name a
// first-party target — no generator marker, no provider service, no contract
// digest — names no target here and derives no contract edge in the workspace
// graph, and saying so is a finding rather than a silently missing target.
func TestACommittedManifestThatNamesNoFirstPartyTargetIsAFinding(t *testing.T) {
	root := t.TempDir()
	writeWorkspaceFile(t, root, ".putnami/workspace-index.json", `{"version":4,"projects":[{"path":"services/catalog"}]}`)
	writeWorkspaceFile(t, root, "services/catalog/putnami.json", `{"name":"catalog","extensions":["@putnami/clientgen"]}`)
	writeWorkspaceFile(t, root, "services/catalog/schema/openapi.json", markedOpenAPI("catalog"))
	writeWorkspaceFile(t, root, "services/catalog/clients/go/client.putnami.json",
		`{"protocolVersion":1,"generatedBy":"somebody-else","language":"go"}`)

	providers, findings := discoverCommitted(root)
	assertFindingCodeList(t, findings, "clientgen.invalid-manifest")
	if len(providers) != 1 {
		t.Fatalf("providers = %+v, want the provider itself", providers)
	}
	if config := providers[0].config; config != nil && len(config.Targets) != 0 {
		t.Errorf("targets = %+v, want none named by a manifest no generator wrote", config.Targets)
	}
}

// TestCommittedManifestsWithoutAContractAreAFinding: a provider that commits
// generated clients but no contract sidecar has clients nothing can judge. It
// is reported, never skipped as a project that generates nothing.
func TestCommittedManifestsWithoutAContractAreAFinding(t *testing.T) {
	root := t.TempDir()
	writeWorkspaceFile(t, root, ".putnami/workspace-index.json", `{"version":4,"projects":[{"path":"services/catalog"}]}`)
	writeWorkspaceFile(t, root, "services/catalog/putnami.json", `{"name":"catalog","extensions":["@putnami/clientgen"]}`)
	writeWorkspaceFile(t, root, "services/catalog/clients/go/client.putnami.json", committedManifest("go"))

	providers, findings := discoverCommitted(root)
	if len(providers) != 0 {
		t.Fatalf("a provider without a contract was admitted: %+v", providers)
	}
	assertFindingCodeList(t, findings, "clientgen.missing-contract")

	// An indexed client package INSIDE a provider's output directory holds a
	// manifest at its own root: it is a client the project is, not one it
	// generates, and it is neither a provider nor a finding.
	writeWorkspaceFile(t, root, ".putnami/workspace-index.json",
		`{"version":4,"projects":[{"path":"services/catalog"},{"path":"services/catalog/clients/go"}]}`)
	writeWorkspaceFile(t, root, "services/catalog/schema/openapi.json", markedOpenAPI("catalog"))
	writeWorkspaceFile(t, root, "services/catalog/clients/go/putnami.json", `{"name":"catalog-go-client"}`)
	providers, findings = discoverCommitted(root)
	if len(findings) != 0 || len(providers) != 1 || providers[0].rel != "services/catalog" {
		t.Fatalf("nested client package handling: providers=%+v findings=%+v", providers, findings)
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
	writeWorkspaceFile(t, root, ".gen/clientgen/config.json", `{"targets":[]}`)

	writeWorkspaceFile(t, root, ".gen/schema/openapi.json", document(`,
      "x-putnami-external-contract": "OCI Distribution Specification v1.1"`))
	providers, findings := discover(root)
	if len(findings) != 0 {
		t.Fatalf("discovery findings = %+v", findings)
	}
	if len(providers) != 1 || len(providers[0].operations) != 1 || providers[0].operations[0].OperationID != "getV2PutnamiCapabilities" {
		t.Fatalf("provider operations = %+v, want only the first-party one", providers)
	}

	writeWorkspaceFile(t, root, ".gen/schema/openapi.json", document(`,
      "x-putnami-external-contract": " "`))
	_, findings = discover(root)
	if len(findings) != 1 || findings[0].Code != "clientgen.invalid-operation-contract" ||
		!strings.Contains(findings[0].Message, "GET /v2/{name}/manifests/{reference}") || findings[0].ServiceID != "oci-server" {
		t.Fatalf("findings = %+v, want one invalid-operation-contract naming the route and the service", findings)
	}

	writeWorkspaceFile(t, root, ".gen/schema/openapi.json", document(``))
	_, findings = discover(root)
	assertFindingCodeList(t, findings, "clientgen.missing-operation-contract")
}
