package http

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/app"
	"go.putnami.dev/errors"
	"go.putnami.dev/logger"
	httproutes "go.putnami.dev/protocol/http-routes"
	runtimeproto "go.putnami.dev/protocol/runtime"

	"go.putnami.dev/protocol/features/spectest"
)

const healthPath = "/_/health"

// startApplication starts a for real on an ephemeral port and returns the base
// URL its server listens on, read from the readiness marker of the listening
// record. The application stops when the test ends.
func startApplication(t *testing.T, a *app.Application, server *ServerPlugin) string {
	t.Helper()
	t.Setenv("PORT", "0")

	var out bytes.Buffer
	server.log = logger.New("http", logger.LevelDebug, logger.NewJSONSinkWriter(&out))
	a.Run(func(context.Context) error { return nil })
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("start application: %v", err)
	}
	t.Cleanup(func() {
		if err := a.Stop(context.Background()); err != nil {
			t.Errorf("stop application: %v", err)
		}
	})

	for line := range strings.SplitSeq(strings.TrimSpace(out.String()), "\n") {
		var record map[string]any
		if json.Unmarshal([]byte(line), &record) != nil {
			continue
		}
		if data, ok := runtimeproto.ReadyMarkerFromLogRecord(record); ok && len(data.Endpoints) == 1 {
			return fmt.Sprintf("http://127.0.0.1:%d", data.Endpoints[0].Port)
		}
	}
	t.Fatalf("no readiness marker in the server output:\n%s", out.String())
	return ""
}

// getStatus requests url and returns the status code and the body.
func getStatus(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return resp.StatusCode, string(body)
}

// healthRegistrations counts the route registrations of GET /_/health on server.
func healthRegistrations(server *ServerPlugin) int {
	count := 0
	for _, fact := range server.httpRouteFacts {
		if fact.method == "GET" && fact.path == healthPath {
			count++
		}
	}
	return count
}

func TestHealthPlugin_MountsItselfOnTheApplicationServer(t *testing.T) {
	spectest.Proves(t, "go/http-services", "health-mounting", "a-health-plugin-added-to-an-application-answers-on-its-server")
	orders := map[string]func(a *app.Application, server *ServerPlugin, health *HealthPlugin){
		"server first": func(a *app.Application, server *ServerPlugin, health *HealthPlugin) {
			a.Use(server)
			a.Use(health)
		},
		"health first": func(a *app.Application, server *ServerPlugin, health *HealthPlugin) {
			a.Use(health)
			a.Use(server)
		},
		"health in a sub-module": func(a *app.Application, server *ServerPlugin, health *HealthPlugin) {
			a.Use(app.NewModule("ops").Use(health))
			a.Use(server)
		},
	}
	for name, compose := range orders {
		t.Run(name, func(t *testing.T) {
			server := NewServerPlugin(ServerConfig{})
			a := app.New("health-mount")
			compose(a, server, NewHealthPlugin())

			base := startApplication(t, a, server)

			status, body := getStatus(t, base+healthPath)
			if status != http.StatusOK {
				t.Fatalf("GET %s = %d %s, want 200", healthPath, status, body)
			}
			if !strings.Contains(body, `"status":"ok"`) {
				t.Errorf("body = %s, want status ok", body)
			}
			if got := healthRegistrations(server); got != 1 {
				t.Errorf("GET %s is registered %d times, want 1", healthPath, got)
			}
		})
	}
}

func TestHealthPlugin_SelfMountedRouteIsUnavailableUntilStart(t *testing.T) {
	spectest.Proves(t, "go/http-services", "health-mounting", "the-mounted-route-answers-503-until-the-application-starts")
	server := NewServerPlugin(ServerConfig{})
	a := app.New("health-before-start")
	a.Use(server)
	a.Use(NewHealthPlugin())

	if err := a.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, healthPath, nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET %s before start = %d %s, want 503", healthPath, rec.Code, rec.Body)
	}

	base := startApplication(t, a, server)
	if status, body := getStatus(t, base+healthPath); status != http.StatusOK {
		t.Errorf("GET %s after start = %d %s, want 200", healthPath, status, body)
	}
}

