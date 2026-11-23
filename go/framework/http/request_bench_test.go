package http

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"go.putnami.dev/inject"
	"go.putnami.dev/logger"
)

// discardSink drops every log entry.
type discardSink struct{}

func (discardSink) Write(logger.LogEntry) {}
func (discardSink) Flush() error          { return nil }
func (discardSink) Close() error          { return nil }

// benchWriter is a ResponseWriter that keeps no body and reuses its header map.
type benchWriter struct {
	header http.Header
	status int
}

func (w *benchWriter) Header() http.Header         { return w.header }
func (w *benchWriter) Write(b []byte) (int, error) { return len(b), nil }
func (w *benchWriter) WriteHeader(status int)      { w.status = status }

// benchServer is a started server with a DI container and the default
// middleware chain: Recovery, RequestID and Logging at the info level.
func benchServer(b *testing.B) http.HandlerFunc {
	b.Helper()
	cc := inject.NewContainerContext("bench")
	if err := cc.Register(inject.AutoProvide(newTestService)); err != nil {
		b.Fatal(err)
	}
	if err := cc.Start(); err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = cc.Close() })

	server := NewServerPlugin(ServerConfig{})
	server.setContainerContext(cc)
	server.Use(Recovery())
	server.Use(RequestID())
	server.Use(Logging(LoggerOptions{Logger: logger.New("http", logger.LevelInfo, discardSink{})}))
	server.GET("/health", func(_ *Context) *Response {
		return JSON(map[string]string{"status": "ok"})
	})
	ih := Inject(func(ctx *Context, svc *testService) *Response {
		return JSON(map[string]string{"id": ctx.Param("id"), "name": svc.Name})
	})
	server.GET("/tasks/{id}", ih.Handle)
	if err := ih.Finalize(cc); err != nil {
		b.Fatal(err)
	}
	server.wrapRoutesOnce()
	return server.buildHandler()
}

func benchmarkRoute(b *testing.B, path string) {
	handler := benchServer(b)
	req := httptest.NewRequest(http.MethodGet, path, nil)
	w := &benchWriter{header: make(http.Header)}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		clear(w.header)
		req.Body = http.NoBody
		handler(w, req)
		if w.status != http.StatusOK {
			b.Fatalf("status %d", w.status)
		}
	}
}

// BenchmarkRequestJSONRoute measures one JSON route through the default
// middleware chain, the request deadline and the request scope.
func BenchmarkRequestJSONRoute(b *testing.B) {
	benchmarkRoute(b, "/health")
}

// BenchmarkRequestInjectedRoute measures one injected route with a path
// parameter through the same chain.
func BenchmarkRequestInjectedRoute(b *testing.B) {
	benchmarkRoute(b, "/tasks/42")
}
