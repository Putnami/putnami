package openapi

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"go.putnami.dev/api"
	"go.putnami.dev/app"
	phttp "go.putnami.dev/http"
	clientcontract "go.putnami.dev/protocol/clientcontract"
	"go.putnami.dev/protocol/features/spectest"
)

// MultiStatusDraft is the body a caller creates a widget from.
type MultiStatusDraft struct {
	Name string `json:"name"`
}

// MultiStatusWidget is the widget every success of the provider below answers
// with, except an accepted deployment.
type MultiStatusWidget struct {
	ID string `json:"id"`
}

// MultiStatusJob is what an accepted deployment answers with.
type MultiStatusJob struct {
	JobID string `json:"jobId"`
}

// MultiStatusRef names one widget in the path.
type MultiStatusRef struct {
	ID string `json:"id"`
}

// multiStatusProvider declares the three shapes a provider answers with more
// than one success status, and serves them for real:
//
//   - POST /widgets: 201 when it creates the widget, 200 when it exists — the
//     same body under both;
//   - POST /widgets/{id}/deploy: 200 with the widget when the deployment is
//     done, 202 with a job when it is accepted — a different body under each;
//   - DELETE /widgets/{id}: 200 with the removed widget, or 204 and nothing;
//   - PUT /widgets/{id}: 201 when it creates the widget, 204 when it replaces
//     it — no body under either.
func multiStatusProvider() (*phttp.ServerPlugin, *api.Plugin) {
	httpServer := phttp.NewServerPlugin(phttp.ServerConfig{})
	apiPlugin := api.New(httpServer, api.WithClientService(api.ClientServiceOptions{
		Service: clientcontract.Service{ID: "widgets", Audience: "https://widgets.internal"},
	}))
	apiPlugin.Register(api.Endpoint("POST", "/widgets").
		Body(api.Type[MultiStatusDraft]()).
		Returns(api.Type[MultiStatusWidget]()).
		Response(http.StatusCreated, "Created", api.Type[MultiStatusWidget]()).
		Handle(func(ctx *phttp.EndpointContext) *phttp.Response {
			draft, err := phttp.BodyAs[MultiStatusDraft](ctx)
			if err != nil {
				return phttp.InternalError(err.Error())
			}
			if draft.Name == "existing" {
				return phttp.JSON(MultiStatusWidget{ID: draft.Name})
			}
			return phttp.JSONStatus(http.StatusCreated, MultiStatusWidget{ID: draft.Name})
		}))
	apiPlugin.Register(api.Endpoint("POST", "/widgets/{id}/deploy").
		Params(api.Type[MultiStatusRef]()).
		Returns(api.Type[MultiStatusWidget]()).
		Response(http.StatusAccepted, "Accepted", api.Type[MultiStatusJob]()).
		Handle(func(ctx *phttp.EndpointContext) *phttp.Response {
			ref, err := phttp.ParamsAs[MultiStatusRef](ctx)
			if err != nil {
				return phttp.InternalError(err.Error())
			}
			if ref.ID == "now" {
				return phttp.JSON(MultiStatusWidget(ref))
			}
			return phttp.JSONStatus(http.StatusAccepted, MultiStatusJob{JobID: "job-" + ref.ID})
		}))
	apiPlugin.Register(api.Endpoint("DELETE", "/widgets/{id}").
		Params(api.Type[MultiStatusRef]()).
		Returns(api.Type[MultiStatusWidget]()).
		Response(http.StatusNoContent, "No Content", nil).
		Handle(func(ctx *phttp.EndpointContext) *phttp.Response {
			ref, err := phttp.ParamsAs[MultiStatusRef](ctx)
			if err != nil {
				return phttp.InternalError(err.Error())
			}
			if ref.ID == "keep" {
				return phttp.JSON(MultiStatusWidget(ref))
			}
			return phttp.NoContent()
		}))
	apiPlugin.Register(api.Endpoint("PUT", "/widgets/{id}").
		Params(api.Type[MultiStatusRef]()).
		Body(api.Type[MultiStatusDraft]()).
		Response(http.StatusCreated, "Created", nil).
		Response(http.StatusNoContent, "No Content", nil).
		Handle(func(ctx *phttp.EndpointContext) *phttp.Response {
			ref, err := phttp.ParamsAs[MultiStatusRef](ctx)
			if err != nil {
				return phttp.InternalError(err.Error())
			}
			if ref.ID == "fresh" {
				return phttp.NewResponse(http.StatusCreated)
			}
			return phttp.NoContent()
		}))
	return httpServer, apiPlugin
}

