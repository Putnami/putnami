package openapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"go.putnami.dev/api"
	"go.putnami.dev/app"
	phttp "go.putnami.dev/http"
	clientcontract "go.putnami.dev/protocol/clientcontract"
	"go.putnami.dev/protocol/features/spectest"
)

const ociDistribution = "OCI Distribution Specification v1.1"

// RegistryCapabilities is what the Putnami route of the registry answers.
type RegistryCapabilities struct {
	Copy   bool `json:"copy"`
	Revert bool `json:"revert"`
}

// RegistryManifestRef names one manifest the OCI leg serves.
type RegistryManifestRef struct {
	Name      string `json:"name"`
	Reference string `json:"reference"`
}

const ociManifest = `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json"}`

// registryProvider is the repro of the issue: one api.Plugin serves an OCI
// Distribution Specification leg and a Putnami route, and publishes a
// first-party contract for the Putnami route alone.
func registryProvider() (*phttp.ServerPlugin, *api.Plugin) {
	httpServer := phttp.NewServerPlugin(phttp.ServerConfig{})
	apiPlugin := api.New(httpServer, api.WithClientService(api.ClientServiceOptions{
		Service: clientcontract.Service{ID: "oci-server", Audience: "oci-server"},
		Credentials: map[string]clientcontract.CredentialProfile{
			"user": {Kind: clientcontract.CredentialForwardedUserToken},
		},
	}))
	apiPlugin.Register(api.Endpoint("GET", "/v2/{name}/manifests/{reference}").
		Params(api.Type[RegistryManifestRef]()).
		Client(api.ClientOperationOptions{External: ociDistribution}).
		HandleRaw(func(_ *phttp.Context) *phttp.Response {
			return phttp.Bytes(http.StatusOK, "application/vnd.oci.image.manifest.v1+json", []byte(ociManifest))
		}))
	apiPlugin.Register(api.Endpoint("GET", "/v2/_putnami/capabilities").
		Returns(api.Type[RegistryCapabilities]()).
		HandleRaw(func(_ *phttp.Context) *phttp.Response {
			return phttp.JSON(RegistryCapabilities{Copy: true, Revert: false})
		}))
	return httpServer, apiPlugin
}

// The emitted document keeps both paths. The OCI leg carries its authority and
// no x-putnami-client, and is documented the way a provider without a contract
// documents a route; the Putnami route carries its operation metadata, and the
// provider's own strict validation accepts the document.
func TestOpenAPI_ExternalRouteIsPublishedWithItsAuthorityAndNoClientMetadata(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "external-contract-operations",
		"an-external-route-is-published-with-its-authority-and-no-client-metadata")

	_, apiPlugin := registryProvider()
	openapiPlugin := NewPlugin(PluginOptions{Title: "Registry", Version: "1.0.0"}).From(apiPlugin)
	if err := apiPlugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("api Configure: %v", err)
	}
	if err := openapiPlugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("openapi Configure: %v", err)
	}
	body, err := openapiPlugin.OpenAPISpecJSON()
	if err != nil {
		t.Fatalf("OpenAPISpecJSON: %v", err)
	}
	var document map[string]any
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatal(err)
	}
	paths := document["paths"].(map[string]any)
	external, ok := paths["/v2/{name}/manifests/{reference}"].(map[string]any)["get"].(map[string]any)
	if !ok {
		t.Fatalf("the external route left the document: %s", body)
	}
	if external[clientcontract.ExternalContractKey] != ociDistribution {
		t.Fatalf("external operation marker = %v, want %q", external[clientcontract.ExternalContractKey], ociDistribution)
	}
	if _, carries := external[clientcontract.ExtensionKey]; carries {
		t.Fatal("the external operation carries x-putnami-client")
	}
	if len(external["parameters"].([]any)) != 2 {
		t.Fatalf("the external operation lost its path parameters: %v", external["parameters"])
	}
	// The standard error body, not the first-party envelope: the route is
	// served by the standard pipeline, which answers that body.
	errorSchema := external["responses"].(map[string]any)["400"].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)["schema"].(map[string]any)
	if _, closed := errorSchema["additionalProperties"]; closed {
		t.Fatalf("the external operation documents the first-party error envelope: %v", errorSchema)
	}
	if _, declared := errorSchema["properties"].(map[string]any)["details"]; !declared {
		t.Fatalf("the external operation's error body lost details: %v", errorSchema)
	}

	firstParty := paths["/v2/_putnami/capabilities"].(map[string]any)["get"].(map[string]any)
	if _, carries := firstParty[clientcontract.ExtensionKey]; !carries {
		t.Fatal("the first-party operation lost x-putnami-client")
	}
	if _, carries := firstParty[clientcontract.ExternalContractKey]; carries {
		t.Fatal("the first-party operation carries the external marker")
	}
	if _, carries := document[clientcontract.ExtensionKey]; !carries {
		t.Fatal("the document lost its first-party marker")
	}

	ir, err := api.ReadOpenAPISpec(body)
	if err != nil {
		t.Fatalf("the strict reader refused the provider document: %v", err)
	}
	methods := 0
	for _, service := range ir.Services {
		for _, method := range service.Methods {
			methods++
			if method.Path != "/v2/_putnami/capabilities" {
				t.Fatalf("the reader produced a method for %s", method.Path)
			}
		}
	}
	if methods != 1 {
		t.Fatalf("the reader produced %d methods, want the first-party one", methods)
	}
}

