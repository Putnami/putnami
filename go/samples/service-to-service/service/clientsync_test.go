package service

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"

	"go.putnami.dev/api"
	"go.putnami.dev/openapi"
)

// matrixFeature is the feature this sample owns in putnami.features.json. The
// tests below bind the checks it declares, so a result becomes evidence through
// the verification wire rather than through this file.
const matrixFeature = "samples/go-first-party-client-matrix"

// describedContract runs the provider's describe step, the one `putnami build`
// runs, and returns the OpenAPI contract it writes. The client project builds
// its client from that contract once the provider commits it.
func describedContract(t *testing.T) []byte {
	t.Helper()
	out := t.TempDir()
	if err := NewApp().Describe(out, []string{"openapi", "clients"}); err != nil {
		t.Fatalf("describe: %v", err)
	}
	contract, err := os.ReadFile(filepath.Join(out, openapi.DefaultDescribePath)) //nolint:gosec // G304: test-owned temporary directory
	if err != nil {
		t.Fatalf("read described contract: %v", err)
	}
	return contract
}

// emitClient runs the emitter putnami-client-generate-go runs, with this
// sample's package and client names, and returns the files it wrote.
func emitClient(t *testing.T, contract []byte) map[string][]byte {
	t.Helper()
	out := t.TempDir()
	if _, err := api.EmitGoClient(contract, out, api.EmitGoClientOptions{PackageName: ClientPackage, ClientName: ClientName}); err != nil {
		t.Fatalf("emit client: %v", err)
	}
	entries, err := os.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{}
	for _, entry := range entries {
		body, err := os.ReadFile(filepath.Join(out, entry.Name())) //nolint:gosec // G304: test-owned temporary directory
		if err != nil {
			t.Fatal(err)
		}
		files[entry.Name()] = body
	}
	return files
}

// The emitter renders the client from the contract the provider describes:
// the same bytes on every run, one file, and each operation attributed to the
// project and feature the contract names.
func TestTheClientIsWhatTheEmitterRendersFromTheDescribedContract(t *testing.T) {
	spectest.Proves(t, matrixFeature, "generation-is-deterministic-and-a-break-is-detected", "the-client-is-what-the-emitter-renders-from-the-described-contract")
	contract := describedContract(t)

	spec, err := api.ReadOpenAPISpec(contract)
	if err != nil {
		t.Fatalf("read contract: %v", err)
	}
	operations := 0
	for _, service := range spec.Services {
		for _, method := range service.Methods {
			operations++
			if method.Client == nil {
				t.Errorf("%s %s carries no x-putnami-client", method.HTTPMethod, method.Path)
				continue
			}
			producer := method.Client.Producer
			if producer == nil || producer.Project != ProjectName || producer.Feature != FeatureID {
				t.Errorf("%s %s producer = %+v, want %s / %s", method.HTTPMethod, method.Path, producer, ProjectName, FeatureID)
			}
		}
	}
	if operations == 0 {
		t.Fatal("the described contract declares no operation")
	}

	first := emitClient(t, contract)
	second := emitClient(t, describedContract(t))
	if len(first) != 1 || first[api.GoClientFile] == nil {
		t.Fatalf("the emitter wrote %d file(s), want only %s", len(first), api.GoClientFile)
	}
	if !bytes.Equal(first[api.GoClientFile], second[api.GoClientFile]) {
		t.Fatal("two describes and emissions of the same provider rendered different clients")
	}
	source := string(first[api.GoClientFile])
	const watchAttribution = `{OperationID: "getItems_Id_Watch", Method: "GET", Path: "/items/{id}/watch", ProducerProject: "` + ProjectName + `", ProducerFeature: "` + FeatureID + `"}`
	if !strings.Contains(source, watchAttribution) {
		t.Errorf("the client does not attribute the SSE operation from the contract; want %s", watchAttribution)
	}
}

// TestARemovedOperationIsDetected renders the contract without one operation. A
// removed operation is the contract break a consumer must not discover at run
// time: the render moves the bytes and the method is gone, so the consumer that
// still calls it stops compiling.
func TestARemovedOperationIsDetected(t *testing.T) {
	spectest.Proves(t, matrixFeature, "generation-is-deterministic-and-a-break-is-detected", "a-removed-operation-is-detected-rather-than-rendered-through")
	contract := describedContract(t)
	rendered := string(emitClient(t, contract)[api.GoClientFile])

	ir, err := api.ReadOpenAPISpec(contract)
	if err != nil {
		t.Fatalf("read contract: %v", err)
	}
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
		Design:      &api.ClientDesignOptions{},
	})
	if err != nil {
		t.Fatalf("generate client from the amputated contract: %v", err)
	}
	if broken == rendered {
		t.Fatal("removing a declared operation rendered the same client")
	}
	// The method itself, not the embedded descriptor: a client generated from a
	// contract without the operation exposes no way to call it.
	const whoamiMethod = ") ListWhoami(ctx context.Context"
	if strings.Contains(broken, whoamiMethod) {
		t.Fatal("the render kept a method for an operation the contract no longer declares")
	}
	if !strings.Contains(rendered, whoamiMethod) {
		t.Fatal("the described contract no longer declares the operation this check removes")
	}
}