// multiStatusConsumer runs inside a throwaway module, in the generated
// package, against the provider above over a real socket.
const multiStatusConsumer = `package widgetsclient

import (
	"context"
	"os"
	"testing"

	"go.putnami.dev/client"
)

// The provider's fields are optional on the wire, so the generated ones are
// pointers.
func ref(value string) *string { return &value }

func text(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func TestEachDeclaredSuccessStatusReachesTheCaller(t *testing.T) {
	widgets, err := NewWidgetsClientBinding(client.ServiceBinding{
		URL: os.Getenv("MULTI_STATUS_PROVIDER_URL"), ClientID: "consumer", AllowInsecure: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	created, err := widgets.CreateWidgets(ctx, CreateWidgetsInput{Body: MultiStatusDraft{Name: ref("fresh")}})
	if err != nil || created.Status != 201 || created.Body == nil || text(created.Body.Id) != "fresh" {
		t.Fatalf("create a new widget: %+v, %v", created, err)
	}
	existing, err := widgets.CreateWidgets(ctx, CreateWidgetsInput{Body: MultiStatusDraft{Name: ref("existing")}})
	if err != nil || existing.Status != 200 || existing.Body == nil || text(existing.Body.Id) != "existing" {
		t.Fatalf("create an existing widget: %+v, %v", existing, err)
	}

	var deploy CreateWidgetsDeployInput
	deploy.Path.Id = "now"
	done, err := widgets.CreateWidgetsDeploy(ctx, deploy)
	if err != nil || done.Status != 200 || done.Body200 == nil || text(done.Body200.Id) != "now" || done.Body202 != nil {
		t.Fatalf("a finished deployment: %+v, %v", done, err)
	}
	deploy.Path.Id = "later"
	accepted, err := widgets.CreateWidgetsDeploy(ctx, deploy)
	if err != nil || accepted.Status != 202 || accepted.Body202 == nil || text(accepted.Body202.JobId) != "job-later" || accepted.Body200 != nil {
		t.Fatalf("an accepted deployment: %+v, %v", accepted, err)
	}

	var remove DeleteWidgetsInput
	remove.Path.Id = "keep"
	kept, err := widgets.DeleteWidgets(ctx, remove)
	if err != nil || kept.Status != 200 || kept.Body == nil || text(kept.Body.Id) != "keep" {
		t.Fatalf("a removal that answers the widget: %+v, %v", kept, err)
	}
	remove.Path.Id = "gone"
	gone, err := widgets.DeleteWidgets(ctx, remove)
	if err != nil || gone.Status != 204 || gone.Body != nil {
		t.Fatalf("a removal that answers nothing: %+v, %v", gone, err)
	}

	// No status declares a body: the status is the whole answer.
	var replace UpdateWidgetsInput
	replace.Path.Id = "fresh"
	replace.Body = MultiStatusDraft{Name: ref("fresh")}
	put, err := widgets.UpdateWidgets(ctx, replace)
	if err != nil || put.Status != 201 {
		t.Fatalf("a replace that creates: %+v, %v", put, err)
	}
	replace.Path.Id = "old"
	put, err = widgets.UpdateWidgets(ctx, replace)
	if err != nil || put.Status != 204 {
		t.Fatalf("a replace that overwrites: %+v, %v", put, err)
	}
}
`

