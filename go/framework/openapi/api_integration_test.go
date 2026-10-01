package openapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"go.putnami.dev/api"
	"go.putnami.dev/app"
	"go.putnami.dev/client"
	perrors "go.putnami.dev/errors"
	phttp "go.putnami.dev/http"
	clientcontract "go.putnami.dev/protocol/clientcontract"
	"go.putnami.dev/protocol/contracts"

	"go.putnami.dev/protocol/features/spectest"
)

func TestPlugin_CacheControl(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "spec-disclosure", "the-openapi-document-is-no-store-unless-a-cacheable-directive-is-declared")
	invokeSpec := func(p *Plugin) string {
		if err := p.Configure(context.Background(), nil); err != nil {
			t.Fatalf("Configure: %v", err)
		}
		req := httptest.NewRequest("GET", "/_/openapi.json", nil)
		resp := p.handler()(phttp.NewContext(httptest.NewRecorder(), req))
		return resp.Headers.Get("Cache-Control")
	}

	// Secure default: the unauthenticated, security-model-disclosing
	// document must not be stored by shared caches/CDNs.
	if got := invokeSpec(NewPlugin(PluginOptions{Title: "X"})); got != "no-store" {
		t.Errorf("default Cache-Control = %q, want %q", got, "no-store")
	}

	// Explicit opt-in still honored.
	if got := invokeSpec(NewPlugin(PluginOptions{Title: "X", CacheControl: "public, max-age=3600"})); got != "public, max-age=3600" {
		t.Errorf("custom Cache-Control = %q, want %q", got, "public, max-age=3600")
	}
}

type apiUserParams struct {
	ID string `json:"id" validate:"required,uuid"`
}

type apiCreateBody struct {
	Name  string `json:"name" validate:"required,minlen=2"`
	Email string `json:"email" validate:"required,email"`
}

type apiUser struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

type apiFirstPartyUser struct {
	ID       string            `json:"id" validate:"required,uuid"`
	Nickname *string           `json:"nickname"`
	Labels   map[string]string `json:"labels"`
}

type apiThrownNotFound struct {
	Resource string `json:"resource"`
}

// TestOpenAPI_DeclaredErrorSchemaDescribesDetailsNotTheEnvelope pins ADR 0006 on
// the provider side. The projection used to put the `{code, error, message}`
// envelope into `x-putnami-client.errors[].schema`, and to let a `Throws` schema
// overwrite it. A generated client validates that schema against the envelope's
// `details` member, so a well-formed error was rejected as a contract violation.
// Neither shape is a details body, and neither reaches the client contract; both
// keep documenting the response in `responses`, where they belong.
func TestOpenAPI_DeclaredErrorSchemaDescribesDetailsNotTheEnvelope(t *testing.T) {
	server := &fakeServer{}
	apiPlugin := api.New(server,
		api.WithClientService(api.ClientServiceOptions{
			Service: clientcontract.Service{ID: "users", Audience: "https://users.internal"},
		}),
	)
	apiPlugin.Register(api.Endpoint("GET", "/users/{id}").
		Params(api.Type[apiUserParams]()).
		Returns(api.Type[apiFirstPartyUser]()).
		MayThrow(perrors.CodeNotFound).
		// Documents the 404 body. It is not a declared details body:
		// MayThrowDetails is the only declaration of one.
		Throws(http.StatusNotFound, "Not found", api.Type[apiThrownNotFound]()).
		Document())

	openapiPlugin := NewPlugin(PluginOptions{Title: "Users", Version: "1.0.0"}).From(apiPlugin)
	if err := apiPlugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("api Configure: %v", err)
	}
	if err := openapiPlugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("openapi Configure: %v", err)
	}

	operation := openapiPlugin.Spec().Paths["/users/{id}"]["get"]
	contract := operation.ClientContract
	if contract == nil {
		t.Fatal("first-party operation has no client contract")
	}

	// No declared error carries a details schema: the framework raises the
	// envelope and nothing more.
	var notFound *clientcontract.DeclaredError
	for i, declared := range contract.Errors {
		if declared.Schema != nil {
			t.Errorf("declared error %d/%q carries a details schema it never sends: %#v",
				declared.Status, declared.Code, declared.Schema)
		}
		if declared.Code == string(perrors.CodeNotFound) {
			notFound = &contract.Errors[i]
		}
	}
	if notFound == nil || notFound.Status != http.StatusNotFound {
		t.Fatalf("MayThrow code lost from the contract: %#v", contract.Errors)
	}

	// The envelope did not disappear from the document; it never belonged in the
	// client contract in the first place.
	envelope := operation.Responses["400"].Content["application/json"].Schema
	if envelope == nil {
		t.Fatal("400 response lost its body schema")
	}
	for _, property := range []string{"code", "error", "message"} {
		if _, declared := envelope.Properties[property]; !declared {
			t.Errorf("400 response body no longer documents %q: %#v", property, envelope)
		}
	}
	// And a Throws schema still documents its own response.
	thrown := operation.Responses["404"].Content["application/json"].Schema
	if thrown == nil || (thrown.Ref == "" && thrown.Properties["resource"].Type != "string") {
		t.Fatalf("404 response lost the declared Throws schema: %#v", thrown)
	}
}

// TestOpenAPI_SeveralCodesOnOneStatusWithAThrowsKeepEveryCode: two
// declared codes that share HTTP 409, plus a Throws(409) that documents the
// status, are a valid first-party declaration: the codes discriminate the
// error, and the Throws documents the status.
func TestOpenAPI_SeveralCodesOnOneStatusWithAThrowsKeepEveryCode(t *testing.T) {
	server := &fakeServer{}
	apiPlugin := api.New(server,
		api.WithClientService(api.ClientServiceOptions{
			Service: clientcontract.Service{ID: "users", Audience: "https://users.internal"},
		}),
	)
	apiPlugin.Register(api.Endpoint("POST", "/users").
		Body(api.Type[apiCreateBody]()).
		Returns(api.Type[apiFirstPartyUser]()).
		MayThrow(perrors.CodeConflict, perrors.CodeAlreadyExists).
		Throws(http.StatusConflict, "User conflict", nil).
		Document())

	openapiPlugin := NewPlugin(PluginOptions{Title: "Users", Version: "1.0.0"}).From(apiPlugin)
	if err := apiPlugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("api Configure: %v", err)
	}
	if err := openapiPlugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("openapi Configure refused several codes on one status plus a Throws for it: %v", err)
	}

	operation := openapiPlugin.Spec().Paths["/users"]["post"]
	if operation.ClientContract == nil {
		t.Fatal("first-party operation has no client contract")
	}
	var conflicts []string
	for _, declared := range operation.ClientContract.Errors {
		if declared.Code == "" {
			t.Errorf("declared error %d carries an empty code: %#v", declared.Status, operation.ClientContract.Errors)
		}
		if declared.Status == http.StatusConflict {
			conflicts = append(conflicts, declared.Code)
		}
	}
	want := []string{string(perrors.CodeAlreadyExists), string(perrors.CodeConflict)}
	sort.Strings(want)
	if !reflect.DeepEqual(conflicts, want) {
		t.Fatalf("409 codes = %v, want %v", conflicts, want)
	}
	// The Throws keeps documenting the status.
	if got := operation.Responses["409"].Description; got != "User conflict" {
		t.Errorf("409 description = %q, want the Throws description", got)
	}
}

// TestOpenAPI_ThrowsOnAnImplicitStatusNeedsNoMayThrow pins the parity half of
// the Throws rule: the implicit 400 and 500 codes are declared wire codes, so a
// Throws(500) with no MayThrow has a stable code, exactly as a TypeScript
// .throws(500) does. The implicit code carries the envelope and never details
// (ADR 0006), so the Throws schema never becomes its details schema.
func TestOpenAPI_ThrowsOnAnImplicitStatusNeedsNoMayThrow(t *testing.T) {
	server := &fakeServer{}
	apiPlugin := api.New(server,
		api.WithClientService(api.ClientServiceOptions{
			Service: clientcontract.Service{ID: "users", Audience: "https://users.internal"},
		}),
	)
	apiPlugin.Register(api.Endpoint("POST", "/users").
		Body(api.Type[apiCreateBody]()).
		Returns(api.Type[apiFirstPartyUser]()).
		Throws(http.StatusInternalServerError, "Storage failure", api.Type[apiThrownNotFound]()).
		Document())

	openapiPlugin := NewPlugin(PluginOptions{Title: "Users", Version: "1.0.0"}).From(apiPlugin)
	if err := apiPlugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("api Configure: %v", err)
	}
	if err := openapiPlugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("openapi Configure refused a Throws(500) the implicit code discriminates: %v", err)
	}

	operation := openapiPlugin.Spec().Paths["/users"]["post"]
	if operation.ClientContract == nil {
		t.Fatal("first-party operation has no client contract")
	}
	var internal []clientcontract.DeclaredError
	for _, declared := range operation.ClientContract.Errors {
		if declared.Status == http.StatusInternalServerError {
			internal = append(internal, declared)
		}
	}
	want := []clientcontract.DeclaredError{{Status: http.StatusInternalServerError, Code: string(perrors.CodeInternalServer)}}
	if !reflect.DeepEqual(internal, want) {
		t.Fatalf("500 declared errors = %#v, want only the schema-free implicit code %#v", internal, want)
	}
	if got := operation.Responses["500"].Description; got != "Storage failure" {
		t.Errorf("500 description = %q, want the Throws description", got)
	}
}

// TestOpenAPI_ThrowsWithNoCodeAtItsStatusIsRefused pins the other half of the
// Throws rule: a Throws whose status no declared code shares has no stable wire
// code. The projection records that status with an empty code, and strict
// first-party validation refuses it, so an undiscriminated error never reaches
// a generated client.
func TestOpenAPI_ThrowsWithNoCodeAtItsStatusIsRefused(t *testing.T) {
	route := DiscoveredRoute{
		Method:     "POST",
		Path:       "/users",
		ErrorCodes: []perrors.Code{perrors.CodeConflict},
		Throws:     []ThrowsMeta{{Status: http.StatusUnprocessableEntity, Description: "Unprocessable"}},
	}
	var unprocessable []clientcontract.DeclaredError
	for _, declared := range clientErrors(newSchemaGen(nil), route) {
		if declared.Status == http.StatusUnprocessableEntity {
			unprocessable = append(unprocessable, declared)
		}
	}
	if want := []clientcontract.DeclaredError{{Status: http.StatusUnprocessableEntity}}; !reflect.DeepEqual(unprocessable, want) {
		t.Fatalf("422 declared errors = %#v, want the empty-code entry %#v", unprocessable, want)
	}

	server := &fakeServer{}
	apiPlugin := api.New(server,
		api.WithClientService(api.ClientServiceOptions{
			Service: clientcontract.Service{ID: "users", Audience: "https://users.internal"},
		}),
	)
	apiPlugin.Register(api.Endpoint("POST", "/users").
		Body(api.Type[apiCreateBody]()).
		Returns(api.Type[apiFirstPartyUser]()).
		MayThrow(perrors.CodeConflict).
		Throws(http.StatusUnprocessableEntity, "Unprocessable", nil).
		Document())
	openapiPlugin := NewPlugin(PluginOptions{Title: "Users", Version: "1.0.0"}).From(apiPlugin)
	if err := apiPlugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("api Configure: %v", err)
	}
	err := openapiPlugin.Configure(context.Background(), nil)
	if err == nil {
		t.Fatal("openapi Configure accepted a Throws with no declared code at its status")
	}
	for _, want := range []string{"POST /users", ".code"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %v, want it to contain %q", err, want)
		}
	}
}

