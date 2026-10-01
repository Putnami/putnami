package workspaceclient

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	clientcontract "go.putnami.dev/protocol/clientcontract"
	"go.putnami.dev/protocol/features/spectest"
)

func TestManualClientScanResolvesAliasesAndProjectScopedEndpointVariables(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "handwritten-client-guard", "source-aware-scanning-covers-http-connect-sse-and-websocket-aliases")
	root := t.TempDir()
	writeSourceIndex(t, root, "consumer")
	writeTestFile(t, root, "consumer/putnami.json", `{"name":"consumer"}`)
	writeTestFile(t, root, "consumer/service.ts", `export const service = "catalog-service";`)
	writeTestFile(t, root, "consumer/call.ts", `
import { createClient as makeClient } from '@connectrpc/connect';
export const catalog = makeClient(schema, transport);
`)
	writeTestFile(t, root, "consumer/go.mod", "module example.dev/consumer\n")
	writeTestFile(t, root, "consumer/config.go", "package consumer\nconst audience = \"catalog-service\"\n")
	writeTestFile(t, root, "consumer/call.go", `package consumer
import wire "net/http"
func call(endpoint string) { _, _ = wire.NewRequest("GET", endpoint, nil) }
`)
	item := provider{
		classification: ClassificationFirstParty,
		document: &clientcontract.DocumentV1{
			ProtocolVersion: clientcontract.ProtocolVersion,
			Service:         clientcontract.Service{ID: "catalog-service", Audience: "catalog-service"},
			Credentials:     map[string]clientcontract.CredentialProfile{},
		},
		operations: []clientcontract.GeneratedOperation{{
			OperationID: "catalog.get", Stream: clientcontract.StreamUnary,
			Transports: []clientcontract.Transport{{Protocol: clientcontract.TransportRESTJSON, Path: "/catalog/items", Encoding: clientcontract.EncodingJSON}},
		}},
	}
	adaptations, findings := scanManualClientsFromWorkspace(t, root, []provider{item}, Report{})
	if len(adaptations) != 2 || len(findings) != 2 {
		t.Fatalf("aliases and split endpoint config produced adaptations=%+v findings=%+v", adaptations, findings)
	}
	seen := map[string]bool{}
	for _, adaptation := range adaptations {
		seen[adaptation.Path] = true
	}
	for _, path := range []string{"consumer/call.go", "consumer/call.ts"} {
		if !seen[path] {
			t.Errorf("manual transport %s was not detected", path)
		}
	}
}

func TestManualClientScanClassifiesTestsAsAllowedLowLevelUsage(t *testing.T) {
	root := t.TempDir()
	writeSourceIndex(t, root, "consumer")
	writeTestFile(t, root, "consumer/putnami.json", `{"name":"consumer"}`)
	writeTestFile(t, root, "consumer/client_test.go", `package consumer
import "net/http"
func helper() { _, _ = http.NewRequest("GET", "/catalog/items", nil) }
`)
	item := provider{
		classification: ClassificationFirstParty,
		document: &clientcontract.DocumentV1{
			ProtocolVersion: clientcontract.ProtocolVersion,
			Service:         clientcontract.Service{ID: "catalog-service", Audience: "catalog-service"},
			Credentials:     map[string]clientcontract.CredentialProfile{},
		},
		operations: []clientcontract.GeneratedOperation{{
			OperationID: "catalog.get", Stream: clientcontract.StreamUnary,
			Transports: []clientcontract.Transport{{Protocol: clientcontract.TransportRESTJSON, Path: "/catalog/items", Encoding: clientcontract.EncodingJSON}},
		}},
	}
	adaptations, findings := scanManualClientsFromWorkspace(t, root, []provider{item}, Report{})
	if len(adaptations) != 0 || len(findings) != 0 {
		t.Fatalf("test-only low-level usage must remain allowed: adaptations=%+v findings=%+v", adaptations, findings)
	}
}

