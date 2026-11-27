package openapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"go.putnami.dev/api"
	"go.putnami.dev/app"
	phttp "go.putnami.dev/http"
	clientcontract "go.putnami.dev/protocol/clientcontract"
	"go.putnami.dev/protocol/features/spectest"
)

// RedirectArtifact is what the resolving route answers when it holds the
// artifact itself.
type RedirectArtifact struct {
	ID string `json:"id"`
}

// RedirectRef names one artifact in the path.
type RedirectRef struct {
	ID string `json:"id"`
}

// redirectProvider declares and serves two redirecting routes, toward target:
//
//   - GET /artifacts/{id}: a 302 alone, the artifact's location;
//   - GET /artifacts/{id}/resolve: the artifact under 200 for "here", else a
//     307 to where it lives.
func redirectProvider(target string) (*phttp.ServerPlugin, *api.Plugin) {
	httpServer := phttp.NewServerPlugin(phttp.ServerConfig{})
	apiPlugin := api.New(httpServer, api.WithClientService(api.ClientServiceOptions{
		Service: clientcontract.Service{ID: "artifacts", Audience: "https://artifacts.internal"},
	}))
	apiPlugin.Register(api.Endpoint("GET", "/artifacts/{id}").
		Params(api.Type[RedirectRef]()).
		Response(http.StatusFound, "The artifact's location", nil).
		Handle(func(ctx *phttp.EndpointContext) *phttp.Response {
			ref, err := phttp.ParamsAs[RedirectRef](ctx)
			if err != nil {
				return phttp.InternalError(err.Error())
			}
			return phttp.Redirect(target+"/blobs/"+ref.ID, http.StatusFound)
		}))
	apiPlugin.Register(api.Endpoint("GET", "/artifacts/{id}/resolve").
		Params(api.Type[RedirectRef]()).
		Returns(api.Type[RedirectArtifact]()).
		Response(http.StatusTemporaryRedirect, "Where the artifact lives", nil).
		Handle(func(ctx *phttp.EndpointContext) *phttp.Response {
			ref, err := phttp.ParamsAs[RedirectRef](ctx)
			if err != nil {
				return phttp.InternalError(err.Error())
			}
			if ref.ID == "here" {
				return phttp.JSON(RedirectArtifact(ref))
			}
			return phttp.Redirect(target+"/blobs/"+ref.ID, http.StatusTemporaryRedirect)
		}))
	return httpServer, apiPlugin
}

// describeRedirectProvider publishes the provider's OpenAPI and client
// contract into a temporary directory.
func describeRedirectProvider(t *testing.T, apiPlugin *api.Plugin) string {
	t.Helper()
	openapiPlugin := NewPlugin(PluginOptions{Title: "Artifacts", Version: "1.0.0"}).From(apiPlugin)
	clientsPlugin := api.Clients(api.ClientsOptions{}).From(apiPlugin)
	application := app.New("artifacts")
	application.Use(apiPlugin).Use(openapiPlugin).Use(clientsPlugin)
	out := t.TempDir()
	if err := application.Describe(out, []string{"openapi", "clients"}); err != nil {
		t.Fatalf("Describe: %v", err)
	}
	return out
}

