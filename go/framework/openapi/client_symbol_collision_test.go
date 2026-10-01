package openapi

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/api"
	"go.putnami.dev/app"
	phttp "go.putnami.dev/http"
	clientcontract "go.putnami.dev/protocol/clientcontract"
	"go.putnami.dev/protocol/features/spectest"
)

// SymbolCollisionWorkspace names the workspace in the path.
type SymbolCollisionWorkspace struct {
	Workspace string `json:"workspace"`
}

// SymbolCollisionRelease names one release of a workspace in the path.
type SymbolCollisionRelease struct {
	Workspace string `json:"workspace"`
	Release   string `json:"release"`
}

// SymbolCollisionRun is one deployment run.
type SymbolCollisionRun struct {
	ID string `json:"id"`
}

// SymbolCollisionRunList is every deployment run of a workspace.
type SymbolCollisionRunList struct {
	Runs []SymbolCollisionRun `json:"runs"`
}

// symbolCollisionProvider declares two routes that differ only by a trailing
// path parameter and serves both for
// real: the list answers the workspace's runs, the item answers the one run
// its path names.
func symbolCollisionProvider() (*phttp.ServerPlugin, *api.Plugin) {
	httpServer := phttp.NewServerPlugin(phttp.ServerConfig{})
	apiPlugin := api.New(httpServer, api.WithClientService(api.ClientServiceOptions{
		Service: clientcontract.Service{ID: "deploys", Audience: "https://deploys.internal"},
	}))
	apiPlugin.Register(api.Endpoint("GET", "/v1/workspaces/{workspace}/deploy").
		Params(api.Type[SymbolCollisionWorkspace]()).
		Returns(api.Type[SymbolCollisionRunList]()).
		Handle(func(ctx *phttp.EndpointContext) *phttp.Response {
			ref, err := phttp.ParamsAs[SymbolCollisionWorkspace](ctx)
			if err != nil {
				return phttp.InternalError(err.Error())
			}
			return phttp.JSON(SymbolCollisionRunList{Runs: []SymbolCollisionRun{{ID: ref.Workspace + "/first"}, {ID: ref.Workspace + "/second"}}})
		}))
	apiPlugin.Register(api.Endpoint("GET", "/v1/workspaces/{workspace}/deploy/{release}").
		Params(api.Type[SymbolCollisionRelease]()).
		Returns(api.Type[SymbolCollisionRun]()).
		Handle(func(ctx *phttp.EndpointContext) *phttp.Response {
			ref, err := phttp.ParamsAs[SymbolCollisionRelease](ctx)
			if err != nil {
				return phttp.InternalError(err.Error())
			}
			return phttp.JSON(SymbolCollisionRun{ID: ref.Workspace + "/" + ref.Release})
		}))
	return httpServer, apiPlugin
}

// symbolCollisionConsumer runs inside a throwaway module, in the generated
// package, against the provider above over a real socket. Each method must
// reach its own route: the list answers two runs, the item the one it names.
const symbolCollisionConsumer = `package deploysclient

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"go.putnami.dev/client"
)

func TestEachRouteKeepsItsOwnMethod(t *testing.T) {
	transport, err := client.NewServiceClientBinding(client.ServiceBinding{
		URL: os.Getenv("SYMBOL_COLLISION_PROVIDER_URL"), ClientID: "consumer", AllowInsecure: true,
	}, serviceDescriptor)
	if err != nil {
		t.Fatal(err)
	}
	deploys := NewDeploysClient(transport)
	ctx := context.Background()

	var list GetV1WorkspacesDeployInput
	list.Path.Workspace = "acme"
	runs, err := deploys.GetV1WorkspacesDeploy(ctx, list)
	if err != nil {
		t.Fatalf("list the runs: %v", err)
	}
	if got, _ := json.Marshal(runs); string(got) != ` + "`" + `{"runs":[{"id":"acme/first"},{"id":"acme/second"}]}` + "`" + ` {
		t.Fatalf("list the runs = %s", got)
	}

	var get GetV1WorkspacesWorkspaceDeployReleaseInput
	get.Path.Workspace = "acme"
	get.Path.Release = "r7"
	run, err := deploys.GetV1WorkspacesWorkspaceDeployRelease(ctx, get)
	if err != nil {
		t.Fatalf("get one run: %v", err)
	}
	if got, _ := json.Marshal(run); string(got) != ` + "`" + `{"id":"acme/r7"}` + "`" + ` {
		t.Fatalf("get one run = %s", got)
	}
}
`

