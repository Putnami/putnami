package http

import (
	"net/http"
	"net/http/httptest"
)

// TestServer creates an httptest.Server wired with this server's routes and middleware.
// This eliminates the need to manually recreate routing logic in tests.
//
// It runs the exact production request path: injected handlers are finalized,
// the middleware chain is pre-composed onto every route (as Start does), and
// requests are dispatched through buildHandler — so the body-size limit, the
// per-request DI scope, and the pre-wrapped handlers all behave identically to
// production. In particular, http.Inject handlers work through this helper.
//
// Example:
//
//	server := http.NewServerPlugin(http.ServerConfig{Port: 0})
//	server.GET("/tasks", handler.List)
//	server.POST("/tasks", handler.Create)
//
//	ts := server.TestServer()
//	defer ts.Close()
//
//	resp, _ := http.Get(ts.URL + "/tasks")
func (p *ServerPlugin) TestServer() *httptest.Server {
	// Finalize injected handlers so http.Inject routes don't panic when called.
	// Mirrors ServerPlugin.Configure; Finalize is idempotent and handles a nil
	// container (context-only handlers) gracefully.
	p.mu.RLock()
	cc := p.cc
	p.mu.RUnlock()
	for _, ih := range p.pending {
		if err := ih.Finalize(cc); err != nil {
			p.log.Error("finalize injected handler for test server", err)
		}
	}

	// Pre-compose the middleware chain onto all routes, exactly like Start().
	p.wrapRoutesOnce()

	mux := http.NewServeMux()
	mux.HandleFunc("/", p.buildHandler())

	return httptest.NewServer(mux)
}

// NewTestStreamContext builds a StreamContext wired to the given send function
// and inbound raw-message channel, without standing up a real SSE/WebSocket
// transport. It lets downstream packages (e.g. go.putnami.dev/api) unit-test
// stream handler adapters and the typed decode path.
//
// send receives each value passed to StreamContext.Send; pass nil to model a
// receive-only stream (Send then reports the stream as unavailable). rawMessages
// supplies the frames returned by StreamContext.RawMessages; pass nil to model a
// send-only stream (RawMessages then yields a closed channel).
func NewTestStreamContext(ctx *Context, send func(any) error, rawMessages <-chan []byte) *StreamContext {
	return newStreamContext(ctx, send, rawMessages)
}