// successStatuses lists the 2xx and 3xx response keys of one operation.
func successStatuses(t *testing.T, document []byte, path, method string) []string {
	t.Helper()
	var spec struct {
		Paths map[string]map[string]struct {
			Responses map[string]json.RawMessage `json:"responses"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(document, &spec); err != nil {
		t.Fatalf("decode OpenAPI: %v", err)
	}
	operation, ok := spec.Paths[path][method]
	if !ok {
		t.Fatalf("OpenAPI has no %s %s", method, path)
	}
	var statuses []string
	for status := range operation.Responses {
		if strings.HasPrefix(status, "2") || strings.HasPrefix(status, "3") {
			statuses = append(statuses, status)
		}
	}
	sort.Strings(statuses)
	return statuses
}

// A route that declares a redirect and no primary success publishes the 3xx
// alone; a route that also declares its primary body keeps its 200.
func TestARedirectingEndpointDeclaresItsRedirectAlone(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "redirect-success", "a-redirecting-endpoint-declares-its-3xx-alone")
	_, apiPlugin := redirectProvider("https://blobs.example")
	out := describeRedirectProvider(t, apiPlugin)
	document, err := os.ReadFile(filepath.Join(out, DefaultDescribePath)) //nolint:gosec // G304: test-owned temporary directory
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(successStatuses(t, document, "/artifacts/{id}", "get"), ","); got != "302" {
		t.Errorf("GET /artifacts/{id} success statuses = %s, want 302 alone", got)
	}
	if got := strings.Join(successStatuses(t, document, "/artifacts/{id}/resolve", "get"), ","); got != "200,307" {
		t.Errorf("GET /artifacts/{id}/resolve success statuses = %s, want 200,307", got)
	}
}

// redirectConsumer runs inside a throwaway module, in the generated package,
// against the provider above over a real socket.
const redirectConsumer = `package artifactsclient

import (
	"context"
	"os"
	"testing"

	"go.putnami.dev/client"
)

func TestADeclaredRedirectIsTheAnswer(t *testing.T) {
	artifacts, err := NewArtifactsClientBinding(client.ServiceBinding{
		URL: os.Getenv("REDIRECT_PROVIDER_URL"), ClientID: "consumer", AllowInsecure: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	target := os.Getenv("REDIRECT_TARGET_URL")

	var open GetArtifactsInput
	open.Path.Id = "a1"
	opened, err := artifacts.GetArtifacts(ctx, open)
	if err != nil || opened.Status != 302 || opened.Location != target+"/blobs/a1" {
		t.Fatalf("open a redirecting artifact: %+v, %v", opened, err)
	}

	var resolve GetArtifactsResolveInput
	resolve.Path.Id = "here"
	held, err := artifacts.GetArtifactsResolve(ctx, resolve)
	if err != nil || held.Status != 200 || held.Body == nil || held.Body.Id == nil || *held.Body.Id != "here" || held.Location != "" {
		t.Fatalf("resolve an artifact the provider holds: %+v, %v", held, err)
	}
	resolve.Path.Id = "far"
	moved, err := artifacts.GetArtifactsResolve(ctx, resolve)
	if err != nil || moved.Status != 307 || moved.Body != nil || moved.Location != target+"/blobs/far" {
		t.Fatalf("resolve an artifact that lives elsewhere: %+v, %v", moved, err)
	}
}
`

// Provider declaration → generation → compiled client → real bound call, for
// a redirecting route: the emitted Go client hands back the declared 3xx and
// its Location, and never follows it to the target.
func TestTheEmittedGoClientAnswersADeclaredRedirectWithoutFollowingIt(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "redirect-success",
		"the-emitted-go-client-answers-a-declared-redirect-without-following-it-through-the-real-runtime")
	var followed atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		followed.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	httpServer, apiPlugin := redirectProvider(target.URL)
	out := describeRedirectProvider(t, apiPlugin)
	source, err := emitDescribedGoClient(t, out, "artifactsclient", "ArtifactsClient")
	if err != nil {
		t.Fatalf("read generated client: %v", err)
	}
	for _, want := range []string{
		"func (c *ArtifactsClient) GetArtifacts(ctx context.Context, in GetArtifactsInput) (*GetArtifactsResult, error) {",
		"func (c *ArtifactsClient) GetArtifactsResolve(ctx context.Context, in GetArtifactsResolveInput) (*GetArtifactsResolveResult, error) {",
	} {
		if !strings.Contains(string(source), want) {
			t.Fatalf("generated client is missing %q\n%s", want, source)
		}
	}

	provider := httptest.NewServer(httpServer.Handler())
	defer provider.Close()

	moduleDir := t.TempDir()
	writeGeneratedClientModule(t, moduleDir, string(source))
	if err := os.WriteFile(filepath.Join(moduleDir, "consumer_test.go"), []byte(redirectConsumer), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("go", "test", "-count=1", "./...")
	command.Dir = moduleDir
	command.Env = append(os.Environ(), "GOPROXY=off", "GOFLAGS=-mod=readonly", "GOWORK=off",
		"REDIRECT_PROVIDER_URL="+provider.URL, "REDIRECT_TARGET_URL="+target.URL)
	if output, testErr := command.CombinedOutput(); testErr != nil {
		t.Fatalf("the emitted client failed against the real provider: %v\n%s\n--- source:\n%s", testErr, output, source)
	}
	if hits := followed.Load(); hits != 0 {
		t.Fatalf("the emitted client followed the redirect to its target %d time(s)", hits)
	}
}

// A route that declares a redirect advertises no Connect transport: a Connect
// response answers every success with HTTP 200, so the redirect would reach
// the caller as an error.
func TestARedirectingRouteAdvertisesNoConnectTransport(t *testing.T) {
	projection := connectProjection{
		methods:   map[string]string{connectRouteKey("GET", "/artifacts/{id}"): "/artifacts.v1.Artifacts/Get"},
		encodings: []clientcontract.Encoding{clientcontract.EncodingJSON},
	}
	plain := DiscoveredRoute{Method: "GET", Path: "/artifacts/{id}", Returns: reflect.TypeFor[RedirectArtifact](), ReturnsStatus: http.StatusOK}
	if len(projection.transportsFor(plain)) != 1 {
		t.Fatal("the projection must advertise Connect for a plain unary route")
	}
	redirecting := plain
	redirecting.AdditionalReturns = []ThrowsMeta{{Status: http.StatusTemporaryRedirect}}
	if transports := projection.transportsFor(redirecting); len(transports) != 0 {
		t.Fatalf("a redirecting route advertises Connect: %+v", transports)
	}
	stream := DiscoveredRoute{Method: "GET", Path: "/artifacts/{id}", StreamMode: "server",
		AdditionalReturns: []ThrowsMeta{{Status: http.StatusFound}}}
	if len(projection.transportsFor(stream)) != 1 {
		t.Fatal("a server stream publishes no 3xx and keeps its Connect transport")
	}
	notModified := plain
	notModified.AdditionalReturns = []ThrowsMeta{{Status: http.StatusNotModified}}
	if len(projection.transportsFor(notModified)) != 1 {
		t.Fatal("a 304 is no redirect and keeps the Connect transport")
	}
}
