package workspaceclient

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	clientcontract "go.putnami.dev/protocol/clientcontract"
	"go.putnami.dev/protocol/features/spectest"
)

func TestExternalInventorySchemaMatchesStrictParser(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "external-authority", "external-contract-inventory-schema-matches-the-strict-parser")
	// Version 2 is a project's file; version 1 is the older workspace-root
	// layout, whose entries also name their project.
	for version, entry := range map[int]reflect.Type{
		1: reflect.TypeOf(ExternalContract{}),
		2: reflect.TypeOf(externalContractV2{}),
	} {
		name := fmt.Sprintf("clientgen-external-v%d.json", version)
		data, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "protocols", "clientcontract", "schemas", name))
		if err != nil {
			t.Fatal(err)
		}
		var schema map[string]any
		if err := json.Unmarshal(data, &schema); err != nil {
			t.Fatal(err)
		}
		if got := schema["$id"]; got != "https://putnami.dev/schemas/"+name {
			t.Fatalf("schema $id = %v", got)
		}
		assertJSONFieldNames(t, schema["properties"].(map[string]any), reflect.TypeOf(externalInventoryV2{}))
		definitions := schema["$defs"].(map[string]any)
		contract := definitions["contract"].(map[string]any)
		assertJSONFieldNames(t, contract["properties"].(map[string]any), entry)
		callsite := definitions["callsite"].(map[string]any)
		assertJSONFieldNames(t, callsite["properties"].(map[string]any), reflect.TypeOf(ExternalCallsite{}))
	}
}

func assertJSONFieldNames(t *testing.T, properties map[string]any, typ reflect.Type) {
	t.Helper()
	got := make([]string, 0, len(properties))
	for name := range properties {
		got = append(got, name)
	}
	sort.Strings(got)
	want := make([]string, 0, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		want = append(want, strings.Split(typ.Field(i).Tag.Get("json"), ",")[0])
	}
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("schema properties = %v, parser fields = %v", got, want)
	}
}

func TestExternalInventoryRequiresConcreteAuthorityAndCannotExemptFirstParty(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "external-authority", "external-contracts-carry-authority-adapter-owner-tests-and-reason-without-first-party-bypass")
	root := t.TempDir()
	writeSourceIndex(t, root, "vendors/payments")
	writeWorkspaceFile(t, root, "vendors/payments/putnami.json", `{"name":"payments-adapter"}`)
	writeWorkspaceFile(t, root, "vendors/payments/adapter.go", "package payments\n")
	writeWorkspaceFile(t, root, "vendors/payments/adapter_test.go", "package payments\n")
	thirdParty := provider{rel: "vendors/payments", classification: ClassificationThirdParty}

	_, findings := loadAndValidateExternalInventory(indexedView(root), []provider{thirdParty})
	assertFindingCodeList(t, findings, "clientgen.unclassified-external-contract")

	writeWorkspaceFile(t, root, "vendors/payments/"+externalInventoryFile, `{
  "protocolVersion": 2,
  "contracts": [{
	    "authority": "https://payments.example/openapi.json",
	    "adapter": "vendors/payments/adapter.go",
	    "callsites": [{"path":"vendors/payments/adapter.go","transport":"http","symbol":"http.Get","fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}],
	    "owner": "payments-platform",
    "tests": ["vendors/payments/adapter_test.go"],
    "reason": "External vendor API maintained outside this workspace"
  }]
}`)
	contracts, findings := loadAndValidateExternalInventory(indexedView(root), []provider{thirdParty})
	if len(findings) != 0 || len(contracts) != 1 {
		t.Fatalf("valid external inventory contracts=%+v findings=%+v", contracts, findings)
	}

	// The bypass judges the authority: an entry must never exempt calls TO a
	// marked first-party provider, named by service identity or project path.
	catalog := provider{rel: "services/catalog", classification: ClassificationFirstParty,
		document: &clientcontract.DocumentV1{Service: clientcontract.Service{ID: "catalog"}}}
	for _, authority := range []string{"catalog", "services/catalog"} {
		writeWorkspaceFile(t, root, "vendors/payments/"+externalInventoryFile, `{
  "protocolVersion": 2,
  "contracts": [{
    "authority": "`+authority+`",
    "adapter": "vendors/payments/adapter.go",
    "callsites": [{"path":"vendors/payments/adapter.go","transport":"http","symbol":"http.Get","fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}],
    "owner": "payments-platform",
    "tests": ["vendors/payments/adapter_test.go"],
    "reason": "claimed external"
  }]
}`)
		contracts, findings = loadAndValidateExternalInventory(indexedView(root), []provider{thirdParty, catalog})
		assertFindingCodeList(t, findings, "clientgen.first-party-external-bypass")
		if len(contracts) != 0 || findings[0].ServiceID != "catalog" {
			t.Fatalf("authority %q: contracts=%+v findings=%+v", authority, contracts, findings)
		}
	}
}

