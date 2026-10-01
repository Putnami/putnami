package api

import (
	"context"
	"testing"

	phttp "go.putnami.dev/http"
	clientcontract "go.putnami.dev/protocol/clientcontract"
)

func TestWithClientServiceCarriesDefensiveFirstPartyContract(t *testing.T) {
	timeoutMs := 1_000
	maxAttempts := 3
	credentials := map[string]clientcontract.CredentialProfile{
		"workload": {Kind: clientcontract.CredentialServiceToken, Scopes: []string{"items:read"}},
	}
	plugin := New(&fakeServer{}, WithClientService(ClientServiceOptions{
		Service:     clientcontract.Service{ID: "items", Audience: "https://items.internal"},
		Credentials: credentials,
		Defaults: &clientcontract.Defaults{Resilience: &clientcontract.ResiliencePolicy{
			TimeoutMs: &timeoutMs,
			Retry:     &clientcontract.RetryPolicy{MaxAttempts: &maxAttempts},
		}},
	}))

	// Mutating the authoring input after construction must not change the
	// provider contract observed by OpenAPI or the client generator.
	credentials["workload"] = clientcontract.CredentialProfile{Kind: clientcontract.CredentialForwardedUserToken}
	timeoutMs = 9_000
	maxAttempts = 9
	contract := plugin.ClientServiceContract()
	if contract == nil {
		t.Fatal("client service contract is nil")
	}
	if contract.ProtocolVersion != clientcontract.ProtocolVersion {
		t.Fatalf("protocolVersion = %d, want %d", contract.ProtocolVersion, clientcontract.ProtocolVersion)
	}
	if contract.Service.ID != "items" || contract.Service.Audience != "https://items.internal" {
		t.Fatalf("service = %+v", contract.Service)
	}
	if got := contract.Credentials["workload"]; got.Kind != "service-token" || len(got.Scopes) != 1 {
		t.Fatalf("credential profile = %+v", got)
	}
	if got := *contract.Defaults.Resilience.TimeoutMs; got != 1_000 {
		t.Fatalf("default timeout aliases authoring input: got %d", got)
	}
	if got := *contract.Defaults.Resilience.Retry.MaxAttempts; got != 3 {
		t.Fatalf("retry attempts alias authoring input: got %d", got)
	}

	// Mutating a returned value must likewise leave the declaration unchanged.
	contract.Credentials["workload"] = clientcontract.CredentialProfile{Kind: clientcontract.CredentialAPIKey, Header: "X-Key"}
	*contract.Defaults.Resilience.TimeoutMs = 12_000
	*contract.Defaults.Resilience.Retry.MaxAttempts = 12
	if got := plugin.ClientServiceContract().Credentials["workload"].Kind; got != "service-token" {
		t.Fatalf("returned contract aliases provider state: kind = %q", got)
	}
	if got := *plugin.ClientServiceContract().Defaults.Resilience.TimeoutMs; got != 1_000 {
		t.Fatalf("returned contract aliases provider timeout: got %d", got)
	}
	if got := *plugin.ClientServiceContract().Defaults.Resilience.Retry.MaxAttempts; got != 3 {
		t.Fatalf("returned contract aliases provider retry attempts: got %d", got)
	}
}

func TestEndpointClientPolicySurvivesRouteDiscoveryWithoutAliasing(t *testing.T) {
	security := clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{
		AllOf: []clientcontract.SecurityRequirement{{Profile: "workload", Scopes: []string{"items:read"}}},
	}}}
	definition := Endpoint("GET", "/items").
		Client(ClientOperationOptions{Security: security}).
		HandleRaw(func(_ *phttp.Context) *phttp.Response { return phttp.JSON(nil) })
	security.Alternatives[0].AllOf[0].Scopes[0] = "mutated"

	plugin := New(&fakeServer{})
	plugin.Register(definition)
	if err := plugin.Configure(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	routes := plugin.DiscoveredRoutes()
	if len(routes) != 1 || routes[0].Meta.ClientOptions == nil {
		t.Fatalf("discovered client policy = %+v", routes)
	}
	got := routes[0].Meta.ClientOptions.Security.Alternatives[0].AllOf[0]
	if got.Profile != "workload" || len(got.Scopes) != 1 || got.Scopes[0] != "items:read" {
		t.Fatalf("security requirement = %+v", got)
	}

	returned := definition.ClientOptions()
	returned.Security.Alternatives[0].AllOf[0].Scopes[0] = "changed"
	if got := definition.ClientOptions().Security.Alternatives[0].AllOf[0].Scopes[0]; got != "items:read" {
		t.Fatalf("returned operation policy aliases endpoint state: scope = %q", got)
	}
}

