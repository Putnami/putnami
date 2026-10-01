package api

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/app"
	clientcontract "go.putnami.dev/protocol/clientcontract"
	protofeatures "go.putnami.dev/protocol/features"
	"go.putnami.dev/protocol/features/spectest"
)

// strictSpecWithUnsupportedOperation is a first-party contract with one
// operation the Go emitter represents (getItem) and one it cannot: listItems
// declares a typed response header, which the Go client does not carry.
const strictSpecWithUnsupportedOperation = `{"openapi":"3.0.3","info":{"title":"Items","version":"1.0.0"},"x-putnami-client":{"protocolVersion":1,"service":{"id":"items","audience":"urn:items"},"credentials":{}},"paths":{` +
	`"/items/{id}":{"get":{"operationId":"getItem","parameters":[{"name":"id","in":"path","required":true,"schema":{"type":"string"}}],"responses":{"204":{"description":"No Content"}},"x-putnami-client":{"stream":"unary","transports":[{"protocol":"rest-json","path":"/items/{id}","encoding":"json"}],"security":{"alternatives":[{"allOf":[]}]},"errors":[],"idempotency":{"kind":"safe"}}}},` +
	`"/items":{"get":{"operationId":"listItems","responses":{"200":{"description":"OK","headers":{"X-Total-Count":{"required":true,"schema":{"type":"integer","format":"int32"}}},"content":{"application/json":{"schema":{"type":"array","items":{"type":"string"}}}}}},"x-putnami-client":{"stream":"unary","transports":[{"protocol":"rest-json","path":"/items","encoding":"json"}],"security":{"alternatives":[{"allOf":[]}]},"errors":[],"idempotency":{"kind":"safe"}}}}` +
	`}}`

// describeItemsClient runs the in-app describer over the contract above and
// returns the client-generation contract and the manifest it staged.
func describeItemsClient(t *testing.T, omit []string) (config, manifest []byte, source string, err error) {
	t.Helper()
	tmp := t.TempDir()
	fake := &fakeSpecSource{name: "openapi", spec: []byte(strictSpecWithUnsupportedOperation)}
	module := app.New("items-service").Use(fake)
	clients := Clients(ClientsOptions{Go: GoClientOptions{PackageName: "itemsclient", ClientName: "ItemsClient", OmitOperations: omit}})
	if configureErr := clients.Configure(context.Background(), module); configureErr != nil {
		t.Fatal(configureErr)
	}
	if err := clients.Describe(&app.DescribeContext{OutputDir: tmp, Targets: []string{"all"}}); err != nil {
		return nil, nil, "", err
	}
	config = readTestFile(t, filepath.Join(tmp, ClientGenConfigPath))
	manifest = readTestFile(t, filepath.Join(tmp, ClientStageDir, "go", clientcontract.GeneratedManifestFile))
	source = string(readTestFile(t, filepath.Join(tmp, ClientStageDir, "go", "client.gen.go")))
	return config, manifest, source, nil
}

func readTestFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

// One operation the Go emitter cannot represent fails the whole provider by
// default. go.omitOperations leaves that operation out instead, names it in
// the client's doc comment, the build output and the ownership manifest, and
// keeps every other operation. The in-app describer and the workspace
// generator agree on every byte of the result.
func TestOmitOperationsLeavesTheNamedOperationOutOfTheGoTarget(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "multiple-success-statuses", "an-operation-go-omit-operations-names-is-left-out-and-named")

	// Default: the provider fails, and the refusal names the operation.
	if _, _, _, err := describeItemsClient(t, nil); err == nil || !strings.Contains(err.Error(), "listItems") {
		t.Fatalf("describe without the option = %v, want a refusal naming listItems", err)
	}

	config, manifestBytes, source, err := describeItemsClient(t, []string{"listItems"})
	if err != nil {
		t.Fatalf("describe with go.omitOperations: %v", err)
	}
	if !strings.Contains(string(config), `"omitOperations": [`+"\n"+`      "listItems"`) {
		t.Fatalf("the client-generation contract does not carry the option:\n%s", config)
	}
	if !strings.Contains(source, "//   - listItems (GET /items)") {
		t.Errorf("the client doc comment does not name the omitted operation:\n%s", source)
	}
	if !strings.Contains(source, "func (c *ItemsClient) GetItem(") || strings.Contains(source, "ListItems") {
		t.Errorf("the client should keep GetItem and have no ListItems:\n%s", source)
	}
	manifest, diagnostics := clientcontract.ParseAndValidateGeneratedManifest(manifestBytes)
	if len(diagnostics) != 0 {
		t.Fatalf("manifest diagnostics = %v", diagnostics)
	}
	if len(manifest.Operations) != 1 || manifest.Operations[0].OperationID != "getItem" ||
		len(manifest.OmittedOperations) != 1 || manifest.OmittedOperations[0] != "listItems" {
		t.Fatalf("manifest operations = %#v omitted = %#v", manifest.Operations, manifest.OmittedOperations)
	}

	// The workspace generator reads the same contract and writes the same bytes.
	projectRoot := t.TempDir()
	writeProjectFile(t, projectRoot, ".gen/clientgen/config.json", string(config))
	writeProjectFile(t, projectRoot, ".gen/schema/openapi.json", strictSpecWithUnsupportedOperation)
	writeProjectFile(t, projectRoot, "go.mod", "module go.putnami.dev/api\n\ngo 1.25\n")
	result, err := GenerateProjectClients(projectRoot)
	if err != nil {
		t.Fatalf("GenerateProjectClients: %v", err)
	}
	if !result.Generated || len(result.Omitted) != 1 || result.Omitted[0] != "listItems" {
		t.Fatalf("result = %#v, want the omitted operation reported", result)
	}
	if got := readProjectFile(t, projectRoot, "clients/go/"+clientcontract.GeneratedManifestFile); got != string(manifestBytes) {
		t.Fatalf("describer and workspace manifests differ:\n%s\n---\n%s", manifestBytes, got)
	}
	if got := readProjectFile(t, projectRoot, "clients/go/client.gen.go"); got != source {
		t.Fatal("describer and workspace clients differ")
	}

	// A name the contract does not declare fails instead of changing nothing.
	writeProjectFile(t, projectRoot, ".gen/clientgen/config.json", strings.Replace(string(config), `"listItems"`, `"listItem"`, 1))
	if _, err := GenerateProjectClients(projectRoot); err == nil || !strings.Contains(err.Error(), `go.omitOperations names operation "listItem", which the contract does not declare`) {
		t.Fatalf("unknown omitted operation = %v, want a refusal naming it", err)
	}
}