// The Go half of provider declaration → generation → compiled client → real
// bound call, for operations with more than one success status: a real api
// provider declares them, the in-app describer generates the Go client from
// the contract it publishes, and the emitted package — compiled against this
// repository's runtime — calls the provider over a real socket and sees each
// status with the body that status declares.
func TestTheEmittedGoClientAnswersEachDeclaredSuccessStatusThroughTheRealRuntime(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "multiple-success-statuses",
		"the-emitted-go-client-answers-each-declared-success-status-through-the-real-runtime")
	spectest.Proves(t, "go/api-contracts", "explicit-client-binding",
		"a-client-constructed-without-di-calls-the-real-declared-provider")
	httpServer, apiPlugin := multiStatusProvider()
	openapiPlugin := NewPlugin(PluginOptions{Title: "Widgets", Version: "1.0.0"}).From(apiPlugin)
	clientsPlugin := api.Clients(api.ClientsOptions{
		Go: api.GoClientOptions{PackageName: "widgetsclient", ClientName: "WidgetsClient"},
	}).From(apiPlugin)
	application := app.New("widgets")
	application.Use(apiPlugin).Use(openapiPlugin).Use(clientsPlugin)
	out := t.TempDir()
	if err := application.Describe(out, []string{"openapi", "clients"}); err != nil {
		t.Fatalf("Describe: %v", err)
	}
	source, err := os.ReadFile(filepath.Join(out, api.ClientStageDir, "go", "client.gen.go"))
	if err != nil {
		t.Fatalf("read generated client: %v", err)
	}
	for _, want := range []string{
		"func (c *WidgetsClient) CreateWidgets(ctx context.Context, in CreateWidgetsInput) (*CreateWidgetsResult, error) {",
		"func (c *WidgetsClient) CreateWidgetsDeploy(ctx context.Context, in CreateWidgetsDeployInput) (*CreateWidgetsDeployResult, error) {",
		"func (c *WidgetsClient) DeleteWidgets(ctx context.Context, in DeleteWidgetsInput) (*DeleteWidgetsResult, error) {",
		"func (c *WidgetsClient) UpdateWidgets(ctx context.Context, in UpdateWidgetsInput) (*UpdateWidgetsResult, error) {",
	} {
		if !strings.Contains(string(source), want) {
			t.Fatalf("generated client is missing %q\n%s", want, source)
		}
	}

	provider := httptest.NewServer(httpServer.Handler())
	defer provider.Close()

	moduleDir := t.TempDir()
	writeGeneratedClientModule(t, moduleDir, string(source))
	if err := os.WriteFile(filepath.Join(moduleDir, "consumer_test.go"), []byte(multiStatusConsumer), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("go", "test", "-count=1", "./...")
	command.Dir = moduleDir
	// GOPROXY=off: the module resolves from this checkout and the local cache
	// alone, so whether the emitted client works never depends on a fetch.
	command.Env = append(os.Environ(), "GOPROXY=off", "GOFLAGS=-mod=readonly", "GOWORK=off",
		"MULTI_STATUS_PROVIDER_URL="+provider.URL)
	if output, testErr := command.CombinedOutput(); testErr != nil {
		t.Fatalf("the emitted client failed against the real provider: %v\n%s\n--- source:\n%s", testErr, output, source)
	}
}

var goModModuleLine = regexp.MustCompile(`(?m)^module\s+\S+`)
var goModRelativeReplace = regexp.MustCompile(`(?m)^replace\s+(\S+)\s+=>\s+(\.\S+)`)

// writeGeneratedClientModule materializes a module whose dependency closure is
// exactly the client runtime's own: go/framework/client's go.mod with every
// relative replace pointed at this checkout, plus the client module itself.
func writeGeneratedClientModule(t *testing.T, moduleDir, source string) {
	t.Helper()
	clientDir := filepath.Join(workspaceRoot(t), "go", "framework", "client")
	body, err := os.ReadFile(filepath.Join(clientDir, "go.mod"))
	if err != nil {
		t.Fatalf("read client go.mod: %v", err)
	}
	rewritten := goModModuleLine.ReplaceAllString(string(body), "module generated.example/widgetsclient")
	rewritten = goModRelativeReplace.ReplaceAllStringFunc(rewritten, func(match string) string {
		parts := goModRelativeReplace.FindStringSubmatch(match)
		return "replace " + parts[1] + " => " + filepath.Clean(filepath.Join(clientDir, parts[2]))
	})
	rewritten = strings.TrimRight(rewritten, "\n") +
		"\n\nrequire go.putnami.dev/client v0.0.0\n\nreplace go.putnami.dev/client => " + clientDir + "\n"
	sum, err := os.ReadFile(filepath.Join(clientDir, "go.sum"))
	if err != nil {
		t.Fatalf("read client go.sum: %v", err)
	}
	for name, content := range map[string][]byte{
		"go.mod": []byte(rewritten), "go.sum": sum, "client.gen.go": []byte(source),
	} {
		if err := os.WriteFile(filepath.Join(moduleDir, name), content, 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
}

// workspaceRoot walks up from the test's working directory to the directory
// holding go.work.
func workspaceRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.work")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.work not found above the test working directory")
		}
		dir = parent
	}
}
