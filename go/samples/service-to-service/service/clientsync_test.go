package service

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"

	"go.putnami.dev/api"
	"go.putnami.dev/http"
	"go.putnami.dev/openapi"
	clientcontract "go.putnami.dev/protocol/clientcontract"
)

// matrixFeature is the feature this sample owns in putnami.features.json. The
// tests below bind the checks it declares, so a result becomes evidence through
// the verification wire rather than through this file.
const matrixFeature = "samples/go-first-party-client-matrix"

// TestGeneratedClientInSync regenerates the Go client from the provider's routes
// through the same pipeline `putnami build` uses (the api.Clients describer reads
// the OpenAPI spec and runs GenerateClientFromIR) and asserts it matches the
// committed clients/go/client.gen.go. Drift here means a build would rewrite the
// committed client. Regenerate intentional changes through the workspace
// `putnami clientgen` command so source and ownership manifest stay atomic.
func TestGeneratedClientInSync(t *testing.T) {
	spectest.Proves(t, matrixFeature, "generation-is-deterministic-and-a-break-is-detected", "the-committed-client-is-what-the-generator-renders")
	// The same run proves the other half of the requirement: the render of an
	// amputated contract below differs from the committed one.
	spectest.Proves(t, matrixFeature, "generation-is-deterministic-and-a-break-is-detected", "a-removed-operation-is-detected-rather-than-rendered-through")
	server := http.NewServerPlugin(http.ServerConfig{})
	apiPlugin := api.New(server, ClientContract())
	Register(apiPlugin)
	protoPlugin, bridge := ConnectPlugins(apiPlugin, server)
	if err := apiPlugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("configure api: %v", err)
	}
	// The same lifecycle order NewApp runs: the descriptor first, then the
	// bridge that serves it, so the rendered spec advertises Connect exactly as
	// the running provider does.
	if err := protoPlugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("configure proto: %v", err)
	}
	if err := bridge.Configure(context.Background(), nil); err != nil {
		t.Fatalf("configure connect bridge: %v", err)
	}
	openapiPlugin := openapi.NewPlugin(openapi.PluginOptions{Title: "Items API", Version: "1.0.0"}).From(apiPlugin)
	if err := openapiPlugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("configure openapi: %v", err)
	}

	spec, err := openapiPlugin.OpenAPISpecJSON()
	if err != nil {
		t.Fatalf("render spec: %v", err)
	}
	ir, err := api.ReadOpenAPISpec(spec)
	if err != nil {
		t.Fatalf("read spec: %v", err)
	}
	// Producer attribution is per operation. This provider declares one feature
	// on its root module, so every route it owns resolves to that feature — the
	// describer derives the same table from the module tree at build time.
	producers := make([]api.ClientOperationProducer, 0, len(apiPlugin.DiscoveredRoutes()))
	for _, route := range apiPlugin.DiscoveredRoutes() {
		producers = append(producers, api.ClientOperationProducer{
			Method:          route.Method,
			Path:            route.Path,
			ProducerProject: ProjectName,
			ProducerFeature: FeatureID,
		})
	}
	src, err := api.GenerateClientFromIR(ir, api.ClientGenOptions{
		PackageName: ClientPackage,
		ClientName:  ClientName,
		Design:      &api.ClientDesignOptions{Operations: producers},
	})
	if err != nil {
		t.Fatalf("generate client: %v", err)
	}

	path := filepath.Join("..", "clients", "go", "client.gen.go")
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read committed client (run `putnami clientgen --projects %s` to create): %v", ProjectName, err)
	}
	if string(want) != src {
		t.Errorf("committed clients/go/client.gen.go is out of sync with the generator;\nrun: putnami clientgen --projects %s", ProjectName)
	}

	manifestBytes, err := os.ReadFile(filepath.Join("..", "clients", "go", clientcontract.GeneratedManifestFile))
	if err != nil {
		t.Fatalf("read committed generated manifest: %v", err)
	}
	manifest, diagnostics := clientcontract.ParseAndValidateGeneratedManifest(manifestBytes)
	if len(diagnostics) != 0 {
		t.Fatalf("generated manifest diagnostics = %v", diagnostics)
	}
	contractHash := sha256.Sum256(spec)
	sourceHash := sha256.Sum256(want)
	if manifest.ContractSHA256 != fmt.Sprintf("%x", contractHash) || len(manifest.Files) != 1 ||
		manifest.Files[0].Path != "client.gen.go" || manifest.Files[0].SHA256 != fmt.Sprintf("%x", sourceHash) {
		t.Fatalf("generated manifest does not own the current provider contract/client: %#v", manifest)
	}
	// A removed operation is the contract break a consumer must not discover at
	// run time. Rendering the amputated contract moves the bytes and leaves the
	// committed manifest owning an operation the render no longer has, which is
	// the pair the workspace clientgen guard compares.
	amputated := ir
	amputated.Services = nil
	for _, service := range ir.Services {
		kept := make([]api.MethodIR, 0, len(service.Methods))
		for _, method := range service.Methods {
			if method.Path != "/whoami" {
				kept = append(kept, method)
			}
		}
		service.Methods = kept
		if len(kept) > 0 {
			amputated.Services = append(amputated.Services, service)
		}
	}
	broken, err := api.GenerateClientFromIR(amputated, api.ClientGenOptions{
		PackageName: ClientPackage,
		ClientName:  ClientName,
		Design:      &api.ClientDesignOptions{Operations: producers},
	})
	if err != nil {
		t.Fatalf("generate client from the amputated contract: %v", err)
	}
	if broken == src {
		t.Fatal("removing a declared operation rendered the same client")
	}
	// The method itself, not the embedded descriptor: a client generated from a
	// contract without the operation exposes no way to call it.
	const whoamiMethod = ") ListWhoami(ctx context.Context"
	if strings.Contains(broken, whoamiMethod) {
		t.Fatal("the render kept a method for an operation the contract no longer declares")
	}
	if !strings.Contains(src, whoamiMethod) {
		t.Fatal("the committed contract no longer declares the operation this check removes")
	}

	foundStream := false
	for _, operation := range manifest.Operations {
		for _, transport := range operation.Transports {
			foundStream = foundStream || operation.OperationID == "getItems_Id_Watch" && transport.Protocol == clientcontract.TransportSSE
		}
	}
	if !foundStream {
		t.Fatalf("generated manifest omits provider SSE operation: %#v", manifest.Operations)
	}
}