func TestWithClientServiceNormalizesAnonymousCredentialRegistry(t *testing.T) {
	plugin := New(&fakeServer{}, WithClientService(ClientServiceOptions{
		Service: clientcontract.Service{ID: "public", Audience: "https://public.internal"},
	}))

	contract := plugin.ClientServiceContract()
	if contract.Credentials == nil || len(contract.Credentials) != 0 {
		t.Fatalf("credentials = %#v, want a non-nil empty registry", contract.Credentials)
	}
}

func TestPublishClientProtobufIsFirstPartyAndDefensive(t *testing.T) {
	plugin := New(&fakeServer{}, WithClientService(ClientServiceOptions{
		Service: clientcontract.Service{ID: "rpc", Audience: "urn:rpc"},
	}))
	descriptor := &clientcontract.ProtobufDescriptor{
		Syntax:  "proto3",
		Package: "rpc.v1",
		Services: []clientcontract.ProtobufService{{
			Name:    "RpcService",
			Methods: []clientcontract.ProtobufMethod{{Name: "Get", Input: "GetRequest", Output: "GetReply"}},
		}},
		Messages: []clientcontract.ProtobufMessage{
			{Name: "GetRequest", Fields: []clientcontract.ProtobufField{}},
			{Name: "GetReply", Fields: []clientcontract.ProtobufField{}},
		},
		Enums: []clientcontract.ProtobufEnum{},
	}
	plugin.PublishClientProtobuf(ClientProtobufProjection{Descriptor: descriptor})
	descriptor.Services[0].Methods[0].Name = "Mutated"

	returned := plugin.ClientServiceContract()
	if got := returned.Protobuf.Services[0].Methods[0].Name; got != "Get" {
		t.Fatalf("published protobuf descriptor aliases input: %q", got)
	}
	returned.Protobuf.Services[0].Methods[0].Name = "Changed"
	if got := plugin.ClientServiceContract().Protobuf.Services[0].Methods[0].Name; got != "Get" {
		t.Fatalf("returned protobuf descriptor aliases provider state: %q", got)
	}

	unmarked := New(&fakeServer{})
	unmarked.PublishClientProtobuf(ClientProtobufProjection{Descriptor: descriptor})
	if got := unmarked.ClientServiceContract(); got != nil {
		t.Fatalf("unmarked provider unexpectedly published client contract: %#v", got)
	}
}

// A service whose every route the proto projection left out (opaque JSON has no
// lossless proto3 form) publishes an empty method list. The defensive copy must
// keep it a present, empty list: folding it into nil made the provider's own
// contract fail validation as a missing field.
func TestPublishClientProtobufKeepsAnEmptyMethodListPresent(t *testing.T) {
	plugin := New(&fakeServer{}, WithClientService(ClientServiceOptions{
		Service: clientcontract.Service{ID: "audit", Audience: "urn:audit"},
	}))
	plugin.PublishClientProtobuf(ClientProtobufProjection{Descriptor: &clientcontract.ProtobufDescriptor{
		Syntax:   "proto3",
		Package:  "audit.v1",
		Services: []clientcontract.ProtobufService{{Name: "ApiService", Methods: []clientcontract.ProtobufMethod{}}},
		Messages: []clientcontract.ProtobufMessage{},
		Enums:    []clientcontract.ProtobufEnum{},
	}, RouteMethods: map[string]string{}})
	contract := plugin.ClientServiceContract()
	if contract.Protobuf.Services[0].Methods == nil {
		t.Fatalf("the copy folded an empty list into an absent one: %+v", contract.Protobuf)
	}
	if diags := clientcontract.ValidateDocument(contract); len(diags) != 0 {
		t.Fatalf("a descriptor with no method fails validation: %v", diags)
	}
}