func TestOpenAPI_FirstPartyContractIsProjectedFromProviderRoutes(t *testing.T) {
	server := &fakeServer{}
	apiPlugin := api.New(server,
		api.WithPrefix("/v1"),
		api.WithClientService(api.ClientServiceOptions{
			Service: clientcontract.Service{ID: "users", Audience: "https://users.internal"},
		}),
	)
	apiPlugin.Register(api.Endpoint("GET", "/users/{id}").
		Params(api.Type[apiUserParams]()).
		Returns(api.Type[apiFirstPartyUser]()).
		MayThrowWith(perrors.CodeNotFound, api.ErrorOptions{Retryable: false}).
		Document())
	apiPlugin.Register(api.Endpoint("GET", "/users/watch").
		Returns(api.StreamOf[apiFirstPartyUser]()).
		Document())

	openapiPlugin := NewPlugin(PluginOptions{Title: "Users", Version: "1.0.0"}).From(apiPlugin)
	if err := apiPlugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("api Configure: %v", err)
	}
	if err := openapiPlugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("openapi Configure: %v", err)
	}

	spec := openapiPlugin.Spec()
	if spec.ClientContract == nil || spec.ClientContract.ProtocolVersion != clientcontract.ProtocolVersion {
		t.Fatalf("document client contract = %#v", spec.ClientContract)
	}
	if spec.ClientContract.Credentials == nil {
		t.Fatal("anonymous credential registry must serialize as an object")
	}
	unary := spec.Paths["/v1/users/{id}"]["get"].ClientContract
	if unary == nil || unary.Stream != clientcontract.StreamUnary {
		t.Fatalf("unary client contract = %#v", unary)
	}
	if len(unary.Transports) != 1 || unary.Transports[0].Protocol != clientcontract.TransportRESTJSON || unary.Transports[0].Path != "/v1/users/{id}" {
		t.Fatalf("unary transports = %#v", unary.Transports)
	}
	if unary.Idempotency.Kind != clientcontract.IdempotencySafe {
		t.Fatalf("GET idempotency = %#v", unary.Idempotency)
	}
	if len(unary.Security.Alternatives) != 1 || len(unary.Security.Alternatives[0].AllOf) != 0 {
		t.Fatalf("anonymous security = %#v", unary.Security)
	}
	foundNotFound := false
	for _, declared := range unary.Errors {
		if declared.Code == string(perrors.CodeNotFound) {
			foundNotFound = declared.Retryable != nil && !*declared.Retryable
		}
	}
	if !foundNotFound {
		t.Fatalf("declared errors lost MayThrow code: %#v", unary.Errors)
	}

	// N3 restored the websocket transport together with the server that honors
	// it: go/framework/http negotiates putnami.service.v1 and reassembles
	// continuations, and go/framework/api drives the published admission state
	// machine over it. The declared order is the dispatch order.
	stream := spec.Paths["/v1/users/watch"]["get"].ClientContract
	if stream == nil || stream.Stream != clientcontract.StreamServer || len(stream.Transports) != 2 {
		t.Fatalf("server stream client contract = %#v", stream)
	}
	if stream.Transports[0].Protocol != clientcontract.TransportSSE {
		t.Fatalf("server stream declares SSE first: %#v", stream.Transports)
	}
	webSocket := stream.Transports[1]
	if webSocket.Protocol != clientcontract.TransportWebSocket || webSocket.Encoding != clientcontract.EncodingJSON {
		t.Fatalf("server stream websocket transport = %#v", webSocket)
	}
	if webSocket.WebSocket == nil || webSocket.WebSocket.Subprotocol != clientcontract.WebSocketSubprotocolV1 {
		t.Fatalf("server stream websocket subprotocol = %#v", webSocket.WebSocket)
	}

	userSchema := spec.Components.Schemas["apiFirstPartyUser"]
	if nullable := userSchema.Properties["nickname"].Nullable; nullable == nil || !*nullable {
		t.Fatalf("pointer nullability was lost: %#v", userSchema.Properties["nickname"])
	}
	labels := userSchema.Properties["labels"].AdditionalProperties
	if labels == nil || labels.Schema == nil || labels.Schema.Type != "string" {
		t.Fatalf("typed map values were lost: %#v", userSchema.Properties["labels"])
	}
}

func TestOpenAPI_FirstPartyRealHTTPProviderPreservesEveryStreamMessageShape(t *testing.T) {
	httpServer := phttp.NewServerPlugin(phttp.ServerConfig{})
	apiPlugin := api.New(httpServer,
		api.WithClientService(api.ClientServiceOptions{
			Service: clientcontract.Service{ID: "streams", Audience: "https://streams.internal"},
		}),
	)
	apiPlugin.Register(api.Endpoint("GET", "/events").
		Returns(api.StreamOf[apiFirstPartyUser]()).
		Handle(api.ServerStream(func(_ *api.ServerStreamContext[apiFirstPartyUser]) error { return nil })))

	openapiPlugin := NewPlugin(PluginOptions{Title: "Streams", Version: "1.0.0"}).From(apiPlugin)
	if err := apiPlugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("api Configure: %v", err)
	}
	if err := openapiPlugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("openapi Configure: %v", err)
	}

	raw, err := openapiPlugin.OpenAPISpecJSON()
	if err != nil {
		t.Fatal(err)
	}
	ir, err := api.ReadOpenAPISpec(raw)
	if err != nil {
		t.Fatalf("strict reader rejected real provider stream contract: %v\n%s", err, raw)
	}
	byID := map[string]api.MethodIR{}
	for _, service := range ir.Services {
		for _, method := range service.Methods {
			byID[method.OperationID] = method
		}
	}
	server := byID["getEvents"].Client
	if server == nil || server.Messages == nil || server.Messages.Input != nil || server.Messages.Output == nil ||
		server.Messages.Output.Ref != "#/components/schemas/apiFirstPartyUser" {
		t.Fatalf("server stream messages = %#v", server)
	}
	if len(server.Transports) != 2 || server.Transports[0].Protocol != clientcontract.TransportSSE ||
		server.Transports[1].Protocol != clientcontract.TransportWebSocket {
		t.Fatalf("server stream transports = %#v", server.Transports)
	}
	if ws := server.Transports[1].WebSocket; ws == nil || ws.Subprotocol != clientcontract.WebSocketSubprotocolV1 {
		t.Fatalf("server stream websocket transport = %#v", server.Transports[1])
	}
}

// N3 gave client and bidirectional streams the transport they need, so the
// refusal moves to the shape it still catches: a stream mode this projection
// does not know how to carry. Publishing a REST JSON transport for it — the
// former default branch — would have handed a generated client a unary call the
// provider never serves.
func TestOpenAPI_FirstPartyRefusesStreamsWithNoHonorableTransport(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "generation-refuses-unhonorable-declarations", "a-stream-with-no-honorable-transport-is-refused-at-generation")
	generator := &schemaGen{}
	route := DiscoveredRoute{Method: "GET", Path: "/multiplexed", StreamMode: "multiplexed"}
	safe := clientcontract.Idempotency{Kind: clientcontract.IdempotencySafe}
	if transports := clientTransportsFor(generator, route, connectProjection{}, safe); len(transports) != 0 {
		t.Fatalf("unknown stream shape published %#v", transports)
	}
	if generator.generationErr == nil {
		t.Fatal("an unknown stream shape was projected without a generation refusal")
	}
	want := "GET /multiplexed declares a multiplexed stream shape and this provider has no transport that can carry it"
	if !strings.Contains(generator.generationErr.Error(), want) {
		t.Fatalf("refusal = %v, want %q", generator.generationErr, want)
	}
}

// The two stream shapes that only WebSocket can carry are published again, and
// each declares the negotiated first-party subprotocol.
func TestOpenAPI_FirstPartyPublishesWebSocketForClientAndBidirectionalStreams(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		register func(*api.Plugin)
		want     clientcontract.StreamMode
	}{
		{
			name: "client stream",
			path: "/uploads",
			register: func(plugin *api.Plugin) {
				plugin.Register(api.Endpoint("GET", "/uploads").
					Body(api.StreamOf[apiFirstPartyUser]()).
					Returns(api.Type[apiCreateBody]()).
					Handle(api.ClientStream(func(_ *api.ClientStreamContext[apiFirstPartyUser, apiCreateBody]) error { return nil })))
			},
			want: clientcontract.StreamClient,
		},
		{
			name: "bidirectional stream",
			path: "/chat",
			register: func(plugin *api.Plugin) {
				plugin.Register(api.Endpoint("GET", "/chat").
					Body(api.StreamOf[apiCreateBody]()).
					Returns(api.StreamOf[apiFirstPartyUser]()).
					Handle(api.BidiStream(func(_ *api.BidiStreamContext[apiCreateBody, apiFirstPartyUser]) error { return nil })))
			},
			want: clientcontract.StreamBidirectional,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			httpServer := phttp.NewServerPlugin(phttp.ServerConfig{})
			apiPlugin := api.New(httpServer,
				api.WithClientService(api.ClientServiceOptions{
					Service: clientcontract.Service{ID: "streams", Audience: "https://streams.internal"},
				}),
			)
			tc.register(apiPlugin)
			openapiPlugin := NewPlugin(PluginOptions{Title: "Streams", Version: "1.0.0"}).From(apiPlugin)
			if err := apiPlugin.Configure(context.Background(), nil); err != nil {
				t.Fatalf("api Configure: %v", err)
			}
			if err := openapiPlugin.Configure(context.Background(), nil); err != nil {
				t.Fatalf("openapi Configure: %v", err)
			}
			spec := openapiPlugin.Spec()
			contract := spec.Paths[tc.path]["get"].ClientContract
			if contract == nil || contract.Stream != tc.want {
				t.Fatalf("client contract = %#v", contract)
			}
			if len(contract.Transports) != 1 || contract.Transports[0].Protocol != clientcontract.TransportWebSocket {
				t.Fatalf("transports = %#v", contract.Transports)
			}
			if ws := contract.Transports[0].WebSocket; ws == nil || ws.Subprotocol != clientcontract.WebSocketSubprotocolV1 {
				t.Fatalf("websocket transport = %#v", contract.Transports[0])
			}
			if contract.Transports[0].Encoding != clientcontract.EncodingJSON {
				t.Fatalf("websocket encoding = %q, want json", contract.Transports[0].Encoding)
			}
		})
	}
}

type canonicalParams struct {
	Zeta  string `json:"zeta" validate:"required"`
	Alpha string `json:"alpha" validate:"required"`
}

type canonicalQuery struct {
	Zulu  string `json:"zulu" validate:"required"`
	Alpha string `json:"alpha" validate:"required"`
}

type canonicalResponse struct {
	Zeta  string `json:"zeta" validate:"required"`
	Alpha string `json:"alpha" validate:"required"`
}

