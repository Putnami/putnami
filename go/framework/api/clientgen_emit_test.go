package api

import (
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

const clientgenFeature = "tooling/cross-language-rest-clients"

// strictItemsContract is a first-party contract with two operations. getItem
// names its producer; listItems declares a typed response header, which the
// Go client cannot carry, and names no producer.
const strictItemsContract = `{"openapi":"3.0.3","info":{"title":"Items","version":"1.0.0"},"x-putnami-client":{"protocolVersion":1,"service":{"id":"items","audience":"urn:items"},"credentials":{}},"paths":{` +
	`"/items/{id}":{"get":{"operationId":"getItem","parameters":[{"name":"id","in":"path","required":true,"schema":{"type":"string"}}],"responses":{"204":{"description":"No Content"}},"x-putnami-client":{"stream":"unary","transports":[{"protocol":"rest-json","path":"/items/{id}","encoding":"json"}],"security":{"alternatives":[{"allOf":[]}]},"errors":[],"idempotency":{"kind":"safe"},"producer":{"project":"items-service","feature":"items/read"}}}},` +
	`"/items":{"get":{"operationId":"listItems","responses":{"200":{"description":"OK","headers":{"X-Total-Count":{"required":true,"schema":{"type":"integer","format":"int32"}}},"content":{"application/json":{"schema":{"type":"array","items":{"type":"string"}}}}}},"x-putnami-client":{"stream":"unary","transports":[{"protocol":"rest-json","path":"/items","encoding":"json"}],"security":{"alternatives":[{"allOf":[]}]},"errors":[],"idempotency":{"kind":"safe"}}}}` +
	`}}`

const strictEmptyContract = `{"openapi":"3.0.3","info":{"title":"Items","version":"1.0.0"},"x-putnami-client":{"protocolVersion":1,"service":{"id":"items","audience":"urn:items"},"credentials":{}},"paths":{}}`

// unmarkedContract is a one-operation OpenAPI document with no x-putnami-client.
const unmarkedContract = `{"openapi":"3.0.3","info":{"title":"Items","version":"1.0.0"},"paths":{"/items/{id}":{"get":{"operationId":"getItem","parameters":[{"name":"id","in":"path","required":true,"schema":{"type":"string"}}],"responses":{"204":{"description":"No Content"}}}}}}`

var itemsClientOptions = EmitGoClientOptions{PackageName: "itemsclient", ClientName: "ItemsClient", OmitOperations: []string{"listItems"}}

func readTestFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

func listDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func TestEmitGoClient_WritesOnlyTheClientFile(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "no-committed-client", "the-go-emitter-writes-no-manifest")
	output := filepath.Join(t.TempDir(), "client")
	result, err := EmitGoClient([]byte(strictItemsContract), output, itemsClientOptions)
	if err != nil {
		t.Fatalf("EmitGoClient: %v", err)
	}
	if !result.Written {
		t.Fatal("Written = false, want true")
	}
	if got := listDir(t, output); !reflect.DeepEqual(got, []string{GoClientFile}) {
		t.Fatalf("output holds %v, want only %s: no manifest, go.mod or putnami.json", got, GoClientFile)
	}
}

func TestEmitGoClient_OutputIsGofmtCleanAndParses(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "generated-from-contract", "the-output-is-gofmt-clean-and-compilable")
	output := t.TempDir()
	if _, err := EmitGoClient([]byte(strictItemsContract), output, itemsClientOptions); err != nil {
		t.Fatalf("EmitGoClient: %v", err)
	}
	source := readTestFile(t, filepath.Join(output, GoClientFile))
	if _, err := parser.ParseFile(token.NewFileSet(), GoClientFile, source, parser.AllErrors); err != nil {
		t.Fatalf("emitted client does not parse as Go:\n%s\n--- error: %v", source, err)
	}
	formatted, err := format.Source(source)
	if err != nil {
		t.Fatalf("format emitted client: %v", err)
	}
	if string(formatted) != string(source) {
		t.Error("emitted client is not gofmt-clean")
	}
	if !strings.Contains(string(source), "package itemsclient") {
		t.Errorf("emitted client is not in the requested package:\n%s", source)
	}
}

func TestEmitGoClient_DeterministicAcrossRuns(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "generated-from-contract", "generating-twice-is-byte-identical")
	output := t.TempDir()
	if _, err := EmitGoClient([]byte(strictItemsContract), output, itemsClientOptions); err != nil {
		t.Fatalf("first run: %v", err)
	}
	first := readTestFile(t, filepath.Join(output, GoClientFile))
	if _, err := EmitGoClient([]byte(strictItemsContract), output, itemsClientOptions); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if second := readTestFile(t, filepath.Join(output, GoClientFile)); string(second) != string(first) {
		t.Error("client.gen.go is not byte-stable across runs")
	}
}

