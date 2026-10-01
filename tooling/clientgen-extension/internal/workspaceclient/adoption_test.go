package workspaceclient

import (
	"strings"
	"testing"

	clientcontract "go.putnami.dev/protocol/clientcontract"
	"go.putnami.dev/protocol/features/spectest"
)

func TestClassifiedInventoryClaimsEveryDetectedCallsiteOrFails(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "classified-transport-inventory",
		"every-detected-transport-callsite-is-claimed-or-fails")
	root := t.TempDir()
	writeSourceIndex(t, root, "primitive")
	writeTestFile(t, root, "primitive/putnami.json", `{"name":"@putnami/web"}`)
	source := "export const load = (url: string) => fetch(url);\nexport const post = (url: string) => fetch(url, {method:'POST'});\n"
	writeTestFile(t, root, "primitive/form.ts", source)
	writeTestFile(t, root, "primitive/form.test.ts", "// primitive transport test\n")
	calls := tsTransportCallsites("primitive/form.ts", source)
	if len(calls) != 2 {
		t.Fatalf("fixture must hold two callsites, got %d", len(calls))
	}
	entry := FrameworkTransport{
		Project: "primitive", Runtime: "@putnami/web", Status: StatusTransportPrimitive, Adapter: "primitive/form.ts",
		Callsites: []ExternalCallsite{externalIdentity(calls[0].callsite)}, Owner: "@putnami/web",
		Tests: []string{"primitive/form.test.ts"}, Reason: "the caller supplies the URL",
	}
	writeFrameworkInventory(t, root, []FrameworkTransport{entry})
	transports, findings := loadAndValidateFrameworkInventory(root)
	if len(findings) != 0 {
		t.Fatalf("a valid transport-primitive entry was refused: %+v", findings)
	}
	_, findings = scanManualClientsFromWorkspace(t, root, nil, Report{FrameworkTransports: transports})
	assertFindingCodeList(t, findings, "clientgen.unclassified-transport")

	// Claiming the second callsite empties the inventory; dropping one of the
	// two claims again makes the leftover entry stale rather than silent.
	entry.Callsites = append(entry.Callsites, externalIdentity(calls[1].callsite))
	sortExternalCallsites(entry.Callsites)
	writeFrameworkInventory(t, root, []FrameworkTransport{entry})
	transports, findings = loadAndValidateFrameworkInventory(root)
	if len(findings) != 0 {
		t.Fatalf("the completed entry was refused: %+v", findings)
	}
	if _, findings = scanManualClientsFromWorkspace(t, root, nil,
		Report{FrameworkTransports: transports}); len(findings) != 0 {
		t.Fatalf("a fully claimed adapter still reported findings: %+v", findings)
	}
	writeTestFile(t, root, "primitive/form.ts", "export const load = (url: string) => fetch(url);\n")
	transports, _ = loadAndValidateFrameworkInventory(root)
	_, findings = scanManualClientsFromWorkspace(t, root, nil, Report{FrameworkTransports: transports})
	assertFindingCodeList(t, findings, "clientgen.stale-framework-callsite")
}

func TestPendingProviderContractNamesItsOperationsAndItsClosingWork(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "classified-transport-inventory",
		"a-pending-provider-contract-entry-names-its-operations-and-its-closing-work")
	root := t.TempDir()
	writeSourceIndex(t, root, "events")
	writeTestFile(t, root, "events/putnami.json", `{"name":"@putnami/events"}`)
	source := "export const publish = (url: string) => fetch(url, {method:'POST'});\n"
	writeTestFile(t, root, "events/transport.ts", source)
	writeTestFile(t, root, "events/transport.test.ts", "// event transport test\n")
	calls := tsTransportCallsites("events/transport.ts", source)
	base := FrameworkTransport{
		Project: "events", Runtime: "@putnami/events", Status: StatusPendingProviderContract,
		Adapter: "events/transport.ts", Callsites: []ExternalCallsite{externalIdentity(calls[0].callsite)},
		Owner: "@putnami/events", Tests: []string{"events/transport.test.ts"},
		Reason: "the provider declaration is owned by Putnami Cloud",
	}
	writeFrameworkInventory(t, root, []FrameworkTransport{base})
	if _, findings := loadAndValidateFrameworkInventory(root); len(findings) == 0 {
		t.Fatal("a pending entry without operations or closing work was accepted")
	}

	settled := base
	settled.Status = StatusTransportPrimitive
	settled.Operations = []string{"eventserver.publish"}
	settled.PendingWork = "Putnami Cloud provider-side client declaration"
	writeFrameworkInventory(t, root, []FrameworkTransport{settled})
	if _, findings := loadAndValidateFrameworkInventory(root); len(findings) == 0 {
		t.Fatal("a settled status was allowed to claim a pending state")
	}

	pending := base
	pending.Operations = []string{"eventserver.publish"}
	pending.PendingWork = "Putnami Cloud provider-side client declaration"
	writeFrameworkInventory(t, root, []FrameworkTransport{pending})
	transports, findings := loadAndValidateFrameworkInventory(root)
	if len(findings) != 0 || len(transports) != 1 {
		t.Fatalf("a complete pending entry was refused: transports=%+v findings=%+v", transports, findings)
	}
}