// A workload that serves a first-party API routinely also consumes an external
// contract (OTLP export to a collector). project names the CONSUMER, so the
// provider's own client contract must not turn its entry into a bypass.
func TestFirstPartyProviderOwnsAnExternalContractAdapter(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "external-authority", "a-first-party-provider-may-consume-an-external-contract")
	const source = `package server
import "net/http"
func export(endpoint string) { _, _ = http.Post(endpoint, "application/x-protobuf", nil) }
`
	root := t.TempDir()
	writeSourceIndex(t, root, "services/events")
	writeWorkspaceFile(t, root, "services/events/putnami.json", `{"name":"events"}`)
	writeWorkspaceFile(t, root, "services/events/telemetry.go", source)
	writeWorkspaceFile(t, root, "services/events/telemetry_test.go", "package server\n")
	calls := goTransportCallsites("services/events/telemetry.go", filepath.Join(root, "services/events/telemetry.go"), []byte(source))
	if len(calls) != 1 {
		t.Fatalf("telemetry transport callsites = %+v", calls)
	}
	callsite := externalIdentity(calls[0].callsite)
	writeWorkspaceFile(t, root, "services/events/"+externalInventoryFile, `{
  "protocolVersion": 2,
  "contracts": [{
    "authority": "OpenTelemetry OTLP/HTTP 1.x",
    "adapter": "services/events/telemetry.go",
    "callsites": [{"path":"services/events/telemetry.go","transport":"`+callsite.Transport+`","symbol":"`+callsite.Symbol+`","fingerprint":"`+callsite.Fingerprint+`"}],
    "owner": "observability",
    "tests": ["services/events/telemetry_test.go"],
    "reason": "OTLP export to the collector is not a Putnami provider"
  }]
}`)
	events := provider{rel: "services/events", classification: ClassificationFirstParty, document: &clientcontract.DocumentV1{
		ProtocolVersion: 1, Service: clientcontract.Service{ID: "events"}, Credentials: map[string]clientcontract.CredentialProfile{},
	}}
	contracts, findings := loadAndValidateExternalInventory(indexedView(root), []provider{events})
	if len(findings) != 0 || len(contracts) != 1 {
		t.Fatalf("provider-owned external adapter contracts=%+v findings=%+v", contracts, findings)
	}
	adaptations, findings := scanManualClientsFromWorkspace(t, root, []provider{events}, Report{ExternalContracts: contracts})
	if len(adaptations) != 0 || len(findings) != 0 {
		t.Fatalf("provider-owned external callsite was treated as first-party: adaptations=%+v findings=%+v", adaptations, findings)
	}
}