func TestTypeScriptTransportScannerIgnoresCommentsAndStringFixtures(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "handwritten-client-guard", "comments-strings-tests-and-validated-generated-files-do-not-trigger")
	content := `
// fetch("https://catalog.internal/watch")
const fixture = 'new WebSocket("wss://catalog.internal/watch")';
const importFixture = 'import { BaseClient } from "@putnami/client"; new BaseClient({})';
`
	if got := tsTransportEvidence(content); len(got) != 0 {
		t.Fatalf("comment and string fixtures produced transport evidence: %v", got)
	}
}

func TestTypeScriptTransportScannerPreservesExecutableTemplateAndJSXExpressions(t *testing.T) {
	content := `
const rendered = ` + "`" + `result=${await fetch(url)}` + "`" + `;
const view = <Panel load={() => globalThis.fetch(url)}>fetch(external)</Panel>;
const regex = /fetch\("catalog-service"\)/;
`
	evidence := tsTransportEvidence(content)
	if len(evidence) != 1 || evidence[0] != "fetch" {
		t.Fatalf("executable template/JSX expressions or regex masking produced evidence %v", evidence)
	}
}

func TestTypeScriptMaskerSeparatesNestedExecutableCodeFromLiteralSyntax(t *testing.T) {
	content := `
/* fetch(blockComment) */
const quoted = "new WebSocket(quoted)";
const regex = /new WebSocket\(["']catalog["']\)/giu;
const divided = total / count;
const nested = ` + "`" + `literal fetch(text) ${condition ? ` + "`" + `inner ${await fetch(url)}` + "`" + ` : ""}` + "`" + `;
const view = <Panel title="fetch(text)" load={() => new WebSocket(url)}>
  new EventSource(text)
  <Widget onLoad={() => new EventSource(url)} />
</Panel>;
`
	evidence := tsTransportEvidence(content)
	want := []string{"EventSource", "WebSocket", "fetch"}
	if !reflect.DeepEqual(evidence, want) {
		t.Fatalf("nested TypeScript literal/code masking evidence = %v, want %v", evidence, want)
	}
}

func TestTransportScannersCoverDirectCallsWhitespaceAliasesAndShadows(t *testing.T) {
	goSource := []byte(`package consumer
import (
  web "net/http"
  socket "github.com/gorilla/websocket"
)
func call(url string) {
  _, _ = web.Get(url)
  _, _ = web.Post(url, "application/json", nil)
  _, _ = web.DefaultClient.Do(nil)
  _, _, _ = socket.DefaultDialer.Dial(url, nil)
  _ = web.Client{}
}`)
	goEvidence := goTransportEvidence("client.go", goSource)
	for _, want := range []string{"web.Get", "web.Post", "web.DefaultClient.Do", "socket.DefaultDialer.Dial", "web.Client"} {
		if !containsString(goEvidence, want) {
			t.Errorf("Go evidence %v does not contain %q", goEvidence, want)
		}
	}

	tsSource := `
const request = globalThis.fetch;
const { WebSocket: Socket, EventSource } = globalThis;
request (url);
new Socket (url);
new EventSource (url);
window.fetch (url);
function local(fetch: (url: string) => void, WebSocket: new (url: string) => object) {
  fetch(url);
  new WebSocket(url);
}
`
	tsEvidence := tsTransportEvidence(tsSource)
	for _, want := range []string{"request", "Socket", "EventSource", "fetch"} {
		if !containsString(tsEvidence, want) {
			t.Errorf("TypeScript evidence %v does not contain %q", tsEvidence, want)
		}
	}
	shadowOnly := `const fetch = localFetch; class WebSocket {}; fetch(url); new WebSocket(url);`
	if got := tsTransportEvidence(shadowOnly); len(got) != 0 {
		t.Fatalf("shadowed platform symbols produced transport evidence: %v", got)
	}
}