func TestOpenAPI_CanonicalProviderBytesKeepClientHashThroughUnchangedPublication(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "generated-from-contract", "canonical-provider-bytes-keep-client-hash-through-unchanged-publication")

	server := &fakeServer{}
	apiPlugin := api.New(server,
		api.WithClientService(api.ClientServiceOptions{
			Service: clientcontract.Service{ID: "widgets", Audience: "https://widgets.internal"},
		}),
	)
	apiPlugin.Register(api.Endpoint("GET", "/widgets/{zeta}/{alpha}").
		Params(api.Type[canonicalParams]()).
		Query(api.Type[canonicalQuery]()).
		Returns(api.Type[canonicalResponse]()).
		Handle(func(_ *phttp.EndpointContext) *phttp.Response { return phttp.JSON(nil) }))
	openapiPlugin := NewPlugin(PluginOptions{Title: "Canonical API", Version: "1.0.0"}).From(apiPlugin)
	clientsPlugin := api.Clients(api.ClientsOptions{
		Go: api.GoClientOptions{PackageName: "widgetsclient", ClientName: "WidgetsClient"},
	}).From(apiPlugin)

	application := app.New("canonical-provider")
	application.Module.Feature(app.Feature{
		ID:      "canonical/widgets",
		Name:    "Canonical widgets",
		Outcome: "Consumers call the canonical widgets contract",
		Owner:   "go",
	})
	application.Use(apiPlugin).Use(openapiPlugin).Use(clientsPlugin)

	projectRoot := t.TempDir()
	genDir := filepath.Join(projectRoot, ".gen")
	if err := application.Describe(genDir, []string{"openapi", "clients"}); err != nil {
		t.Fatalf("Describe: %v", err)
	}

	described, err := os.ReadFile(filepath.Join(genDir, DefaultDescribePath))
	if err != nil {
		t.Fatalf("read described spec: %v", err)
	}
	inMemory, err := openapiPlugin.OpenAPISpecJSON()
	if err != nil {
		t.Fatalf("OpenAPISpecJSON: %v", err)
	}
	req := httptest.NewRequest("GET", "/_/openapi.json", nil)
	runtimeResponse := openapiPlugin.handler()(phttp.NewContext(httptest.NewRecorder(), req))
	runtimeBody, err := runtimeResponse.BodyBytes()
	if err != nil {
		t.Fatalf("runtime response: %v", err)
	}
	for _, surface := range []struct {
		name string
		body []byte
	}{{name: "in-memory", body: inMemory}, {name: "runtime", body: runtimeBody}} {
		if !bytes.Equal(surface.body, described) {
			t.Errorf("%s OpenAPI bytes differ from Describe\n%s:\n%s\nDescribe:\n%s", surface.name, surface.name, surface.body, described)
		}
	}

	var document Document
	if err := json.Unmarshal(described, &document); err != nil {
		t.Fatalf("parse described spec: %v", err)
	}
	operation := document.Paths["/widgets/{zeta}/{alpha}"]["get"]
	parameterKeys := make([]string, 0, len(operation.Parameters))
	for _, parameter := range operation.Parameters {
		parameterKeys = append(parameterKeys, parameter.In+"/"+parameter.Name)
	}
	if want := []string{"path/alpha", "path/zeta", "query/alpha", "query/zulu"}; !reflect.DeepEqual(parameterKeys, want) {
		t.Fatalf("parameter order = %v, want %v", parameterKeys, want)
	}
	if required := document.Components.Schemas["canonicalResponse"].Required; !reflect.DeepEqual(required, []string{"alpha", "zeta"}) {
		t.Fatalf("required order = %v, want [alpha zeta]", required)
	}

	// The Go extension's publication boundary decodes into an untyped object
	// before encoding it. GenerateSpec already normalized the structural arrays
	// asserted above, so this independent pass models the remaining key-order and
	// newline normalization without importing the extension into the framework.
	published := canonicalObjectJSON(t, described)
	if !bytes.Equal(published, described) {
		t.Fatalf("publication would rewrite provider bytes\nprovider:\n%s\npublished:\n%s", described, published)
	}

	expectedHash := fmt.Sprintf("%x", sha256.Sum256(published))[:16]
	stagedClient, err := os.ReadFile(filepath.Join(genDir, api.ClientStageDir, "go", "client.gen.go"))
	if err != nil {
		t.Fatalf("read staged client: %v", err)
	}
	if !strings.Contains(string(stagedClient), `SpecHash: "`+expectedHash+`"`) {
		t.Fatalf("staged client does not carry published spec hash %s", expectedHash)
	}

	// Publish the unchanged provider document and drive the workspace clientgen
	// entry point. Its output must remain byte-identical to the client staged
	// inside Describe. A publication that merges an additional static route is a
	// different contract and is deliberately outside this invariant.
	schemaPath := filepath.Join(projectRoot, DefaultDescribePath)
	if err := os.MkdirAll(filepath.Dir(schemaPath), 0o750); err != nil {
		t.Fatalf("prepare published schema: %v", err)
	}
	// The workspace generator emits the client as a package inside the provider's
	// own Go module, so the ownership manifest names <module>/clients/go. A Go
	// provider project always carries that go.mod; the fixture states it rather
	// than letting the manifest be written without an import path.
	if err := os.WriteFile(filepath.Join(projectRoot, "go.mod"), []byte("module example.dev/canonical\n\ngo 1.25\n"), 0o600); err != nil {
		t.Fatalf("write provider go.mod: %v", err)
	}
	if err := os.WriteFile(schemaPath, published, 0o600); err != nil {
		t.Fatalf("publish schema: %v", err)
	}
	if _, err := api.GenerateProjectClients(projectRoot); err != nil {
		t.Fatalf("GenerateProjectClients: %v", err)
	}
	workspaceClient, err := os.ReadFile(filepath.Join(projectRoot, "clients", "go", "client.gen.go"))
	if err != nil {
		t.Fatalf("read workspace client: %v", err)
	}
	if !bytes.Equal(workspaceClient, stagedClient) {
		t.Fatalf("workspace client differs after publication\nstaged:\n%s\nworkspace:\n%s", stagedClient, workspaceClient)
	}
}

func canonicalObjectJSON(t *testing.T, body []byte) []byte {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var document map[string]any
	if err := decoder.Decode(&document); err != nil {
		t.Fatalf("decode OpenAPI object: %v", err)
	}
	canonical, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		t.Fatalf("encode canonical OpenAPI object: %v", err)
	}
	return append(canonical, '\n')
}

type CreatedUser struct {
	ID string `json:"id"`
}

type ProjectionStatus string
type ProjectionChange interface{}
type ProjectionRequest struct {
	Status ProjectionStatus `json:"status"`
	Change ProjectionChange `json:"change"`
}

func TestContractProjection_RoundTripsThroughClientGeneration(t *testing.T) {
	manifest := &contracts.Manifest{
		ProtocolVersion: contracts.ProtocolVersion,
		Name:            "example/projection",
		Enums: []contracts.Enum{{
			Name: "ProjectionStatus",
			Values: []contracts.EnumValue{
				{Name: "Pending", Value: "pending"},
				{Name: "Applied", Value: "applied"},
			},
		}, {
			Name:   "UnusedProjectionType",
			Values: []contracts.EnumValue{{Name: "Unused", Value: "unused"}},
		}},
		Unions: []contracts.Union{{
			Name:          "ProjectionChange",
			Discriminator: "kind",
			Variants: []contracts.UnionVariant{
				{Tag: "rename", Fields: []contracts.Field{{Name: "kind", Type: "string"}, {Name: "name", Type: "string"}}},
				{Tag: "archive", Fields: []contracts.Field{{Name: "reason", Type: "string", Optional: true}}},
			},
		}},
		Structs: []contracts.Struct{{
			Name: "ProjectionRequest",
			Fields: []contracts.Field{
				{Name: "status", Type: "ProjectionStatus"},
				{Name: "change", Type: "ProjectionChange"},
			},
		}},
	}

	doc := GenerateSpec([]DiscoveredRoute{{
		Method:  "POST",
		Path:    "/projection",
		Body:    reflect.TypeOf(ProjectionRequest{}),
		Returns: reflect.TypeOf(ProjectionRequest{}),
	}}, Options{Title: "Projection", Version: "1.0.0", Contract: manifest})

	if got := doc.Paths["/projection"]["post"].RequestBody.Content["application/json"].Schema.Ref; got != "#/components/schemas/ProjectionRequest" {
		t.Fatalf("request schema ref = %q", got)
	}
	if got := doc.Components.Schemas["ProjectionStatus"].Enum; len(got) != 2 || got[0] != "pending" {
		t.Fatalf("projected enum = %#v", got)
	}
	if got := doc.Components.Schemas["ProjectionChange"]; len(got.OneOf) != 2 || got.Discriminator.PropertyName != "kind" {
		t.Fatalf("projected union = %#v", got)
	} else {
		for i, wantTag := range []string{"rename", "archive"} {
			discriminator := got.OneOf[i].Properties["kind"]
			if len(discriminator.Enum) != 1 || discriminator.Enum[0] != wantTag {
				t.Fatalf("variant %d discriminator = %#v, want %q", i, discriminator, wantTag)
			}
		}
	}
	if _, ok := doc.Components.Schemas["UnusedProjectionType"]; ok {
		t.Fatal("unreferenced contract type was projected")
	}

	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	ir, err := api.ReadOpenAPISpec(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(ir.Enums["ProjectionStatus"]) != 2 || len(ir.Unions["ProjectionChange"].Variants) != 2 {
		t.Fatalf("contract types lost in client IR: %#v", ir)
	}
	clientSource, err := api.GenerateClientFromIR(ir, api.ClientGenOptions{PackageName: "projectionclient"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"type ProjectionStatus string",
		`ProjectionStatusPending ProjectionStatus = "pending"`,
		"type ProjectionChange struct",
		"Change ProjectionChange",
	} {
		if !strings.Contains(clientSource, want) {
			t.Errorf("generated client missing %q\n%s", want, clientSource)
		}
	}
}

// fakeServer satisfies api.Server for the integration test without spinning up a real
// listener. It records the registered handlers and pending injected handlers.
type fakeServer struct {
	calls   [][2]string
	pending []*phttp.InjectedHandler
}

func (f *fakeServer) Handle(method, path string, _ phttp.Handler) {
	f.calls = append(f.calls, [2]string{method, path})
}

func (f *fakeServer) AddPendingInjectedHandler(ih *phttp.InjectedHandler) {
	f.pending = append(f.pending, ih)
}

func TestOpenAPI_AutoDiscovery_FromAPIPlugin(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "single-declaration", "the-plugin-exposes-the-same-metadata-to-contract-consumers")
	server := &fakeServer{}
	apiPlugin := api.New(server, api.WithPrefix("/v1"))

	apiPlugin.Register(api.Endpoint("GET", "/users/{id}").
		Description("Get a user").
		Params(api.Type[apiUserParams]()).
		Returns(api.Type[apiUser]()).
		Throws(404, "Not found", nil).
		Handle(func(_ *phttp.EndpointContext) *phttp.Response { return phttp.JSON(nil) }))

	apiPlugin.Register(api.Endpoint("POST", "/users").
		Description("Create a user").
		Body(api.Type[apiCreateBody]()).
		Returns(api.Type[apiUser]()).
		Response(201, "Created", api.Type[apiUser]()).
		Handle(func(_ *phttp.EndpointContext) *phttp.Response { return phttp.JSON(nil) }))

	openapiPlugin := NewPlugin(PluginOptions{Title: "Auto API", Version: "1.0.0"}).From(apiPlugin)

	if err := apiPlugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("api Configure: %v", err)
	}
	if err := openapiPlugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("openapi Configure: %v", err)
	}

	spec := openapiPlugin.Spec()
	if spec == nil {
		t.Fatal("spec should be generated after Configure")
	}

	getItem, ok := spec.Paths["/v1/users/{id}"]
	if !ok {
		t.Fatalf("path /v1/users/{id} missing from spec; paths = %v", keys(spec.Paths))
	}
	getOp, hasGet := getItem["get"]
	if !hasGet {
		t.Errorf("GET operation missing for /v1/users/{id}")
	} else if getOp.Description != "Get a user" {
		t.Errorf("description = %q, want %q", getOp.Description, "Get a user")
	}

	postItem, ok := spec.Paths["/v1/users"]
	if !ok {
		t.Fatalf("path /v1/users missing from spec; paths = %v", keys(spec.Paths))
	}
	if _, hasPost := postItem["post"]; !hasPost {
		t.Errorf("POST operation missing for /v1/users")
	}
}

