package api

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

// describedProviderProbe runs inside the throwaway module below. It is the
// whole path from a provider declaration to the generator's reader: a provider
// built from the real api and openapi plugins declares api.Clients(), describes
// its contract, and the Go emitter reads each operation's producer from the
// described bytes alone.
const describedProviderProbe = `package providerprobe

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.putnami.dev/api"
	"go.putnami.dev/app"
	phttp "go.putnami.dev/http"
	"go.putnami.dev/openapi"
	"go.putnami.dev/protocol/clientcontract"
)

type providerServer struct{}

func (providerServer) Handle(string, string, phttp.Handler) {}

func (providerServer) AddPendingInjectedHandler(*phttp.InjectedHandler) {}

type widgetParams struct {
	ID string ` + "`" + `json:"id" validate:"required"` + "`" + `
}

type widget struct {
	ID string ` + "`" + `json:"id" validate:"required"` + "`" + `
}

func TestTheDescribedContractCarriesEachOperationsProducer(t *testing.T) {
	apiPlugin := api.New(providerServer{},
		api.WithClientService(api.ClientServiceOptions{
			Service: clientcontract.Service{ID: "widgets", Audience: "https://widgets.internal"},
		}),
	)
	handle := func(_ *phttp.EndpointContext) *phttp.Response { return phttp.JSON(nil) }
	apiPlugin.Register(api.Endpoint("GET", "/widgets/{id}").
		Params(api.Type[widgetParams]()).
		Returns(api.Type[widget]()).
		Handle(handle))
	apiPlugin.Register(api.Endpoint("DELETE", "/widgets/{id}").
		Params(api.Type[widgetParams]()).
		Returns(api.Type[widget]()).
		Handle(handle))
	openapiPlugin := openapi.NewPlugin(openapi.PluginOptions{Title: "Widgets API", Version: "1.0.0"}).From(apiPlugin)
	clientsPlugin := api.Clients(api.ClientsOptions{}).From(apiPlugin)

	application := app.New("widgets-provider")
	application.Module.Feature(app.Feature{
		ID:      "widgets/catalog",
		Name:    "Widget catalog",
		Outcome: "Consumers read and remove widgets",
		Owner:   "samples",
	})
	application.Use(apiPlugin).Use(openapiPlugin).Use(clientsPlugin)

	described := t.TempDir()
	if err := application.Describe(described, []string{"openapi", "clients"}); err != nil {
		t.Fatalf("Describe: %v", err)
	}
	contract, err := os.ReadFile(filepath.Join(described, openapi.DefaultDescribePath))
	if err != nil {
		t.Fatalf("read the described contract: %v", err)
	}
	if _, err := os.Stat(filepath.Join(described, "clientgen")); err == nil {
		t.Fatalf("describe wrote %s/clientgen: the declaration names no target", described)
	}

	// The reader of the Go emitter, on the described bytes.
	spec, err := api.ReadOpenAPISpec(contract)
	if err != nil {
		t.Fatalf("read the described contract as the emitter does: %v", err)
	}
	want := &clientcontract.OperationProducer{Project: "widgets-provider", Feature: "widgets/catalog"}
	operations := 0
	for _, service := range spec.Services {
		for _, method := range service.Methods {
			operations++
			if method.Client == nil || !reflect.DeepEqual(method.Client.Producer, want) {
				t.Errorf("operation %s carries %+v, want producer %+v", method.OperationID, method.Client, want)
			}
		}
	}
	if operations != 2 {
		t.Fatalf("the contract declares %d operations, want the 2 the provider registered", operations)
	}

	// The emitter turns that attribution into the generated client's design,
	// with no input but the contract.
	output := t.TempDir()
	if _, err := api.EmitGoClient(contract, output, api.EmitGoClientOptions{PackageName: "widgetsclient", ClientName: "WidgetsClient"}); err != nil {
		t.Fatalf("emit the Go client: %v", err)
	}
	client, err := os.ReadFile(filepath.Join(output, api.GoClientFile))
	if err != nil {
		t.Fatalf("read the emitted client: %v", err)
	}
	for _, value := range []string{want.Project, want.Feature} {
		if !strings.Contains(string(client), ` + "`" + `"` + "`" + `+value+` + "`" + `"` + "`" + `) {
			t.Errorf("the emitted client does not name the producer's %q:\n%s", value, client)
		}
	}
}
`

var goModRelativePath = regexp.MustCompile(`=>\s+(\.\.?/\S+)`)

// writeProviderModule materializes a module whose dependency closure is the
// openapi plugin's own: it starts from go/framework/openapi's go.mod, rewrites
// every relative replace to this checkout's absolute path, and adds the
// requirement and the replace for the openapi module itself. The api package
// cannot import the plugin that reads its routes, so a provider that holds both
// is built beside it, the way writeGeneratedClientModule builds a client beside
// the runtime.
func writeProviderModule(t *testing.T, moduleDir string) {
	t.Helper()
	openapiDir := filepath.Join(repoRoot(t), "go", "framework", "openapi")
	body, err := os.ReadFile(filepath.Join(openapiDir, "go.mod"))
	if err != nil {
		t.Fatalf("read openapi go.mod: %v", err)
	}
	rewritten := goModModuleLine.ReplaceAllString(string(body), "module provider.example/providerprobe")
	rewritten = goModRelativePath.ReplaceAllStringFunc(rewritten, func(match string) string {
		parts := goModRelativePath.FindStringSubmatch(match)
		return "=> " + filepath.Clean(filepath.Join(openapiDir, parts[1]))
	})
	rewritten = strings.TrimRight(rewritten, "\n") +
		"\n\nrequire go.putnami.dev/openapi v0.0.0\n\nreplace go.putnami.dev/openapi => " + openapiDir + "\n"
	if err := os.WriteFile(filepath.Join(moduleDir, "go.mod"), []byte(rewritten), 0o600); err != nil {
		t.Fatalf("write module go.mod: %v", err)
	}
	sum, err := os.ReadFile(filepath.Join(openapiDir, "go.sum"))
	if err != nil {
		t.Fatalf("read openapi go.sum: %v", err)
	}
	if err := os.WriteFile(filepath.Join(moduleDir, "go.sum"), sum, 0o600); err != nil {
		t.Fatalf("write module go.sum: %v", err)
	}
}

// A Go provider that declares api.Clients() describes a contract in which each
// operation names the project and the feature that own it, and the Go emitter
// reads that attribution from the contract alone. The provider is the real one:
// the routes this package attributes reach the contract through the openapi
// plugin, compiled from this repository beside the api package.
func TestADescribedGoProviderContractCarriesEachOperationsProducer(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "contract-carries-attribution",
		"the-contract-carries-each-operations-producer-attribution")
	moduleDir := t.TempDir()
	writeProviderModule(t, moduleDir)
	if err := os.WriteFile(filepath.Join(moduleDir, "provider_test.go"), []byte(describedProviderProbe), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("go", "test", "-count=1", "./...")
	command.Dir = moduleDir
	command.Env = append(os.Environ(), "GOPROXY=off", "GOFLAGS=-mod=readonly", "GOWORK=off")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("the described provider contract does not carry each operation's producer: %v\n%s", err, output)
	}
}