func TestStaleGeneratedManifestCannotExemptHandwrittenSource(t *testing.T) {
	item, expected := analyzerFixture(t)
	root := t.TempDir()
	writeSourceIndex(t, root, "services/catalog")
	for path, content := range expected {
		writeWorkspaceFile(t, root, path, string(content))
	}
	writeWorkspaceFile(t, root, "services/catalog/putnami.json", `{"name":"catalog"}`)
	manifestPath := "services/catalog/clients/go/client.putnami.json"
	var manifest clientcontract.GeneratedClientManifestV1
	if err := json.Unmarshal(expected[manifestPath], &manifest); err != nil {
		t.Fatal(err)
	}
	writeWorkspaceFile(t, root, "services/catalog/clients/go/client.gen.go", `package catalog
import client "go.putnami.dev/client"
const service = "catalog"
func handwritten() { _, _ = client.NewBuilder().BaseURL(service).Build() }
`)
	report := Report{Providers: []ProviderReport{{
		Project: "services/catalog", Classification: ClassificationFirstParty, ServiceID: "catalog",
		Targets: []TargetReport{{Language: clientcontract.GeneratedLanguageGo, Manifest: manifestPath, Binding: &manifest.Binding}},
	}}}
	adaptations, findings := scanManualClientsFromWorkspace(t, root, []provider{item}, report)
	if len(adaptations) != 1 || len(findings) != 1 {
		t.Fatalf("stale generated inventory exempted handwritten source: adaptations=%+v findings=%+v", adaptations, findings)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestManualClientScanBlocksUnknownBuildersUntilServiceAuthorityIsPresent(t *testing.T) {
	root := t.TempDir()
	writeSourceIndex(t, root, "consumer")
	writeTestFile(t, root, "consumer/putnami.json", `{"name":"consumer"}`)
	writeTestFile(t, root, "consumer/call.ts", `
import { ClientBuilder } from '@putnami/client';
// catalog-service is only documentation for an unrelated fetch.
const payload = { kind: "catalog-service" };
export const unrelated = () => fetch(externalUrl);
export const catalog = () => ClientBuilder.for(ItemsClient).baseUrl(catalogUrl).buildSync();
`)
	writeTestFile(t, root, "consumer/call.go", `package consumer
import client "go.putnami.dev/client"
// catalog-service is only documentation for an unrelated builder.
func catalog() { _, _ = client.NewBuilder().BaseURL(catalogURL).Build() }
`)
	item := provider{
		classification: ClassificationFirstParty,
		document: &clientcontract.DocumentV1{
			ProtocolVersion: clientcontract.ProtocolVersion,
			Service:         clientcontract.Service{ID: "catalog-service", Audience: "catalog-service"},
			Credentials:     map[string]clientcontract.CredentialProfile{},
		},
	}
	adaptations, findings := scanManualClientsFromWorkspace(t, root, []provider{item}, Report{})
	if len(adaptations) != 3 || len(findings) != 3 {
		t.Fatalf("unowned transports were not queued individually: adaptations=%+v findings=%+v", adaptations, findings)
	}
	for _, finding := range findings {
		if finding.Code != "clientgen.unclassified-transport" || finding.Line == 0 || finding.Column == 0 ||
			!strings.Contains(finding.Message, "fingerprint=") {
			t.Fatalf("unknown transport finding has no exact callsite: %+v", finding)
		}
	}

	writeTestFile(t, root, "consumer/config.ts", `export const service = "catalog-service";`)
	adaptations, findings = scanManualClientsFromWorkspace(t, root, []provider{item}, Report{})
	if len(adaptations) != 3 || len(findings) != 3 {
		t.Fatalf("framework builders were not detected after a real service reference: adaptations=%+v findings=%+v", adaptations, findings)
	}
}

func TestAdaptationQueueUsesEveryActualGeneratedBindingSymbol(t *testing.T) {
	report := Report{Providers: []ProviderReport{{
		Project: "providers/catalog", Classification: ClassificationFirstParty, ServiceID: "catalog-service",
		Targets: []TargetReport{{
			Language: clientcontract.GeneratedLanguageTypeScript,
			Binding: &clientcontract.GeneratedBinding{
				ImportPath: "@acme/catalog-client",
				Clients: []clientcontract.GeneratedBindingClient{
					{Service: "CatalogAdmin", ClientSymbol: "CatalogAdminClient", BindingSymbol: "registerCatalogAdminClient"},
					{Service: "CatalogPublic", ClientSymbol: "CatalogPublicClient", BindingSymbol: "registerCatalogPublicClient"},
				},
			},
		}},
	}}}
	bindings := adaptationBindings(report, "ts", "catalog-service")
	if len(bindings) != 2 {
		t.Fatalf("binding candidates = %+v", bindings)
	}
	if bindings[0].BindingSymbol != "registerCatalogAdminClient" || bindings[1].BindingSymbol != "registerCatalogPublicClient" {
		t.Fatalf("binding symbols were derived or reordered incorrectly: %+v", bindings)
	}
	if bindings[0].ImportPath != "@acme/catalog-client" || bindings[0].ProviderProject != "providers/catalog" {
		t.Fatalf("binding provenance was lost: %+v", bindings[0])
	}
}

func TestOpaqueTransportUsesProjectDependencyAndBlocksAmbiguousProviders(t *testing.T) {
	root := t.TempDir()
	writeSourceIndex(t, root, "consumer", "providers/catalog", "providers/inventory")
	writeTestFile(t, root, "providers/catalog/putnami.json", `{"name":"catalog"}`)
	writeTestFile(t, root, "providers/inventory/putnami.json", `{"name":"inventory"}`)
	writeTestFile(t, root, "consumer/putnami.json", `{
  "name":"consumer",
  "dependencies":["/providers/catalog"]
}`)
	writeTestFile(t, root, "consumer/call.go", `package consumer
import (
  "net/http"
  "os"
)
func call() { _, _ = http.Get(os.Getenv("UPSTREAM_URL")) }
`)
	providers := []provider{
		{rel: "providers/catalog", classification: ClassificationFirstParty, document: &clientcontract.DocumentV1{
			ProtocolVersion: 1, Service: clientcontract.Service{ID: "catalog"}, Credentials: map[string]clientcontract.CredentialProfile{},
		}},
		{rel: "providers/inventory", classification: ClassificationFirstParty, document: &clientcontract.DocumentV1{
			ProtocolVersion: 1, Service: clientcontract.Service{ID: "inventory"}, Credentials: map[string]clientcontract.CredentialProfile{},
		}},
	}
	adaptations, findings := scanManualClientsFromWorkspace(t, root, providers, Report{})
	if len(adaptations) != 1 || adaptations[0].ServiceID != "catalog" || len(findings) != 1 {
		t.Fatalf("dependency-associated opaque transport adaptations=%+v findings=%+v", adaptations, findings)
	}

	writeTestFile(t, root, "consumer/putnami.json", `{
  "name":"consumer",
  "dependencies":["/providers/catalog","/providers/inventory"]
}`)
	adaptations, findings = scanManualClientsFromWorkspace(t, root, providers, Report{})
	if len(adaptations) != 1 || adaptations[0].ServiceID != "" || len(findings) != 1 ||
		findings[0].Code != "clientgen.unclassified-handwritten-transport" {
		t.Fatalf("ambiguous opaque transport adaptations=%+v findings=%+v", adaptations, findings)
	}
}

func TestValidatedExternalAdapterOwnsOpaqueLowLevelTransport(t *testing.T) {
	root := t.TempDir()
	writeSourceIndex(t, root, "consumer", "providers/catalog")
	writeTestFile(t, root, "providers/catalog/putnami.json", `{"name":"catalog"}`)
	writeTestFile(t, root, "consumer/putnami.json", `{"name":"consumer","dependencies":["/providers/catalog"]}`)
	writeTestFile(t, root, "consumer/vendor.ts", `export const callVendor = () => fetch(process.env.VENDOR_URL!);`)
	item := provider{rel: "providers/catalog", classification: ClassificationFirstParty, document: &clientcontract.DocumentV1{
		ProtocolVersion: 1, Service: clientcontract.Service{ID: "catalog"}, Credentials: map[string]clientcontract.CredentialProfile{},
	}}
	calls := tsTransportCallsites("consumer/vendor.ts", `export const callVendor = () => fetch(process.env.VENDOR_URL!);`)
	if len(calls) != 1 {
		t.Fatalf("vendor transport callsites = %+v", calls)
	}
	report := Report{ExternalContracts: []ExternalContract{{
		Project: "consumer", Adapter: "consumer/vendor.ts", Authority: "vendor SDK", Owner: "integrations",
		Callsites: []ExternalCallsite{externalIdentity(calls[0].callsite)}, Tests: []string{"consumer/vendor.test.ts"}, Reason: "external service",
	}}}
	adaptations, findings := scanManualClientsFromWorkspace(t, root, []provider{item}, report)
	if len(adaptations) != 0 || len(findings) != 0 {
		t.Fatalf("explicit external adapter was treated as first-party: adaptations=%+v findings=%+v", adaptations, findings)
	}
}

func TestSourceInventoryIgnoresContextOutputsAndUnindexedTrees(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "handwritten-client-guard", "only-indexed-production-sources-enter-the-transport-inventory")
	root := t.TempDir()
	writeSourceIndex(t, root, "consumer")
	writeTestFile(t, root, "consumer/putnami.json", `{"name":"consumer"}`)
	writeTestFile(t, root, "consumer/call.ts", `export const call = () => fetch(url);`)
	writeTestFile(t, root, ".context/capture/call.ts", `export const captured = () => fetch(url);`)
	writeTestFile(t, root, "scratch/call.ts", `export const scratch = () => fetch(url);`)
	writeTestFile(t, root, "consumer/out/call.ts", `export const output = () => fetch(url);`)

	records, err := discoverSourceRecords(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].path != "consumer/call.ts" || len(records[0].calls) != 1 {
		t.Fatalf("production source records = %+v", records)
	}
}

func TestExternalAuthorityOwnsOnlyItsExactCallsite(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "external-authority", "external-authority-is-scoped-to-exact-transport-callsites")
	root := t.TempDir()
	writeSourceIndex(t, root, "consumer")
	writeTestFile(t, root, "consumer/putnami.json", `{"name":"consumer"}`)
	source := "export const vendor = () => fetch(vendorURL);\nexport const added = () => fetch(otherURL);\n"
	writeTestFile(t, root, "consumer/vendor.ts", source)
	calls := tsTransportCallsites("consumer/vendor.ts", source)
	if len(calls) != 2 {
		t.Fatalf("transport callsites = %+v", calls)
	}
	report := Report{ExternalContracts: []ExternalContract{{
		Project: "consumer", Authority: "vendor SDK", Adapter: "consumer/vendor.ts",
		Callsites: []ExternalCallsite{externalIdentity(calls[0].callsite)}, Owner: "integrations",
		Tests: []string{"consumer/vendor.test.ts"}, Reason: "external service",
	}}}
	adaptations, findings := scanManualClientsFromWorkspace(t, root, nil, report)
	if len(adaptations) != 1 || len(findings) != 1 || findings[0].Code != "clientgen.unclassified-transport" ||
		findings[0].Line != calls[1].callsite.Line {
		t.Fatalf("new call in inventoried adapter escaped classification: adaptations=%+v findings=%+v", adaptations, findings)
	}
}

