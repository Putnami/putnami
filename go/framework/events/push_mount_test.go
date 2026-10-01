package events

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.putnami.dev/app"
	"go.putnami.dev/errors"
	phttp "go.putnami.dev/http"
	"go.putnami.dev/protocol/features/spectest"
)

// pushPlugin returns a plugin whose code sets push delivery with a secured
// receiver. The recording transport keeps the test off the network.
func pushPlugin() *Plugin {
	return Events(PluginConfig{
		Delivery:  DeliveryPush,
		Push:      PushConfig{Issuer: "https://accounts.google.com", AllowedServiceAccounts: []string{"pusher@sa.example"}},
		Transport: &recordingTransport{},
	})
}

// configureApplication configures a and leaves its container open, so the
// routes it mounted answer requests. It clears the process-wide transport the
// events plugin sets when the test ends.
func configureApplication(t *testing.T, a *app.Application) error {
	t.Helper()
	t.Cleanup(func() { SetTransport(nil) })
	return a.Prepare(context.Background())
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

// postPush posts an unauthenticated push delivery through the composed request
// handler of server, the handler its listener serves, and returns the status.
func postPush(t *testing.T, server *phttp.ServerPlugin) int {
	t.Helper()
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()
	body := pushBody(t, Envelope{Topic: "order.created", Payload: map[string]any{"id": "o-1"}})
	resp, err := http.Post(ts.URL+"/_putnami/events/orders", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST push receiver: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func TestPushPlugin_MountsTheSecuredReceiverOnTheApplicationServer(t *testing.T) {
	spectest.Proves(t, "go/event-delivery", "push-receiver-mounting", "a-push-plugin-added-to-an-application-mounts-the-secured-receiver")
	orders := map[string]func(a *app.Application, server *phttp.ServerPlugin, p *Plugin){
		"server first": func(a *app.Application, server *phttp.ServerPlugin, p *Plugin) {
			a.Use(server)
			a.Use(p)
		},
		"events first": func(a *app.Application, server *phttp.ServerPlugin, p *Plugin) {
			a.Use(p)
			a.Use(server)
		},
	}
	for name, compose := range orders {
		t.Run(name, func(t *testing.T) {
			var calls int
			p := pushPlugin()
			p.Register(&HandlerDefinition{Topic: "order.created", Handler: func(context.Context, *Envelope) error {
				calls++
				return nil
			}})
			server := phttp.NewServerPlugin(phttp.ServerConfig{})
			a := app.New("push-mount")
			compose(a, server, p)

			startApplication(t, a)

			// The receiver is mounted and fail-closed: a delivery without a
			// pusher token is refused before any handler runs.
			if status := postPush(t, server); status != http.StatusUnauthorized {
				t.Fatalf("POST push receiver without a token = %d, want 401", status)
			}
			if calls != 0 {
				t.Errorf("an unauthenticated delivery invoked the handler %d times", calls)
			}
		})
	}
}

func TestPushPlugin_ConfiguringAgainMountsTheReceiverOnce(t *testing.T) {
	spectest.Proves(t, "go/event-delivery", "push-receiver-mounting", "configuring-again-mounts-the-receiver-once")
	server := phttp.NewServerPlugin(phttp.ServerConfig{})
	a := app.New("push-twice")
	a.Use(server)
	a.Use(pushPlugin())

	// A second registration of the route panics in the router, so a start that
	// returns after a validate proves the receiver registered once.
	if err := a.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if err := configureApplication(t, a); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	startApplication(t, a)

	if status := postPush(t, server); status != http.StatusUnauthorized {
		t.Errorf("POST push receiver without a token = %d, want 401", status)
	}
}

func TestPushPlugin_NoServerFailsConfigure(t *testing.T) {
	spectest.Proves(t, "go/event-delivery", "push-receiver-mounting", "a-push-application-without-a-server-fails-configure")
	a := app.New("push-no-server")
	a.Use(pushPlugin())

	err := configureApplication(t, a)
	if err == nil {
		t.Fatal("configure succeeds with push delivery and no server, want an error")
	}
	if !errors.Is(err, phttp.CodeNoServer) {
		t.Errorf("error code = %q, want %q: %v", errors.GetCode(err), phttp.CodeNoServer, err)
	}
	for _, want := range []string{"events", "RegisterOn"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

func TestPushPlugin_SeveralServersFailConfigure(t *testing.T) {
	spectest.Proves(t, "go/event-delivery", "push-receiver-mounting", "a-push-application-with-several-servers-fails-configure")
	first := phttp.NewServerPlugin(phttp.ServerConfig{})
	second := phttp.NewServerPlugin(phttp.ServerConfig{})
	a := app.New("push-two-servers")
	a.Use(first)
	a.Use(second)
	a.Use(pushPlugin())

	err := configureApplication(t, a)
	if err == nil {
		t.Fatal("configure succeeds with push delivery and two servers, want an error")
	}
	if !errors.Is(err, phttp.CodeAmbiguousServer) {
		t.Errorf("error code = %q, want %q: %v", errors.GetCode(err), phttp.CodeAmbiguousServer, err)
	}
	for _, want := range []string{"events", "RegisterOn"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
	for _, server := range []*phttp.ServerPlugin{first, second} {
		if status := postPush(t, server); status != http.StatusNotFound {
			t.Errorf("POST push receiver after a failed configure = %d, want 404", status)
		}
	}
}

func TestPushPlugin_RegisterOnChoosesTheServer(t *testing.T) {
	spectest.Proves(t, "go/event-delivery", "push-receiver-mounting", "registeron-chooses-the-server-and-is-never-repeated")
	chosen := phttp.NewServerPlugin(phttp.ServerConfig{})
	other := phttp.NewServerPlugin(phttp.ServerConfig{})
	p := pushPlugin()
	p.RegisterOn(chosen)

	a := app.New("push-explicit")
	a.Use(chosen)
	a.Use(other)
	a.Use(p)

	// Two servers: only the explicit choice lets the application configure, and
	// a second registration of the route would panic in the router.
	if err := configureApplication(t, a); err != nil {
		t.Fatalf("configure: %v", err)
	}
	if status := postPush(t, chosen); status != http.StatusUnauthorized {
		t.Errorf("POST push receiver on the chosen server = %d, want 401", status)
	}
	if status := postPush(t, other); status != http.StatusNotFound {
		t.Errorf("POST push receiver on the other server = %d, want 404", status)
	}
}

func TestPushPlugin_PullDeliveryNeedsNoServer(t *testing.T) {
	spectest.Proves(t, "go/event-delivery", "push-receiver-mounting", "pull-and-stream-delivery-need-no-server")
	for _, delivery := range []DeliveryMode{"", DeliveryPull, DeliveryStream} {
		t.Run("delivery "+string(delivery), func(t *testing.T) {
			a := app.New("pull-no-server")
			a.Use(Events(PluginConfig{Delivery: delivery, Transport: &recordingTransport{}}))
			if err := configureApplication(t, a); err != nil {
				t.Fatalf("configure without a server: %v", err)
			}
		})
	}

	t.Run("a server in the tree gains no receiver", func(t *testing.T) {
		server := phttp.NewServerPlugin(phttp.ServerConfig{})
		a := app.New("pull-with-server")
		a.Use(server)
		a.Use(Events(PluginConfig{Transport: &recordingTransport{}}))
		if err := configureApplication(t, a); err != nil {
			t.Fatalf("configure: %v", err)
		}
		if status := postPush(t, server); status != http.StatusNotFound {
			t.Errorf("POST push receiver under pull delivery = %d, want 404", status)
		}
	})
}

func TestPushPlugin_ConfigDocumentSelectsPush(t *testing.T) {
	spectest.Proves(t, "go/event-delivery", "push-receiver-mounting", "push-selected-by-the-events-config-mounts-the-receiver")
	const pushDocument = "events:\n  delivery: push\n  push:\n    issuer: https://accounts.google.com\n"

	t.Run("mounts on the single server", func(t *testing.T) {
		t.Setenv("CONFIG_DATA", pushDocument)
		server := phttp.NewServerPlugin(phttp.ServerConfig{})
		a := app.New("document-push")
		a.Use(server)
		a.Use(Events(PluginConfig{Transport: &recordingTransport{}}))

		if err := configureApplication(t, a); err != nil {
			t.Fatalf("configure: %v", err)
		}
		if status := postPush(t, server); status != http.StatusUnauthorized {
			t.Errorf("POST push receiver without a token = %d, want 401", status)
		}
	})

	t.Run("fails without a server", func(t *testing.T) {
		t.Setenv("CONFIG_DATA", pushDocument)
		a := app.New("document-push-no-server")
		a.Use(Events(PluginConfig{Transport: &recordingTransport{}}))

		if err := configureApplication(t, a); !errors.Is(err, phttp.CodeNoServer) {
			t.Fatalf("configure = %v, want a %q error", err, phttp.CodeNoServer)
		}
	})

	t.Run("mounts on the server RegisterOn named", func(t *testing.T) {
		t.Setenv("CONFIG_DATA", pushDocument)
		chosen := phttp.NewServerPlugin(phttp.ServerConfig{})
		other := phttp.NewServerPlugin(phttp.ServerConfig{})
		p := Events(PluginConfig{Transport: &recordingTransport{}})
		// The code sets pull delivery, so RegisterOn registers nothing yet.
		p.RegisterOn(chosen)
		if status := postPush(t, chosen); status != http.StatusNotFound {
			t.Fatalf("the chosen server answers %d on the receiver path before configure, want 404", status)
		}

		a := app.New("document-push-explicit")
		a.Use(chosen)
		a.Use(other)
		a.Use(p)
		if err := configureApplication(t, a); err != nil {
			t.Fatalf("configure: %v", err)
		}
		if status := postPush(t, chosen); status != http.StatusUnauthorized {
			t.Errorf("POST push receiver on the chosen server = %d, want 401", status)
		}
		if status := postPush(t, other); status != http.StatusNotFound {
			t.Errorf("POST push receiver on the other server = %d, want 404", status)
		}
	})

	t.Run("a disabled receiver mounts and answers 503", func(t *testing.T) {
		t.Setenv("CONFIG_DATA", pushDocument+"    enabled: false\n")
		server := phttp.NewServerPlugin(phttp.ServerConfig{})
		a := app.New("document-push-disabled")
		a.Use(server)
		a.Use(Events(PluginConfig{Transport: &recordingTransport{}}))

		if err := configureApplication(t, a); err != nil {
			t.Fatalf("configure: %v", err)
		}
		if status := postPush(t, server); status != http.StatusServiceUnavailable {
			t.Errorf("POST disabled push receiver = %d, want 503", status)
		}
	})
}
