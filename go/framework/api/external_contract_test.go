package api

import (
	"context"
	stderrors "errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	perrors "go.putnami.dev/errors"
	phttp "go.putnami.dev/http"
	clientcontract "go.putnami.dev/protocol/clientcontract"
	"go.putnami.dev/protocol/features/spectest"
)

const ociAuthority = "OCI Distribution Specification v1.1"

func registryClientService() Option {
	return WithClientService(ClientServiceOptions{
		Service: clientcontract.Service{ID: "oci-server", Audience: "oci-server"},
		Credentials: map[string]clientcontract.CredentialProfile{
			"user": {Kind: clientcontract.CredentialForwardedUserToken},
		},
	})
}

type externalManifestBody struct {
	MediaType string `json:"mediaType" validate:"required"`
}

// Every contradictory External declaration fails Configure with an actionable
// message, and it fails before anything is bound: a provider never serves half
// of an API it refused.
func TestExternalContract_ConfigureRefusesContradictionsBeforeBinding(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "external-contract-operations",
		"a-contradictory-external-declaration-fails-the-provider-before-any-route-is-bound")

	one := 1
	cases := []struct {
		name    string
		options []Option
		client  ClientOperationOptions
		want    string
	}{
		{"blank authority", []Option{registryClientService()}, ClientOperationOptions{External: " \t "}, "blank authority"},
		{"security", []Option{registryClientService()}, ClientOperationOptions{External: ociAuthority,
			Security: clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{AllOf: []clientcontract.SecurityRequirement{{Profile: "user"}}}}}},
			"together with Security"},
		{"authorization", []Option{registryClientService()}, ClientOperationOptions{External: ociAuthority,
			Security: clientcontract.Security{Authorization: &clientcontract.Authorization{}}}, "together with Security"},
		{"idempotency", []Option{registryClientService()}, ClientOperationOptions{External: ociAuthority,
			Idempotency: &clientcontract.Idempotency{Kind: clientcontract.IdempotencySafe}}, "together with Idempotency"},
		{"resilience", []Option{registryClientService()}, ClientOperationOptions{External: ociAuthority,
			Resilience: &clientcontract.ResiliencePolicy{TimeoutMs: &one}}, "together with Resilience"},
		{"transports", []Option{registryClientService()}, ClientOperationOptions{External: ociAuthority,
			Transports: []clientcontract.TransportProtocol{clientcontract.TransportRESTJSON}}, "together with Transports"},
		{"connect encodings", []Option{registryClientService()}, ClientOperationOptions{External: ociAuthority,
			ConnectEncodings: []clientcontract.Encoding{clientcontract.EncodingJSON}}, "together with ConnectEncodings"},
		{"resume", []Option{registryClientService()}, ClientOperationOptions{External: ociAuthority, Resume: true},
			"together with Resume"},
		{"every combined option is named", []Option{registryClientService()}, ClientOperationOptions{External: ociAuthority,
			Idempotency: &clientcontract.Idempotency{Kind: clientcontract.IdempotencySafe}, Resume: true},
			"together with Idempotency, Resume"},
		{"no first-party contract", nil, ClientOperationOptions{External: ociAuthority},
			"this API publishes no first-party client contract"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := &fakeServer{}
			plugin := New(server, tc.options...)
			plugin.Register(Endpoint("GET", "/v2/_putnami/capabilities").
				HandleRaw(func(_ *phttp.Context) *phttp.Response { return phttp.JSON(nil) }))
			plugin.Register(Endpoint("GET", "/v2/{name}/manifests/{reference}").
				Client(tc.client).
				HandleRaw(func(_ *phttp.Context) *phttp.Response { return phttp.JSON(nil) }))

			err := plugin.Configure(context.Background(), nil)
			if err == nil {
				t.Fatal("Configure accepted a contradictory External declaration")
			}
			if !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "GET /v2/{name}/manifests/{reference}") {
				t.Fatalf("error = %q, want the route and %q", err, tc.want)
			}
			var coded *perrors.Error
			if !stderrors.As(err, &coded) || coded.Code() != CodeClientGenConfig {
				t.Fatalf("error code = %v, want %s", err, CodeClientGenConfig)
			}
			if len(server.calls) != 0 || len(plugin.DiscoveredRoutes()) != 0 {
				t.Fatalf("a refused Configure bound %d routes and discovered %d", len(server.calls), len(plugin.DiscoveredRoutes()))
			}
		})
	}
}

