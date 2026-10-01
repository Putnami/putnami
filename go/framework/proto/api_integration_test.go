package proto

import (
	"context"
	"testing"

	"go.putnami.dev/api"
	phttp "go.putnami.dev/http"
	clientcontract "go.putnami.dev/protocol/clientcontract"
)

// protoFakeServer satisfies api.Server so the integration test can drive the real
// api.Plugin discovery path without standing up a live listener.
type protoFakeServer struct{}

func (protoFakeServer) Handle(_, _ string, _ phttp.Handler) {}

// TestPlugin_FromAPIPluginThroughConfigure exercises the production wiring —
// From(apiPlugin) → Configure reading apiPlugin.DiscoveredRoutes() → Document() —
// rather than the configureWithRoutes shortcut other tests use. It locks in the
// proto ← api integration seam (registration order, schema discovery).
func TestPlugin_FromAPIPluginThroughConfigure(t *testing.T) {
	apiPlugin := api.New(protoFakeServer{}, api.WithClientService(api.ClientServiceOptions{
		Service: clientcontract.Service{ID: "users", Audience: "urn:users"},
	}))
	apiPlugin.Register(api.Endpoint("GET", "/users").
		Description("List users").
		Returns(api.Type[ProtoUser]()).
		HandleRaw(func(_ *phttp.Context) *phttp.Response { return phttp.JSON(nil) }))
	apiPlugin.Register(api.Endpoint("POST", "/users").
		Body(api.Type[ProtoCreateUserBody]()).
		Returns(api.Type[ProtoUser]()).
		HandleRaw(func(_ *phttp.Context) *phttp.Response { return phttp.JSON(nil) }))

	p := NewPlugin(PluginOptions{PackageName: "test.v1"}).From(apiPlugin)

	if p.Document() != nil {
		t.Fatal("Document() should be nil before Configure")
	}

	// api.Plugin.Configure must run first so DiscoveredRoutes is populated.
	if err := apiPlugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("api Configure: %v", err)
	}
	if err := p.Configure(context.Background(), nil); err != nil {
		t.Fatalf("proto Configure: %v", err)
	}

	doc := p.Document()
	if doc == nil {
		t.Fatal("Document() nil after Configure")
	}
	if doc.PackageName != "test.v1" {
		t.Errorf("package name = %q, want test.v1", doc.PackageName)
	}

	rpcs := map[string]bool{}
	for _, r := range doc.Service.RPCs {
		rpcs[r.Name] = true
	}
	if !rpcs["ListUsers"] || !rpcs["CreateUsers"] {
		t.Fatalf("discovered RPCs = %v, want ListUsers + CreateUsers", rpcs)
	}

	body := findMessage(t, *doc, "CreateUsersBody")
	if !hasField(body, "name", "string") || !hasField(body, "email", "string") {
		t.Errorf("CreateUsersBody not derived from api discovery: %+v", body.Fields)
	}
	reply := findMessage(t, *doc, "CreateUsersReply")
	if !hasField(reply, "id", "string") {
		t.Errorf("CreateUsersReply not derived from ProtoUser: %+v", reply.Fields)
	}
	clientDescriptor := apiPlugin.ClientServiceContract().Protobuf
	if clientDescriptor == nil || clientDescriptor.Package != "test.v1" || len(clientDescriptor.Services) != 1 {
		t.Fatalf("proto descriptor was not published to first-party API contract: %#v", clientDescriptor)
	}
}
