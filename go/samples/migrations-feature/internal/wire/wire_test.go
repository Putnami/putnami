package wire

import (
	"context"
	"encoding/json"
	stderrors "errors"
	nethttp "net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"go.putnami.dev/app"
	"go.putnami.dev/database"
	"go.putnami.dev/database/testprovider"
	"go.putnami.dev/http"
)

// TestBuildAppAnswersHealth starts the production graph on an isolated,
// provisioned database and requests GET /_/health on its server. It needs
// DATABASE_TEST_BINDINGS, which the workspace test environment sets; without
// it the test skips.
func TestBuildAppAnswersHealth(t *testing.T) {
	if os.Getenv(testprovider.EnvTestBinding) == "" {
		t.Skipf("%s not set; skipping the started-application health check", testprovider.EnvTestBinding)
	}
	result, err := testprovider.Provision(context.Background(), testprovider.Options{})
	if err != nil {
		if stderrors.Is(err, testprovider.ErrSkip) {
			t.Skipf("test provider mode=skip: %v", err)
		}
		t.Fatalf("provision a database: %v", err)
	}
	t.Cleanup(func() {
		if err := result.Cleanup(); err != nil {
			t.Errorf("drop the provisioned database: %v", err)
		}
	})
	if _, ok := result.Binding.Databases["default"]; !ok {
		t.Fatalf("the provisioned binding has no %q datasource", "default")
	}
	binding, err := json.Marshal(result.Binding)
	if err != nil {
		t.Fatalf("encode the database binding: %v", err)
	}
	t.Setenv(database.EnvBinding, string(binding))
	t.Setenv("PORT", "0")

	a := BuildApp()
	a.Run(func(context.Context) error { return nil })
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("start the application: %v", err)
	}
	t.Cleanup(func() {
		if err := a.Stop(context.Background()); err != nil {
			t.Errorf("stop the application: %v", err)
		}
	})

	servers := app.Collect[*http.ServerPlugin](a.Module)
	if len(servers) != 1 {
		t.Fatalf("the application holds %d HTTP servers, want 1", len(servers))
	}
	rr := httptest.NewRecorder()
	servers[0].Handler().ServeHTTP(rr, httptest.NewRequest(nethttp.MethodGet, "/_/health", nil))
	if rr.Code != nethttp.StatusOK {
		t.Fatalf("GET /_/health = %d %s, want 200", rr.Code, rr.Body)
	}
}

// TestBuildAppMountsEachOperationalRouteOnce configures the production graph
// the way the build describes it, without a database connection, and reads the
// routes its server holds. Starting the graph applies migrations, which needs
// a live database: `putnami qualify` covers the started application.
func TestBuildAppMountsEachOperationalRouteOnce(t *testing.T) {
	out := t.TempDir()
	if err := BuildApp().Describe(out, []string{"http"}); err != nil {
		t.Fatalf("describe the application: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(out, http.HTTPRoutesDescribePath))
	if err != nil {
		t.Fatalf("read the route inventory: %v", err)
	}
	var inventory struct {
		Routes []struct {
			Path string `json:"path"`
		} `json:"routes"`
	}
	if err := json.Unmarshal(body, &inventory); err != nil {
		t.Fatalf("decode the route inventory: %v", err)
	}
	mounted := map[string]int{}
	for _, route := range inventory.Routes {
		mounted[route.Path]++
	}
	for _, path := range []string{"/_/health", "/livez", "/healthz", "/readyz", "/version", "/users"} {
		if mounted[path] != 1 {
			t.Errorf("the server holds %s %d times, want 1: %s", path, mounted[path], body)
		}
	}
	if len(inventory.Routes) != 6 {
		t.Errorf("the server holds %d routes, want 6: %s", len(inventory.Routes), body)
	}
}
