package workspaceclient

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

func TestFrameworkInventorySchemaMatchesStrictParser(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "handwritten-client-guard", "framework-runtime-authority-is-bound-to-exact-callsite-identities")
	// Version 2 is a project's file; version 1 is the older workspace-root
	// layout, whose entries also name their project.
	for version, entry := range map[int]reflect.Type{
		1: reflect.TypeOf(FrameworkTransport{}),
		2: reflect.TypeOf(frameworkTransportV2{}),
	} {
		name := fmt.Sprintf("clientgen-framework-v%d.json", version)
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
		assertJSONFieldNames(t, schema["properties"].(map[string]any), reflect.TypeOf(frameworkInventoryV2{}))
		definitions := schema["$defs"].(map[string]any)
		assertJSONFieldNames(t, definitions["transport"].(map[string]any)["properties"].(map[string]any), entry)
		assertJSONFieldNames(t, definitions["callsite"].(map[string]any)["properties"].(map[string]any), reflect.TypeOf(ExternalCallsite{}))
	}
}

func TestFrameworkRuntimeAuthorityRequiresIndexedRuntimeIdentity(t *testing.T) {
	root := t.TempDir()
	writeSourceIndex(t, root, "runtime")
	writeTestFile(t, root, "runtime/putnami.json", `{"name":"consumer"}`)
	writeTestFile(t, root, "runtime/http.ts", `export const request = () => fetch(url);`)
	writeTestFile(t, root, "runtime/http.test.ts", "// runtime transport test\n")
	calls := tsTransportCallsites("runtime/http.ts", `export const request = () => fetch(url);`)
	writeFrameworkInventory(t, root, []FrameworkTransport{{
		Project: "runtime", Runtime: "@putnami/client", Status: "framework-runtime", Adapter: "runtime/http.ts",
		Callsites: []ExternalCallsite{externalIdentity(calls[0].callsite)}, Owner: "framework",
		Tests: []string{"runtime/http.test.ts"}, Reason: "implements generated client bindings",
	}})
	_, findings := loadAndValidateFrameworkInventory(root)
	assertFindingCodeList(t, findings, "clientgen.invalid-framework-inventory")

	writeTestFile(t, root, "runtime/putnami.json", `{"name":"@putnami/client"}`)
	transports, findings := loadAndValidateFrameworkInventory(root)
	if len(findings) != 0 || len(transports) != 1 {
		t.Fatalf("framework transports=%+v findings=%+v", transports, findings)
	}
}

func TestFrameworkCallsiteValidationUsesFrameworkDiagnostics(t *testing.T) {
	root := t.TempDir()
	writeSourceIndex(t, root, "runtime")
	writeTestFile(t, root, "runtime/putnami.json", `{"name":"@putnami/client"}`)
	writeTestFile(t, root, "runtime/http.ts", `export const request = () => fetch(url);`)
	writeTestFile(t, root, "runtime/http.test.ts", "// runtime transport test\n")
	writeFrameworkInventory(t, root, []FrameworkTransport{{
		Project: "runtime", Runtime: "@putnami/client", Status: "framework-runtime", Adapter: "runtime/http.ts",
		Callsites: []ExternalCallsite{{Path: "runtime/http.ts", Transport: "http", Symbol: "fetch", Fingerprint: "invalid"}},
		Owner:     "framework", Tests: []string{"runtime/http.test.ts"}, Reason: "implements generated client bindings",
	}})
	_, findings := loadAndValidateFrameworkInventory(root)
	if len(findings) == 0 {
		t.Fatal("invalid framework callsite was accepted")
	}
	for _, finding := range findings {
		if finding.Code != "clientgen.invalid-framework-inventory" || finding.Path != "runtime/"+frameworkInventoryFile {
			t.Fatalf("framework callsite diagnostic leaked external inventory identity: %+v", findings)
		}
	}
}

func TestFrameworkInventoryRejectsUnknownDuplicateAndNullFields(t *testing.T) {
	root := t.TempDir()
	writeSourceIndex(t, root, "runtime")
	cases := map[string]string{
		"unknown":   `{"protocolVersion":2,"transports":[],"strict":false}`,
		"duplicate": `{"protocolVersion":2,"protocolVersion":2,"transports":[]}`,
		"null":      `{"protocolVersion":2,"transports":null}`,
	}
	for name, document := range cases {
		t.Run(name, func(t *testing.T) {
			writeTestFile(t, root, "runtime/"+frameworkInventoryFile, document)
			_, findings := loadAndValidateFrameworkInventory(root)
			assertFindingCodeList(t, findings, "clientgen.invalid-framework-inventory")
		})
	}
}

