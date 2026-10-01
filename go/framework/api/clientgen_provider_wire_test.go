package api

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	clientcontract "go.putnami.dev/protocol/clientcontract"
	"go.putnami.dev/protocol/features/spectest"
)

// providerWireFixture is the shared corpus document that declares both
// provider-owned wires: a byte stream and a typed frame stream under
// putnami.events.v1.
func providerWireFixture(t *testing.T) SpecIR {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(clientContractFixtureDir, "valid", "provider-websocket.openapi.json"))
	if err != nil {
		t.Fatal(err)
	}
	spec, err := ReadOpenAPISpec(raw)
	if err != nil {
		t.Fatalf("ReadOpenAPISpec: %v", err)
	}
	return spec
}

func TestTheEmittedGoClientReturnsAByteStreamOrAFrameStream(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "provider-owned-websocket-wires",
		"the-emitted-go-client-returns-a-byte-stream-or-a-frame-stream")
	source, err := GenerateClientFromIR(providerWireFixture(t), ClientGenOptions{
		PackageName: "gatewayclient", ClientName: "GatewayClient",
	})
	if err != nil {
		t.Fatalf("GenerateClientFromIR: %v", err)
	}
	for _, want := range []string{
		"func (c *GatewayClient) ConnectDatabase(ctx context.Context, in ConnectDatabaseInput) (*client.ByteStream, error) {",
		"return client.OpenByteStream(ctx, c.transport, request, connectDatabaseOperation, decodeConnectDatabaseError)",
		"func (c *GatewayClient) SubscribeEvents(ctx context.Context, in SubscribeEventsInput) (*client.FrameStream[EventClientFrame, EventServerFrame], error) {",
		"return client.OpenFrameStream[EventClientFrame, EventServerFrame](ctx, c.transport, request, subscribeEventsOperation)",
		`query.Add("database"`,
	} {
		if !strings.Contains(source, want) {
			t.Fatalf("generated client is missing %q\n%s", want, source)
		}
	}
	// No frame vocabulary reaches the emitted code: the runtime owns the
	// socket, the provider owns the frames.
	for _, forbidden := range []string{"putnami.service.v1", "OpenBidiStream", "WebSocketInitFrameV1"} {
		if strings.Contains(source, forbidden) {
			t.Fatalf("generated client names %q\n%s", forbidden, source)
		}
	}

	moduleDir := t.TempDir()
	writeGeneratedClientModule(t, moduleDir, source)
	command := exec.Command("go", "build", "./...")
	command.Dir = moduleDir
	command.Env = append(os.Environ(), "GOPROXY=off", "GOFLAGS=-mod=readonly", "GOWORK=off")
	if output, buildErr := command.CombinedOutput(); buildErr != nil {
		t.Fatalf("generated client does not compile: %v\n%s\n--- source:\n%s", buildErr, output, source)
	}
}

func TestTheGoEmitterRefusesAProviderWireItCannotCarry(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "provider-owned-websocket-wires",
		"the-emitted-go-client-returns-a-byte-stream-or-a-frame-stream")
	spec := providerWireFixture(t)
	for i := range spec.Services {
		for j := range spec.Services[i].Methods {
			method := &spec.Services[i].Methods[j]
			if method.OperationID == "subscribeEvents" {
				method.Client.Messages = nil
			}
		}
	}
	if _, err := GenerateClientFromIR(spec, ClientGenOptions{PackageName: "gatewayclient"}); err == nil ||
		!strings.Contains(err.Error(), "subscribeEvents") {
		t.Fatalf("a typed wire without message schemas generated: %v", err)
	}
	var transport clientcontract.Transport
	for _, service := range providerWireFixture(t).Services {
		for _, method := range service.Methods {
			if method.OperationID == "connectDatabase" {
				transport = method.Client.Transports[0]
			}
		}
	}
	if !transport.ByteStream() {
		t.Fatalf("the corpus byte stream reads as %+v", transport)
	}
}
