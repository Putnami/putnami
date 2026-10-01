package grpc

import (
	"encoding/json"
	"strings"
	"testing"

	"go.putnami.dev/api"
	phttp "go.putnami.dev/http"
	"go.putnami.dev/openapi"
	"go.putnami.dev/proto"
	clientcontract "go.putnami.dev/protocol/clientcontract"
)

type opaqueOnlyRecord struct {
	Value      any                        `json:"value" validate:"required"`
	Payload    json.RawMessage            `json:"payload"`
	Attributes map[string]json.RawMessage `json:"attributes"`
}

// When every route of a provider carries opaque JSON, the proto plugin still
// publishes a descriptor — one that binds no route. That descriptor decides:
// the bridge mounts no Connect URL, Start succeeds, the route keeps its REST
// URL, and the contract declares REST alone. Falling back to computed names
// because the route map is empty would serve methods no descriptor declares.
func TestApiBridge_AProviderWhoseOnlyRouteCarriesOpaqueJSONServesNoConnectURL(t *testing.T) {
	server := newFakeServer()
	apiPlugin := api.New(server, api.WithClientService(api.ClientServiceOptions{
		Service: clientcontract.Service{ID: "audit", Audience: "urn:audit"},
	}))
	apiPlugin.Register(api.Endpoint("POST", "/audit").
		Body(api.Type[opaqueOnlyRecord]()).
		Returns(api.Type[opaqueOnlyRecord]()).
		Handle(func(_ *phttp.EndpointContext) *phttp.Response { return phttp.JSON(opaqueOnlyRecord{}) }))
	protoPlugin := proto.NewPlugin(proto.PluginOptions{PackageName: "audit.v1"}).From(apiPlugin)
	bridge := NewApiBridge(apiPlugin, server, WithPackage("audit.v1"))
	openapiPlugin := openapi.NewPlugin(openapi.PluginOptions{Title: "Audit", Version: "1.0.0"}).From(apiPlugin)

	if err := apiPlugin.Configure(t.Context(), nil); err != nil {
		t.Fatalf("api Configure: %v", err)
	}
	if err := protoPlugin.Configure(t.Context(), nil); err != nil {
		t.Fatalf("proto Configure refused an opaque route instead of leaving it out: %v", err)
	}
	if contract := apiPlugin.ClientServiceContract(); contract == nil || contract.Protobuf == nil {
		t.Fatal("the proto plugin published no descriptor, so this test would not exercise the rule")
	}
	if methods := apiPlugin.ClientProtobufMethods(); len(methods) != 0 {
		t.Fatalf("the descriptor binds %v, want no route", methods)
	}
	if err := bridge.Configure(t.Context(), nil); err != nil {
		t.Fatalf("bridge Configure: %v", err)
	}
	if err := bridge.Start(t.Context(), nil); err != nil {
		t.Fatalf("bridge Start: %v", err)
	}
	mounted := sortedCallKeys(server)
	if _, ok := server.calls["POST /audit"]; !ok {
		t.Fatalf("the route lost its REST URL; mounted %v", mounted)
	}
	for _, key := range mounted {
		if strings.HasPrefix(key, "POST /audit.v1.") {
			t.Errorf("the bridge mounted %s for a route the descriptor leaves out", key)
		}
	}

	if err := openapiPlugin.Configure(t.Context(), nil); err != nil {
		t.Fatalf("openapi Configure: %v", err)
	}
	transports := openapiPlugin.Spec().Paths["/audit"]["post"].ClientContract.Transports
	if len(transports) != 1 || transports[0].Protocol != clientcontract.TransportRESTJSON {
		t.Fatalf("contract transports = %+v, want REST alone", transports)
	}
}