func TestAStatusIsBoundToTheExactProjectIdentity(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "classified-transport-inventory",
		"a-status-can-only-be-claimed-by-the-exact-project-identity")
	root := t.TempDir()
	writeSourceIndex(t, root, "web")
	writeTestFile(t, root, "web/putnami.json", `{"name":"@putnami/web"}`)
	source := "export const load = (url: string) => fetch(url);\n"
	writeTestFile(t, root, "web/form.ts", source)
	writeTestFile(t, root, "web/form.test.ts", "// primitive transport test\n")
	calls := tsTransportCallsites("web/form.ts", source)
	entry := FrameworkTransport{
		Project: "web", Runtime: "@putnami/client", Status: StatusFrameworkRuntime, Adapter: "web/form.ts",
		Callsites: []ExternalCallsite{externalIdentity(calls[0].callsite)}, Owner: "@putnami/web",
		Tests: []string{"web/form.test.ts"}, Reason: "borrowed identity",
	}
	writeFrameworkInventory(t, root, []FrameworkTransport{entry})
	if _, findings := loadAndValidateFrameworkInventory(root); len(findings) == 0 {
		t.Fatal("a project claimed a generated-binding runtime identity it does not declare")
	}

	entry.Runtime = "@putnami/webb"
	entry.Status = StatusTransportPrimitive
	writeFrameworkInventory(t, root, []FrameworkTransport{entry})
	if _, findings := loadAndValidateFrameworkInventory(root); len(findings) == 0 {
		t.Fatal("a transport primitive was accepted under a name its manifest does not declare")
	}

	entry.Runtime = "@putnami/web"
	writeFrameworkInventory(t, root, []FrameworkTransport{entry})
	if _, findings := loadAndValidateFrameworkInventory(root); len(findings) != 0 {
		t.Fatalf("the exact project identity was refused: %+v", findings)
	}
}

func TestAnInventoryEntryCannotSilenceAFirstPartyServiceReference(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "classified-transport-inventory",
		"an-inventory-entry-cannot-silence-a-first-party-service-reference")
	root := t.TempDir()
	writeSourceIndex(t, root, "consumer")
	writeTestFile(t, root, "consumer/putnami.json", `{"name":"@putnami/web"}`)
	source := "export const call = () => fetch('catalog-service');\n"
	writeTestFile(t, root, "consumer/call.ts", source)
	writeTestFile(t, root, "consumer/call.test.ts", "// transport test\n")
	calls := tsTransportCallsites("consumer/call.ts", source)
	writeFrameworkInventory(t, root, []FrameworkTransport{{
		Project: "consumer", Runtime: "@putnami/web", Status: StatusTransportPrimitive, Adapter: "consumer/call.ts",
		Callsites: []ExternalCallsite{externalIdentity(calls[0].callsite)}, Owner: "@putnami/web",
		Tests: []string{"consumer/call.test.ts"}, Reason: "claims to be a primitive",
	}})
	transports, findings := loadAndValidateFrameworkInventory(root)
	if len(findings) != 0 {
		t.Fatalf("the entry itself is invalid, which is not what this proves: %+v", findings)
	}
	item := provider{
		classification: ClassificationFirstParty,
		document: &clientcontract.DocumentV1{
			ProtocolVersion: clientcontract.ProtocolVersion,
			Service:         clientcontract.Service{ID: "catalog-service", Audience: "catalog-service"},
			Credentials:     map[string]clientcontract.CredentialProfile{},
		},
	}
	_, findings = scanManualClientsFromWorkspace(t, root, []provider{item},
		Report{FrameworkTransports: transports})
	assertFindingCodeList(t, findings, "clientgen.handwritten-first-party-client")
}

