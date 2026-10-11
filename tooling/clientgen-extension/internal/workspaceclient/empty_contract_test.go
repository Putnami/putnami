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

// TestADeclaredEmptyFirstPartyContractNeedsNoGeneratedTarget pins the
// empty-first-party-contract declaration described in ADR 0004.
// A provider may declare its service identity and leave every route out of the
// client contract; go.putnami.dev/api then stages no client at all. The guard
// must read that as a satisfied declaration, not as a target whose generation
// broke — and it must reach the same verdict on the cold clone, where the only
// input is the committed contract sidecar.
func TestADeclaredEmptyFirstPartyContractNeedsNoGeneratedTarget(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "empty-first-party-contract",
		"a-contract-that-declares-no-first-party-operation-needs-no-generated-target")
	for _, contract := range []struct {
		name     string
		document string
	}{
		{"every route owned by an external authority", allExternalContractDocument},
		{"no route at all", noRouteContractDocument},
	} {
		t.Run(contract.name, func(t *testing.T) {
			root := emptyContractWorkspace(t, contract.document)
			// The warm tree: the session's build wrote the generation contract
			// the provider's papi.Clients(...) call declares, naming both
			// targets. This is the state the issue reported.
			writeWorkspaceFile(t, root, "services/gomod-server/.gen/clientgen/config.json", `{"targets":["go","ts"]}`)

			report := InspectCommitted(root, fixtureMembers(t, root))
			if !report.Clean() {
				t.Fatalf("a declared empty first-party contract produced findings: %+v", report.Findings)
			}
			if report.Coverage.Required != 0 || report.Coverage.Covered != 0 || report.Coverage.Percent != 100 {
				t.Fatalf("coverage = %+v, want 0/0 at 100%%", report.Coverage)
			}
			if len(report.Providers) != 1 || report.Providers[0].ServiceID != "gomod-server" ||
				report.Providers[0].Classification != ClassificationFirstParty {
				t.Fatalf("the empty contract stopped being an audited first-party provider: %+v", report.Providers)
			}

			// The cold clone: nothing under .gen exists, so no generation
			// contract and no committed manifest name a target. The verdict
			// must not change.
			if err := os.RemoveAll(filepath.Join(root, "services", "gomod-server", ".gen")); err != nil {
				t.Fatal(err)
			}
			report = InspectCommitted(root, fixtureMembers(t, root))
			if !report.Clean() {
				t.Fatalf("the cold-tree verdict differs from the built-tree one: %+v", report.Findings)
			}
			if len(report.Providers) != 1 || report.Providers[0].ServiceID != "gomod-server" {
				t.Fatalf("the cold tree lost the declared service identity: %+v", report.Providers)
			}

			// A generation contract that configures no target stays satisfied:
			// there is nothing for clientgen.no-targets to be about.
			writeWorkspaceFile(t, root, "services/gomod-server/.gen/clientgen/config.json", `{"targets":[]}`)
			if report = InspectCommitted(root, fixtureMembers(t, root)); !report.Clean() {
				t.Fatalf("an empty contract with no configured target produced findings: %+v", report.Findings)
			}
		})
	}
}

// TestAContractWithFirstPartyOperationsStillNeedsItsGeneratedTarget is the other
// half of the declaration: only a genuinely empty first-party operation set is satisfied
// by no generated target. A provider that declares operations and commits no
// client still fails exactly as it did before.
func TestAContractWithFirstPartyOperationsStillNeedsItsGeneratedTarget(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "empty-first-party-contract",
		"a-contract-with-first-party-operations-still-needs-its-generated-target")
	root := emptyContractWorkspace(t, string(catalogContractForFixture(t)))
	writeWorkspaceFile(t, root, "services/gomod-server/.gen/clientgen/config.json", `{"targets":["go","ts"]}`)

	report := InspectCommitted(root, fixtureMembers(t, root))
	assertFindingCodes(t, report, "clientgen.missing-manifest")
	if count := findingCount(report, "clientgen.missing-manifest"); count != 2 {
		t.Fatalf("missing-manifest findings = %d, want one per declared target", count)
	}
	if report.Coverage.Required != 4 || report.Coverage.Covered != 0 {
		t.Fatalf("coverage = %+v, want the two operations required per target and none covered", report.Coverage)
	}

	// The cold clone of the same provider names no target, so the guard says
	// the provider generates clients and configured none.
	if err := os.RemoveAll(filepath.Join(root, "services", "gomod-server", ".gen")); err != nil {
		t.Fatal(err)
	}
	assertFindingCodes(t, InspectCommitted(root, fixtureMembers(t, root)), "clientgen.missing-config")
}

// TestAContractDiscoveryCouldNotReadIsNeverEmpty: an operation the guard could
// not classify leaves the provider's inventory empty for a reason that is not a
// declaration. Such a contract keeps failing for both reasons — the unreadable
// operation and the target that never materialized — so a broken build, or an
// external marker that names no authority, can never pass as a deliberately
// empty contract.
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
			writeWorkspaceFile(t, root, "services/gomod-server/.gen/clientgen/config.json", `{"targets":["go"]}`)
			assertFindingCodes(t, InspectCommitted(root, fixtureMembers(t, root)), contract.code, "clientgen.missing-manifest")
		})
	}
}

