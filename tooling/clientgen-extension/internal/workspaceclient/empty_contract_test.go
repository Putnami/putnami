package workspaceclient

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	clientcontract "go.putnami.dev/protocol/clientcontract"
	"go.putnami.dev/protocol/features/spectest"
)

// allExternalContractDocument is a literal repro of the case: a
// provider that proxies a standard protocol — a Go module proxy here — declares
// its service identity through the document-level x-putnami-client marker and
// marks every route with the authority that owns its wire. Discovery leaves an
// externally owned operation out of the provider's inventory, so the
// first-party operation set is empty while the service identity, and the
// machine-checkable record of what was deliberately left out, both survive.
const allExternalContractDocument = `{
  "openapi": "3.1.0",
  "x-putnami-client": {
    "protocolVersion": 1,
    "service": {"id": "gomod-server", "audience": "gomod-server"},
    "credentials": {}
  },
  "paths": {
    "/mod/{module}/@v/list": {"get": {
      "operationId": "listModuleVersions",
      "responses": {"200": {"description": "version list"}},
      "x-putnami-external-contract": "Go module proxy protocol"
    }},
    "/mod/{module}/@v/{version}.info": {"get": {
      "operationId": "getModuleVersionInfo",
      "responses": {"200": {"description": "version metadata"}},
      "x-putnami-external-contract": "Go module proxy protocol"
    }}
  }
}
`

// noRouteContractDocument is the other shape that reaches the same state: a
// declared service that serves no route at all. Both must be readable as one
// fact — the first-party operation set is empty — rather than as two cases.
const noRouteContractDocument = `{
  "openapi": "3.1.0",
  "x-putnami-client": {
    "protocolVersion": 1,
    "service": {"id": "gomod-server", "audience": "gomod-server"},
    "credentials": {}
  },
  "paths": {}
}
`

// unreadableContractDocument declares a route the guard cannot classify: no
// x-putnami-client operation marker, and no external authority either. Its
// first-party operation set is empty only because discovery could not read it.
const unreadableContractDocument = `{
  "openapi": "3.1.0",
  "x-putnami-client": {
    "protocolVersion": 1,
    "service": {"id": "gomod-server", "audience": "gomod-server"},
    "credentials": {}
  },
  "paths": {
    "/mod/{module}/@v/list": {"get": {"operationId": "listModuleVersions", "responses": {"200": {"description": "ok"}}}}
  }
}
`

// brokenExternalContractDocument names no authority. The operation is neither
// first-party nor legibly external, which is exactly the state the empty
// declaration must not absorb.
const brokenExternalContractDocument = `{
  "openapi": "3.1.0",
  "x-putnami-client": {
    "protocolVersion": 1,
    "service": {"id": "gomod-server", "audience": "gomod-server"},
    "credentials": {}
  },
  "paths": {
    "/mod/{module}/@v/list": {"get": {
      "operationId": "listModuleVersions",
      "responses": {"200": {"description": "ok"}},
      "x-putnami-external-contract": " "
    }}
  }
}
`

func emptyContractWorkspace(t *testing.T, contract string) string {
	t.Helper()
	root := t.TempDir()
	writeWorkspaceFile(t, root, ".putnami/workspace-index.json", `{"version":4,"projects":[{"path":"services/gomod-server"}]}`)
	writeWorkspaceFile(t, root, "services/gomod-server/putnami.json", `{"name":"gomod-server","extensions":["@putnami/clientgen"]}`)
	writeWorkspaceFile(t, root, "services/gomod-server/schema/openapi.json", contract)
	return root
}