// The design graph records what the Go client was generated from: an
// operation go.omitOperations leaves out has no generatedFrom edge from the Go
// client, while the TypeScript client, which has it, keeps its edge.
func TestOmitOperationsLeavesNoGeneratedFromEdgeOnTheGoClient(t *testing.T) {
	tmp := t.TempDir()
	fake := &fakeSpecSource{name: "openapi", spec: readSharedSpec(t, "operations")}
	apiPlugin := New(&fakeServer{})
	apiPlugin.Register(Endpoint("GET", "/users").Document())
	apiPlugin.Register(Endpoint("POST", "/users").Document())
	apiPlugin.Register(Endpoint("GET", "/orders/{id}").Document())
	clients := Clients(ClientsOptions{
		Targets:    []string{"go", "ts"},
		ThirdParty: true,
		Go:         GoClientOptions{PackageName: "itemsclient", ClientName: "ItemsClient", OmitOperations: []string{"getOrder"}},
		TS:         TSClientOptions{PackageName: "@example/items-client"},
	}).From(apiPlugin)
	feature := app.NewModule("items").Feature(app.Feature{
		ID: "items/manage", Name: "Item management",
		Outcome: "Consumers can manage items", Owner: "samples",
	}).Use(apiPlugin).Use(fake).Use(clients)
	application := app.New("items-api")
	application.Use(feature)
	if err := application.Describe(tmp, []string{"design"}); err != nil {
		t.Fatalf("Describe: %v", err)
	}
	graph, err := protofeatures.ParseDesignGraph(readTestFile(t, filepath.Join(tmp, filepath.FromSlash(protofeatures.DesignGraphArtifact))))
	if err != nil {
		t.Fatal(err)
	}
	edges := map[string][]string{}
	for _, edge := range graph.Edges {
		if edge.Kind == protofeatures.DesignEdgeGeneratedFrom {
			language := "ts"
			if strings.HasPrefix(edge.From, "client.generated:go:") {
				language = "go"
			}
			edges[language] = append(edges[language], edge.Properties["operationId"])
		}
	}
	if len(edges["go"]) != 2 || len(edges["ts"]) != 3 {
		t.Fatalf("generatedFrom edges = %v, want 2 for go and 3 for ts", edges)
	}
	for _, operationID := range edges["go"] {
		if operationID == "getOrder" {
			t.Fatalf("the Go client claims an operation it leaves out: %v", edges["go"])
		}
	}
}

// A third-party contract has no manifest, so the generated source is the only
// committed record of an omission: the reflect-route emitter names every
// operation go.omitOperations leaves out, as the strict emitter does.
func TestOmitOperationsNamesTheOperationInAThirdPartyClient(t *testing.T) {
	opts := ClientGenOptions{PackageName: "itemsclient", ClientName: "ItemsClient"}
	full := generateFromFixture(t, "operations", opts)
	opts.OmitOperations = []string{"getOrder"}
	source := generateFromFixture(t, "operations", opts)

	if strings.Contains(full, "go.omitOperations") {
		t.Errorf("a client without omissions mentions go.omitOperations:\n%s", full)
	}
	if !strings.Contains(source, "// ItemsClient is a typed client for the discovered API endpoints.\n//\n"+
		"// The client configuration leaves these operations out of the Go target\n"+
		"// (go.omitOperations), so they have no method here:\n//\n"+
		"//   - getOrder (GET /orders/{id})\ntype ItemsClient struct") {
		t.Errorf("the client doc comment does not name the omitted operation:\n%s", source)
	}
	methods := func(src string) int { return strings.Count(src, "func (c *ItemsClient) ") }
	if methods(source) != methods(full)-1 {
		t.Errorf("methods = %d, want one fewer than the %d of the full client", methods(source), methods(full))
	}
}