func TestHealthPlugin_ExplicitRegisterOnIsNotRepeated(t *testing.T) {
	spectest.Proves(t, "go/http-services", "health-mounting", "an-explicit-registration-is-never-repeated")
	chosen := NewServerPlugin(ServerConfig{})
	other := NewServerPlugin(ServerConfig{})
	health := NewHealthPlugin()
	health.RegisterOn(chosen)

	a := app.New("health-explicit")
	a.Use(chosen)
	a.Use(other)
	a.Use(health)

	// Two servers: only the explicit choice lets the application configure, and
	// a second registration of the route would panic in the router.
	if err := a.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got := healthRegistrations(chosen); got != 1 {
		t.Errorf("the chosen server registers GET %s %d times, want 1", healthPath, got)
	}
	if got := healthRegistrations(other); got != 0 {
		t.Errorf("the other server registers GET %s %d times, want 0", healthPath, got)
	}
}

func TestHealthPlugin_ConfiguringAgainRegistersOnce(t *testing.T) {
	spectest.Proves(t, "go/http-services", "health-mounting", "configuring-again-registers-the-route-once")
	passes := map[string]func(t *testing.T, a *app.Application){
		"validate then start": func(t *testing.T, a *app.Application) {
			if err := a.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
		},
		"prepare then start": func(t *testing.T, a *app.Application) {
			if err := a.Prepare(context.Background()); err != nil {
				t.Fatalf("Prepare: %v", err)
			}
		},
		"describe then start": func(t *testing.T, a *app.Application) {
			if err := a.Describe(t.TempDir(), []string{"http"}); err != nil {
				t.Fatalf("Describe: %v", err)
			}
		},
	}
	for name, firstPass := range passes {
		t.Run(name, func(t *testing.T) {
			server := NewServerPlugin(ServerConfig{})
			a := app.New("health-twice")
			a.Use(server)
			a.Use(NewHealthPlugin())

			firstPass(t, a)
			base := startApplication(t, a, server)

			if got := healthRegistrations(server); got != 1 {
				t.Fatalf("GET %s is registered %d times, want 1", healthPath, got)
			}
			if status, body := getStatus(t, base+healthPath); status != http.StatusOK {
				t.Errorf("GET %s = %d %s, want 200", healthPath, status, body)
			}
		})
	}
}