type routeRegisteringPlugin struct {
	name   string
	api    *api.Plugin
	method string
	path   string
}

func (p *routeRegisteringPlugin) Name() string { return p.name }

func (p *routeRegisteringPlugin) Configure(_ context.Context, _ *app.Module) error {
	p.api.Register(api.Endpoint(p.method, p.path).
		HandleRaw(func(_ *phttp.Context) *phttp.Response { return phttp.NoContent() }))
	return nil
}

func TestOpenAPI_StartRefreshesAfterAPIPluginConfigures(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "late-route-discovery", "start-re-renders-the-document-after-a-later-plugin-registers-routes")
	server := &fakeServer{}
	apiPlugin := api.New(server)
	openapiPlugin := NewPlugin(PluginOptions{Title: "Late API", Version: "1.0.0"}).From(apiPlugin)

	// OpenAPI configures before the API plugin has dispatched pending endpoints.
	if err := openapiPlugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("openapi Configure: %v", err)
	}
	apiPlugin.Register(api.Endpoint("GET", "/late").
		HandleRaw(func(_ *phttp.Context) *phttp.Response { return phttp.NoContent() }))
	if err := apiPlugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("api Configure: %v", err)
	}

	if err := openapiPlugin.Start(context.Background(), nil); err != nil {
		t.Fatalf("openapi Start: %v", err)
	}
	if _, ok := openapiPlugin.Spec().Paths["/late"]; !ok {
		t.Fatalf("Start did not refresh auto-discovered routes; paths = %v", keys(openapiPlugin.Spec().Paths))
	}

	// A second refresh must not duplicate the operation.
	if err := openapiPlugin.Start(context.Background(), nil); err != nil {
		t.Fatalf("openapi Start (second): %v", err)
	}
	if got := len(openapiPlugin.Spec().Paths["/late"]); got != 1 {
		t.Fatalf("duplicate operations after repeated refresh: got %d", got)
	}
}

func TestOpenAPI_HandlerRefreshesBeforeStartCompletes(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "late-route-discovery", "the-served-handler-refreshes-before-start-completes")
	server := &fakeServer{}
	apiPlugin := api.New(server)
	openapiPlugin := NewPlugin(PluginOptions{Title: "Late API", Version: "1.0.0"}).From(apiPlugin)

	if err := openapiPlugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("openapi Configure: %v", err)
	}
	apiPlugin.Register(api.Endpoint("GET", "/late").
		HandleRaw(func(_ *phttp.Context) *phttp.Response { return phttp.NoContent() }))
	if err := apiPlugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("api Configure: %v", err)
	}

	req := httptest.NewRequest("GET", "/_/openapi.json", nil)
	resp := openapiPlugin.handler()(phttp.NewContext(httptest.NewRecorder(), req))
	if resp.Status != 200 {
		t.Fatalf("status = %d, want 200", resp.Status)
	}
	body, err := resp.BodyBytes()
	if err != nil {
		t.Fatalf("BodyBytes: %v", err)
	}
	var spec Document
	if err := json.Unmarshal(body, &spec); err != nil {
		t.Fatalf("parse served OpenAPI spec: %v", err)
	}
	if _, ok := spec.Paths["/late"]; !ok {
		t.Fatalf("handler served stale spec before Start; paths = %v", keys(spec.Paths))
	}
}

func TestOpenAPI_DescribeRefreshesAfterAllConfigurers(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "late-route-discovery", "describe-re-renders-the-document-after-every-configurer")
	server := &fakeServer{}
	apiPlugin := api.New(server)
	openapiPlugin := NewPlugin(PluginOptions{Title: "Control Plane", Version: "1.2.3"}).From(apiPlugin)

	a := app.New("ordering")
	a.Use(&routeRegisteringPlugin{name: "v1-routes", api: apiPlugin, method: "GET", path: "/v1/users"})
	a.Use(openapiPlugin)
	a.Use(&routeRegisteringPlugin{name: "config-routes", api: apiPlugin, method: "POST", path: "/api/configs"})
	a.Use(apiPlugin)

	out := t.TempDir()
	if err := a.Describe(out, nil); err != nil {
		t.Fatalf("Describe: %v", err)
	}

	body, err := os.ReadFile(filepath.Join(out, DefaultDescribePath))
	if err != nil {
		t.Fatalf("read described OpenAPI spec: %v", err)
	}
	var spec Document
	if err := json.Unmarshal(body, &spec); err != nil {
		t.Fatalf("parse described OpenAPI spec: %v", err)
	}
	if spec.Info.Title != "Control Plane" || spec.Info.Version != "1.2.3" {
		t.Fatalf("metadata fell back unexpectedly: %+v", spec.Info)
	}
	for _, path := range []string{"/v1/users", "/api/configs"} {
		if _, ok := spec.Paths[path]; !ok {
			t.Fatalf("described OpenAPI spec missing %s; paths = %v", path, keys(spec.Paths))
		}
	}
}

func TestOpenAPI_AutoDiscovery_RespectsExplicitAddRoute(t *testing.T) {
	apiPlugin := api.New(&fakeServer{})
	apiPlugin.Register(api.Endpoint("GET", "/auto").
		HandleRaw(func(_ *phttp.Context) *phttp.Response { return phttp.JSON(nil) }))

	openapiPlugin := NewPlugin(PluginOptions{Title: "Mixed", Version: "1.0.0"}).From(apiPlugin)
	openapiPlugin.AddRoute(DiscoveredRoute{Method: "GET", Path: "/manual"})

	if err := apiPlugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("api Configure: %v", err)
	}
	if err := openapiPlugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("openapi Configure: %v", err)
	}

	spec := openapiPlugin.Spec()
	if _, ok := spec.Paths["/manual"]; !ok {
		t.Errorf("manual route not in spec; paths = %v", keys(spec.Paths))
	}
	if _, ok := spec.Paths["/auto"]; !ok {
		t.Errorf("auto-discovered route not in spec; paths = %v", keys(spec.Paths))
	}
}

type opaqueRule struct{}

func (opaqueRule) Middleware() phttp.Middleware {
	return func(_ *phttp.Context, next func() *phttp.Response) *phttp.Response { return next() }
}

type declarativeRule struct {
	policy phttp.SecurityAuthorization
}

func (r declarativeRule) Middleware() phttp.Middleware {
	return func(_ *phttp.Context, next func() *phttp.Response) *phttp.Response { return next() }
}

func (r declarativeRule) SecurityAuthorizationPolicy() phttp.SecurityAuthorization {
	return r.policy
}

func TestOpenAPI_FirstPartyDeclarativeSecurityIsProjectedWithoutLoss(t *testing.T) {
	apiPlugin := api.New(&fakeServer{}, api.WithClientService(api.ClientServiceOptions{
		Service: clientcontract.Service{ID: "secure", Audience: "https://secure.internal"},
		Credentials: map[string]clientcontract.CredentialProfile{
			"workload": {Kind: clientcontract.CredentialServiceToken},
		},
	}))
	apiPlugin.Register(api.Endpoint("GET", "/secure").
		Secure(declarativeRule{policy: phttp.SecurityAuthorization{
			Clients:   []string{"billing-worker"},
			ScopesAll: []string{"accounts:read"},
			ScopesAny: []string{"region:eu", "region:us"},
			RolesAll:  []string{"reader"},
			RolesAny:  []string{"operator", "admin"},
		}}).
		Returns(api.Type[apiUser]()).
		Handle(func(_ *phttp.EndpointContext) *phttp.Response { return phttp.JSON(nil) }))
	if err := apiPlugin.Configure(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	openapiPlugin := NewPlugin(PluginOptions{Title: "Secure", Version: "1.0.0"}).From(apiPlugin)
	if err := openapiPlugin.Configure(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	security := openapiPlugin.Spec().Paths["/secure"]["get"].ClientContract.Security
	if len(security.Alternatives) != 1 || len(security.Alternatives[0].AllOf) != 1 {
		t.Fatalf("credential alternatives = %#v", security.Alternatives)
	}
	requirement := security.Alternatives[0].AllOf[0]
	if requirement.Profile != "workload" || !reflect.DeepEqual(requirement.Scopes, []string{"accounts:read"}) ||
		!reflect.DeepEqual(requirement.Roles, []string{"reader"}) {
		t.Fatalf("generated credential requirement = %#v", requirement)
	}
	wantAuthorization := &clientcontract.Authorization{
		Clients:   []string{"billing-worker"},
		ScopesAll: []string{"accounts:read"},
		ScopesAny: []string{"region:eu", "region:us"},
		RolesAll:  []string{"reader"},
		RolesAny:  []string{"operator", "admin"},
	}
	if !reflect.DeepEqual(security.Authorization, wantAuthorization) {
		t.Fatalf("authorization = %#v, want %#v", security.Authorization, wantAuthorization)
	}
}

func TestOpenAPI_FirstPartyOpaqueSecurityFailsExplicitly(t *testing.T) {
	server := &fakeServer{}
	apiPlugin := api.New(server, api.WithClientService(api.ClientServiceOptions{
		Service: clientcontract.Service{ID: "secure", Audience: "https://secure.internal"},
		Credentials: map[string]clientcontract.CredentialProfile{
			"workload": {Kind: clientcontract.CredentialServiceToken},
		},
	}))
	apiPlugin.Register(api.Endpoint("GET", "/secure").
		Secure(opaqueRule{}).
		Returns(api.Type[apiUser]()).
		Handle(func(_ *phttp.EndpointContext) *phttp.Response { return phttp.JSON(nil) }))
	if err := apiPlugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("api Configure: %v", err)
	}

	openapiPlugin := NewPlugin(PluginOptions{Title: "Secure", Version: "1.0.0"}).From(apiPlugin)
	err := openapiPlugin.Configure(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "client_contract.invalid_security") {
		t.Fatalf("Configure error = %v, want explicit invalid client security", err)
	}
}

// unsupportedInterfacePayload holds a non-empty interface: encoding/json cannot
// decode into it, so it declares nothing a client could send.
type unsupportedInterfacePayload struct {
	Value fmt.Stringer `json:"value"`
}

type unsupportedMapKeyPayload struct {
	Values map[int]string `json:"values"`
}

func TestOpenAPI_FirstPartyUnrepresentableGoSchemasFailExplicitly(t *testing.T) {
	tests := []struct {
		name string
		typ  reflect.Type
	}{
		{name: "non-empty interface", typ: api.Type[unsupportedInterfacePayload]()},
		{name: "non-string map key", typ: api.Type[unsupportedMapKeyPayload]()},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			apiPlugin := api.New(&fakeServer{}, api.WithClientService(api.ClientServiceOptions{
				Service: clientcontract.Service{ID: "strict", Audience: "https://strict.internal"},
			}))
			apiPlugin.Register(api.Endpoint("GET", "/strict").Returns(tc.typ).Document())
			if err := apiPlugin.Configure(context.Background(), nil); err != nil {
				t.Fatal(err)
			}
			openapiPlugin := NewPlugin(PluginOptions{Title: "Strict", Version: "1.0.0"}).From(apiPlugin)
			err := openapiPlugin.Configure(context.Background(), nil)
			if err == nil || !strings.Contains(err.Error(), "client_contract.invalid_schema") {
				t.Fatalf("Configure error = %v, want unrepresentable schema failure", err)
			}
		})
	}
}