func TestExternalInventoryOwnsAnSDKAdapterWithoutSyntheticOpenAPI(t *testing.T) {
	root := t.TempDir()
	writeSourceIndex(t, root, "integrations/vendor")
	writeWorkspaceFile(t, root, "integrations/vendor/putnami.json", `{"name":"vendor-sdk-adapter"}`)
	writeWorkspaceFile(t, root, "integrations/vendor/client.ts", "export const call = () => fetch(process.env.VENDOR_URL!);\n")
	writeWorkspaceFile(t, root, "integrations/vendor/client.test.ts", "// executable vendor contract test\n")
	writeWorkspaceFile(t, root, "integrations/vendor/"+externalInventoryFile, `{
  "protocolVersion": 2,
  "contracts": [{
	    "authority": "vendor-sdk@4 contract documentation",
	    "adapter": "integrations/vendor/client.ts",
	    "callsites": [{"path":"integrations/vendor/client.ts","transport":"http","symbol":"fetch","fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}],
	    "owner": "integrations",
    "tests": ["integrations/vendor/client.test.ts"],
    "reason": "Vendor SDK has no OpenAPI document"
  }]
}`)
	contracts, findings := loadAndValidateExternalInventory(indexedView(root), nil)
	if len(findings) != 0 || len(contracts) != 1 {
		t.Fatalf("SDK-only external inventory contracts=%+v findings=%+v", contracts, findings)
	}
}

func TestExternalInventoryCannotClaimAFrameworkRuntime(t *testing.T) {
	root := t.TempDir()
	writeSourceIndex(t, root, "runtime")
	writeWorkspaceFile(t, root, "runtime/putnami.json", `{"name":"@putnami/client"}`)
	writeWorkspaceFile(t, root, "runtime/http.ts", `export const call = () => fetch(url);`)
	writeWorkspaceFile(t, root, "runtime/http.test.ts", "// runtime test\n")
	writeWorkspaceFile(t, root, "runtime/"+externalInventoryFile, `{
  "protocolVersion":2,
  "contracts":[{"authority":"claimed external","adapter":"runtime/http.ts",
    "callsites":[{"path":"runtime/http.ts","transport":"http","symbol":"fetch","fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}],
    "owner":"framework","tests":["runtime/http.test.ts"],"reason":"claimed external"}]
}`)
	_, findings := loadAndValidateExternalInventory(indexedView(root), nil)
	assertFindingCodeList(t, findings, "clientgen.framework-external-bypass")
}

func TestExternalInventoryAllowsSeveralExactAdaptersButRejectsDuplicateCallsites(t *testing.T) {
	root := t.TempDir()
	writeSourceIndex(t, root, "integrations/vendor")
	writeWorkspaceFile(t, root, "integrations/vendor/putnami.json", `{"name":"vendor"}`)
	writeWorkspaceFile(t, root, "integrations/vendor/a.ts", `export const a = () => fetch(aURL);`)
	writeWorkspaceFile(t, root, "integrations/vendor/b.ts", `export const b = () => fetch(bURL);`)
	writeWorkspaceFile(t, root, "integrations/vendor/client.test.ts", "// contract test\n")
	writeWorkspaceFile(t, root, "integrations/vendor/"+externalInventoryFile, `{
  "protocolVersion":2,
  "contracts":[
    {"authority":"vendor-a","adapter":"integrations/vendor/a.ts",
     "callsites":[{"path":"integrations/vendor/a.ts","transport":"http","symbol":"fetch","fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}],
     "owner":"integrations","tests":["integrations/vendor/client.test.ts"],"reason":"vendor a"},
    {"authority":"vendor-b","adapter":"integrations/vendor/b.ts",
     "callsites":[{"path":"integrations/vendor/b.ts","transport":"http","symbol":"fetch","fingerprint":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}],
     "owner":"integrations","tests":["integrations/vendor/client.test.ts"],"reason":"vendor b"}
  ]
}`)
	contracts, findings := loadAndValidateExternalInventory(indexedView(root), nil)
	if len(findings) != 0 || len(contracts) != 2 {
		t.Fatalf("several exact external adapters contracts=%+v findings=%+v", contracts, findings)
	}
}