// TestADeclaredEmptyFirstPartyContractNeedsNoClient pins the
// empty-first-party-contract declaration described in ADR 0004. A provider may
// declare its service identity and leave every route out of the client
// contract; go.putnami.dev/api then generates no client at all. The guard reads
// that as a satisfied declaration and names no client for it, and it reaches
// the same verdict on the cold clone, where the only input is the committed
// contract sidecar.
func TestADeclaredEmptyFirstPartyContractNeedsNoClient(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "empty-first-party-contract",
		"a-contract-that-declares-no-first-party-operation-needs-no-client")
	for _, contract := range []struct {
		name     string
		document string
	}{
		{"every route owned by an external authority", allExternalContractDocument},
		{"no route at all", noRouteContractDocument},
	} {
		t.Run(contract.name, func(t *testing.T) {
			root := emptyContractWorkspace(t, contract.document)
			// The provider commits a generation contract that names an
			// in-module Go target, and a client project names its service.
			writeWorkspaceFile(t, root, "services/gomod-server/clientgen.json", `{"targets":["go"],"go":{"output":"client"}}`)
			writeWorkspaceFile(t, root, "services/gomod-server/go.mod", "module example.dev/gomod\n")
			writeWorkspaceFile(t, root, ".putnami/workspace-index.json",
				`{"version":4,"projects":[{"path":"clients/gomod"},{"path":"services/gomod-server"}]}`)
			writeWorkspaceFile(t, root, "clients/gomod/putnami.json",
				`{"name":"gomod-client","generatedFrom":{"service":"gomod-server","language":"go"}}`)
			writeWorkspaceFile(t, root, "clients/gomod/go.mod", "module example.dev/gomodclient\n")

			report := InspectCommitted(root, fixtureMembers(t, root))
			if !report.Clean() {
				t.Fatalf("a declared empty first-party contract produced findings: %+v", report.Findings)
			}
			if len(report.Providers) != 1 || report.Providers[0].ServiceID != "gomod-server" ||
				report.Providers[0].Classification != ClassificationFirstParty || !report.Providers[0].EmptyContract {
				t.Fatalf("the empty contract stopped being an audited first-party provider: %+v", report.Providers)
			}
			if clients := report.Providers[0].Clients; len(clients) != 0 {
				t.Fatalf("an empty contract reported %d client(s) that generate nothing: %+v", len(clients), clients)
			}

			// Without the generation contract the verdict does not change.
			if err := os.Remove(filepath.Join(root, "services", "gomod-server", "clientgen.json")); err != nil {
				t.Fatal(err)
			}
			report = InspectCommitted(root, fixtureMembers(t, root))
			if !report.Clean() {
				t.Fatalf("the verdict without a generation contract differs: %+v", report.Findings)
			}
			if len(report.Providers) != 1 || report.Providers[0].ServiceID != "gomod-server" || !report.Providers[0].EmptyContract {
				t.Fatalf("the cold tree lost the declared service identity: %+v", report.Providers)
			}
		})
	}
}

// TestAContractWithFirstPartyOperationsIsNotEmpty is the other half of the
// declaration: only a genuinely empty first-party operation set counts. A
// contract that declares an operation, including one under the trace method
// the guard once dropped, is no empty contract, so a consumer is pointed at a
// client and not told to classify.
func TestAContractWithFirstPartyOperationsIsNotEmpty(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "empty-first-party-contract",
		"a-contract-with-first-party-operations-is-not-empty")
	for name, document := range map[string]string{
		"two operations":      string(catalogContractForFixture(t)),
		"one trace operation": traceOperationContractDocument,
	} {
		t.Run(name, func(t *testing.T) {
			root := emptyContractWorkspace(t, document)
			report := InspectCommitted(root, fixtureMembers(t, root))
			if !report.Clean() {
				t.Fatalf("a provider without clients produced findings: %+v", report.Findings)
			}
			if len(report.Providers) != 1 || report.Providers[0].EmptyContract {
				t.Fatalf("a provider with an operation was reported as an empty contract: %+v", report.Providers)
			}
		})
	}
}

// TestAContractDiscoveryCouldNotReadIsNeverEmpty: an operation the guard could
// not classify leaves the provider's inventory empty for a reason that is not a
// declaration. Such a contract fails for the unreadable operation and is never
// reported as empty, so a broken build, or an external marker that names no
// authority, can never pass as a deliberately empty contract.
func TestAContractDiscoveryCouldNotReadIsNeverEmpty(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "empty-first-party-contract",
		"a-contract-discovery-could-not-read-is-never-empty")
	for _, contract := range []struct {
		name     string
		document string
		code     string
	}{
		{"no operation metadata at all", unreadableContractDocument, "clientgen.missing-operation-contract"},
		{"an external marker that names no authority", brokenExternalContractDocument, "clientgen.invalid-operation-contract"},
	} {
		t.Run(contract.name, func(t *testing.T) {
			root := emptyContractWorkspace(t, contract.document)
			report := InspectCommitted(root, fixtureMembers(t, root))
			assertFindingCodes(t, report, contract.code)
			if len(report.Providers) != 1 || report.Providers[0].EmptyContract {
				t.Fatalf("an unreadable contract was reported as empty: %+v", report.Providers)
			}
		})
	}
}

// TestTheGuardReadsExactlyTheMethodsTheGeneratorGenerates pins httpMethods to
// the generator's own walk. The two lists live in different modules — the guard
// is tooling, the emitter is go.putnami.dev/api — so neither can import the
// other and only a test can hold them equal.
//
// The authority is httpMethodOrder in go/framework/api/clientir.go; the strict
// path-item member check at clientir.go and HTTP_OPERATION_KEYS in
// typescript/framework/client/src/generator/openapi-reader.ts spell the same
// eight. A method the guard drops is invisible to it: the operation never
// reaches the discovery loop, so it is neither generated nor unread, and a
// provider whose only operations use it would read as a contract that is empty
// by design while the generator happily generates a client for it.
func TestTheGuardReadsExactlyTheMethodsTheGeneratorGenerates(t *testing.T) {
	generated := []string{"get", "post", "put", "patch", "delete", "head", "options", "trace"}
	if len(httpMethods) != len(generated) {
		t.Fatalf("httpMethods has %d methods, the generator walks %d: %v vs %v",
			len(httpMethods), len(generated), sortedMethodKeys(httpMethods), generated)
	}
	for _, method := range generated {
		if !httpMethods[method] {
			t.Errorf("httpMethods drops %q, which go/framework/api/clientir.go httpMethodOrder generates", method)
		}
	}
}