func TestExternalAuthorityRejectsExpressionSubstitutionAtSamePosition(t *testing.T) {
	root := t.TempDir()
	writeSourceIndex(t, root, "consumer")
	writeTestFile(t, root, "consumer/putnami.json", `{"name":"consumer"}`)
	original := `export const vendor = () => fetch(vendorURL);`
	replaced := `export const vendor = () => fetch(secretURL);`
	originalCall := tsTransportCallsites("consumer/vendor.ts", original)[0].callsite
	replacedCall := tsTransportCallsites("consumer/vendor.ts", replaced)[0].callsite
	if originalCall.Line != replacedCall.Line || originalCall.Column != replacedCall.Column ||
		originalCall.Fingerprint == replacedCall.Fingerprint {
		t.Fatalf("fixture did not preserve position while changing identity: original=%+v replaced=%+v", originalCall, replacedCall)
	}
	writeTestFile(t, root, "consumer/vendor.ts", replaced)
	report := Report{ExternalContracts: []ExternalContract{{
		Project: "consumer", Authority: "vendor SDK", Adapter: "consumer/vendor.ts",
		Callsites: []ExternalCallsite{externalIdentity(originalCall)}, Owner: "integrations",
		Tests: []string{"consumer/vendor.test.ts"}, Reason: "external service",
	}}}
	_, findings := scanManualClientsFromWorkspace(t, root, nil, report)
	assertFindingCodeList(t, findings, "clientgen.unclassified-transport", "clientgen.stale-external-callsite")
}