func TestExternalInventoryRejectsUnknownDuplicateAndNullFields(t *testing.T) {
	root := t.TempDir()
	writeSourceIndex(t, root, "integrations/vendor")
	cases := map[string]string{
		"unknown":   `{"protocolVersion":2,"contracts":[],"strict":false}`,
		"duplicate": `{"protocolVersion":2,"protocolVersion":2,"contracts":[]}`,
		"null":      `{"protocolVersion":2,"contracts":null}`,
	}
	for name, document := range cases {
		t.Run(name, func(t *testing.T) {
			writeWorkspaceFile(t, root, "integrations/vendor/"+externalInventoryFile, document)
			_, findings := loadAndValidateExternalInventory(indexedView(root), nil)
			assertFindingCodeList(t, findings, "clientgen.invalid-external-inventory")
		})
	}
}

func writeWorkspaceFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertFindingCodeList(t *testing.T, findings []Finding, codes ...string) {
	t.Helper()
	found := map[string]bool{}
	for _, finding := range findings {
		found[finding.Code] = true
	}
	for _, code := range codes {
		if !found[code] {
			t.Errorf("missing finding %q in %s", code, formatFindingList(findings))
		}
	}
}

func formatFindingList(findings []Finding) string {
	parts := make([]string, 0, len(findings))
	for _, finding := range findings {
		parts = append(parts, finding.Code+":"+finding.Message)
	}
	return strings.Join(parts, "; ")
}

// externalEntry is a valid version 2 entry for project, whose adapter a.ts
// and test a.test.ts exist.
func externalEntry(t *testing.T, root, project, fingerprint string) string {
	t.Helper()
	writeWorkspaceFile(t, root, project+"/putnami.json", `{"name":"`+project+`"}`)
	writeWorkspaceFile(t, root, project+"/a.ts", `export const a = () => fetch(aURL);`)
	writeWorkspaceFile(t, root, project+"/a.test.ts", "// contract test\n")
	return `{"authority":"vendor","adapter":"` + project + `/a.ts",
     "callsites":[{"path":"` + project + `/a.ts","transport":"http","symbol":"fetch","fingerprint":"` + fingerprint + `"}],
     "owner":"integrations","tests":["` + project + `/a.test.ts"],"reason":"vendor wire"}`
}

// rootLayoutEntry is entry as the older workspace-root layout writes it: the
// entry names its project.
func rootLayoutEntry(project, entry string) string {
	return `{"project":"` + project + `",` + strings.TrimPrefix(entry, "{")
}