func TestOpenAPI_AutoDiscovery_PropagatesAdditionalReturns(t *testing.T) {
	server := &fakeServer{}
	apiPlugin := api.New(server)

	apiPlugin.Register(api.Endpoint("POST", "/users").
		Body(api.Type[apiCreateBody]()).
		Returns(api.Type[apiUser]()).
		Response(201, "Created", api.Type[apiUser]()).
		Response(204, "No content", nil).
		Handle(func(_ *phttp.EndpointContext) *phttp.Response { return phttp.JSON(nil) }))

	openapiPlugin := NewPlugin(PluginOptions{Title: "Returns", Version: "1.0.0"}).From(apiPlugin)
	if err := apiPlugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("api Configure: %v", err)
	}
	if err := openapiPlugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("openapi Configure: %v", err)
	}

	spec := openapiPlugin.Spec()
	op, ok := spec.Paths["/users"]["post"]
	if !ok {
		t.Fatalf("POST /users missing from spec; paths = %v", keys(spec.Paths))
	}
	for _, status := range []string{"200", "201", "204"} {
		if _, present := op.Responses[status]; !present {
			t.Errorf("response %s missing; have %v", status, keys(op.Responses))
		}
	}
	if op.Responses["204"].Description != "No content" {
		t.Errorf("204 description = %q, want %q", op.Responses["204"].Description, "No content")
	}
	// 204 should not have a body schema since Schema was nil.
	if op.Responses["204"].Content != nil {
		t.Errorf("204 should have no body content; got %+v", op.Responses["204"].Content)
	}
}

func TestOpenAPI_AutoDiscovery_PublishesPrimaryCreatedStatusAndGeneratesTypedClient(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "single-declaration", "a-primary-created-status-is-published-and-keeps-generated-response-type")
	apiPlugin := api.New(&fakeServer{},
		api.WithClientService(api.ClientServiceOptions{
			Service: clientcontract.Service{ID: "users", Audience: "https://users.internal"},
		}),
	)
	apiPlugin.Register(api.Endpoint("POST", "/users").
		Body(api.Type[CreatedUser]()).
		ReturnsStatus(http.StatusCreated, "Created", api.Type[CreatedUser]()).
		Document())
	openapiPlugin := NewPlugin(PluginOptions{Title: "Returns", Version: "1.0.0"}).From(apiPlugin)
	clientsPlugin := api.Clients(api.ClientsOptions{
		Go: api.GoClientOptions{PackageName: "createdusers", ClientName: "CreatedUsersClient"},
	}).From(apiPlugin)
	application := app.New("returns-status")
	application.Use(apiPlugin).Use(openapiPlugin).Use(clientsPlugin)

	out := t.TempDir()
	if err := application.Describe(out, []string{"openapi", "clients"}); err != nil {
		t.Fatalf("Describe: %v", err)
	}

	responses := openapiPlugin.Spec().Paths["/users"]["post"].Responses
	if _, exists := responses["200"]; exists {
		t.Fatalf("provider declaration invented HTTP 200: %#v", responses)
	}
	created := responses["201"]
	if created.Description != "Created" || created.Content["application/json"].Schema == nil {
		t.Fatalf("created response = %#v", created)
	}

	clientSource, err := os.ReadFile(filepath.Join(out, api.ClientStageDir, "go", "client.gen.go"))
	if err != nil {
		t.Fatalf("read generated client: %v", err)
	}
	wantMethod := "func (c *CreatedUsersClient) CreateUsers(ctx context.Context, in CreateUsersInput) (*CreatedUser, error) {"
	if !strings.Contains(string(clientSource), wantMethod) {
		t.Fatalf("generated client lost the 201 response schema; want %q\n%s", wantMethod, clientSource)
	}
}

func TestOpenAPI_AutoDiscovery_PropagatesMayThrowErrorCodes(t *testing.T) {
	server := &fakeServer{}
	apiPlugin := api.New(server)

	apiPlugin.Register(api.Endpoint("GET", "/users/{id}").
		MayThrow(perrors.CodeNotFound, perrors.CodeConflict).
		Handle(func(_ *phttp.EndpointContext) *phttp.Response { return phttp.JSON(nil) }))

	openapiPlugin := NewPlugin(PluginOptions{Title: "Errors", Version: "1.0.0"}).From(apiPlugin)
	if err := apiPlugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("api Configure: %v", err)
	}
	if err := openapiPlugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("openapi Configure: %v", err)
	}

	op := openapiPlugin.Spec().Paths["/users/{id}"]["get"]
	for _, status := range []string{"400", "404", "409", "500"} {
		if _, present := op.Responses[status]; !present {
			t.Errorf("response %s missing; have %v", status, keys(op.Responses))
		}
	}
}

func TestOpenAPI_AutoDiscovery_PropagatesSecurityMetadata(t *testing.T) {
	server := &fakeServer{}
	apiPlugin := api.New(server)

	apiPlugin.Register(api.Endpoint("GET", "/admin").
		Secure(typedRule{roles: []string{"admin"}, scopes: []string{"users:read"}}).
		Handle(func(_ *phttp.EndpointContext) *phttp.Response { return phttp.JSON(nil) }))

	apiPlugin.Register(api.Endpoint("GET", "/public").
		Handle(func(_ *phttp.EndpointContext) *phttp.Response { return phttp.JSON(nil) }))

	openapiPlugin := NewPlugin(PluginOptions{Title: "Secure", Version: "1.0.0"}).From(apiPlugin)
	if err := apiPlugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("api Configure: %v", err)
	}
	if err := openapiPlugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("openapi Configure: %v", err)
	}

	spec := openapiPlugin.Spec()
	adminGet := spec.Paths["/admin"]["get"]
	if len(adminGet.Security) == 0 {
		t.Error("secured endpoint missing Security in OpenAPI operation")
	}
	if !strings.Contains(adminGet.Description, "admin") {
		t.Errorf("admin role not surfaced in description; got %q", adminGet.Description)
	}
	if !strings.Contains(adminGet.Description, "users:read") {
		t.Errorf("scope not surfaced in description; got %q", adminGet.Description)
	}

	publicGet := spec.Paths["/public"]["get"]
	if len(publicGet.Security) != 0 {
		t.Errorf("public endpoint should have no Security; got %v", publicGet.Security)
	}
}

// typedRule implements the optional phttp.SecurityClaims accessor (like the real
// security.Options), so the generator reads its roles/scopes through the typed
// path instead of reflecting on field names.
type typedRule struct {
	roles  []string
	scopes []string
}

func (typedRule) Middleware() phttp.Middleware {
	return func(_ *phttp.Context, next func() *phttp.Response) *phttp.Response { return next() }
}
func (r typedRule) SecurityRoles() []string  { return r.roles }
func (r typedRule) SecurityScopes() []string { return r.scopes }

func TestOpenAPI_AutoDiscovery_SecurityMetadataViaTypedAccessor(t *testing.T) {
	server := &fakeServer{}
	apiPlugin := api.New(server)
	apiPlugin.Register(api.Endpoint("GET", "/admin").
		Secure(typedRule{roles: []string{"admin"}, scopes: []string{"users:read"}}).
		Handle(func(_ *phttp.EndpointContext) *phttp.Response { return phttp.JSON(nil) }))

	openapiPlugin := NewPlugin(PluginOptions{Title: "Typed", Version: "1.0.0"}).From(apiPlugin)
	if err := apiPlugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("api Configure: %v", err)
	}
	if err := openapiPlugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("openapi Configure: %v", err)
	}

	adminGet := openapiPlugin.Spec().Paths["/admin"]["get"]
	if len(adminGet.Security) == 0 {
		t.Error("secured endpoint missing Security in OpenAPI operation")
	}
	if !strings.Contains(adminGet.Description, "admin") || !strings.Contains(adminGet.Description, "users:read") {
		t.Errorf("typed roles/scopes not surfaced in description; got %q", adminGet.Description)
	}
}

func TestOpenAPI_PluginOptions_SecuritySchemesOverrideDefault(t *testing.T) {
	server := &fakeServer{}
	apiPlugin := api.New(server)
	apiPlugin.Register(api.Endpoint("GET", "/admin").
		Secure(typedRule{roles: []string{"admin"}}).
		Handle(func(_ *phttp.EndpointContext) *phttp.Response { return phttp.JSON(nil) }))

	openapiPlugin := NewPlugin(PluginOptions{
		Title:   "Schemes",
		Version: "1.0.0",
		SecuritySchemes: map[string]SecuritySchemeObject{
			"customAuth": {Type: "http", Scheme: "bearer", Description: "custom scheme via the plugin"},
		},
	}).From(apiPlugin)
	if err := apiPlugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("api Configure: %v", err)
	}
	if err := openapiPlugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("openapi Configure: %v", err)
	}

	comps := openapiPlugin.Spec().Components
	if comps == nil || comps.SecuritySchemes == nil {
		t.Fatal("expected components.securitySchemes to be generated for a secured route")
	}
	if _, ok := comps.SecuritySchemes["customAuth"]; !ok {
		t.Errorf("PluginOptions.SecuritySchemes not threaded through Configure; have %v", keys(comps.SecuritySchemes))
	}
	if _, ok := comps.SecuritySchemes["bearerAuth"]; ok {
		t.Error("the default bearerAuth scheme should be replaced when SecuritySchemes is set")
	}

	// Every op.Security entry must reference a scheme that actually exists in
	// components.securitySchemes, otherwise the spec is invalid and codegen
	// drops auth. With the override the operation must point at customAuth, not
	// the now-undefined bearerAuth.
	adminGet := openapiPlugin.Spec().Paths["/admin"]["get"]
	if len(adminGet.Security) == 0 {
		t.Fatal("secured operation missing Security block")
	}
	for _, req := range adminGet.Security {
		for name := range req {
			if _, ok := comps.SecuritySchemes[name]; !ok {
				t.Errorf("op.Security references %q, absent from components.securitySchemes %v", name, keys(comps.SecuritySchemes))
			}
			if name == "bearerAuth" {
				t.Error("op.Security still references the replaced bearerAuth scheme")
			}
		}
	}
}