// One operation the Go emitter cannot represent fails the whole client by
// default. omitOperations leaves that operation out instead, names it in the
// client's doc comment and keeps every other operation.
func TestEmitGoClient_LeavesAnOmittedOperationOut(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "named-omissions", "an-omitted-operation-is-absent-from-the-generated-client")
	spectest.Proves(t, "go/api-contracts", "multiple-success-statuses", "an-operation-go-omit-operations-names-is-left-out-and-named")

	if _, err := EmitGoClient([]byte(strictItemsContract), t.TempDir(), EmitGoClientOptions{}); err == nil ||
		!strings.Contains(err.Error(), "listItems") {
		t.Fatalf("emission without the omission = %v, want a refusal naming listItems", err)
	}

	output := t.TempDir()
	result, err := EmitGoClient([]byte(strictItemsContract), output, itemsClientOptions)
	if err != nil {
		t.Fatalf("EmitGoClient: %v", err)
	}
	if !reflect.DeepEqual(result.Omitted, []string{"listItems"}) {
		t.Fatalf("Omitted = %v, want [listItems]", result.Omitted)
	}
	source := string(readTestFile(t, filepath.Join(output, GoClientFile)))
	if !strings.Contains(source, "//   - listItems (GET /items)") {
		t.Errorf("the client doc comment does not name the omitted operation:\n%s", source)
	}
	if !strings.Contains(source, "func (c *ItemsClient) GetItem(") || strings.Contains(source, "ListItems") ||
		strings.Contains(source, `"listItems"`) {
		t.Errorf("the client should keep GetItem and carry nothing of listItems:\n%s", source)
	}
}

func TestEmitGoClient_OmittingAnUndeclaredOperationFails(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "named-omissions", "omitting-an-undeclared-operation-fails-generation")
	output := t.TempDir()
	_, err := EmitGoClient([]byte(strictItemsContract), output, EmitGoClientOptions{OmitOperations: []string{"listItems", "listItem"}})
	if err == nil || !strings.Contains(err.Error(), `omitOperations names operation "listItem", which the contract does not declare`) {
		t.Fatalf("unknown omitted operation = %v, want a refusal naming it", err)
	}
	if got := listDir(t, output); len(got) != 0 {
		t.Fatalf("a failed emission wrote %v", got)
	}
}

// Each operation's producer comes from its x-putnami-client in the contract.
// An operation whose contract names none stays unattributed.
func TestEmitGoClient_AttributesEachOperationFromTheContract(t *testing.T) {
	contract := strings.Replace(strictItemsContract, `"headers":{"X-Total-Count":{"required":true,"schema":{"type":"integer","format":"int32"}}},`, "", 1)
	output := t.TempDir()
	if _, err := EmitGoClient([]byte(contract), output, EmitGoClientOptions{ClientName: "ItemsClient"}); err != nil {
		t.Fatalf("EmitGoClient: %v", err)
	}
	source := string(readTestFile(t, filepath.Join(output, GoClientFile)))
	for _, want := range []string{
		`{OperationID: "getItem", Method: "GET", Path: "/items/{id}", ProducerProject: "items-service", ProducerFeature: "items/read"},`,
		`{OperationID: "listItems", Method: "GET", Path: "/items"},`,
		`FeatureTrace: ItemsClientDesign.Trace("getItem")`,
	} {
		if !strings.Contains(source, want) {
			t.Errorf("generated client missing %q:\n%s", want, source)
		}
	}

	// A contract that names no producer embeds no design descriptor.
	unattributed := strings.Replace(contract, `,"producer":{"project":"items-service","feature":"items/read"}`, "", 1)
	if _, err := EmitGoClient([]byte(unattributed), output, EmitGoClientOptions{ClientName: "ItemsClient"}); err != nil {
		t.Fatalf("EmitGoClient: %v", err)
	}
	if source := string(readTestFile(t, filepath.Join(output, GoClientFile))); strings.Contains(source, "ItemsClientDesign") {
		t.Errorf("a client with no attributed operation embeds a design descriptor:\n%s", source)
	}
}

func TestEmitGoClient_RefusesAContractWithNoClientMetadata(t *testing.T) {
	output := t.TempDir()
	_, err := EmitGoClient([]byte(unmarkedContract), output, EmitGoClientOptions{})
	if err == nil || !strings.Contains(err.Error(), "x-putnami-client") {
		t.Fatalf("EmitGoClient error = %v, want the missing-contract refusal", err)
	}
	if got := listDir(t, output); len(got) != 0 {
		t.Fatalf("a refused emission wrote %v", got)
	}
}

func TestEmitGoClient_AContractWithNoOperationRemovesTheClient(t *testing.T) {
	output := t.TempDir()
	if _, err := EmitGoClient([]byte(strictItemsContract), output, itemsClientOptions); err != nil {
		t.Fatalf("EmitGoClient: %v", err)
	}
	result, err := EmitGoClient([]byte(strictEmptyContract), output, EmitGoClientOptions{})
	if err != nil {
		t.Fatalf("EmitGoClient over an empty contract: %v", err)
	}
	if result.Written {
		t.Fatal("Written = true for a contract with no operation")
	}
	if got := listDir(t, output); len(got) != 0 {
		t.Fatalf("output still holds %v", got)
	}
	// With nothing to remove, the emission still succeeds.
	if _, err := EmitGoClient([]byte(strictEmptyContract), filepath.Join(output, "absent"), EmitGoClientOptions{}); err != nil {
		t.Fatalf("EmitGoClient into a missing directory: %v", err)
	}
}

// repoRoot walks up from the working directory to the workspace root (the dir
// containing go.work).
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.work")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("go.work not found above the test working directory")
		}
		dir = parent
	}
}