func TestEachProjectCommitsItsOwnInventories(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "external-authority", "each-project-commits-its-own-inventories")
	root := t.TempDir()
	writeSourceIndex(t, root, "integrations/a", "integrations/b")
	a := externalEntry(t, root, "integrations/a", strings.Repeat("a", 64))
	b := externalEntry(t, root, "integrations/b", strings.Repeat("b", 64))
	fileA, fileB := "integrations/a/"+externalInventoryFile, "integrations/b/"+externalInventoryFile

	// Entries spread across two project files merge, each named by its directory.
	writeWorkspaceFile(t, root, fileA, `{"protocolVersion":2,"contracts":[`+a+`]}`)
	writeWorkspaceFile(t, root, fileB, `{"protocolVersion":2,"contracts":[`+b+`]}`)
	contracts, findings := loadAndValidateExternalInventory(indexedView(root), nil)
	if len(findings) != 0 || len(contracts) != 2 ||
		contracts[0].Project != "integrations/a" || contracts[1].Project != "integrations/b" {
		t.Fatalf("two project inventories did not merge: contracts=%+v findings=%+v", contracts, findings)
	}

	// A project's file names no project: the strict decoder refuses the member.
	writeWorkspaceFile(t, root, fileB, `{"protocolVersion":2,"contracts":[`+rootLayoutEntry("integrations/b", b)+`]}`)
	contracts, findings = loadAndValidateExternalInventory(indexedView(root), nil)
	if len(contracts) != 1 || len(findings) != 1 || findings[0].Path != fileB ||
		!strings.Contains(findings[0].Message, `unknown field "project"`) {
		t.Fatalf("a version 2 entry naming its project was accepted: contracts=%+v findings=%+v", contracts, findings)
	}

	// The older root layout in a project directory fails closed.
	writeWorkspaceFile(t, root, fileB, `{"protocolVersion":1,"contracts":[`+rootLayoutEntry("integrations/b", b)+`]}`)
	contracts, findings = loadAndValidateExternalInventory(indexedView(root), nil)
	if len(contracts) != 1 || len(findings) != 1 || findings[0].Path != fileB ||
		!strings.Contains(findings[0].Message, "remove project from every entry") {
		t.Fatalf("a version 1 document in a project directory was accepted: contracts=%+v findings=%+v", contracts, findings)
	}

	// The older root layout at the root reads no entry and lists where each goes.
	writeWorkspaceFile(t, root, fileA, `{"protocolVersion":2,"contracts":[]}`)
	writeWorkspaceFile(t, root, fileB, `{"protocolVersion":2,"contracts":[]}`)
	writeWorkspaceFile(t, root, externalInventoryFile, `{"protocolVersion":1,"contracts":[`+
		rootLayoutEntry("integrations/a", a)+`,`+rootLayoutEntry("integrations/b", b)+`]}`)
	contracts, findings = loadAndValidateExternalInventory(indexedView(root), nil)
	want := "move each entry into the clientgen.external.json of the project it names (" +
		"integrations/a/clientgen.external.json, integrations/b/clientgen.external.json) as a protocolVersion 2 file"
	if len(contracts) != 0 || len(findings) != 1 || findings[0].Path != externalInventoryFile ||
		findings[0].Code != "clientgen.invalid-external-inventory" || !strings.Contains(findings[0].Message, want) {
		t.Fatalf("an older workspace-root inventory was not refused: contracts=%+v findings=%+v", contracts, findings)
	}

	// A version 2 file at a root that is not a project belongs to no project.
	writeWorkspaceFile(t, root, externalInventoryFile, `{"protocolVersion":2,"contracts":[`+a+`]}`)
	contracts, findings = loadAndValidateExternalInventory(indexedView(root), nil)
	if len(contracts) != 0 || len(findings) != 1 || findings[0].Path != externalInventoryFile ||
		!strings.Contains(findings[0].Message, "the workspace root is not a Putnami project") {
		t.Fatalf("a version 2 root file outside a root project was read: contracts=%+v findings=%+v", contracts, findings)
	}
}

func TestAWorkspaceRootProjectOwnsTheRootInventory(t *testing.T) {
	root := t.TempDir()
	writeSourceIndex(t, root, ".")
	writeWorkspaceFile(t, root, "putnami.json", `{"name":"root"}`)
	writeWorkspaceFile(t, root, "a.ts", `export const a = () => fetch(aURL);`)
	writeWorkspaceFile(t, root, "a.test.ts", "// contract test\n")
	entry := `{"authority":"vendor","adapter":"a.ts",
     "callsites":[{"path":"a.ts","transport":"http","symbol":"fetch","fingerprint":"` + strings.Repeat("a", 64) + `"}],
     "owner":"integrations","tests":["a.test.ts"],"reason":"vendor wire"}`
	writeWorkspaceFile(t, root, externalInventoryFile, `{"protocolVersion":2,"contracts":[`+entry+`]}`)
	contracts, findings := loadAndValidateExternalInventory(indexedView(root), nil)
	if len(findings) != 0 || len(contracts) != 1 || contracts[0].Project != "." {
		t.Fatalf("the root project's inventory was refused: contracts=%+v findings=%+v", contracts, findings)
	}

	// The root project's file is still a project file: the older layout fails there.
	writeWorkspaceFile(t, root, externalInventoryFile, `{"protocolVersion":1,"contracts":[`+rootLayoutEntry(".", entry)+`]}`)
	contracts, findings = loadAndValidateExternalInventory(indexedView(root), nil)
	if len(contracts) != 0 || len(findings) != 1 || !strings.Contains(findings[0].Message, "older workspace-root layout") {
		t.Fatalf("an older root layout was read in a root project: contracts=%+v findings=%+v", contracts, findings)
	}
}