type apiModuleParams struct {
	Module string `json:"module" validate:"required"`
}

func TestOpenAPI_AutoDiscovery_CatchAllPathParam(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "catch-all-paths", "openapi-renders-a-catch-all-as-a-single-segment-parameter-describing-its-semantics")
	server := &fakeServer{}
	apiPlugin := api.New(server)

	apiPlugin.Register(api.Endpoint("POST", "/{module...}/-/blobs/upload").
		Description("Upload a blob for a multi-segment module path").
		Params(api.Type[apiModuleParams]()).
		HandleRaw(func(_ *phttp.Context) *phttp.Response { return phttp.NoContent() }))

	openapiPlugin := NewPlugin(PluginOptions{Title: "Mods", Version: "1.0.0"}).From(apiPlugin)
	if err := apiPlugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("api Configure: %v", err)
	}
	if err := openapiPlugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("openapi Configure: %v", err)
	}

	spec := openapiPlugin.Spec()

	// Path key must NOT contain the `...` marker — OpenAPI 3.0 only knows `{name}`.
	item, ok := spec.Paths["/{module}/-/blobs/upload"]
	if !ok {
		t.Fatalf("path /{module}/-/blobs/upload missing; paths = %v", keys(spec.Paths))
	}
	op, hasPost := item["post"]
	if !hasPost {
		t.Fatalf("POST operation missing for catch-all path")
	}

	// The single path parameter named "module" must be present, required, type=string.
	var moduleParam *Parameter
	for i := range op.Parameters {
		if op.Parameters[i].Name == "module" {
			moduleParam = &op.Parameters[i]
			break
		}
	}
	if moduleParam == nil {
		t.Fatalf("path parameter 'module' missing from operation; got %+v", op.Parameters)
	}
	if moduleParam.In != "path" || !moduleParam.Required {
		t.Errorf("module param shape wrong: %+v", moduleParam)
	}
	if moduleParam.Schema == nil || moduleParam.Schema.Type != "string" {
		t.Errorf("module param should be a string schema; got %+v", moduleParam.Schema)
	}
	if !strings.Contains(moduleParam.Description, "Multi-segment") {
		t.Errorf("multi-segment note missing from description; got %q", moduleParam.Description)
	}
	// operationId must be a clean identifier — no `...` leakage.
	if strings.Contains(op.OperationID, "...") {
		t.Errorf("operationId leaks catch-all marker: %q", op.OperationID)
	}
}

func TestPlugin_ServesPrecomputedSpecBytes(t *testing.T) {
	p := NewPlugin(PluginOptions{Title: "Cached", Version: "2.0.0"})
	if err := p.Configure(context.Background(), nil); err != nil {
		t.Fatalf("Configure: %v", err)
	}

	call := func() *phttp.Response {
		req := httptest.NewRequest("GET", "/_/openapi.json", nil)
		return p.handler()(phttp.NewContext(httptest.NewRecorder(), req))
	}

	resp := call()
	if resp.Status != 200 {
		t.Fatalf("status = %d, want 200", resp.Status)
	}
	if ct := resp.Headers.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	body, err := resp.BodyBytes()
	if err != nil {
		t.Fatalf("BodyBytes: %v", err)
	}
	// Served bytes must be the marshaled spec: valid JSON carrying the title.
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("served spec is not valid JSON: %v", err)
	}
	if !strings.Contains(string(body), `"Cached"`) {
		t.Errorf("served spec missing configured title; body = %s", body)
	}

	// Repeated requests serve identical, precomputed bytes.
	body2, err := call().BodyBytes()
	if err != nil {
		t.Fatalf("BodyBytes (2nd call): %v", err)
	}
	if string(body) != string(body2) {
		t.Errorf("served bytes differ across requests:\n first = %s\nsecond = %s", body, body2)
	}
}

// TestOpenAPI_AutoDiscovery_IncludesDocumentOnlyRoutes pins the documented half
// of the document-only contract. A document-only endpoint IS served — by a
// handler mounted directly on the transport — so leaving it out of the
// specification would publish an incomplete API. The Protobuf service and the
// Connect bridge deliberately go the other way, because they map an api-plugin
// handler a document-only endpoint does not have.
func TestOpenAPI_AutoDiscovery_IncludesDocumentOnlyRoutes(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "document-only", "a-document-only-route-is-published-in-the-openapi-document")
	server := &fakeServer{}
	apiPlugin := api.New(server)

	apiPlugin.Register(api.Endpoint("GET", "/manifest").
		Description("Mounted directly on the server, documented here").
		Returns(api.Type[apiUser]()).
		Document())

	openapiPlugin := NewPlugin(PluginOptions{Title: "Doc Only", Version: "1.0.0"}).From(apiPlugin)
	if err := apiPlugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("api Configure: %v", err)
	}
	if err := openapiPlugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("openapi Configure: %v", err)
	}

	// The transport never received the route.
	if len(server.calls) != 0 {
		t.Errorf("document-only endpoint must not bind a transport handler, got %v", server.calls)
	}

	spec := openapiPlugin.Spec()
	if spec == nil {
		t.Fatal("spec should be generated after Configure")
	}
	item, ok := spec.Paths["/manifest"]
	if !ok {
		t.Fatalf("document-only route missing from spec; paths = %v", keys(spec.Paths))
	}
	op, hasGet := item["get"]
	if !hasGet {
		t.Fatalf("GET operation missing for /manifest")
	}
	if op.Description != "Mounted directly on the server, documented here" {
		t.Errorf("description = %q, want the declared description", op.Description)
	}
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

type retiredWidget struct {
	ID string `json:"id" validate:"required"`
}

// describeWidgetProvider runs a real first-party provider through Describe and
// returns the published OpenAPI bytes and the staged Go client. retired adds a
// second route so the caller can compare a provider that has it against one
// that never did.
func describeWidgetProvider(t *testing.T, projectRoot string, retired bool) (spec, clientSource []byte) {
	t.Helper()
	apiPlugin := api.New(&fakeServer{},
		api.WithClientService(api.ClientServiceOptions{
			Service: clientcontract.Service{ID: "widgets", Audience: "https://widgets.internal"},
		}),
	)
	apiPlugin.Register(api.Endpoint("GET", "/widgets").
		Returns(api.Type[retiredWidget]()).
		Handle(func(_ *phttp.EndpointContext) *phttp.Response { return phttp.JSON(nil) }))
	if retired {
		apiPlugin.Register(api.Endpoint("GET", "/widgets/retired").
			Returns(api.Type[retiredWidget]()).
			Handle(func(_ *phttp.EndpointContext) *phttp.Response { return phttp.JSON(nil) }))
	}
	openapiPlugin := NewPlugin(PluginOptions{Title: "Widgets", Version: "1.0.0"}).From(apiPlugin)
	clientsPlugin := api.Clients(api.ClientsOptions{
		Go: api.GoClientOptions{PackageName: "widgetsclient", ClientName: "WidgetsClient"},
	}).From(apiPlugin)
	application := app.New("widget-provider")
	application.Use(apiPlugin).Use(openapiPlugin).Use(clientsPlugin)

	genDir := filepath.Join(projectRoot, ".gen")
	if err := application.Describe(genDir, []string{"openapi", "clients"}); err != nil {
		t.Fatalf("Describe: %v", err)
	}
	spec, err := os.ReadFile(filepath.Join(genDir, DefaultDescribePath))
	if err != nil {
		t.Fatalf("read described spec: %v", err)
	}
	clientSource, err = os.ReadFile(filepath.Join(genDir, api.ClientStageDir, "go", "client.gen.go"))
	if err != nil {
		t.Fatalf("read staged client: %v", err)
	}
	return spec, clientSource
}

// A route the provider stops registering must vanish from every surface the
// build produces, byte for byte: the described document, the staged client, and
// the client the workspace generator rewrites over the previous one. The
// publication half of the same invariant lives in the Go extension's
// openapiutil tests, which own the merge.
func TestOpenAPI_RemovedRouteDoesNotSurviveDescribeOrClientGeneration(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "generated-from-contract", "a-route-the-provider-removed-leaves-no-residue-in-the-generated-client")

	// A provider that never declared the route: the reference bytes.
	freshSpec, freshClient := describeWidgetProvider(t, t.TempDir(), false)

	// A provider that declared it, published, generated — then removed it.
	projectRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(projectRoot, "go.mod"), []byte("module example.dev/widgets\n\ngo 1.25\n"), 0o600); err != nil {
		t.Fatalf("write provider go.mod: %v", err)
	}
	retiredSpec, _ := describeWidgetProvider(t, projectRoot, true)
	if !bytes.Contains(retiredSpec, []byte("/widgets/retired")) {
		t.Fatalf("fixture did not declare the retired route:\n%s", retiredSpec)
	}
	if err := os.MkdirAll(filepath.Dir(filepath.Join(projectRoot, DefaultDescribePath)), 0o750); err != nil {
		t.Fatalf("prepare published schema: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projectRoot, DefaultDescribePath), retiredSpec, 0o600); err != nil {
		t.Fatalf("publish schema: %v", err)
	}
	if _, err := api.GenerateProjectClients(projectRoot); err != nil {
		t.Fatalf("GenerateProjectClients (with retired route): %v", err)
	}
	generatedPath := filepath.Join(projectRoot, "clients", "go", "client.gen.go")
	withRetired, err := os.ReadFile(generatedPath)
	if err != nil {
		t.Fatalf("read generated client: %v", err)
	}
	if !bytes.Contains(withRetired, []byte("ListWidgetsRetired")) {
		t.Fatalf("fixture client has no method for the retired route:\n%s", withRetired)
	}

	// Re-describe without the route, publish, and regenerate over the existing
	// client and its ownership manifest.
	trimmedSpec, trimmedStaged := describeWidgetProvider(t, projectRoot, false)
	if !bytes.Equal(trimmedSpec, freshSpec) {
		t.Fatalf("describe after removal differs from a provider that never declared the route\nafter removal:\n%s\nnever declared:\n%s", trimmedSpec, freshSpec)
	}
	if !bytes.Equal(trimmedStaged, freshClient) {
		t.Fatalf("staged client after removal differs from a provider that never declared the route\nafter removal:\n%s\nnever declared:\n%s", trimmedStaged, freshClient)
	}
	if err := os.WriteFile(filepath.Join(projectRoot, DefaultDescribePath), trimmedSpec, 0o600); err != nil {
		t.Fatalf("publish trimmed schema: %v", err)
	}
	if _, err := api.GenerateProjectClients(projectRoot); err != nil {
		t.Fatalf("GenerateProjectClients (after removal): %v", err)
	}
	regenerated, err := os.ReadFile(generatedPath)
	if err != nil {
		t.Fatalf("read regenerated client: %v", err)
	}
	if bytes.Contains(regenerated, []byte("ListWidgetsRetired")) || bytes.Contains(regenerated, []byte("/widgets/retired")) {
		t.Fatalf("removed route survived client regeneration:\n%s", regenerated)
	}
	if !bytes.Equal(regenerated, trimmedStaged) {
		t.Fatalf("workspace client differs from the staged client after removal\nworkspace:\n%s\nstaged:\n%s", regenerated, trimmedStaged)
	}
}

type searchQueryBody struct {
	Query string `json:"query" validate:"required"`
}

