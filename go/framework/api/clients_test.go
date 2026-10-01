package api

import (
	"go.putnami.dev/protocol/features/spectest"

	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.putnami.dev/app"
	clientcontract "go.putnami.dev/protocol/clientcontract"
	protofeatures "go.putnami.dev/protocol/features"
)

// fakeSpecSource is a minimal SpecSource registered in the module so the client
// describer can discover it via app.Collect — standing in for the openapi plugin.
type fakeSpecSource struct {
	name string
	spec []byte
	err  error
}

func (f *fakeSpecSource) Name() string                     { return f.name }
func (f *fakeSpecSource) OpenAPISpecJSON() ([]byte, error) { return f.spec, f.err }

func readSharedSpec(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(sharedOpenAPIDir(), name+".openapi.json"))
	if err != nil {
		t.Skipf("shared fixtures not available (%v)", err)
	}
	return raw
}

func TestClients_EmitsConfigAndClient(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "generated-from-contract", "the-provider-document-is-read-in-memory-from-the-owning-plugin")
	tmp := t.TempDir()
	spec := readSharedSpec(t, "ref-named-types")

	fake := &fakeSpecSource{name: "openapi", spec: spec}
	module := app.New("svc").Use(fake)

	clients := Clients(ClientsOptions{
		ThirdParty: true,
		Go:         GoClientOptions{ModulePath: "example.com/svc/clients/go"},
	})
	if err := clients.Configure(context.Background(), module); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	ctx := &app.DescribeContext{OutputDir: tmp, Targets: []string{"all"}}
	if err := clients.Describe(ctx); err != nil {
		t.Fatalf("Describe: %v", err)
	}

	// Contract.
	configBody, err := os.ReadFile(filepath.Join(tmp, "clientgen", "config.json"))
	if err != nil {
		t.Fatalf("read config.json: %v", err)
	}
	var cfg clientGenConfig
	if err := json.Unmarshal(configBody, &cfg); err != nil {
		t.Fatalf("parse config.json: %v", err)
	}
	if len(cfg.Targets) != 1 || cfg.Targets[0] != "go" {
		t.Errorf("targets = %v, want [go]", cfg.Targets)
	}
	if !cfg.ThirdParty {
		t.Error("thirdParty = false, want explicit external-contract opt-in")
	}
	if cfg.Go.Output != "clients/go" || cfg.Go.ModulePath != "example.com/svc/clients/go" {
		t.Errorf("go config = %+v", cfg.Go)
	}
	if cfg.Go.PackageName != "client" || cfg.Go.ClientName != "Client" {
		t.Errorf("go naming defaults = %+v", cfg.Go)
	}

	// Generated client.
	clientBody, err := os.ReadFile(filepath.Join(tmp, "clientgen", "go", "client.gen.go"))
	if err != nil {
		t.Fatalf("read client.gen.go: %v", err)
	}
	if _, err := parser.ParseFile(token.NewFileSet(), "client.gen.go", clientBody, parser.AllErrors); err != nil {
		t.Fatalf("staged client does not parse: %v\n%s", err, clientBody)
	}
	for _, want := range []string{"package client", "type Model1 struct", "func (c *Client) GetUsers("} {
		if !strings.Contains(string(clientBody), want) {
			t.Errorf("client.gen.go missing %q", want)
		}
	}

	// go.mod (module path set).
	gomod, err := os.ReadFile(filepath.Join(tmp, "clientgen", "go", "go.mod"))
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	for _, want := range []string{"module example.com/svc/clients/go", "go.putnami.dev/client v0.0.1"} {
		if !strings.Contains(string(gomod), want) {
			t.Errorf("go.mod missing %q\n%s", want, gomod)
		}
	}

	// putnami.json (module path set): the same scaffold the cross-language
	// GenerateProjectClients emits, so the mirrored client module is a workspace
	// project whichever describer produced it.
	project, err := os.ReadFile(filepath.Join(tmp, "clientgen", "go", "putnami.json"))
	if err != nil {
		t.Fatalf("read putnami.json: %v", err)
	}
	if want := renderGeneratedGoProject(cfg.Go.ModulePath); string(project) != want {
		t.Errorf("staged putnami.json differs from the cross-language scaffold:\n%s\nwant:\n%s", project, want)
	}
	var meta struct {
		Name       string   `json:"name"`
		Extensions []string `json:"extensions"`
	}
	if err := json.Unmarshal(project, &meta); err != nil {
		t.Fatalf("parse staged putnami.json: %v", err)
	}
	if meta.Name != "example.com/svc/clients/go" || len(meta.Extensions) != 1 || meta.Extensions[0] != "@putnami/go" {
		t.Errorf("staged project metadata = %#v", meta)
	}
}

