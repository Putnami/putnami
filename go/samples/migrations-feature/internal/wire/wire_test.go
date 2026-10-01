package wire

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"go.putnami.dev/http"
)

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
