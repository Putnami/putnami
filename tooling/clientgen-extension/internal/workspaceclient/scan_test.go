package workspaceclient

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	clientcontract "go.putnami.dev/protocol/clientcontract"
)

func scanManualClientsFromWorkspace(t *testing.T, workspaceRoot string, providers []provider, report Report) ([]Adaptation, []Finding) {
	t.Helper()
	records, err := workspaceSourceRecords(indexedView(workspaceRoot), report)
	if err != nil {
		t.Fatalf("scan workspace sources: %v", err)
	}
	return scanManualClients(workspaceFiles{root: workspaceRoot}, providers, records, report)
}

func scanConsumerEdgesFromWorkspace(t *testing.T, workspaceRoot string, providers []provider, report Report) []ConsumerEdge {
	t.Helper()
	records, err := workspaceSourceRecords(indexedView(workspaceRoot), report)
	if err != nil {
		t.Fatalf("scan workspace sources: %v", err)
	}
	return scanConsumerEdges(records, providers, report)
}

// TestInspectScansWorkspaceSourcesOnce pins the cost contract of the check: one
// check reads and parses every indexed project's production sources exactly
// once. The scan is the dominant cost of `putnami validate`'s workspace guard,
// and the way it regresses is a second caller re-deriving records it was given.
// A count of the call sites is what states that in the only place it can break.
func TestInspectScansWorkspaceSourcesOnce(t *testing.T) {
	callers := map[string][]string{}
	fileSet := token.NewFileSet()
	sources, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("list package sources: %v", err)
	}
	for _, path := range sources {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, parseErr := parser.ParseFile(fileSet, path, nil, parser.SkipObjectResolution)
		if parseErr != nil {
			t.Fatalf("parse %s: %v", path, parseErr)
		}
		enclosing := ""
		ast.Inspect(file, func(node ast.Node) bool {
			if function, ok := node.(*ast.FuncDecl); ok {
				enclosing = function.Name.Name
				return true
			}
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			name, ok := call.Fun.(*ast.Ident)
			if !ok {
				return true
			}
			if name.Name == "discoverSourceRecords" || name.Name == "workspaceSourceRecords" {
				callers[name.Name] = append(callers[name.Name], path+":"+enclosing)
			}
			return true
		})
	}
	assertSingleCaller := func(callee, wantEnclosing string) {
		t.Helper()
		sites := callers[callee]
		if len(sites) != 1 || !strings.HasSuffix(sites[0], ":"+wantEnclosing) {
			t.Fatalf("%s is called from %v, want exactly one call from %s", callee, sites, wantEnclosing)
		}
	}
	// The adoption codemod is a separate command with its own run; the check
	// path reaches the scan only through workspaceSourceRecords, and inspect is
	// the only place that calls it.
	sites := callers["discoverSourceRecords"]
	if len(sites) != 2 {
		t.Fatalf("discoverSourceRecords is called from %v, want exactly the codemod and workspaceSourceRecords", sites)
	}
	assertSingleCaller("workspaceSourceRecords", "inspect")
}