// End to end: a real Go provider declares two routes that differ
// only by a path parameter and asks for Go and TypeScript clients. The in-app
// describer generates the Go client from the contract the provider publishes;
// the two operations get two methods, the ownership manifest names them as the
// client does, the renamed one is the exported form of the TypeScript method
// name, and the emitted package — compiled against this repository's runtime —
// calls each route over a real socket.
func TestTheEmittedGoClientCallsEachRouteThatDiffersOnlyByAPathParameter(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "stable-operation-identity",
		"the-emitted-go-client-calls-each-route-that-differs-only-by-a-path-parameter")
	httpServer, apiPlugin := symbolCollisionProvider()
	openapiPlugin := NewPlugin(PluginOptions{Title: "Deploys", Version: "1.0.0"}).From(apiPlugin)
	clientsPlugin := api.Clients(api.ClientsOptions{
		Targets: []string{"go", "ts"},
		Go:      api.GoClientOptions{PackageName: "deploysclient", ClientName: "DeploysClient"},
	}).From(apiPlugin)
	application := app.New("deploys")
	application.Use(apiPlugin).Use(openapiPlugin).Use(clientsPlugin)
	out := t.TempDir()
	if err := application.Describe(out, []string{"openapi", "clients"}); err != nil {
		t.Fatalf("Describe: %v", err)
	}

	listID := api.CanonicalOperationID("GET", "/v1/workspaces/{workspace}/deploy")
	getID := api.CanonicalOperationID("GET", "/v1/workspaces/{workspace}/deploy/{release}")
	want := map[string]string{listID: "GetV1WorkspacesDeploy", getID: "GetV1WorkspacesWorkspaceDeployRelease"}

	var config struct {
		Targets []string `json:"targets"`
	}
	configBytes, err := os.ReadFile(filepath.Join(out, api.ClientGenConfigPath))
	if err != nil {
		t.Fatalf("read clientgen config: %v", err)
	}
	if err := json.Unmarshal(configBytes, &config); err != nil || strings.Join(config.Targets, ",") != "go,ts" {
		t.Fatalf("clientgen targets = %v (%v), want go and ts", config.Targets, err)
	}

	source, err := os.ReadFile(filepath.Join(out, api.ClientStageDir, "go", "client.gen.go"))
	if err != nil {
		t.Fatalf("read generated client: %v", err)
	}
	manifestBytes, err := os.ReadFile(filepath.Join(out, api.ClientStageDir, "go", clientcontract.GeneratedManifestFile))
	if err != nil {
		t.Fatalf("read generated manifest: %v", err)
	}
	manifest, diagnostics := clientcontract.ParseAndValidateGeneratedManifest(manifestBytes)
	if len(diagnostics) > 0 {
		t.Fatalf("manifest diagnostics = %v", diagnostics)
	}
	if len(manifest.Operations) != len(want) {
		t.Fatalf("manifest operations = %+v, want %v", manifest.Operations, want)
	}
	for _, operation := range manifest.Operations {
		if operation.MethodSymbol != want[operation.OperationID] {
			t.Errorf("manifest names %s %q, want %q", operation.OperationID, operation.MethodSymbol, want[operation.OperationID])
		}
		signature := "func (c *DeploysClient) " + operation.MethodSymbol + "(ctx context.Context, in " + operation.MethodSymbol + "Input) ("
		if !strings.Contains(string(source), signature) {
			t.Errorf("the client declares no %q for %s:\n%s", signature, operation.OperationID, source)
		}
	}

	// The TypeScript emitter names its method from the same published
	// operationId; the IR carries that name. The renamed Go symbol is its
	// exported form, so the two targets of one contract agree.
	document, err := openapiPlugin.OpenAPISpecJSON()
	if err != nil {
		t.Fatalf("read the published document: %v", err)
	}
	ir, err := api.ReadOpenAPISpec(document)
	if err != nil {
		t.Fatalf("strict reader refused the published document: %v", err)
	}
	for _, service := range ir.Services {
		for _, method := range service.Methods {
			if method.OperationID == getID && strings.ToUpper(method.Name[:1])+method.Name[1:] != want[getID] {
				t.Errorf("TypeScript method %q is not the lowercase form of Go method %q", method.Name, want[getID])
			}
		}
	}

	provider := httptest.NewServer(httpServer.Handler())
	defer provider.Close()

	moduleDir := t.TempDir()
	writeGeneratedClientModule(t, moduleDir, string(source))
	if err := os.WriteFile(filepath.Join(moduleDir, "consumer_test.go"), []byte(symbolCollisionConsumer), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("go", "test", "-count=1", "./...")
	command.Dir = moduleDir
	// GOPROXY=off: the module resolves from this checkout and the local cache
	// alone, so whether the emitted client works never depends on a fetch.
	command.Env = append(os.Environ(), "GOPROXY=off", "GOFLAGS=-mod=readonly", "GOWORK=off",
		"SYMBOL_COLLISION_PROVIDER_URL="+provider.URL)
	if output, testErr := command.CombinedOutput(); testErr != nil {
		t.Fatalf("the emitted client failed against the real provider: %v\n%s\n--- source:\n%s", testErr, output, source)
	}
}
