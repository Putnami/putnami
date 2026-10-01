package platform

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"go.putnami.dev/app"
	"go.putnami.dev/errors"
	"go.putnami.dev/http"

	"go.putnami.dev/protocol/features/spectest"
)

var corePaths = []string{"/livez", "/healthz", "/readyz", "/version"}

// rootWithServer returns a root module that holds an HTTP server, the tree a
// platform plugin mounts itself on when it configures.
func rootWithServer() *app.Module {
	root := app.NewModule("root")
	root.Use(http.NewServerPlugin(http.ServerConfig{}))
	return root
}

// startApplication starts a for real on an ephemeral port and stops it when the
// test ends.
func startApplication(t *testing.T, a *app.Application) {
	t.Helper()
	t.Setenv("PORT", "0")
	a.Run(func(context.Context) error { return nil })
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("start application: %v", err)
	}
	t.Cleanup(func() {
		if err := a.Stop(context.Background()); err != nil {
			t.Errorf("stop application: %v", err)
		}
	})
}

// statusOn requests path through the composed request handler of server, the
// handler its listener serves.
func statusOn(t *testing.T, server *http.ServerPlugin, path string) int {
	t.Helper()
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()
	status, _ := getStatus(t, ts.URL+path)
	return status
}

func TestPlugin_MountsItselfOnTheApplicationServer(t *testing.T) {
	spectest.Proves(t, "go/platform-endpoints", "self-mounting", "a-plugin-added-to-an-application-answers-on-its-server")
	orders := map[string]func(a *app.Application, server *http.ServerPlugin, p *Plugin){
		"server first": func(a *app.Application, server *http.ServerPlugin, p *Plugin) {
			a.Use(server)
			a.Use(p)
		},
		"platform first": func(a *app.Application, server *http.ServerPlugin, p *Plugin) {
			a.Use(p)
			a.Use(server)
		},
		"platform in a sub-module": func(a *app.Application, server *http.ServerPlugin, p *Plugin) {
			a.Use(app.NewModule("ops").Use(p))
			a.Use(server)
		},
	}
	for name, compose := range orders {
		t.Run(name, func(t *testing.T) {
			server := http.NewServerPlugin(http.ServerConfig{})
			a := app.New("platform-mount")
			compose(a, server, NewPlugin(Config{}))

			startApplication(t, a)

			for _, path := range corePaths {
				if status := statusOn(t, server, path); status != 200 {
					t.Errorf("GET %s = %d, want 200", path, status)
				}
			}
		})
	}
}

func TestPlugin_SelfMountAppliesThePrefix(t *testing.T) {
	server := http.NewServerPlugin(http.ServerConfig{})
	a := app.New("platform-prefix")
	a.Use(server)
	a.Use(NewPlugin(Config{Prefix: "/_"}))

	startApplication(t, a)

	if status := statusOn(t, server, "/_/healthz"); status != 200 {
		t.Errorf("GET /_/healthz = %d, want 200", status)
	}
	if status := statusOn(t, server, "/healthz"); status != 404 {
		t.Errorf("GET /healthz under the /_ prefix = %d, want 404", status)
	}
}

func TestPlugin_ExplicitRegisterOnIsNotRepeated(t *testing.T) {
	spectest.Proves(t, "go/platform-endpoints", "self-mounting", "an-explicit-registration-is-never-repeated")
	chosen := http.NewServerPlugin(http.ServerConfig{})
	other := http.NewServerPlugin(http.ServerConfig{})
	p := NewPlugin(Config{})
	p.RegisterOn(chosen)

	a := app.New("platform-explicit")
	a.Use(chosen)
	a.Use(other)
	a.Use(p)

	// Two servers: only the explicit choice lets the application configure, and
	// a second registration of a route would panic in the router.
	if err := a.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if status := statusOn(t, chosen, "/livez"); status != 200 {
		t.Errorf("GET /livez on the chosen server = %d, want 200", status)
	}
	if status := statusOn(t, other, "/livez"); status != 404 {
		t.Errorf("GET /livez on the other server = %d, want 404", status)
	}
}

func TestPlugin_ConfiguringAgainRegistersOnce(t *testing.T) {
	spectest.Proves(t, "go/platform-endpoints", "self-mounting", "configuring-again-registers-the-endpoints-once")
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
	}
	for name, firstPass := range passes {
		t.Run(name, func(t *testing.T) {
			server := http.NewServerPlugin(http.ServerConfig{})
			a := app.New("platform-twice")
			a.Use(server)
			a.Use(NewPlugin(Config{}))

			// A second registration of a route panics in the router, so a start
			// that returns proves each endpoint registered once.
			firstPass(t, a)
			startApplication(t, a)

			for _, path := range corePaths {
				if status := statusOn(t, server, path); status != 200 {
					t.Errorf("GET %s = %d, want 200", path, status)
				}
			}
		})
	}
}

func TestPlugin_SelfMountedAggregatesAreUnavailableUntilStart(t *testing.T) {
	server := http.NewServerPlugin(http.ServerConfig{})
	a := app.New("platform-before-start")
	a.Use(server)
	a.Use(NewPlugin(Config{}))

	if err := a.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	for path, want := range map[string]int{"/livez": 200, "/healthz": 503, "/readyz": 503} {
		if status := statusOn(t, server, path); status != want {
			t.Errorf("GET %s before start = %d, want %d", path, status, want)
		}
	}

	startApplication(t, a)
	for _, path := range []string{"/healthz", "/readyz"} {
		if status := statusOn(t, server, path); status != 200 {
			t.Errorf("GET %s after start = %d, want 200", path, status)
		}
	}
}

func TestPlugin_NoServerFailsConfigure(t *testing.T) {
	spectest.Proves(t, "go/platform-endpoints", "self-mounting", "an-application-without-a-server-fails-configure")
	a := app.New("platform-no-server")
	a.Use(NewPlugin(Config{}))

	err := a.Validate()
	if err == nil {
		t.Fatal("Validate succeeds with a platform plugin and no server, want an error")
	}
	if !errors.Is(err, http.CodeNoServer) {
		t.Errorf("error code = %q, want %q: %v", errors.GetCode(err), http.CodeNoServer, err)
	}
	for _, want := range []string{"platform", "RegisterOn"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

func TestPlugin_SeveralServersFailConfigure(t *testing.T) {
	spectest.Proves(t, "go/platform-endpoints", "self-mounting", "an-application-with-several-servers-fails-configure")
	first := http.NewServerPlugin(http.ServerConfig{})
	second := http.NewServerPlugin(http.ServerConfig{})
	a := app.New("platform-two-servers")
	a.Use(first)
	a.Use(app.NewModule("admin").Use(second))
	a.Use(NewPlugin(Config{}))

	err := a.Validate()
	if err == nil {
		t.Fatal("Validate succeeds with a platform plugin and two servers, want an error")
	}
	if !errors.Is(err, http.CodeAmbiguousServer) {
		t.Errorf("error code = %q, want %q: %v", errors.GetCode(err), http.CodeAmbiguousServer, err)
	}
	for _, want := range []string{"platform", "RegisterOn"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
	for _, server := range []*http.ServerPlugin{first, second} {
		if status := statusOn(t, server, "/livez"); status != 404 {
			t.Errorf("GET /livez after a failed configure = %d, want 404", status)
		}
	}
}
