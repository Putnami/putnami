package http

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/app"
	httproutes "go.putnami.dev/protocol/http-routes"
)

func TestDescribeHTTPRoutesFromConfiguredRegistry(t *testing.T) {
	server := NewServerPlugin(ServerConfig{})
	server.GET("/healthz", func(*Context) *Response { return Text("ok") })
	server.Handle("GET", "/users/{userID}", func(*Context) *Response { return JSON(map[string]bool{"ok": true}) })
	server.Handle("POST", "/users", func(*Context) *Response { return JSON(map[string]bool{"ok": true}) })

	owner := app.NewModule("example/go-service")
	if err := server.Configure(context.Background(), owner); err != nil {
		t.Fatalf("configure: %v", err)
	}
	out := t.TempDir()
	if err := server.Describe(&app.DescribeContext{OutputDir: out}); err != nil {
		t.Fatalf("describe: %v", err)
	}

	body, err := os.ReadFile(filepath.Join(out, HTTPRoutesDescribePath))
	if err != nil {
		t.Fatalf("read inventory: %v", err)
	}
	manifest, diags := httproutes.ParseAndValidateManifest(body)
	if manifest == nil || len(diags) != 0 {
		t.Fatalf("invalid inventory: manifest=%v diagnostics=%v", manifest, diags)
	}
	if len(manifest.Routes) != 3 {
		t.Fatalf("routes = %d, want 3: %#v", len(manifest.Routes), manifest.Routes)
	}

	assertRoute := func(path string, source httproutes.SourceKind, methods ...string) {
		t.Helper()
		for _, route := range manifest.Routes {
			if route.Path == path {
				if route.Provenance.SourceKind != source {
					t.Fatalf("%s source = %q, want %q", path, route.Provenance.SourceKind, source)
				}
				if strings.Join(route.Methods, ",") != strings.Join(methods, ",") {
					t.Fatalf("%s methods = %v, want %v", path, route.Methods, methods)
				}
				return
			}
		}
		t.Fatalf("route %s not found in %#v", path, manifest.Routes)
	}
	assertRoute("/healthz", httproutes.SourceManual, "GET", "HEAD")
	assertRoute("/users", httproutes.SourceTypedAPI, "POST")
	assertRoute("/users/{userID}", httproutes.SourceTypedAPI, "GET", "HEAD")

	first := string(body)
	if err := server.Describe(&app.DescribeContext{OutputDir: out}); err != nil {
		t.Fatalf("second describe: %v", err)
	}
	second, err := os.ReadFile(filepath.Join(out, HTTPRoutesDescribePath))
	if err != nil {
		t.Fatalf("read second inventory: %v", err)
	}
	if first != string(second) {
		t.Fatal("inventory changed across repeated describes")
	}
}

func TestDescribeHTTPRoutesFromPrefixMount(t *testing.T) {
	server := NewServerPlugin(ServerConfig{})
	server.RouteMount("POST", "/pkg.Service/", func(*Context) *Response { return Text("ok") })

	owner := app.NewModule("example/go-service")
	if err := server.Configure(context.Background(), owner); err != nil {
		t.Fatalf("configure: %v", err)
	}
	out := t.TempDir()
	if err := server.Describe(&app.DescribeContext{OutputDir: out}); err != nil {
		t.Fatalf("describe: %v", err)
	}

	body, err := os.ReadFile(filepath.Join(out, HTTPRoutesDescribePath))
	if err != nil {
		t.Fatalf("read inventory: %v", err)
	}
	manifest, diags := httproutes.ParseAndValidateManifest(body)
	if manifest == nil || len(diags) != 0 {
		t.Fatalf("invalid inventory: manifest=%v diagnostics=%v", manifest, diags)
	}
	if len(manifest.Routes) != 1 {
		t.Fatalf("routes = %d, want 1: %#v", len(manifest.Routes), manifest.Routes)
	}
	route := manifest.Routes[0]
	if route.Path != "/pkg.Service/" {
		t.Errorf("path = %q, want %q", route.Path, "/pkg.Service/")
	}
	if route.Match != httproutes.MatchPrefix {
		t.Errorf("match = %q, want %q", route.Match, httproutes.MatchPrefix)
	}
	if route.Provenance.SourceKind != httproutes.SourceStaticMount {
		t.Errorf("sourceKind = %q, want %q", route.Provenance.SourceKind, httproutes.SourceStaticMount)
	}
	if !route.PublicEdge {
		t.Errorf("publicEdge = %v, want true", route.PublicEdge)
	}
	if strings.Join(route.Methods, ",") != "POST" {
		t.Errorf("methods = %v, want [POST]", route.Methods)
	}

	first := string(body)
	if err := server.Describe(&app.DescribeContext{OutputDir: out}); err != nil {
		t.Fatalf("second describe: %v", err)
	}
	second, err := os.ReadFile(filepath.Join(out, HTTPRoutesDescribePath))
	if err != nil {
		t.Fatalf("read second inventory: %v", err)
	}
	if first != string(second) {
		t.Fatal("inventory changed across repeated describes")
	}
}