func TestFrameworkFingerprintSurvivesFormattingAndRejectsSamePositionSubstitution(t *testing.T) {
	baseSource := "const url = process.env.VENDOR_URL!;\nexport const request = () => fetch( url );\n"
	formattedSource := "// inserted above\nconst url=process.env.VENDOR_URL!;\nexport const request = () => fetch(/* context */ url);\n"
	base := tsTransportCallsites("runtime/http.ts", baseSource)
	formatted := tsTransportCallsites("runtime/http.ts", formattedSource)
	if len(base) != 1 || len(formatted) != 1 || base[0].callsite.Fingerprint != formatted[0].callsite.Fingerprint {
		t.Fatalf("formatting changed normalized fingerprint: base=%+v formatted=%+v", base, formatted)
	}
	replaced := tsTransportCallsites("runtime/http.ts", "const url = process.env.FIRST_PARTY_URL!;\nexport const request = () => fetch( url );\n")
	if len(replaced) != 1 || base[0].callsite.Fingerprint == replaced[0].callsite.Fingerprint {
		t.Fatalf("separate endpoint binding substitution retained authority: base=%+v replaced=%+v", base, replaced)
	}
	// A copy of the same invocation is a new callsite: it gets its own
	// fingerprint and no entry claims it. The invocation that was already there
	// keeps its authority, which is what stops an unrelated edit from revoking
	// every entry in the file.
	copied := tsTransportCallsites("runtime/http.ts", baseSource+"export const copied = () => fetch( url );\n")
	inherited := 0
	for _, call := range copied {
		if call.callsite.Fingerprint == base[0].callsite.Fingerprint {
			inherited++
		}
	}
	if len(copied) != 2 || copied[0].callsite.Fingerprint == copied[1].callsite.Fingerprint || inherited != 1 {
		t.Fatalf("copied identical invocation inherited authority: base=%+v copied=%+v", base, copied)
	}
}

func TestGoFrameworkFingerprintSurvivesFormattingAndRejectsEndpointSubstitution(t *testing.T) {
	baseSource := []byte("package runtime\nimport \"net/http\"\nvar url = externalURL\nfunc request(){ _, _ = http.Get(url) }\n")
	formattedSource := []byte("package runtime\n\nimport \"net/http\"\n\nvar url=externalURL\n\nfunc request() {\n\t// context\n\t_, _ = http.Get( url )\n}\n")
	base := goTransportCallsites("runtime/http.go", "runtime/http.go", baseSource)
	formatted := goTransportCallsites("runtime/http.go", "runtime/http.go", formattedSource)
	if len(base) != 1 || len(formatted) != 1 || base[0].callsite.Fingerprint != formatted[0].callsite.Fingerprint {
		t.Fatalf("Go formatting changed normalized fingerprint: base=%+v formatted=%+v", base, formatted)
	}
	replacedSource := []byte("package runtime\nimport \"net/http\"\nvar url = firstPartyURL\nfunc request(){ _, _ = http.Get(url) }\n")
	replaced := goTransportCallsites("runtime/http.go", "runtime/http.go", replacedSource)
	if len(replaced) != 1 || base[0].callsite.Fingerprint == replaced[0].callsite.Fingerprint {
		t.Fatalf("Go endpoint binding substitution retained authority: base=%+v replaced=%+v", base, replaced)
	}
}

func TestFrameworkAuthorityOwnsOnlyExistingCallAndBlocksNewRuntimeCall(t *testing.T) {
	root := t.TempDir()
	writeSourceIndex(t, root, "runtime")
	writeTestFile(t, root, "runtime/putnami.json", `{"name":"@putnami/client"}`)
	source := "export const existing = () => fetch(existingURL);\nexport const added = () => fetch(addedURL);\n"
	writeTestFile(t, root, "runtime/http.ts", source)
	calls := tsTransportCallsites("runtime/http.ts", source)
	report := Report{FrameworkTransports: []FrameworkTransport{{
		Project: "runtime", Runtime: "@putnami/client", Status: "framework-runtime", Adapter: "runtime/http.ts",
		Callsites: []ExternalCallsite{externalIdentity(calls[0].callsite)}, Owner: "framework",
		Tests: []string{"runtime/http.test.ts"}, Reason: "implements generated client bindings",
	}}}
	adaptations, findings := scanManualClientsFromWorkspace(t, root, nil, report)
	if len(adaptations) != 1 || len(findings) != 1 || findings[0].Code != "clientgen.unclassified-transport" ||
		findings[0].Line != calls[1].callsite.Line {
		t.Fatalf("new framework transport escaped classification: adaptations=%+v findings=%+v", adaptations, findings)
	}
}