func TestHealthPlugin_NoServerFailsConfigure(t *testing.T) {
	spectest.Proves(t, "go/http-services", "health-mounting", "an-application-without-a-server-fails-configure")
	a := app.New("health-no-server")
	a.Use(NewHealthPlugin())

	err := a.Validate()
	if err == nil {
		t.Fatal("Validate succeeds with a health plugin and no server, want an error")
	}
	if !errors.Is(err, CodeNoServer) {
		t.Errorf("error code = %q, want %q: %v", errors.GetCode(err), CodeNoServer, err)
	}
	for _, want := range []string{"health", "RegisterOn"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

func TestHealthPlugin_SeveralServersFailConfigure(t *testing.T) {
	spectest.Proves(t, "go/http-services", "health-mounting", "an-application-with-several-servers-fails-configure")
	first := NewServerPlugin(ServerConfig{})
	second := NewServerPlugin(ServerConfig{})
	a := app.New("health-two-servers")
	a.Use(first)
	a.Use(app.NewModule("admin").Use(second))
	a.Use(NewHealthPlugin())

	err := a.Validate()
	if err == nil {
		t.Fatal("Validate succeeds with a health plugin and two servers, want an error")
	}
	if !errors.Is(err, CodeAmbiguousServer) {
		t.Errorf("error code = %q, want %q: %v", errors.GetCode(err), CodeAmbiguousServer, err)
	}
	for _, want := range []string{"health", "RegisterOn"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
	if got := healthRegistrations(first) + healthRegistrations(second); got != 0 {
		t.Errorf("GET %s is registered %d times after a failed configure, want 0", healthPath, got)
	}
}

func TestHealthPlugin_HandlerTakenByTheCallerIsNotMountedAgain(t *testing.T) {
	spectest.Proves(t, "go/http-services", "health-mounting", "a-handler-taken-by-the-caller-is-not-mounted-again")

	t.Run("no server", func(t *testing.T) {
		health := NewHealthPlugin()
		_ = health.Handler()
		a := app.New("health-handler")
		a.Use(health)
		if err := a.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
	})

	t.Run("mounted on another path", func(t *testing.T) {
		server := NewServerPlugin(ServerConfig{})
		health := NewHealthPlugin()
		server.GET("/status", health.Handler())
		a := app.New("health-handler")
		a.Use(server)
		a.Use(health)

		base := startApplication(t, a, server)

		if got := healthRegistrations(server); got != 0 {
			t.Errorf("GET %s is registered %d times, want 0", healthPath, got)
		}
		if status, _ := getStatus(t, base+healthPath); status != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", healthPath, status)
		}
		if status, body := getStatus(t, base+"/status"); status != http.StatusOK {
			t.Errorf("GET /status = %d %s, want 200", status, body)
		}
	})
}

func TestHealthPlugin_DescribedRouteInventoryCarriesHealthOnce(t *testing.T) {
	spectest.Proves(t, "go/http-services", "health-mounting", "the-described-route-inventory-carries-the-health-route-once")
	server := NewServerPlugin(ServerConfig{})
	server.GET("/", func(*Context) *Response { return Text("ok") })
	a := app.New("health-describe")
	a.Use(server)
	a.Use(NewHealthPlugin())

	describe := func() []byte {
		t.Helper()
		out := t.TempDir()
		if err := a.Describe(out, []string{"http"}); err != nil {
			t.Fatalf("Describe: %v", err)
		}
		body, err := os.ReadFile(filepath.Join(out, HTTPRoutesDescribePath))
		if err != nil {
			t.Fatalf("read inventory: %v", err)
		}
		return body
	}

	first := describe()
	manifest, diags := httproutes.ParseAndValidateManifest(first)
	if manifest == nil || len(diags) != 0 {
		t.Fatalf("invalid inventory: manifest=%v diagnostics=%v", manifest, diags)
	}
	var health []httproutes.Route
	for _, route := range manifest.Routes {
		if route.Path == healthPath {
			health = append(health, route)
		}
	}
	if len(health) != 1 {
		t.Fatalf("inventory carries %s %d times, want 1: %#v", healthPath, len(health), manifest.Routes)
	}
	if got := strings.Join(health[0].Methods, ","); got != "GET,HEAD" {
		t.Errorf("%s methods = %s, want GET,HEAD", healthPath, got)
	}
	if len(manifest.Routes) != 2 {
		t.Errorf("inventory carries %d routes, want 2: %#v", len(manifest.Routes), manifest.Routes)
	}

	if second := describe(); !bytes.Equal(first, second) {
		t.Errorf("inventory changed across repeated describes:\n%s\n%s", first, second)
	}
	if got := healthRegistrations(server); got != 1 {
		t.Errorf("GET %s is registered %d times after two describes, want 1", healthPath, got)
	}
}

func TestSingleServer(t *testing.T) {
	t.Run("nil owner", func(t *testing.T) {
		server, err := SingleServer(nil, "probe")
		if server != nil || !errors.Is(err, CodeNoServer) {
			t.Fatalf("SingleServer(nil) = %v, %v, want a %q error", server, err, CodeNoServer)
		}
	})

	t.Run("found from a nested module", func(t *testing.T) {
		want := NewServerPlugin(ServerConfig{})
		root := app.NewModule("root")
		root.Use(want)
		child := app.NewModule("child")
		root.Use(child)

		got, err := SingleServer(child, "probe")
		if err != nil {
			t.Fatalf("SingleServer: %v", err)
		}
		if got != want {
			t.Errorf("SingleServer returns %p, want the root's server %p", got, want)
		}
	})

	t.Run("names the plugin", func(t *testing.T) {
		_, err := SingleServer(app.NewModule("root"), "probe")
		if err == nil || !strings.Contains(err.Error(), "the probe plugin") {
			t.Errorf("error = %v, want one that names the probe plugin", err)
		}
	})
}