// A first-party provider may not publish a GET or HEAD request body: the client
// it generates would send a payload the endpoint pipeline never reads and any
// intermediary may drop. Non first-party providers keep main's tolerance, which
// the api package's own body-method tests still pin.
func TestOpenAPI_FirstPartyRefusesARequestBodyOnBodylessMethods(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "generation-refuses-unhonorable-declarations", "a-first-party-request-body-on-get-or-head-is-refused-at-generation")

	apiPlugin := api.New(&fakeServer{},
		api.WithClientService(api.ClientServiceOptions{
			Service: clientcontract.Service{ID: "search", Audience: "https://search.internal"},
		}),
	)
	apiPlugin.Register(api.Endpoint("GET", "/search").
		Body(api.Type[searchQueryBody]()).
		Returns(api.Type[searchQueryBody]()).
		Handle(func(_ *phttp.EndpointContext) *phttp.Response { return phttp.JSON(nil) }))
	openapiPlugin := NewPlugin(PluginOptions{Title: "Search", Version: "1.0.0"}).From(apiPlugin)
	if err := apiPlugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("api Configure: %v", err)
	}
	err := openapiPlugin.Configure(context.Background(), nil)
	if err == nil {
		t.Fatal("openapi Configure published a GET request body")
	}
	for _, want := range []string{"GET /search", "carries none", "query or path parameters"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Configure error = %v, want it to contain %q", err, want)
		}
	}
}

type boundedCounters struct {
	Sequence uint64 `json:"sequence" validate:"required"`
	Small    int32  `json:"small" validate:"required"`
	Capped   int64  `json:"capped" validate:"required,min=1,max=100"`
	Wide     int64  `json:"wide" validate:"required,min=-99999999999999999999"`
}

// D0.2: a first-party integer states its width twice — as the declared format
// and as exact decimal bounds — so a reader that ignores format still sees the
// range, and no bound is rounded through a float. An author bound narrower than
// the format's natural range wins; a wider one is clamped to it.
func TestOpenAPI_FirstPartyIntegersCarryExactDeclaredBounds(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "generation-refuses-unhonorable-declarations", "a-first-party-integer-declares-its-width-and-its-exact-bounds")

	apiPlugin := api.New(&fakeServer{},
		api.WithClientService(api.ClientServiceOptions{
			Service: clientcontract.Service{ID: "counters", Audience: "https://counters.internal"},
		}),
	)
	apiPlugin.Register(api.Endpoint("GET", "/counters").
		Returns(api.Type[boundedCounters]()).
		Handle(func(_ *phttp.EndpointContext) *phttp.Response { return phttp.JSON(nil) }))
	openapiPlugin := NewPlugin(PluginOptions{Title: "Counters", Version: "1.0.0"}).From(apiPlugin)
	if err := apiPlugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("api Configure: %v", err)
	}
	if err := openapiPlugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("openapi Configure: %v", err)
	}
	schema := openapiPlugin.Spec().Components.Schemas["boundedCounters"]

	tests := []struct {
		field   string
		format  string
		minimum string
		maximum string
	}{
		{field: "sequence", format: "uint64", minimum: "0", maximum: "18446744073709551615"},
		{field: "small", format: "int32", minimum: "-2147483648", maximum: "2147483647"},
		{field: "capped", format: "int64", minimum: "1", maximum: "100"},
		{field: "wide", format: "int64", minimum: "-9223372036854775808", maximum: "9223372036854775807"},
	}
	for _, tc := range tests {
		t.Run(tc.field, func(t *testing.T) {
			property := schema.Properties[tc.field]
			if property.Type != "integer" || property.Format != tc.format {
				t.Fatalf("width = %q/%q, want integer/%s", property.Type, property.Format, tc.format)
			}
			if property.Minimum == nil || property.Minimum.String() != tc.minimum {
				t.Fatalf("minimum = %#v, want %s", property.Minimum, tc.minimum)
			}
			if property.Maximum == nil || property.Maximum.String() != tc.maximum {
				t.Fatalf("maximum = %#v, want %s", property.Maximum, tc.maximum)
			}
		})
	}
}

// interopEvent is the provider message shape of the Go-to-Go WebSocket cell.
// Payload is always set: the published wire refuses a JSON null anywhere in a
// frame, so an unset byte slice would not survive the round trip.
type interopEvent struct {
	ID      string `json:"id"`
	Payload []byte `json:"payload"`
}

// interopSummary is the single declared value the client stream returns.
type interopSummary struct {
	Count int `json:"count"`
}

// interopProvider stands up the real first-party provider on a real port and
// returns the descriptor a generated client embeds, read back from the
// provider's own published document.
func interopProvider(t *testing.T) (*client.Client, map[string]client.Operation) {
	t.Helper()
	httpServer := phttp.NewServerPlugin(phttp.ServerConfig{})
	apiPlugin := api.New(httpServer, api.WithClientService(api.ClientServiceOptions{
		Service: clientcontract.Service{ID: "interop", Audience: "https://interop.internal"},
	}))
	apiPlugin.Register(api.Endpoint("GET", "/watch").
		Returns(api.StreamOf[interopEvent]()).
		Handle(api.ServerStream(func(stream *api.ServerStreamContext[interopEvent]) error {
			if err := stream.Send(interopEvent{ID: "one", Payload: []byte("ab")}); err != nil {
				return err
			}
			return stream.Send(interopEvent{ID: "two", Payload: []byte{}})
		})))
	apiPlugin.Register(api.Endpoint("GET", "/upload").
		Body(api.StreamOf[interopEvent]()).
		Returns(api.Type[interopSummary]()).
		Handle(api.ClientStream(func(stream *api.ClientStreamContext[interopEvent, interopSummary]) error {
			total := 0
			for range stream.Messages() {
				total++
			}
			if err := stream.Err(); err != nil {
				return err
			}
			stream.Result(interopSummary{Count: total})
			return nil
		})))
	// A unary operation whose declared error the client must recognize. The
	// provider writes the first-party envelope; the contract declares the code
	// and, per ADR 0006, no details schema, because this endpoint sends none.
	apiPlugin.Register(api.Endpoint("GET", "/missing").
		Returns(api.Type[interopSummary]()).
		MayThrow(perrors.CodeNotFound).
		Handle(func(_ *phttp.EndpointContext) *phttp.Response {
			return phttp.ErrorResponse(perrors.NotFound("no such item"))
		}))
	apiPlugin.Register(api.Endpoint("GET", "/chat").
		Body(api.StreamOf[interopEvent]()).
		Returns(api.StreamOf[interopEvent]()).
		Handle(api.BidiStream(func(stream *api.BidiStreamContext[interopEvent, interopEvent]) error {
			for message := range stream.Messages() {
				if err := stream.Send(interopEvent{ID: "ack:" + message.ID, Payload: []byte{}}); err != nil {
					return err
				}
			}
			if err := stream.Err(); err != nil {
				return err
			}
			stream.Result(interopEvent{ID: "final", Payload: []byte{}})
			return nil
		})))

	openapiPlugin := NewPlugin(PluginOptions{Title: "Interop", Version: "1.0.0"}).From(apiPlugin)
	if err := apiPlugin.Configure(t.Context(), nil); err != nil {
		t.Fatalf("api Configure: %v", err)
	}
	if err := openapiPlugin.Configure(t.Context(), nil); err != nil {
		t.Fatalf("openapi Configure: %v", err)
	}
	raw, err := openapiPlugin.OpenAPISpecJSON()
	if err != nil {
		t.Fatalf("publish provider document: %v", err)
	}
	ir, err := api.ReadOpenAPISpec(raw)
	if err != nil {
		t.Fatalf("strict reader rejected the provider document: %v", err)
	}
	if ir.Contract == nil {
		t.Fatal("the provider document carries no first-party client contract")
	}
	operations := map[string]client.Operation{}
	for _, service := range ir.Services {
		for _, method := range service.Methods {
			if method.Client == nil {
				continue
			}
			operations[method.OperationID] = client.Operation{ID: method.OperationID, Contract: *method.Client}
		}
	}

	server := httptest.NewServer(httpServer.Handler())
	t.Cleanup(server.Close)
	bound, err := client.NewServiceClientBinding(client.ServiceBinding{
		URL: server.URL, ClientID: "interop.consumer", AllowInsecure: true,
	}, client.ServiceDescriptor{Contract: *ir.Contract, Schemas: ir.Schemas})
	if err != nil {
		t.Fatalf("bind the generated client: %v", err)
	}
	return bound, operations
}

// The first-party WebSocket cell, end to end and in one process: the Go
// provider declares the streams, the published document is read back into the
// descriptor a generated client embeds, and the first-party Go client runtime
// drives all three directions over a real socket on a real port. Neither side
// is scripted — the provider is go.putnami.dev/api on go.putnami.dev/http, and
// the consumer is go.putnami.dev/client.
// TestOpenAPI_AGoProviderDeclaredErrorArrivesTypedAtTheGoClient is the end of the
// ADR 0006 chain, run against a real provider on a real socket: declare with
// MayThrow, project the contract, read it with the strict reader, bind the real
// client runtime, and call. The provider used to declare the `{code, error,
// message}` envelope as the error's details schema, so the client validated the
// envelope against `details`, found none, and reported client.response instead of
// the declared code.
func TestOpenAPI_AGoProviderDeclaredErrorArrivesTypedAtTheGoClient(t *testing.T) {
	bound, operations := interopProvider(t)
	operation, ok := operations["getMissing"]
	if !ok {
		t.Fatalf("the provider document declares no getMissing operation: %v", operationIDs(operations))
	}
	_, err := client.Call[interopSummary](t.Context(), bound, &client.Request{Path: "/missing"}, operation)
	var remote *client.RemoteError
	if !errors.As(err, &remote) {
		t.Fatalf("declared provider error = %T %v, want a typed *client.RemoteError", err, err)
	}
	if remote.Code() != string(perrors.CodeNotFound) || remote.StatusCode != http.StatusNotFound {
		t.Fatalf("typed error = %q/%d, want %q/404", remote.Code(), remote.StatusCode, perrors.CodeNotFound)
	}
	if remote.Payload != nil {
		t.Fatalf("no details were declared, so none may be published: %s", remote.Payload)
	}

	for _, declared := range operation.Contract.Errors {
		if declared.Schema != nil {
			t.Errorf("declared error %d/%q carries a details schema the provider never sends: %#v",
				declared.Status, declared.Code, declared.Schema.Type+declared.Schema.Ref)
		}
	}
}

