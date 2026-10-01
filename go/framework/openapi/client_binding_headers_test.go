package openapi

import (
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"go.putnami.dev/api"
	"go.putnami.dev/app"
	phttp "go.putnami.dev/http"
	clientcontract "go.putnami.dev/protocol/clientcontract"
	"go.putnami.dev/security"
)

type BindingHeadersReply struct {
	Revision string `json:"revision"`
	Caller   string `json:"caller"`
}

// This reuses the emitted-client module harness: the provider publishes the
// declaration, the generated package compiles against this checkout, and that
// package calls the real provider over a socket with two distinct user tokens.
func TestEmittedGoClientCarriesBindingHeadersAlongsideForwardedUsers(t *testing.T) {
	httpServer := phttp.NewServerPlugin(phttp.ServerConfig{})
	httpServer.Use(security.IdentityResolver(func(ctx *phttp.Context) *phttp.Claims {
		switch ctx.Header("Authorization") {
		case "Bearer first-user":
			return &phttp.Claims{Subject: "first-user"}
		case "Bearer second-user":
			return &phttp.Claims{Subject: "second-user"}
		default:
			return nil
		}
	}))
	provider := api.New(httpServer, api.WithClientService(api.ClientServiceOptions{
		Service:     clientcontract.Service{ID: "revision-provider", Audience: "revision-provider"},
		Credentials: map[string]clientcontract.CredentialProfile{"user": {Kind: clientcontract.CredentialForwardedUserToken}},
	}))
	provider.Register(api.Endpoint("GET", "/revision").
		Returns(api.Type[BindingHeadersReply]()).
		Secure(security.Options{}).
		Client(api.ClientOperationOptions{Security: clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{
			AllOf: []clientcontract.SecurityRequirement{{Profile: "user"}},
		}}}}).
		Handle(func(ctx *phttp.EndpointContext) *phttp.Response {
			return phttp.JSON(BindingHeadersReply{Revision: ctx.Header("X-Putnami-Observed-Revision"), Caller: ctx.User.Subject})
		}))
	application := app.New("revision-provider")
	application.Use(provider).
		Use(NewPlugin(PluginOptions{Title: "Revisions", Version: "1.0.0"}).From(provider)).
		Use(api.Clients(api.ClientsOptions{Go: api.GoClientOptions{PackageName: "widgetsclient", ClientName: "RevisionsClient"}}).From(provider))
	out := t.TempDir()
	if err := application.Describe(out, []string{"openapi", "clients"}); err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile(filepath.Join(out, api.ClientStageDir, "go", "client.gen.go"))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(httpServer.Handler())
	defer server.Close()
	moduleDir := t.TempDir()
	writeGeneratedClientModule(t, moduleDir, string(source))
	if err := os.WriteFile(filepath.Join(moduleDir, "consumer_test.go"), []byte(bindingHeadersConsumer), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("go", "test", "-count=1", "./...")
	command.Dir = moduleDir
	command.Env = append(os.Environ(), "GOPROXY=off", "GOFLAGS=-mod=readonly", "GOWORK=off", "BINDING_HEADERS_PROVIDER_URL="+server.URL)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("emitted client failed against the real provider: %v\n%s\n%s", err, output, source)
	}
}

const bindingHeadersConsumer = `package widgetsclient

import (
	"os"
	"testing"

	"go.putnami.dev/client"
)

func TestBindingHeadersAndForwardedUsers(t *testing.T) {
	headers := map[string]string{"X-Putnami-Observed-Revision": "revision-one"}
	transport, err := client.NewServiceClientBinding(client.ServiceBinding{
		URL: os.Getenv("BINDING_HEADERS_PROVIDER_URL"), ClientID: "revision-consumer",
		Headers: headers,
		Credentials: map[string]client.CredentialBinding{"user": {Source: client.CredentialSourceForwardedUser}},
	}, serviceDescriptor)
	if err != nil { t.Fatal(err) }
	headers["X-Putnami-Observed-Revision"] = "mutated"
	revisions := NewRevisionsClient(transport)
	for _, user := range []string{"first-user", "second-user"} {
		ctx := client.WithForwardedUserToken(t.Context(), user)
		reply, err := revisions.ListRevision(ctx, ListRevisionInput{})
		if err != nil { t.Fatal(err) }
		if reply.Revision == nil || *reply.Revision != "revision-one" || reply.Caller == nil || *reply.Caller != user {
			t.Fatalf("static header or per-call identity lost: %+v", reply)
		}
	}
}
`