func TestDirectFirstPartyLiteralCannotUseExternalCallsiteAuthority(t *testing.T) {
	root := t.TempDir()
	writeSourceIndex(t, root, "consumer")
	writeTestFile(t, root, "consumer/putnami.json", `{"name":"consumer"}`)
	source := `export const call = () => fetch("catalog");`
	writeTestFile(t, root, "consumer/vendor.ts", source)
	calls := tsTransportCallsites("consumer/vendor.ts", source)
	item := provider{classification: ClassificationFirstParty, document: &clientcontract.DocumentV1{
		ProtocolVersion: 1, Service: clientcontract.Service{ID: "catalog"}, Credentials: map[string]clientcontract.CredentialProfile{},
	}, operations: []clientcontract.GeneratedOperation{{OperationID: "catalog.get", Stream: clientcontract.StreamUnary,
		Transports: []clientcontract.Transport{{Protocol: clientcontract.TransportRESTJSON, Path: "/catalog/items", Encoding: clientcontract.EncodingJSON}}}}}
	report := Report{ExternalContracts: []ExternalContract{{Project: "consumer", Authority: "claimed external",
		Adapter: "consumer/vendor.ts", Callsites: []ExternalCallsite{externalIdentity(calls[0].callsite)}, Owner: "integrations",
		Tests: []string{"consumer/vendor.test.ts"}, Reason: "claimed external"}}}
	_, findings := scanManualClientsFromWorkspace(t, root, []provider{item}, report)
	assertFindingCodeList(t, findings, "clientgen.handwritten-first-party-client", "clientgen.stale-external-callsite")
}