func TestClients_FirstPartyStagesSameValidatedManifestAsWorkspaceGenerator(t *testing.T) {
	tmp := t.TempDir()
	fake := &fakeSpecSource{name: "openapi", spec: []byte(strictMinimalSpec)}
	module := app.New("items-service").Use(fake)
	clients := Clients(ClientsOptions{Go: GoClientOptions{PackageName: "itemsclient", ClientName: "ItemsClient"}})
	if err := clients.Configure(context.Background(), module); err != nil {
		t.Fatal(err)
	}
	if err := clients.Describe(&app.DescribeContext{OutputDir: tmp, Targets: []string{"all"}}); err != nil {
		t.Fatal(err)
	}

	manifestBytes, err := os.ReadFile(filepath.Join(tmp, ClientStageDir, "go", clientcontract.GeneratedManifestFile))
	if err != nil {
		t.Fatalf("read staged manifest: %v", err)
	}
	manifest, diagnostics := clientcontract.ParseAndValidateGeneratedManifest(manifestBytes)
	if len(diagnostics) != 0 {
		t.Fatalf("manifest diagnostics = %v", diagnostics)
	}
	wantHash := sha256.Sum256([]byte(strictMinimalSpec))
	if manifest.ContractSHA256 != fmt.Sprintf("%x", wantHash) {
		t.Fatalf("contract hash = %s", manifest.ContractSHA256)
	}
	if manifest.Binding.ImportPath != "go.putnami.dev/api/clients/go" || len(manifest.Binding.Clients) != 1 ||
		manifest.Binding.Clients[0].BindingSymbol != "RegisterItemsClient" {
		t.Fatalf("binding = %#v", manifest.Binding)
	}
	if len(manifest.Operations) != 1 || manifest.Operations[0].OperationID != "getItem" || manifest.Operations[0].MethodSymbol != "GetItem" {
		t.Fatalf("operations = %#v", manifest.Operations)
	}

	projectRoot := t.TempDir()
	configBytes, err := os.ReadFile(filepath.Join(tmp, ClientGenConfigPath))
	if err != nil {
		t.Fatal(err)
	}
	writeProjectFile(t, projectRoot, ".gen/clientgen/config.json", string(configBytes))
	writeProjectFile(t, projectRoot, ".gen/schema/openapi.json", strictMinimalSpec)
	writeProjectFile(t, projectRoot, "go.mod", "module go.putnami.dev/api\n\ngo 1.25\n")
	if _, err := GenerateProjectClients(projectRoot); err != nil {
		t.Fatal(err)
	}
	workspaceManifest, err := os.ReadFile(filepath.Join(projectRoot, "clients", "go", clientcontract.GeneratedManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(workspaceManifest) != string(manifestBytes) {
		t.Fatalf("api.Clients and workspace generator manifests differ:\napi.Clients:\n%s\nworkspace:\n%s", manifestBytes, workspaceManifest)
	}
}

func TestClients_FirstPartyDefaultRejectsUnmarkedProviderSpec(t *testing.T) {
	tmp := t.TempDir()
	fake := &fakeSpecSource{name: "openapi", spec: readSharedSpec(t, "operations")}
	module := app.New("svc").Use(fake)
	clients := Clients(ClientsOptions{})
	if err := clients.Configure(context.Background(), module); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	err := clients.Describe(&app.DescribeContext{OutputDir: tmp, Targets: []string{"all"}})
	if err == nil || !strings.Contains(err.Error(), "requires x-putnami-client") {
		t.Fatalf("Describe error = %v, want strict first-party marker failure", err)
	}
}

func TestClients_NoModulePath_SkipsGoMod(t *testing.T) {
	tmp := t.TempDir()
	fake := &fakeSpecSource{name: "openapi", spec: readSharedSpec(t, "operations")}
	module := app.New("svc").Use(fake)

	clients := Clients(ClientsOptions{ThirdParty: true}) // no ModulePath → package mode, no go.mod
	_ = clients.Configure(context.Background(), module)
	if err := clients.Describe(&app.DescribeContext{OutputDir: tmp, Targets: []string{"all"}}); err != nil {
		t.Fatalf("Describe: %v", err)
	}

	if _, err := os.Stat(filepath.Join(tmp, "clientgen", "go", "client.gen.go")); err != nil {
		t.Errorf("expected client.gen.go: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tmp, "clientgen", "go", "go.mod")); !os.IsNotExist(err) {
		t.Errorf("expected NO go.mod in package mode, got err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(tmp, "clientgen", "go", "putnami.json")); !os.IsNotExist(err) {
		t.Errorf("expected NO putnami.json in package mode, got err=%v", err)
	}
}

func TestClients_DesignOnlyTargetDiscoversTSClientAndRelationships(t *testing.T) {
	tmp := t.TempDir()
	fake := &fakeSpecSource{name: "openapi", spec: readSharedSpec(t, "operations")}
	apiPlugin := New(&fakeServer{})
	apiPlugin.Register(Endpoint("GET", "/users").Document())
	apiPlugin.Register(Endpoint("POST", "/users").Document())
	apiPlugin.Register(Endpoint("GET", "/orders/{id}").Document())
	clients := Clients(ClientsOptions{
		Targets:    []string{"ts"},
		ThirdParty: true,
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
	data, err := os.ReadFile(filepath.Join(tmp, filepath.FromSlash(protofeatures.DesignGraphArtifact)))
	if err != nil {
		t.Fatalf("read design graph: %v", err)
	}
	graph, err := protofeatures.ParseDesignGraph(data)
	if err != nil {
		t.Fatalf("parse design graph: %v", err)
	}
	found := false
	for _, node := range graph.Nodes {
		if node.Kind == protofeatures.DesignNodeClient && node.Properties["language"] == "ts" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("TypeScript-only generated client is missing from design graph: %#v", graph.Nodes)
	}
	generatedFrom := 0
	knownNodes := make(map[string]struct{}, len(graph.Nodes))
	for _, node := range graph.Nodes {
		knownNodes[node.ID] = struct{}{}
		// Producer identity belongs to the operations, not to the client: a
		// client-wide feature would attribute every operation it holds to one.
		if node.Kind == protofeatures.DesignNodeClient && node.Properties["feature"] != "" {
			t.Errorf("generated client node claims a client-wide feature: %#v", node.Properties)
		}
	}
	for _, edge := range graph.Edges {
		if edge.Kind != protofeatures.DesignEdgeGeneratedFrom {
			continue
		}
		generatedFrom++
		if _, ok := knownNodes[edge.From]; !ok {
			t.Errorf("generatedFrom source %q is missing", edge.From)
		}
		if _, ok := knownNodes[edge.To]; !ok {
			t.Errorf("generatedFrom target %q is missing", edge.To)
		}
		if edge.Properties["operationId"] == "" {
			t.Errorf("generatedFrom edge %q has no canonical operation key", edge.To)
		}
		if edge.Properties["producerFeature"] != "items/manage" {
			t.Errorf("generatedFrom edge %q producer = %#v", edge.To, edge.Properties)
		}
	}
	if generatedFrom != 3 {
		t.Fatalf("generatedFrom edges = %d, want 3: %#v", generatedFrom, graph.Edges)
	}
	if _, err := os.Stat(filepath.Join(tmp, ClientGenConfigPath)); !os.IsNotExist(err) {
		t.Fatalf("design-only target emitted the client contract: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tmp, ClientStageDir, "go")); !os.IsNotExist(err) {
		t.Fatalf("design-only target staged a Go client: %v", err)
	}
}

// multiFeatureSpec mirrors the Cloud shape the per-operation contract exists for:
// one spec whose operations belong to two features plus one that belongs to
// none, and a canonical operation id ("getV1_Operator_Cli-usage") whose
// punctuation no language symbol normalizer can keep.
const multiFeatureSpec = `{
  "openapi": "3.0.3",
  "paths": {
    "/v1/operator/cli-usage": {"get": {"operationId": "getV1_Operator_Cli-usage", "responses": {}}},
    "/v1/billing/invoices": {"get": {"operationId": "getV1_Billing_Invoices", "responses": {}}},
    "/v1/health": {"get": {"operationId": "getV1_Health", "responses": {}}}
  }
}`

// A single generated client whose operations come from two feature modules plus
// one unattributed module must record each operation's own producer — and must
// never fall back to the client generator's own module for the rest.
func TestClients_AttributesEachOperationToItsOwningModule(t *testing.T) {
	tmp := t.TempDir()
	fake := &fakeSpecSource{name: "openapi", spec: []byte(multiFeatureSpec)}

	operatorAPI := New(&fakeServer{})
	operatorAPI.Register(Endpoint("GET", "/v1/operator/cli-usage").Document())
	billingAPI := New(&fakeServer{})
	billingAPI.Register(Endpoint("GET", "/v1/billing/invoices").Document())
	// No feature anywhere on this module's ancestry: its operation must stay
	// unattributed rather than inherit the generator's or a sibling's feature.
	coreAPI := New(&fakeServer{})
	coreAPI.Register(Endpoint("GET", "/v1/health").Document())

	clients := Clients(ClientsOptions{ThirdParty: true, Go: GoClientOptions{ClientName: "PlatformClient"}})
	application := app.New("acme-platform")
	application.Use(fake)
	application.Use(clients)
	application.Use(app.NewModule("operator").Feature(app.Feature{
		ID: "platform/operator-cli-usage", Name: "Operator CLI usage",
		Outcome: "Operators can inspect CLI usage", Owner: "platform",
	}).Use(operatorAPI))
	application.Use(app.NewModule("billing").Feature(app.Feature{
		ID: "platform/billing", Name: "Billing", Outcome: "Customers are billed", Owner: "platform",
	}).Use(billingAPI))
	application.Use(app.NewModule("core").Use(coreAPI))

	if err := application.Describe(tmp, []string{"clients"}); err != nil {
		t.Fatalf("Describe: %v", err)
	}

	configBody, err := os.ReadFile(filepath.Join(tmp, "clientgen", "config.json"))
	if err != nil {
		t.Fatalf("read config.json: %v", err)
	}
	var cfg clientGenConfig
	if err := json.Unmarshal(configBody, &cfg); err != nil {
		t.Fatalf("parse config.json: %v", err)
	}
	if cfg.Design == nil {
		t.Fatalf("contract carries no producer attribution:\n%s", configBody)
	}
	want := []ClientOperationProducer{
		{Method: "GET", Path: "/v1/billing/invoices", ProducerProject: "acme-platform", ProducerFeature: "platform/billing"},
		{
			Method: "GET", Path: "/v1/operator/cli-usage",
			ProducerProject: "acme-platform", ProducerFeature: "platform/operator-cli-usage",
		},
	}
	if !reflect.DeepEqual(cfg.Design.Operations, want) {
		t.Fatalf("attribution table = %+v, want %+v", cfg.Design.Operations, want)
	}

	clientBody, err := os.ReadFile(filepath.Join(tmp, "clientgen", "go", "client.gen.go"))
	if err != nil {
		t.Fatalf("read client.gen.go: %v", err)
	}
	source := string(clientBody)
	for _, wanted := range []string{
		`{OperationID: "getV1_Operator_Cli-usage", Method: "GET", Path: "/v1/operator/cli-usage", ProducerProject: "acme-platform", ProducerFeature: "platform/operator-cli-usage"},`,
		`{OperationID: "getV1_Billing_Invoices", Method: "GET", Path: "/v1/billing/invoices", ProducerProject: "acme-platform", ProducerFeature: "platform/billing"},`,
		`{OperationID: "getV1_Health", Method: "GET", Path: "/v1/health"},`,
		`FeatureTrace: PlatformClientDesign.Trace("getV1_Operator_Cli-usage")`,
	} {
		if !strings.Contains(source, wanted) {
			t.Errorf("generated client missing %q:\n%s", wanted, source)
		}
	}
}

// The client-generation contract is written by two independent emitters (this
// describer and the TypeScript clientGenerator plugin). A shared fixture pins
// the bytes so key order, indentation, and the trailing newline cannot drift —
// the TypeScript half lives in test/generator/config.test.ts.
func TestWriteClientGenConfig_MatchesSharedByteFixture(t *testing.T) {
	fixture := filepath.Join(sharedFixturesRel, "clientgen", "config-with-design.golden.json")
	want, err := os.ReadFile(fixture)
	if err != nil {
		t.Skipf("shared fixtures not available (%v); run inside the monorepo", err)
	}

	tmp := t.TempDir()
	cfg := clientGenConfig{
		Targets: []string{"ts", "go"},
		TS:      clientGenConfigTS{Output: "clients/ts", PackageName: "@demo/widgets-client"},
		Go: clientGenConfigGo{
			Output:      "clients/go",
			ModulePath:  "github.com/demo/widgets/clients/go",
			PackageName: "widgetsclient",
			ClientName:  "WidgetsClient",
		},
		Design: &clientGenConfigDesign{Operations: []ClientOperationProducer{
			{
				Method: "GET", Path: "/v1/billing/invoices",
				ProducerProject: "acme-platform", ProducerFeature: "platform/billing",
			},
			{
				Method: "GET", Path: "/v1/operator/cli-usage",
				ProducerProject: "acme-platform", ProducerFeature: "platform/operator-cli-usage",
			},
		}},
	}
	if err := writeClientGenConfig(tmp, cfg); err != nil {
		t.Fatalf("writeClientGenConfig: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(tmp, ClientGenConfigPath))
	if err != nil {
		t.Fatalf("read emitted config: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("emitted contract diverges from the shared fixture:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// go.omitOperations travels in the same shared contract, so the TypeScript
// clientGenerator can ask the Go emitter to leave an operation out exactly as
// api.Clients does. Both writers emit these bytes; the TypeScript half lives in
// test/generator/config.test.ts.
func TestWriteClientGenConfig_OmitOperationsMatchesSharedByteFixture(t *testing.T) {
	fixture := filepath.Join(sharedFixturesRel, "clientgen", "config-with-omitted-operations.golden.json")
	want, err := os.ReadFile(fixture)
	if err != nil {
		t.Skipf("shared fixtures not available (%v); run inside the monorepo", err)
	}
	options := ClientsOptions{
		Targets: []string{"ts", "go"},
		TS:      TSClientOptions{PackageName: "@demo/widgets-client"},
		Go: GoClientOptions{
			ModulePath:     "github.com/demo/widgets/clients/go",
			PackageName:    "widgetsclient",
			ClientName:     "WidgetsClient",
			OmitOperations: []string{"deployWidget", "listWidgets"},
		},
	}
	tmp := t.TempDir()
	if err := writeClientGenConfig(tmp, Clients(options).resolveConfig()); err != nil {
		t.Fatalf("writeClientGenConfig: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(tmp, ClientGenConfigPath))
	if err != nil {
		t.Fatalf("read emitted config: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("emitted contract diverges from the shared fixture:\ngot:\n%s\nwant:\n%s", got, want)
	}
	// An empty list is no option at all: the key is absent, so every existing
	// contract keeps its bytes.
	options.Go.OmitOperations = []string{}
	if err := writeClientGenConfig(tmp, Clients(options).resolveConfig()); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(tmp, ClientGenConfigPath)); strings.Contains(string(got), "omitOperations") {
		t.Errorf("an empty omission list reached the contract:\n%s", got)
	}
}

func TestClients_NoRoutes_WritesConfigOnly(t *testing.T) {
	tmp := t.TempDir()

	apiPlugin := New(&fakeServer{})
	if err := apiPlugin.Configure(context.Background(), nil); err != nil { // no endpoints registered
		t.Fatalf("api Configure: %v", err)
	}

	clients := Clients(ClientsOptions{}).From(apiPlugin)
	_ = clients.Configure(context.Background(), app.New("svc").Module)
	if err := clients.Describe(&app.DescribeContext{OutputDir: tmp, Targets: []string{"all"}}); err != nil {
		t.Fatalf("Describe: %v", err)
	}

	if _, err := os.Stat(filepath.Join(tmp, "clientgen", "config.json")); err != nil {
		t.Errorf("contract should still be written with no routes: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tmp, "clientgen", "go")); !os.IsNotExist(err) {
		t.Errorf("no client should be generated with zero routes, got err=%v", err)
	}
}

func TestClients_NoSpecSource_Errors(t *testing.T) {
	tmp := t.TempDir()
	// apiPlugin omitted (nil) so the route gate is skipped; module has no SpecSource.
	clients := Clients(ClientsOptions{})
	_ = clients.Configure(context.Background(), app.New("svc").Module)
	err := clients.Describe(&app.DescribeContext{OutputDir: tmp, Targets: []string{"all"}})
	if err == nil {
		t.Fatal("expected an error when no SpecSource is registered")
	}
	if !strings.Contains(err.Error(), "openapi") {
		t.Errorf("error should point at openapi registration: %v", err)
	}
}

func TestClients_NotWanted_Skips(t *testing.T) {
	tmp := t.TempDir()
	clients := Clients(ClientsOptions{})
	_ = clients.Configure(context.Background(), app.New("svc").Module)
	// Targets that don't include "clients" or "all" → describer is a no-op.
	if err := clients.Describe(&app.DescribeContext{OutputDir: tmp, Targets: []string{"openapi"}}); err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tmp, "clientgen")); !os.IsNotExist(err) {
		t.Errorf("unwanted describer should write nothing, got err=%v", err)
	}
}