func TestCallsiteAuthoritySurvivesAnUnrelatedEditToItsFile(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "classified-transport-inventory",
		"a-callsite-authority-survives-an-unrelated-edit-to-its-file")
	before := tsTransportCallsites("runtime/http.ts", "export const call = (url: string) => fetch(url);\n")
	after := tsTransportCallsites("runtime/http.ts",
		"export const unrelated = 1;\nexport const call = (url: string) => fetch(url);\n")
	if len(before) != 1 || len(after) != 1 {
		t.Fatalf("fixture must hold one callsite each: %d and %d", len(before), len(after))
	}
	if before[0].callsite.Fingerprint != after[0].callsite.Fingerprint {
		t.Fatal("an edit elsewhere in the file revoked the callsite's authority")
	}
	sibling := tsTransportCallsites("runtime/http.ts",
		"export const call = (url: string) => fetch(url);\nexport const again = (url: string) => fetch(url);\n")
	if len(sibling) != 2 {
		t.Fatalf("fixture must hold two callsites, got %d", len(sibling))
	}
	if sibling[0].callsite.Fingerprint == sibling[1].callsite.Fingerprint {
		t.Fatal("a second identical call reuses the first one's authority")
	}
	goBefore := goTransportCallsites("runtime/call.go", "runtime/call.go", []byte("package runtime\n\nimport \"net/http\"\n\nfunc call(u string) { _, _ = http.Get(u) }\n"))
	goAfter := goTransportCallsites("runtime/call.go", "runtime/call.go", []byte("package runtime\n\nimport \"net/http\"\n\nconst unrelated = 1\n\nfunc call(u string) { _, _ = http.Get(u) }\n"))
	if len(goBefore) != 1 || len(goAfter) != 1 || goBefore[0].callsite.Fingerprint != goAfter[0].callsite.Fingerprint {
		t.Fatal("a Go callsite's authority did not survive an unrelated edit to its file")
	}
	// The blank identifier binds nothing, so a callsite that names `_` must
	// not take every `x, _ :=` statement of its file as its declaration.
	blankSource := func(key string) []byte {
		return []byte("package runtime\n\nimport \"net/http\"\n\n" +
			"func read(values map[string]string) string { value, _ := values[\"" + key + "\"]; return value }\n\n" +
			"func client() *http.Client {\n\treturn &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}\n}\n")
	}
	blankBefore := goTransportCallsites("runtime/client.go", "runtime/client.go", blankSource("a"))
	blankAfter := goTransportCallsites("runtime/client.go", "runtime/client.go", blankSource("b"))
	if len(blankBefore) != 1 || len(blankAfter) != 1 {
		t.Fatalf("fixture must hold one callsite each: %d and %d", len(blankBefore), len(blankAfter))
	}
	if blankBefore[0].callsite.Fingerprint != blankAfter[0].callsite.Fingerprint {
		t.Fatal("an unrelated `x, _ :=` edit changed the fingerprint of a callsite that names the blank identifier")
	}
}

func TestGlobalTransportDetectionSeparatesCallsFromMembersAndDeclarations(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "transport-detection-precision",
		"a-member-call-or-a-declaration-is-not-a-global-transport")
	source := `
class Repository {
  private async fetch(sql: Sql, only: boolean): Promise<Row[]> { return []; }
  async applied(sql: Sql) { return await this.fetch(sql, true); }
}
export const cached = (cache: Cache) => cache.fetch(() => load());
export const real = (url: string) => fetch(url);
`
	calls := tsTransportCallsites("repository.ts", source)
	if len(calls) != 1 {
		t.Fatalf("member calls and declarations were counted as transports: %+v", calls)
	}
	if calls[0].callsite.Line != 7 {
		t.Fatalf("the detected callsite is not the real global fetch: %+v", calls[0].callsite)
	}
}

func TestAGlobalTransportTakenAsAValueIsACallsite(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "transport-detection-precision",
		"a-global-transport-taken-as-a-value-is-a-callsite")
	source := `
export interface Config { fetch: typeof globalThis.fetch }
export const supported = () => typeof globalThis.WebSocket !== 'undefined';
export const resolve = (config: Partial<Config>) => ({ fetch: config.fetch ?? globalThis.fetch });
export const socket = (config: { webSocket?: unknown }) => config.webSocket ?? globalThis.WebSocket;
`
	calls := tsTransportCallsites("bind.ts", source)
	if len(calls) != 2 {
		t.Fatalf("global transport bindings were miscounted: %+v", calls)
	}
	kinds := map[string]bool{}
	for _, call := range calls {
		kinds[call.callsite.Transport] = true
		if !strings.HasPrefix(call.callsite.Symbol, "globalThis.") {
			t.Fatalf("a bound global transport is not reported under its global identity: %+v", call.callsite)
		}
	}
	if !kinds["http"] || !kinds["websocket"] {
		t.Fatalf("both bound globals must be reported: %+v", calls)
	}
}