func TestExternalAuthorityWinsOverProviderRouteAndShortIDSubstringCoincidences(t *testing.T) {
	root := t.TempDir()
	writeSourceIndex(t, root, "consumer")
	writeTestFile(t, root, "consumer/putnami.json", `{"name":"consumer"}`)
	source := `export const call = () => fetch("https://api.external.test/v1/items");`
	writeTestFile(t, root, "consumer/vendor.ts", source)
	calls := tsTransportCallsites("consumer/vendor.ts", source)
	item := provider{classification: ClassificationFirstParty, document: &clientcontract.DocumentV1{
		ProtocolVersion: 1, Service: clientcontract.Service{ID: "items", Audience: "urn:putnami:items"},
		Credentials: map[string]clientcontract.CredentialProfile{},
	}, operations: []clientcontract.GeneratedOperation{{OperationID: "items.list", Stream: clientcontract.StreamUnary,
		Transports: []clientcontract.Transport{{Protocol: clientcontract.TransportRESTJSON, Path: "/items", Encoding: clientcontract.EncodingJSON}}}}}
	report := Report{ExternalContracts: []ExternalContract{{Project: "consumer", Authority: "vendor SDK",
		Adapter: "consumer/vendor.ts", Callsites: []ExternalCallsite{externalIdentity(calls[0].callsite)}, Owner: "integrations",
		Tests: []string{"consumer/vendor.test.ts"}, Reason: "external resource happens to share a route segment"}}}
	adaptations, findings := scanManualClientsFromWorkspace(t, root, []provider{item}, report)
	if len(adaptations) != 0 || len(findings) != 0 {
		t.Fatalf("route/id substring overrode exact external authority: adaptations=%+v findings=%+v", adaptations, findings)
	}
	adaptations, findings = scanManualClientsFromWorkspace(t, root, []provider{item}, Report{})
	if len(adaptations) != 1 || len(findings) != 1 || findings[0].Code != "clientgen.unclassified-transport" {
		t.Fatalf("unowned external-looking route was not blocked as unknown: adaptations=%+v findings=%+v", adaptations, findings)
	}
}