// A route an external authority owns is served by the standard pipeline on a
// first-party API: its body is decoded leniently and a validation failure
// answers the standard error body, while its first-party neighbor keeps the
// strict decoding and the framework envelope. A stream keeps the raw
// transport instead of negotiating the first-party subprotocol.
func TestExternalContract_RouteIsServedByTheStandardPipeline(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "external-contract-operations",
		"an-external-route-is-served-by-the-standard-pipeline")

	server := &fakeStreamServer{}
	plugin := New(server, registryClientService())
	echo := func(_ *phttp.EndpointContext) *phttp.Response { return phttp.NoContent() }
	plugin.Register(Endpoint("PUT", "/v2/_putnami/manifests").Body(Type[externalManifestBody]()).Handle(echo))
	plugin.Register(Endpoint("PUT", "/v2/{name}/manifests/{reference}").Body(Type[externalManifestBody]()).
		Client(ClientOperationOptions{External: ociAuthority}).Handle(echo))
	type event struct {
		Digest string `json:"digest"`
	}
	stream := func(ctx *ServerStreamContext[event]) error { return ctx.Send(event{Digest: "sha256:0"}) }
	plugin.Register(Endpoint("GET", "/v2/_putnami/events").Returns(StreamOf[event]()).Handle(ServerStream(stream)))
	plugin.Register(Endpoint("GET", "/v2/{name}/events").Returns(StreamOf[event]()).
		Client(ClientOperationOptions{External: ociAuthority}).Handle(ServerStream(stream)))
	if err := plugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("Configure: %v", err)
	}

	handlers := map[string]phttp.Handler{}
	for _, call := range server.calls {
		handlers[call.path] = call.handler
	}
	call := func(path, body string) (int, string) {
		t.Helper()
		request := httptest.NewRequest("PUT", path, strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		response := handlers[path](phttp.NewContext(httptest.NewRecorder(), request))
		raw, err := response.BodyBytes()
		if err != nil {
			t.Fatal(err)
		}
		return response.Status, string(raw)
	}

	// A field the declaration does not name: the standard's clients send them.
	withExtra := `{"mediaType":"application/vnd.oci.image.manifest.v1+json","annotations":{}}`
	if status, body := call("/v2/{name}/manifests/{reference}", withExtra); status != 204 {
		t.Fatalf("external route refused an undeclared field: %d %s", status, body)
	}
	if status, _ := call("/v2/_putnami/manifests", withExtra); status != 400 {
		t.Fatalf("first-party route status = %d for an undeclared field, want 400", status)
	}

	status, body := call("/v2/{name}/manifests/{reference}", `{}`)
	if status != 400 || !strings.Contains(body, `"details"`) || strings.Contains(body, `"code"`) {
		t.Fatalf("external validation failure = %d %s, want the standard error body", status, body)
	}
	status, body = call("/v2/_putnami/manifests", `{}`)
	if status != 400 || !strings.Contains(body, `"code"`) {
		t.Fatalf("first-party validation failure = %d %s, want the framework envelope", status, body)
	}

	subprotocols := map[string]string{}
	for _, registered := range server.streams {
		subprotocols[registered.path] = registered.handler.Subprotocol
	}
	if got := subprotocols["/v2/_putnami/events"]; got != clientcontract.WebSocketSubprotocolV1 {
		t.Fatalf("first-party stream subprotocol = %q, want %q", got, clientcontract.WebSocketSubprotocolV1)
	}
	if got, ok := subprotocols["/v2/{name}/events"]; !ok || got != "" {
		t.Fatalf("external stream subprotocol = %q (bound %v), want the raw transport", got, ok)
	}
	if routes := plugin.DiscoveredRoutes(); len(routes) != 4 {
		t.Fatalf("DiscoveredRoutes = %d, want every route, the external ones included", len(routes))
	}
}

// The Go reader skips an operation an external authority owns: the shared
// fixture yields exactly its first-party method, and the contract itself.
func TestExternalContract_GoReaderSkipsTheOperationWhole(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "external-contract-operations",
		"the-go-reader-skips-an-external-operation-whole")

	raw, err := os.ReadFile(filepath.Join(clientContractFixtureDir, "valid", "external-operation.openapi.json"))
	if err != nil {
		t.Fatal(err)
	}
	spec, err := ReadOpenAPISpec(raw)
	if err != nil {
		t.Fatalf("ReadOpenAPISpec: %v", err)
	}
	if spec.Contract == nil || spec.Contract.Service.ID != "registry" {
		t.Fatalf("contract = %+v", spec.Contract)
	}
	var operations []string
	for _, service := range spec.Services {
		for _, method := range service.Methods {
			operations = append(operations, method.OperationID)
		}
	}
	if len(operations) != 1 || operations[0] != "getV2PutnamiCapabilities" {
		t.Fatalf("operations = %v, want only the first-party getV2PutnamiCapabilities", operations)
	}

	// The marker never excuses an unmarked operation: dropping it leaves an
	// operation with no x-putnami-client, which stays refused.
	unmarked := strings.Replace(string(raw), `"x-putnami-external-contract": "OCI Distribution Specification v1.1"`, `"deprecated": false`, 1)
	if _, err := ReadOpenAPISpec([]byte(unmarked)); err == nil || !strings.Contains(err.Error(), clientcontract.ErrorCodeRequired) {
		t.Fatalf("an unmarked operation without x-putnami-client was read: %v", err)
	}
}