// TestWorkspaceSourceRecordsReadOnlyProviderReports pins the equality the hoist
// rests on: the record set inspect computes before the two scanners run is the
// set each of them used to compute for itself. The only report field the
// generated-file exclusion reads is Providers, which is settled before either
// scanner runs, so filling in the fields that are settled between them cannot
// move the result.
func TestWorkspaceSourceRecordsReadOnlyProviderReports(t *testing.T) {
	root := t.TempDir()
	writeSourceIndex(t, root, "consumer")
	writeTestFile(t, root, "consumer/putnami.json", `{"name":"consumer"}`)
	writeTestFile(t, root, "consumer/call.go", `package consumer

import "net/http"

func call(endpoint string) { _, _ = http.Get(endpoint) }
`)
	writeTestFile(t, root, "consumer/call.ts", "export const call = (url: string) => fetch(url);\n")

	base := Report{Providers: []ProviderReport{{Project: "services/catalog"}}}
	before, err := workspaceSourceRecords(indexedView(root), base)
	if err != nil {
		t.Fatalf("scan workspace sources: %v", err)
	}
	between := base
	between.ExternalContracts = []ExternalContract{{Project: "consumer", Adapter: "consumer/call.ts"}}
	between.FrameworkTransports = []FrameworkTransport{{Project: "consumer", Adapter: "consumer/call.go"}}
	between.ConsumerEdges = []ConsumerEdge{{ConsumerProject: "consumer"}}
	between.Findings = []Finding{{Code: "clientgen.example"}}
	after, err := workspaceSourceRecords(indexedView(root), between)
	if err != nil {
		t.Fatalf("rescan workspace sources: %v", err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("record set changed with report fields the scan must not read:\nbefore=%+v\nafter=%+v", before, after)
	}
}

// TestGoImportGateAdmitsEveryDetectedTransport pins the soundness of skipping
// the full Go parse: the import block decides it, and it must admit every file
// the full scan would have reported. A transport form the gate does not admit
// is a callsite the guard stops seeing, which is worse than a slow guard.
func TestGoImportGateAdmitsEveryDetectedTransport(t *testing.T) {
	sources := map[string]string{
		"plain": `package p

import "net/http"

func call(u string) { _, _ = http.Get(u) }
`,
		"aliased": `package p

import wire "net/http"

func call(u string) { _, _ = wire.NewRequest("GET", u, nil) }
`,
		"dot-imported": `package p

import . "net/http"

func call(u string) { _, _ = Get(u) }
`,
		"default-client": `package p

import "net/http"

func call(r *http.Request) { _, _ = http.DefaultClient.Do(r) }
`,
		"composite-literal": `package p

import "net/http"

var client = http.Client{}
`,
		"putnami-client": `package p

import "go.putnami.dev/client"

func call() { _ = client.NewClient() }
`,
		"connect-suffix": `package p

import "example.dev/catalog/connect"

func call() { _ = connect.NewClient() }
`,
		"websocket-substring": `package p

import "example.dev/vendor/websocket"

func call(u string) { _, _, _ = websocket.Dial(u, "", "") }
`,
	}
	for name, source := range sources {
		t.Run(name, func(t *testing.T) {
			calls := goTransportCallsites("p/call.go", "p/call.go", []byte(source))
			if len(calls) == 0 {
				t.Fatalf("fixture %q detects no transport callsite; it cannot prove the gate", name)
			}
			if !goImportsTransport(goImportPaths("p/call.go", []byte(source))) {
				t.Fatalf("the import gate rejects %q, hiding %d callsite(s) the full scan reports", name, len(calls))
			}
		})
	}
}

// TestGoImportGateRejectsOnlySilentFiles pins the other half: a file the gate
// rejects produces no callsite, so skipping its full parse removes work whose
// answer was already no.
func TestGoImportGateRejectsOnlySilentFiles(t *testing.T) {
	sources := map[string]string{
		"no-imports": `package p

func call(u string) string { return u }
`,
		"unrelated-import": `package p

import "strings"

func call(u string) string { return strings.TrimSpace(u) }
`,
		// A transport name that is not the imported package: the full scan
		// resolves the selector through the import set and reports nothing.
		"shadowed-name": `package p

import "example.dev/http"

func call(u string) { _ = http.Get(u) }
`,
	}
	for name, source := range sources {
		t.Run(name, func(t *testing.T) {
			if goImportsTransport(goImportPaths("p/call.go", []byte(source))) {
				t.Skipf("fixture %q is admitted by the gate, so nothing is skipped", name)
			}
			if calls := goTransportCallsites("p/call.go", "p/call.go", []byte(source)); len(calls) != 0 {
				t.Fatalf("the gate rejects %q but the full scan reports %d callsite(s): %+v", name, len(calls), calls)
			}
		})
	}
}

// TestGoImportPathsOfUnparsableHeaderMatchFullParse pins that the cheap header
// read and the full parse agree on a broken file: both conclude nothing, which
// is what the scan concluded before the gate existed.
func TestGoImportPathsOfUnparsableHeaderMatchFullParse(t *testing.T) {
	source := []byte("package p\n\nimport \"net/http\n\nfunc call() {}\n")
	if paths := goImportPaths("p/broken.go", source); len(paths) != 0 {
		t.Fatalf("import paths of an unparsable header = %v, want none", paths)
	}
	if calls := goTransportCallsites("p/broken.go", "p/broken.go", source); len(calls) != 0 {
		t.Fatalf("callsites of an unparsable file = %+v, want none", calls)
	}
}

// TestBindingImportGateFindsEveryConsumer pins the soundness of skipping the
// per-binding re-read of a file: a project that registers a generated binding
// is still reported, whichever import form it uses.
func TestBindingImportGateFindsEveryConsumer(t *testing.T) {
	root := t.TempDir()
	writeSourceIndex(t, root, "go-plain", "go-aliased", "go-dot", "ts-named", "ts-aliased", "ts-namespace", "no-import")
	for _, project := range []string{"go-plain", "go-aliased", "go-dot", "ts-named", "ts-aliased", "ts-namespace", "no-import"} {
		writeTestFile(t, root, project+"/putnami.json", `{"name":"`+project+`"}`)
	}
	writeTestFile(t, root, "go-plain/main.go", `package main

import "example.dev/catalog/client"

func register(r any) { client.RegisterCatalogClient(r) }
`)
	writeTestFile(t, root, "go-aliased/main.go", `package main

import catalog "example.dev/catalog/client"

func register(r any) { catalog.RegisterCatalogClient(r) }
`)
	writeTestFile(t, root, "go-dot/main.go", `package main

import . "example.dev/catalog/client"

func register(r any) { RegisterCatalogClient(r) }
`)
	writeTestFile(t, root, "ts-named/main.ts", "import { registerCatalogClient } from \"@example/catalog-client\";\nregisterCatalogClient(registry);\n")
	writeTestFile(t, root, "ts-aliased/main.ts", "import { registerCatalogClient as bind } from \"@example/catalog-client\";\nbind(registry);\n")
	writeTestFile(t, root, "ts-namespace/main.ts", "import * as catalog from \"@example/catalog-client\";\ncatalog.registerCatalogClient(registry);\n")
	// Calling the symbol without importing the generated package is not a
	// registration of that binding, before and after the gate.
	writeTestFile(t, root, "no-import/main.ts", "registerCatalogClient(registry);\n")

	records, err := workspaceSourceRecords(indexedView(root), Report{})
	if err != nil {
		t.Fatalf("scan workspace sources: %v", err)
	}
	goConsumers := bindingConsumers(records, clientcontract.GeneratedLanguageGo, "example.dev/catalog/client", "RegisterCatalogClient")
	if !reflect.DeepEqual(goConsumers, []string{"go-aliased", "go-dot", "go-plain"}) {
		t.Fatalf("Go binding consumers = %v, want every import form", goConsumers)
	}
	tsConsumers := bindingConsumers(records, clientcontract.GeneratedLanguageTypeScript, "@example/catalog-client", "registerCatalogClient")
	if !reflect.DeepEqual(tsConsumers, []string{"ts-aliased", "ts-named", "ts-namespace"}) {
		t.Fatalf("TypeScript binding consumers = %v, want every import form and no unimported caller", tsConsumers)
	}
}

// TestSharedTypeScriptMaskMatchesFreshMask pins that reusing the scan's mask
// answers exactly what recomputing it answers. The mask is what makes a comment
// or a string literal not a callsite, so a stale or partial one would change a
// verdict rather than only a duration.
func TestSharedTypeScriptMaskMatchesFreshMask(t *testing.T) {
	source := "" +
		"import { registerCatalogClient } from \"@example/catalog-client\";\n" +
		"// fetch(\"https://commented.example\");\n" +
		"const sample = `fetch(\"https://templated.example\")`;\n" +
		"export const call = (url: string) => fetch(url);\n" +
		"registerCatalogClient(registry);\n"
	code := tsCodeMask(source)
	calls, modules := tsScanSource("consumer/call.ts", source, code)
	if !reflect.DeepEqual(calls, tsTransportCallsites("consumer/call.ts", source)) {
		t.Fatal("callsites from the shared mask differ from callsites from a fresh mask")
	}
	if !tsUsesBinding(source, code, "@example/catalog-client", "registerCatalogClient") {
		t.Fatal("the shared mask hides a binding registration")
	}
	if !tsUsesBinding(source, "", "@example/catalog-client", "registerCatalogClient") {
		t.Fatal("an absent mask must be recomputed, not treated as empty source")
	}
	if !reflect.DeepEqual(modules, []string{"@example/catalog-client"}) {
		t.Fatalf("imported modules = %v, want the generated client specifier", modules)
	}
}

// TestSourceScanFailureDoesNotInventDrift pins what a failed workspace scan
// means: nothing was observed, which is not the same fact as a workspace with
// no transport in it. Every stale-* verdict is a statement about what the scan
// saw, so classifying against an empty record set would report every inventory
// entry and every censused entry as drift — one unreadable file becoming a wall
// of findings about documents that did not change, with the one fact that
// explains them all buried in it.
func TestSourceScanFailureDoesNotInventDrift(t *testing.T) {
	root := t.TempDir()
	writeSourceIndex(t, root, "runtime")
	writeTestFile(t, root, "runtime/putnami.json", `{"name":"@putnami/client"}`)
	writeTestFile(t, root, "runtime/http.ts", `export const request = () => fetch(url);`)
	writeTestFile(t, root, "runtime/http.test.ts", "// runtime transport test\n")
	calls := tsTransportCallsites("runtime/http.ts", `export const request = () => fetch(url);`)
	writeFrameworkInventory(t, root, []FrameworkTransport{{
		Project: "runtime", Runtime: "@putnami/client", Status: "framework-runtime", Adapter: "runtime/http.ts",
		Callsites: []ExternalCallsite{externalIdentity(calls[0].callsite)}, Owner: "framework",
		Tests: []string{"runtime/http.test.ts"}, Reason: "implements generated client bindings",
	}})
	// The fixture must be clean while the scan works, or the assertion below
	// would pass on a workspace that was already reporting drift.
	if findings := Inspect(root, ModeCheck).Findings; len(findings) != 0 {
		t.Fatalf("fixture reports drift before the scan breaks: %+v", findings)
	}

	// A source path the scan must read and cannot. A dangling symlink is the
	// portable way to express it: the walk lists it as a regular source, and
	// the read behind it fails.
	if err := os.Symlink(filepath.Join(root, "runtime", "absent.go"),
		filepath.Join(root, "runtime", "broken.go")); err != nil {
		t.Skipf("this filesystem cannot express an unreadable source: %v", err)
	}

	findings := Inspect(root, ModeCheck).Findings
	codes := map[string]int{}
	for _, finding := range findings {
		codes[finding.Code]++
	}
	if codes["clientgen.source-scan"] != 1 {
		t.Fatalf("a failed scan must report itself exactly once: %+v", findings)
	}
	// The finding names the workspace as ".", never the checkout's absolute
	// directory, so a report from one machine reads the same on another.
	for _, finding := range findings {
		if finding.Code == "clientgen.source-scan" &&
			(finding.Path != "." || strings.Contains(finding.Message, root)) {
			t.Errorf("source-scan finding = %+v, want path \".\" and no absolute directory", finding)
		}
	}
	for _, invented := range []string{
		"clientgen.stale-framework-callsite",
		"clientgen.stale-external-callsite",
		"clientgen.stale-pending-transport",
		"clientgen.unclassified-transport",
	} {
		if codes[invented] != 0 {
			t.Fatalf("a failed scan invented %s: %+v", invented, findings)
		}
	}
}