func TestCallsiteAssociationDoesNotClaimAnUnrelatedTransportInTheSameFile(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "handwritten-client-guard", "provider-association-is-scoped-to-the-transport-expression")
	root := t.TempDir()
	writeSourceIndex(t, root, "consumer")
	writeTestFile(t, root, "consumer/putnami.json", `{"name":"consumer"}`)
	source := "const service = \"catalog\";\nexport const catalog = () => fetch(catalogURL);\nexport const vendor = () => fetch(vendorURL);\n"
	writeTestFile(t, root, "consumer/clients.ts", source)
	calls := tsTransportCallsites("consumer/clients.ts", source)
	var catalogCall, vendorCall detectedTransport
	for _, call := range calls {
		if strings.Contains(call.expression, "catalogURL") {
			catalogCall = call
		} else if strings.Contains(call.expression, "vendorURL") {
			vendorCall = call
		}
	}
	item := provider{classification: ClassificationFirstParty, document: &clientcontract.DocumentV1{
		ProtocolVersion: 1, Service: clientcontract.Service{ID: "catalog"}, Credentials: map[string]clientcontract.CredentialProfile{},
	}, operations: []clientcontract.GeneratedOperation{{OperationID: "catalog.get", Stream: clientcontract.StreamUnary,
		Transports: []clientcontract.Transport{{Protocol: clientcontract.TransportRESTJSON, Path: "/catalog/items", Encoding: clientcontract.EncodingJSON}}}}}
	report := Report{ExternalContracts: []ExternalContract{{Project: "consumer", Authority: "vendor SDK",
		Adapter: "consumer/clients.ts", Callsites: []ExternalCallsite{externalIdentity(vendorCall.callsite)}, Owner: "integrations",
		Tests: []string{"consumer/clients.test.ts"}, Reason: "external service"}}}
	adaptations, findings := scanManualClientsFromWorkspace(t, root, []provider{item}, report)
	if len(adaptations) != 1 || adaptations[0].Callsite.Fingerprint != catalogCall.callsite.Fingerprint ||
		len(findings) != 1 || findings[0].Code != "clientgen.handwritten-first-party-client" {
		t.Fatalf("per-callsite association adaptations=%+v findings=%+v", adaptations, findings)
	}
}

func writeTestFile(t *testing.T, root, relative, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeSourceIndex(t *testing.T, root string, projects ...string) {
	t.Helper()
	type indexedProject struct {
		Path string `json:"path"`
	}
	index := struct {
		Version  int              `json:"version"`
		Projects []indexedProject `json:"projects"`
	}{Version: 4, Projects: make([]indexedProject, 0, len(projects))}
	for _, project := range projects {
		index.Projects = append(index.Projects, indexedProject{Path: project})
	}
	data, err := json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, root, ".putnami/workspace-index.json", string(data))
}