// GenerateSpec refuses an External declaration that contradicts itself or the
// document, for routes that never went through the api plugin's Configure.
func TestGenerateSpec_RefusesAContradictoryExternalDeclaration(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "external-contract-operations",
		"a-contradictory-external-route-handed-to-the-generator-fails-generation")

	contract := &clientcontract.DocumentV1{
		ProtocolVersion: clientcontract.ProtocolVersion,
		Service:         clientcontract.Service{ID: "oci-server", Audience: "oci-server"},
		Credentials:     map[string]clientcontract.CredentialProfile{},
	}
	cases := []struct {
		name     string
		contract *clientcontract.DocumentV1
		options  api.ClientOperationOptions
		want     string
	}{
		{"blank authority", contract, api.ClientOperationOptions{External: "   "}, "blank authority"},
		{"combined with idempotency", contract, api.ClientOperationOptions{External: ociDistribution,
			Idempotency: &clientcontract.Idempotency{Kind: clientcontract.IdempotencySafe}}, "together with Idempotency"},
		{"no first-party contract", nil, api.ClientOperationOptions{External: ociDistribution},
			"publishes no first-party client contract"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			options := tc.options
			spec := GenerateSpec([]DiscoveredRoute{{
				Method: "GET", Path: "/v2/{name}/manifests/{reference}", ClientOptions: &options,
			}}, Options{Title: "Registry", Version: "1", ClientContract: tc.contract})
			_, err := spec.JSON()
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "GET /v2/{name}/manifests/{reference}") {
				t.Fatalf("JSON() error = %v, want the route and %q", err, tc.want)
			}
		})
	}
}

const registryConsumer = `package registryclient

import (
	"context"
	"os"
	"testing"

	"go.putnami.dev/client"
)

func TestTheFirstPartyRouteIsCallable(t *testing.T) {
	transport, err := client.NewServiceClientBinding(client.ServiceBinding{
		URL: os.Getenv("REGISTRY_PROVIDER_URL"), ClientID: "consumer", AllowInsecure: true,
	}, serviceDescriptor)
	if err != nil {
		t.Fatal(err)
	}
	registry := NewRegistryClient(transport)
	capabilities, err := registry.ListV2PutnamiCapabilities(context.Background(), ListV2PutnamiCapabilitiesInput{})
	if err != nil {
		t.Fatal(err)
	}
	if capabilities.Copy == nil || !*capabilities.Copy || capabilities.Revert == nil || *capabilities.Revert {
		t.Fatalf("capabilities = %+v", capabilities)
	}
}
`

// Provider declaration → generation → compiled client → real bound call: the
// in-app describer generates the Go client from the document the provider
// publishes, the emitted package compiles against this repository's client
// runtime and calls the Putnami route over a real socket, and nothing in it
// reaches the OCI leg, which the provider still serves.
func TestTheEmittedGoClientExposesOnlyTheFirstPartyRoutesThroughTheRealRuntime(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "external-contract-operations",
		"the-emitted-go-client-exposes-only-the-first-party-routes-through-the-real-runtime")

	httpServer, apiPlugin := registryProvider()
	openapiPlugin := NewPlugin(PluginOptions{Title: "Registry", Version: "1.0.0"}).From(apiPlugin)
	clientsPlugin := api.Clients(api.ClientsOptions{
		Go: api.GoClientOptions{PackageName: "registryclient", ClientName: "RegistryClient"},
	}).From(apiPlugin)
	application := app.New("registry")
	application.Use(apiPlugin).Use(openapiPlugin).Use(clientsPlugin)
	out := t.TempDir()
	if err := application.Describe(out, []string{"openapi", "clients"}); err != nil {
		t.Fatalf("Describe: %v", err)
	}
	source, err := os.ReadFile(filepath.Join(out, api.ClientStageDir, "go", "client.gen.go"))
	if err != nil {
		t.Fatalf("read generated client: %v", err)
	}
	methods := regexp.MustCompile(`func \(c \*RegistryClient\) (\w+)\(`).FindAllStringSubmatch(string(source), -1)
	if len(methods) != 1 || methods[0][1] != "ListV2PutnamiCapabilities" {
		t.Fatalf("generated client methods = %v, want only ListV2PutnamiCapabilities", methods)
	}
	for _, leaked := range []string{"Manifests", "manifests", ociDistribution} {
		if strings.Contains(string(source), leaked) {
			t.Fatalf("the generated client names the external route (%q):\n%s", leaked, source)
		}
	}

	provider := httptest.NewServer(httpServer.Handler())
	defer provider.Close()

	// The provider still serves the OCI leg, in the standard's own media type.
	response, err := http.Get(provider.URL + "/v2/library/manifests/latest")
	if err != nil {
		t.Fatal(err)
	}
	manifest, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || string(manifest) != ociManifest {
		t.Fatalf("the external route answered %d %s", response.StatusCode, manifest)
	}

	moduleDir := t.TempDir()
	writeGeneratedClientModule(t, moduleDir, string(source))
	if err := os.WriteFile(filepath.Join(moduleDir, "consumer_test.go"), []byte(registryConsumer), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("go", "test", "-count=1", "./...")
	command.Dir = moduleDir
	// GOPROXY=off: the module resolves from this checkout and the local cache
	// alone, so whether the emitted client works never depends on a fetch.
	command.Env = append(os.Environ(), "GOPROXY=off", "GOFLAGS=-mod=readonly", "GOWORK=off",
		"REGISTRY_PROVIDER_URL="+provider.URL)
	if output, testErr := command.CombinedOutput(); testErr != nil {
		t.Fatalf("the emitted client failed against the real provider: %v\n%s\n--- source:\n%s", testErr, output, source)
	}
}