func sortedMethodKeys(methods map[string]bool) []string {
	keys := make([]string, 0, len(methods))
	for method := range methods {
		keys = append(keys, method)
	}
	sort.Strings(keys)
	return keys
}

// traceOperationContractDocument declares one fully-marked first-party
// operation under the method the guard used to drop. It is a real contract
// operation: the Go and TypeScript emitters both walk `trace`.
const traceOperationContractDocument = `{
  "openapi": "3.1.0",
  "x-putnami-client": {
    "protocolVersion": 1,
    "service": {"id": "gomod-server", "audience": "gomod-server"},
    "credentials": {}
  },
  "paths": {
    "/mod/diagnose": {"trace": {"operationId": "diagnoseModuleRoute", "x-putnami-client": {
      "stream": "unary",
      "transports": [{"protocol": "rest-json", "path": "/mod/diagnose", "encoding": "json"}],
      "security": {"alternatives": [{"allOf": []}]}, "errors": [], "idempotency": {"kind": "safe"}
    }}}
  }
}
`

// TestAConsumerOfAnEmptyContractIsToldToClassifyNotToRegenerate pins ADR 0004
// decision 5, the consumer-facing half of the exemption. A Go module proxy is
// exactly the provider whose callers hand-write transports, and the SDK drops a
// job's data payload when the job fails, so this diagnostic sentence is the only
// channel that reaches the developer. Telling them to regenerate a
// client that cannot exist, or to create a client that would generate nothing,
// sends them after work that does not close the finding.
func TestAConsumerOfAnEmptyContractIsToldToClassifyNotToRegenerate(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "empty-first-party-contract",
		"a-consumer-of-an-empty-contract-is-told-to-classify-the-callsite")
	root := t.TempDir()
	writeSourceIndex(t, root, "consumer")
	writeTestFile(t, root, "consumer/putnami.json", `{"name":"consumer"}`)
	writeTestFile(t, root, "consumer/go.mod", "module example.dev/consumer\n")
	writeTestFile(t, root, "consumer/call.go", `package consumer
import "net/http"
func call() { _, _ = http.Get("gomod-server") }
`)
	item := provider{
		rel: "services/gomod-server", classification: ClassificationFirstParty,
		document: &clientcontract.DocumentV1{
			ProtocolVersion: clientcontract.ProtocolVersion,
			Service:         clientcontract.Service{ID: "gomod-server", Audience: "gomod-server"},
			Credentials:     map[string]clientcontract.CredentialProfile{},
		},
	}
	report := Report{Providers: []ProviderReport{{
		Project: "services/gomod-server", Classification: ClassificationFirstParty,
		ServiceID: "gomod-server", EmptyContract: true,
	}}}

	_, findings := scanManualClientsFromWorkspace(t, root, []provider{item}, report)
	if len(findings) != 1 {
		t.Fatalf("expected one handwritten-transport finding, got %+v", findings)
	}
	want := "handwritten first-party transport to gomod-server must be replaced by the generated binding: " +
		"gomod-server declares no first-party operation, so no Go client is generated for it; " +
		"classify this callsite in consumer/clientgen.external.json under the authority that owns the wire"
	if !strings.HasPrefix(findings[0].Message, want) {
		t.Fatalf("the advice does not tell the consumer to classify:\n got  %s\n want %s...", findings[0].Message, want)
	}
	for _, wrong := range []string{"regenerate the client", "declare the target on the provider"} {
		if strings.Contains(findings[0].Message, wrong) {
			t.Fatalf("the advice still sends the consumer after %q: %s", wrong, findings[0].Message)
		}
	}

	// An empty contract names no client at all, so there is no phantom entry
	// for the advice — or any other consumer of the report — to walk.
	writeWorkspaceFile(t, root, "services/gomod-server/putnami.json", `{"name":"gomod-server"}`)
	writeWorkspaceFile(t, root, "services/gomod-server/schema/openapi.json", allExternalContractDocument)
	writeWorkspaceFile(t, root, "services/gomod-server/clientgen.json", `{"targets":["go","ts"]}`)
	writeSourceIndex(t, root, "consumer", "services/gomod-server")
	inspected := InspectCommitted(root, fixtureMembers(t, root))
	var found bool
	for _, reported := range inspected.Providers {
		if reported.ServiceID != "gomod-server" {
			continue
		}
		found = true
		if !reported.EmptyContract {
			t.Fatalf("the empty contract is not marked in the report: %+v", reported)
		}
		if len(reported.Clients) != 0 {
			t.Fatalf("an empty contract reported %d phantom client(s): %+v", len(reported.Clients), reported.Clients)
		}
	}
	if !found {
		t.Fatalf("the provider disappeared from the report: %+v", inspected.Providers)
	}
}