func findingCount(report Report, code string) int {
	count := 0
	for _, finding := range report.Findings {
		if finding.Code == code {
			count++
		}
	}
	return count
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

// TestAnOperationUnderAMethodTheGuardOnceDroppedStillNeedsItsTarget is the
// regression for the hole the exemption opened: a provider whose single
// first-party operation is a `trace` was invisible to discovery, so it reached
// emptyFirstPartyContract with a real operation to generate and the guard
// passed a tree that was genuinely missing its clients.
func TestAnOperationUnderAMethodTheGuardOnceDroppedStillNeedsItsTarget(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "empty-first-party-contract",
		"a-contract-with-first-party-operations-still-needs-its-generated-target")
	root := emptyContractWorkspace(t, traceOperationContractDocument)
	writeWorkspaceFile(t, root, "services/gomod-server/.gen/clientgen/config.json", `{"targets":["go","ts"]}`)

	report := InspectCommitted(root, fixtureMembers(t, root))
	if count := findingCount(report, "clientgen.missing-manifest"); count != 2 {
		t.Fatalf("missing-manifest findings = %d, want one per declared target; findings=%+v", count, report.Findings)
	}
	if report.Coverage.Required != 2 {
		t.Fatalf("coverage = %+v, want the trace operation required once per target", report.Coverage)
	}
	if len(report.Providers) != 1 || report.Providers[0].EmptyContract {
		t.Fatalf("a provider with a trace operation was reported as an empty contract: %+v", report.Providers)
	}
}

// TestACommittedTargetOfAnEmptyContractIsStillJudgedInFull pins ADR 0004
// decision 4. The exemption covers the ABSENCE of a manifest, never the content
// of one: a client generated while the contract still had operations, and left
// behind after the last one moved to an external authority, must fail as drift
// rather than disappear with the target that no longer has to exist.
func TestACommittedTargetOfAnEmptyContractIsStillJudgedInFull(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "empty-first-party-contract",
		"a-committed-target-of-an-empty-contract-is-still-judged-in-full")
	item, expected := analyzerFixture(t)
	// The provider's contract lost every first-party operation; the generated
	// Go client beside it still claims two.
	emptied := item
	emptied.operations = nil

	report := inspectWithReader("/workspace", ModeCheck, []provider{emptied}, nil, expected, expected)
	assertFindingCodes(t, report, "clientgen.operation-coverage-drift")
	if len(report.Providers) != 1 || len(report.Providers[0].Targets) != 1 {
		t.Fatalf("a committed target of an empty contract vanished from the report: %+v", report.Providers)
	}
	if report.Providers[0].EmptyContract != true {
		t.Fatalf("the emptied contract was not reported as empty: %+v", report.Providers[0])
	}

	// The recorded hashes are still checked against the bytes beside them.
	edited := memoryReader{}
	for path, content := range expected {
		edited[path] = append([]byte(nil), content...)
	}
	edited["services/catalog/clients/go/client.gen.go"] = []byte("// hand edit of a generated file\n")
	assertFindingCodes(t, inspectWithReader("/workspace", ModeCheck, []provider{emptied}, nil, edited, edited),
		"clientgen.forged-generated-hash")

	// And so is the lineage: a manifest cut from another contract still fails.
	moved := emptied
	moved.contractHash = "beef" + emptied.contractHash[4:]
	assertFindingCodes(t, inspectWithReader("/workspace", ModeCheck, []provider{moved}, nil, expected, expected),
		"clientgen.contract-drift")
}

// TestAConsumerOfAnEmptyContractIsToldToClassifyNotToRegenerate pins ADR 0004
// decision 5, the consumer-facing half of the exemption. A Go module proxy is
// exactly the provider whose callers hand-write transports, and the SDK drops a
// job's data payload when the job fails, so this diagnostic sentence is the only
// channel that reaches the developer. Telling them to regenerate a
// client that cannot exist, or to declare a target that would generate nothing,
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

	// An empty contract names no target at all, so there is no phantom entry
	// for the advice — or any other consumer of the report — to walk.
	writeWorkspaceFile(t, root, "services/gomod-server/putnami.json", `{"name":"gomod-server"}`)
	writeWorkspaceFile(t, root, "services/gomod-server/schema/openapi.json", allExternalContractDocument)
	writeWorkspaceFile(t, root, "services/gomod-server/.gen/clientgen/config.json", `{"targets":["go","ts"]}`)
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
		if len(reported.Targets) != 0 {
			t.Fatalf("an empty contract reported %d phantom target(s): %+v", len(reported.Targets), reported.Targets)
		}
	}
	if !found {
		t.Fatalf("the provider disappeared from the report: %+v", inspected.Providers)
	}
}