// TestDescribeHTTPRoutesFromGomodCatchAll pins the catch-all contract: a GOPROXY-style
// server whose routes are all rooted on an unbounded '{module...}' catch-all must
// describe into a valid putnami.http-routes.v1 inventory. The router spells the
// catch-all '{name...}', which is exactly the protocol spelling, so the six
// runtime routes flow through the emitter as template routes unchanged.
func TestDescribeHTTPRoutesFromGomodCatchAll(t *testing.T) {
	ok := func(*Context) *Response { return Text("ok") }
	server := NewServerPlugin(ServerConfig{})
	server.GET("/{module...}", ok)
	server.GET("/{module...}/@v/list", ok)
	server.GET("/{module...}/@v/{versionfile}", ok)
	server.GET("/{module...}/@latest", ok)
	server.PUT("/{module...}/@v/{version}", ok)
	server.POST("/{module...}/-/blobs/upload", ok)

	owner := app.NewModule("example/gomod-server")
	if err := server.Configure(context.Background(), owner); err != nil {
		t.Fatalf("configure: %v", err)
	}
	out := t.TempDir()
	if err := server.Describe(&app.DescribeContext{OutputDir: out}); err != nil {
		t.Fatalf("describe: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(out, HTTPRoutesDescribePath))
	if err != nil {
		t.Fatalf("read inventory: %v", err)
	}
	manifest, diags := httproutes.ParseAndValidateManifest(body)
	if manifest == nil || len(diags) != 0 {
		t.Fatalf("invalid inventory: manifest=%v diagnostics=%v", manifest, diags)
	}
	if len(manifest.Routes) != 6 {
		t.Fatalf("routes = %d, want 6: %#v", len(manifest.Routes), manifest.Routes)
	}
	want := map[string][]string{
		"/{module...}":                  {"GET", "HEAD"},
		"/{module...}/@v/list":          {"GET", "HEAD"},
		"/{module...}/@v/{versionfile}": {"GET", "HEAD"},
		"/{module...}/@latest":          {"GET", "HEAD"},
		"/{module...}/@v/{version}":     {"PUT"},
		"/{module...}/-/blobs/upload":   {"POST"},
	}
	for _, route := range manifest.Routes {
		methods, seen := want[route.Path]
		if !seen {
			t.Fatalf("unexpected route %q in %#v", route.Path, manifest.Routes)
		}
		if route.Match != httproutes.MatchTemplate {
			t.Errorf("%s match = %q, want template", route.Path, route.Match)
		}
		if strings.Join(route.Methods, ",") != strings.Join(methods, ",") {
			t.Errorf("%s methods = %v, want %v", route.Path, route.Methods, methods)
		}
		delete(want, route.Path)
	}
	if len(want) != 0 {
		t.Fatalf("missing routes in inventory: %v", want)
	}
}

// TestDescribeHTTPRoutesRejectsWildcard keeps the inventory boundary fail-closed
// if a legacy fact bypasses public registration, which now rejects anonymous
// wildcards before they reach Describe.
func TestDescribeHTTPRoutesRejectsWildcard(t *testing.T) {
	server := NewServerPlugin(ServerConfig{})
	server.httpRouteFacts = append(server.httpRouteFacts, httpRouteFact{
		method: "GET", path: "/files/*", source: routeSourceManual,
	})
	if err := server.Configure(context.Background(), app.NewModule("example/go-service")); err != nil {
		t.Fatalf("configure: %v", err)
	}
	err := server.Describe(&app.DescribeContext{OutputDir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), httproutes.ErrorCodeUnsupportedPattern) {
		t.Fatalf("describe error = %v, want %s", err, httproutes.ErrorCodeUnsupportedPattern)
	}
}

// TestDescribeHTTPRoutesIncludesStreamRoutes pins the stream half of the
// inventory. A route bound through HandleStream is a bound public route, so
// schema/http-routes.json — the artifact a default-deny gateway policy admits
// on — must list it exactly as it lists a unary GET: method GET plus the HEAD
// companion, sourceKind typed-api, publicEdge true. A templated stream path
// must still derive MatchTemplate.
func TestDescribeHTTPRoutesIncludesStreamRoutes(t *testing.T) {
	server := NewServerPlugin(ServerConfig{})
	server.Handle("POST", "/v1/messages", func(*Context) *Response { return JSON(map[string]bool{"ok": true}) })
	server.HandleStream("/v1/events", StreamHandler{
		Mode:   StreamModeServer,
		Handle: func(*StreamContext) error { return nil },
	})
	server.HandleStream("/v1/rooms/{roomID}/tunnel", StreamHandler{
		Mode:        StreamModeBidirectional,
		Subprotocol: "pg.tunnel.v1",
		Handle:      func(*StreamContext) error { return nil },
	})

	owner := app.NewModule("example/go-service")
	if err := server.Configure(context.Background(), owner); err != nil {
		t.Fatalf("configure: %v", err)
	}
	out := t.TempDir()
	if err := server.Describe(&app.DescribeContext{OutputDir: out}); err != nil {
		t.Fatalf("describe: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(out, HTTPRoutesDescribePath))
	if err != nil {
		t.Fatalf("read inventory: %v", err)
	}
	manifest, diags := httproutes.ParseAndValidateManifest(body)
	if manifest == nil || len(diags) != 0 {
		t.Fatalf("invalid inventory: manifest=%v diagnostics=%v", manifest, diags)
	}
	if len(manifest.Routes) != 3 {
		t.Fatalf("routes = %d, want 3: %#v", len(manifest.Routes), manifest.Routes)
	}

	want := map[string]struct {
		match   httproutes.MatchKind
		methods string
	}{
		"/v1/messages":              {httproutes.MatchExact, "POST"},
		"/v1/events":                {httproutes.MatchExact, "GET,HEAD"},
		"/v1/rooms/{roomID}/tunnel": {httproutes.MatchTemplate, "GET,HEAD"},
	}
	for _, route := range manifest.Routes {
		expected, known := want[route.Path]
		if !known {
			t.Fatalf("unexpected route %q in %#v", route.Path, manifest.Routes)
		}
		if route.Match != expected.match {
			t.Errorf("%s match = %q, want %q", route.Path, route.Match, expected.match)
		}
		if got := strings.Join(route.Methods, ","); got != expected.methods {
			t.Errorf("%s methods = %q, want %q", route.Path, got, expected.methods)
		}
		if route.Provenance.SourceKind != httproutes.SourceTypedAPI {
			t.Errorf("%s sourceKind = %q, want %q", route.Path, route.Provenance.SourceKind, httproutes.SourceTypedAPI)
		}
		if !route.PublicEdge {
			t.Errorf("%s publicEdge = false, want true", route.Path)
		}
		delete(want, route.Path)
	}
	if len(want) != 0 {
		t.Fatalf("missing routes in inventory: %v", want)
	}

	first := string(body)
	if err := server.Describe(&app.DescribeContext{OutputDir: out}); err != nil {
		t.Fatalf("second describe: %v", err)
	}
	second, err := os.ReadFile(filepath.Join(out, HTTPRoutesDescribePath))
	if err != nil {
		t.Fatalf("read second inventory: %v", err)
	}
	if first != string(second) {
		t.Fatal("inventory changed across repeated describes")
	}
}