// TestHandwrittenTransportFailureNamesServiceAndGeneratedBinding pins the
// content of the diagnostic itself: it is the only signal that survives a
// failing guard, because the SDK drops the job's data payload — and with it the
// adaptation entry that carries the bindings — when the job returns an error.
func TestHandwrittenTransportFailureNamesServiceAndGeneratedBinding(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "handwritten-client-guard", "the-failure-names-the-service-and-the-generated-binding")
	root := t.TempDir()
	writeSourceIndex(t, root, "consumer")
	writeTestFile(t, root, "consumer/putnami.json", `{"name":"consumer"}`)
	writeTestFile(t, root, "consumer/call.ts", `export const catalog = () => fetch("catalog-service");`)
	writeTestFile(t, root, "consumer/go.mod", "module example.dev/consumer\n")
	writeTestFile(t, root, "consumer/call.go", `package consumer
import "net/http"
func call() { _, _ = http.Get("catalog-service") }
`)
	item := provider{
		classification: ClassificationFirstParty,
		document: &clientcontract.DocumentV1{
			ProtocolVersion: clientcontract.ProtocolVersion,
			Service:         clientcontract.Service{ID: "catalog-service", Audience: "catalog-service"},
			Credentials:     map[string]clientcontract.CredentialProfile{},
		},
	}
	providerReport := ProviderReport{
		Project: "providers/catalog", Classification: ClassificationFirstParty, ServiceID: "catalog-service",
		Targets: []TargetReport{{
			Language: clientcontract.GeneratedLanguageTypeScript,
			Manifest: "providers/catalog/clients/ts/client.putnami.json",
			Binding: &clientcontract.GeneratedBinding{
				ImportPath: "@acme/catalog-client",
				Clients: []clientcontract.GeneratedBindingClient{
					{Service: "Catalog", ClientSymbol: "CatalogClient", BindingSymbol: "registerCatalogClient"},
				},
			},
		}},
	}
	messages := func(report Report) map[string]string {
		_, findings := scanManualClientsFromWorkspace(t, root, []provider{item}, report)
		if len(findings) != 2 {
			t.Fatalf("expected one finding per language, got %+v", findings)
		}
		byPath := map[string]string{}
		for _, finding := range findings {
			if finding.Code != "clientgen.handwritten-first-party-client" || finding.ServiceID != "catalog-service" {
				t.Fatalf("finding lost its classification: %+v", finding)
			}
			byPath[finding.Path] = finding.Message
		}
		return byPath
	}

	got := messages(Report{Providers: []ProviderReport{providerReport}})
	wantTS := "handwritten first-party transport to catalog-service must be replaced by the generated binding: " +
		"import @acme/catalog-client and call registerCatalogClient (CatalogClient) (transport=http symbol=fetch fingerprint="
	if !strings.HasPrefix(got["consumer/call.ts"], wantTS) {
		t.Fatalf("TypeScript diagnostic names neither service nor binding:\n got  %s\n want %s...", got["consumer/call.ts"], wantTS)
	}
	wantGo := "handwritten first-party transport to catalog-service must be replaced by the generated binding: " +
		"catalog-service generates no Go client yet; declare the target on the provider providers/catalog (transport=http"
	if !strings.HasPrefix(got["consumer/call.go"], wantGo) {
		t.Fatalf("Go diagnostic does not say the target is missing:\n got  %s\n want %s...", got["consumer/call.go"], wantGo)
	}

	// A configured target whose manifest is not committed yet is a different
	// instruction: regenerate, do not declare.
	providerReport.Targets[0].Binding = nil
	got = messages(Report{Providers: []ProviderReport{providerReport}})
	wantPending := "the TypeScript target of catalog-service has no committed client manifest at providers/catalog/clients/ts/client.putnami.json; regenerate the client"
	if !strings.Contains(got["consumer/call.ts"], wantPending) {
		t.Fatalf("TypeScript diagnostic does not point at the uncommitted manifest:\n got  %s\n want %s", got["consumer/call.ts"], wantPending)
	}

	// Without a provider report (the provider never reached the inventory),
	// the service is still named.
	got = messages(Report{})
	if !strings.Contains(got["consumer/call.ts"], "to catalog-service must be replaced") ||
		!strings.Contains(got["consumer/call.ts"], "catalog-service generates no TypeScript client yet") {
		t.Fatalf("diagnostic without provider report lost the service: %s", got["consumer/call.ts"])
	}
}