// writeFrameworkInventory writes each entry into the version 2 inventory file
// of the project it names.
func writeFrameworkInventory(t *testing.T, root string, transports []FrameworkTransport) {
	t.Helper()
	byProject := map[string][]frameworkTransportV2{}
	for _, transport := range transports {
		byProject[transport.Project] = append(byProject[transport.Project], frameworkTransportV2{
			Runtime: transport.Runtime, Status: transport.Status, Adapter: transport.Adapter,
			Callsites: transport.Callsites, Owner: transport.Owner, Tests: transport.Tests, Reason: transport.Reason,
			Operations: transport.Operations, PendingWork: transport.PendingWork,
		})
	}
	for project, entries := range byProject {
		data, err := json.Marshal(frameworkInventoryV2{ProtocolVersion: inventoryProtocolVersion, Transports: entries})
		if err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, root, projectInventoryPath(project, frameworkInventoryFile), string(data))
	}
}

func TestFrameworkInventoryIsReadPerProject(t *testing.T) {
	root := t.TempDir()
	projects := []string{"primitive", "web"}
	writeSourceIndex(t, root, projects...)
	entries := make([]FrameworkTransport, 0, len(projects))
	for _, project := range projects {
		source := "export const load = (url: string) => fetch(url);\n"
		writeTestFile(t, root, project+"/putnami.json", `{"name":"@putnami/`+project+`"}`)
		writeTestFile(t, root, project+"/form.ts", source)
		writeTestFile(t, root, project+"/form.test.ts", "// primitive transport test\n")
		calls := tsTransportCallsites(project+"/form.ts", source)
		entries = append(entries, FrameworkTransport{
			Project: project, Runtime: "@putnami/" + project, Status: StatusTransportPrimitive, Adapter: project + "/form.ts",
			Callsites: []ExternalCallsite{externalIdentity(calls[0].callsite)}, Owner: "@putnami/" + project,
			Tests: []string{project + "/form.test.ts"}, Reason: "the caller supplies the URL",
		})
	}
	writeFrameworkInventory(t, root, entries)
	transports, findings := loadAndValidateFrameworkInventory(root)
	if len(findings) != 0 || len(transports) != 2 || transports[0].Project != "primitive" || transports[1].Project != "web" {
		t.Fatalf("two project inventories did not merge: transports=%+v findings=%+v", transports, findings)
	}

	rootLayout, err := json.Marshal(struct {
		ProtocolVersion int                  `json:"protocolVersion"`
		Transports      []FrameworkTransport `json:"transports"`
	}{ProtocolVersion: 1, Transports: entries})
	if err != nil {
		t.Fatal(err)
	}
	webFile := "web/" + frameworkInventoryFile
	writeTestFile(t, root, webFile, strings.Replace(string(rootLayout), `"protocolVersion":1`, `"protocolVersion":2`, 1))
	transports, findings = loadAndValidateFrameworkInventory(root)
	if len(transports) != 1 || len(findings) != 1 || findings[0].Path != webFile ||
		!strings.Contains(findings[0].Message, `unknown field "project"`) {
		t.Fatalf("a version 2 entry naming its project was accepted: transports=%+v findings=%+v", transports, findings)
	}

	writeTestFile(t, root, webFile, string(rootLayout))
	transports, findings = loadAndValidateFrameworkInventory(root)
	if len(transports) != 1 || len(findings) != 1 || findings[0].Path != webFile ||
		!strings.Contains(findings[0].Message, "remove project from every entry") {
		t.Fatalf("a version 1 document in a project directory was accepted: transports=%+v findings=%+v", transports, findings)
	}

	writeTestFile(t, root, "primitive/"+frameworkInventoryFile, `{"protocolVersion":2,"transports":[]}`)
	writeTestFile(t, root, webFile, `{"protocolVersion":2,"transports":[]}`)
	writeTestFile(t, root, frameworkInventoryFile, string(rootLayout))
	transports, findings = loadAndValidateFrameworkInventory(root)
	if len(transports) != 0 || len(findings) != 1 || findings[0].Path != frameworkInventoryFile ||
		findings[0].Code != "clientgen.invalid-framework-inventory" ||
		!strings.Contains(findings[0].Message, "(primitive/clientgen.framework.json, web/clientgen.framework.json)") {
		t.Fatalf("an older workspace-root inventory was not refused: transports=%+v findings=%+v", transports, findings)
	}
}