func operationIDs(operations map[string]client.Operation) []string {
	ids := make([]string, 0, len(operations))
	for id := range operations {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func TestOpenAPI_GoProviderServesTheFirstPartyGoWebSocketClient(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "first-party-websocket-streams", "a-go-provider-serves-the-first-party-go-websocket-client")
	bound, operations := interopProvider(t)

	t.Run("server stream", func(t *testing.T) {
		operation, ok := operations["getWatch"]
		if !ok {
			t.Fatal("the provider document declares no getWatch operation")
		}
		stream, err := client.OpenServerStreamWS[interopEvent](t.Context(), bound, &client.Request{}, operation)
		if err != nil {
			t.Fatalf("OpenServerStreamWS: %v", err)
		}
		got := make([]interopEvent, 0, 2)
		for message := range stream.Messages() {
			got = append(got, message)
		}
		if err := stream.Err(); err != nil {
			t.Fatalf("server stream terminal error: %v", err)
		}
		want := []interopEvent{{ID: "one", Payload: []byte("ab")}, {ID: "two", Payload: []byte{}}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("delivered = %#v, want %#v", got, want)
		}
	})

	t.Run("client stream", func(t *testing.T) {
		operation, ok := operations["getUpload"]
		if !ok {
			t.Fatal("the provider document declares no getUpload operation")
		}
		stream, err := client.OpenClientStream[interopEvent, interopSummary](
			t.Context(), bound, &client.Request{}, operation)
		if err != nil {
			t.Fatalf("OpenClientStream: %v", err)
		}
		defer stream.Close() //nolint:errcheck // Close is idempotent and returns nil
		for _, id := range []string{"a", "b", "c"} {
			if err := stream.Send(t.Context(), interopEvent{ID: id, Payload: []byte{}}); err != nil {
				t.Fatalf("Send %s: %v", id, err)
			}
		}
		if err := stream.CloseSend(); err != nil {
			t.Fatalf("CloseSend: %v", err)
		}
		summary, err := stream.Result(t.Context())
		if err != nil {
			t.Fatalf("Result: %v", err)
		}
		if summary.Count != 3 {
			t.Fatalf("summary = %#v, want three uploaded messages", summary)
		}
	})

	t.Run("bidirectional stream", func(t *testing.T) {
		operation, ok := operations["getChat"]
		if !ok {
			t.Fatal("the provider document declares no getChat operation")
		}
		stream, err := client.OpenBidiStream[interopEvent, interopEvent](
			t.Context(), bound, &client.Request{}, operation)
		if err != nil {
			t.Fatalf("OpenBidiStream: %v", err)
		}
		defer stream.Close() //nolint:errcheck // Close is idempotent and returns nil
		if err := stream.Send(t.Context(), interopEvent{ID: "hello", Payload: []byte{}}); err != nil {
			t.Fatalf("Send: %v", err)
		}
		ack, err := stream.Recv(t.Context())
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		if ack.ID != "ack:hello" {
			t.Fatalf("provider ack = %#v", ack)
		}
		if err := stream.CloseSend(); err != nil {
			t.Fatalf("CloseSend: %v", err)
		}
		final, err := stream.Recv(t.Context())
		if err != nil {
			t.Fatalf("terminal Recv: %v", err)
		}
		if final.ID != "final" {
			t.Fatalf("terminal value = %#v, want the declared final value", final)
		}
		if _, err := stream.Recv(t.Context()); !errors.Is(err, io.EOF) {
			t.Fatalf("Recv after the terminal value = %v, want io.EOF", err)
		}
	})
}

// --- Connect transport projection ---

// configureFirstPartyConnectProvider builds a first-party provider with one
// unary route and one server stream, publishing the protobuf projection and the
// Connect encodings a mounted bridge would publish.
func configureFirstPartyConnectProvider(t *testing.T, mountBridge bool) *Document {
	t.Helper()
	server := &fakeServer{}
	apiPlugin := api.New(server, api.WithClientService(api.ClientServiceOptions{
		Service: clientcontract.Service{ID: "users", Audience: "https://users.internal"},
	}))
	apiPlugin.Register(api.Endpoint("GET", "/users/{id}").
		Params(api.Type[apiUserParams]()).
		Returns(api.Type[apiFirstPartyUser]()).
		Document())
	apiPlugin.Register(api.Endpoint("GET", "/users/watch").
		Returns(api.StreamOf[apiFirstPartyUser]()).
		Document())

	openapiPlugin := NewPlugin(PluginOptions{Title: "Users", Version: "1.0.0"}).From(apiPlugin)
	if err := apiPlugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("api Configure: %v", err)
	}
	apiPlugin.PublishClientProtobuf(api.ClientProtobufProjection{
		Descriptor: &clientcontract.ProtobufDescriptor{
			Syntax:  "proto3",
			Package: "users.v1",
			Services: []clientcontract.ProtobufService{{Name: "ApiService", Methods: []clientcontract.ProtobufMethod{
				{Name: "GetUsers", Input: "GetUsersRequest", Output: "GetUsersReply"},
				{Name: "WatchUsers", Input: "WatchUsersRequest", Output: "WatchUsersReply", ServerStreaming: true},
			}}},
			Messages: []clientcontract.ProtobufMessage{
				{Name: "GetUsersRequest", Fields: []clientcontract.ProtobufField{}},
				{Name: "GetUsersReply", Fields: []clientcontract.ProtobufField{}},
				{Name: "WatchUsersRequest", Fields: []clientcontract.ProtobufField{}},
				{Name: "WatchUsersReply", Fields: []clientcontract.ProtobufField{}},
			},
			Enums: []clientcontract.ProtobufEnum{},
		},
		RouteMethods: map[string]string{
			"GET /users/{id}":  "/users.v1.ApiService/GetUsers",
			"GET /users/watch": "/users.v1.ApiService/WatchUsers",
		},
	})
	if mountBridge {
		apiPlugin.PublishClientConnectTransport([]clientcontract.Encoding{clientcontract.EncodingJSON, clientcontract.EncodingProto})
	}
	if err := openapiPlugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("openapi Configure: %v", err)
	}
	return openapiPlugin.Spec()
}

// A Connect transport is advertised for a unary route whose method the
// descriptor declares and whose bridge is mounted. Its path is the method
// identity, because a Connect URL is the method.
func TestOpenAPI_FirstPartyAnnouncesConnectWhenTheProviderServesIt(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "connect-descriptor-parity", "connect-is-advertised-only-when-a-mounted-bridge-serves-it")
	spec := configureFirstPartyConnectProvider(t, true)

	unary := spec.Paths["/users/{id}"]["get"].ClientContract
	if unary == nil || len(unary.Transports) != 3 {
		t.Fatalf("unary transports = %#v, want rest-json then connect over json and proto", unary)
	}
	if unary.Transports[0].Protocol != clientcontract.TransportRESTJSON {
		t.Errorf("first transport = %#v, want the endpoint's own REST URL", unary.Transports[0])
	}
	connect := unary.Transports[1]
	if connect.Protocol != clientcontract.TransportConnect || connect.Encoding != clientcontract.EncodingJSON {
		t.Fatalf("second transport = %#v, want connect over json", connect)
	}
	if connect.Path != "/users.v1.ApiService/GetUsers" || connect.ProtobufMethod != connect.Path {
		t.Errorf("connect path = %q, protobufMethod = %q; both are the declared method identity", connect.Path, connect.ProtobufMethod)
	}
	// proto is advertised because the bridge published it, and only then: the
	// encodings come from the mounted server, never from the descriptor alone.
	binary := unary.Transports[2]
	if binary.Protocol != clientcontract.TransportConnect || binary.Encoding != clientcontract.EncodingProto {
		t.Fatalf("third transport = %#v, want connect over proto", binary)
	}
	if binary.Path != connect.Path || binary.ProtobufMethod != connect.Path {
		t.Errorf("proto transport path = %q, want the same method identity %q", binary.Path, connect.Path)
	}

	// A declared server stream advertises Connect after the transports a browser
	// already speaks, because the bridge now serves enveloped streamed messages.
	stream := spec.Paths["/users/watch"]["get"].ClientContract
	if len(stream.Transports) != 4 {
		t.Fatalf("stream transports = %#v, want sse, websocket, connect json, connect proto", stream.Transports)
	}
	if stream.Transports[0].Protocol != clientcontract.TransportSSE || stream.Transports[1].Protocol != clientcontract.TransportWebSocket {
		t.Errorf("stream transports = %#v, want SSE and WebSocket declared first", stream.Transports)
	}
	for index, encoding := range []clientcontract.Encoding{clientcontract.EncodingJSON, clientcontract.EncodingProto} {
		transport := stream.Transports[2+index]
		if transport.Protocol != clientcontract.TransportConnect || transport.Encoding != encoding {
			t.Fatalf("stream transport %d = %#v, want connect over %s", 2+index, transport, encoding)
		}
		if transport.Path != "/users.v1.ApiService/WatchUsers" || transport.ProtobufMethod != transport.Path {
			t.Errorf("stream connect path = %q, want the declared method identity", transport.Path)
		}
	}
}

// Connect carries at most one request message per call, so a declared
// conversation has no Connect shape: advertising one would hand a generated
// client a URL that answers a duplex stream with a single buffered reply.
func TestOpenAPI_FirstPartyWithholdsConnectForADuplexStream(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "connect-descriptor-parity", "connect-is-advertised-only-when-a-mounted-bridge-serves-it")
	server := &fakeServer{}
	apiPlugin := api.New(server, api.WithClientService(api.ClientServiceOptions{
		Service: clientcontract.Service{ID: "users", Audience: "https://users.internal"},
	}))
	apiPlugin.Register(api.Endpoint("GET", "/users/chat").
		Body(api.StreamOf[apiFirstPartyUser]()).
		Returns(api.StreamOf[apiFirstPartyUser]()).
		Document())
	openapiPlugin := NewPlugin(PluginOptions{Title: "Users", Version: "1.0.0"}).From(apiPlugin)
	if err := apiPlugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("api Configure: %v", err)
	}
	apiPlugin.PublishClientProtobuf(api.ClientProtobufProjection{
		Descriptor: &clientcontract.ProtobufDescriptor{
			Syntax:  "proto3",
			Package: "users.v1",
			Services: []clientcontract.ProtobufService{{Name: "ApiService", Methods: []clientcontract.ProtobufMethod{
				{Name: "ChatUsers", Input: "ChatUsersRequest", Output: "ChatUsersReply", ClientStreaming: true, ServerStreaming: true},
			}}},
			Messages: []clientcontract.ProtobufMessage{
				{Name: "ChatUsersRequest", Fields: []clientcontract.ProtobufField{}},
				{Name: "ChatUsersReply", Fields: []clientcontract.ProtobufField{}},
			},
			Enums: []clientcontract.ProtobufEnum{},
		},
		RouteMethods: map[string]string{"GET /users/chat": "/users.v1.ApiService/ChatUsers"},
	})
	apiPlugin.PublishClientConnectTransport([]clientcontract.Encoding{clientcontract.EncodingJSON, clientcontract.EncodingProto})
	if err := openapiPlugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("openapi Configure: %v", err)
	}
	contract := openapiPlugin.Spec().Paths["/users/chat"]["get"].ClientContract
	for _, transport := range contract.Transports {
		if transport.Protocol == clientcontract.TransportConnect {
			t.Fatalf("a bidirectional stream must not advertise Connect: %#v", contract.Transports)
		}
	}
}

// Without a mounted bridge the descriptor alone proves nothing about what the
// server answers, so no Connect transport is published.
func TestOpenAPI_FirstPartyWithholdsConnectWithoutAMountedBridge(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "connect-descriptor-parity", "a-descriptor-alone-does-not-advertise-connect")
	spec := configureFirstPartyConnectProvider(t, false)
	unary := spec.Paths["/users/{id}"]["get"].ClientContract
	if len(unary.Transports) != 1 || unary.Transports[0].Protocol != clientcontract.TransportRESTJSON {
		t.Fatalf("unary transports = %#v, want rest-json only", unary.Transports)
	}
}
