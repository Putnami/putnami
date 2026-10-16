package http

import (
	"encoding/json"
	stderrors "errors"
	"net/http"
	"testing"

	"go.putnami.dev/inject"
	"go.putnami.dev/protocol/features/spectest"
)

// envelopeOf reads a response body as the canonical error envelope and fails
// the test when the status, the media type, or any of its three fields differ.
func envelopeOf(t *testing.T, resp *http.Response, status int, code, message string) {
	t.Helper()
	defer resp.Body.Close()
	if resp.StatusCode != status {
		t.Fatalf("status = %d, want %d", resp.StatusCode, status)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	want := map[string]any{
		"code":    code,
		"error":   http.StatusText(status),
		"message": message,
	}
	for key, value := range want {
		if body[key] != value {
			t.Errorf("body[%q] = %v, want %v (body %v)", key, body[key], value, body)
		}
	}
	if len(body) != len(want) {
		t.Errorf("body has %d fields, want %d: %v", len(body), len(want), body)
	}
}

func TestErrorEnvelope_UnknownRouteAnswersNotFound(t *testing.T) {
	spectest.Proves(t, "go/http-services", "error-envelope", "an-unknown-route-answers-404-not-found")
	p := NewServerPlugin(ServerConfig{Port: 0})
	p.GET("/known", func(*Context) *Response { return JSON("ok") })
	ts := p.TestServer()
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/missing")
	if err != nil {
		t.Fatal(err)
	}
	envelopeOf(t, resp, http.StatusNotFound, "not_found", "The requested resource was not found")
}

func TestErrorEnvelope_PanicAnswersInternal(t *testing.T) {
	spectest.Proves(t, "go/http-services", "error-envelope", "a-panic-answers-500-internal-with-the-fixed-message")
	p := NewServerPlugin(ServerConfig{Port: 0})
	p.Use(Recovery())
	p.GET("/boom", func(*Context) *Response { panic("secret panic detail") })
	ts := p.TestServer()
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/boom")
	if err != nil {
		t.Fatal(err)
	}
	envelopeOf(t, resp, http.StatusInternalServerError, "internal", "Internal Server Error")
}

// startedContainer returns a started container context that registers
// providers, closed when the test ends.
func startedContainer(t *testing.T, providers ...inject.Registration) *inject.ContainerContext {
	t.Helper()
	cc := inject.NewContainerContext("http-error-envelope")
	for _, provider := range providers {
		if err := cc.Register(provider); err != nil {
			t.Fatal(err)
		}
	}
	if err := cc.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	return cc
}

func TestErrorEnvelope_ScopeCreationFailureAnswersInternal(t *testing.T) {
	spectest.Proves(t, "go/http-services", "error-envelope", "a-scope-failure-answers-500-internal")
	cc := startedContainer(t)
	p := NewServerPlugin(ServerConfig{Port: 0})
	p.setContainerContext(cc)
	p.GET("/x", func(*Context) *Response { return JSON("ok") })
	ts := p.TestServer()
	defer ts.Close()
	// A closed container refuses to create a request scope.
	if err := cc.Close(); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get(ts.URL + "/x")
	if err != nil {
		t.Fatal(err)
	}
	envelopeOf(t, resp, http.StatusInternalServerError, "internal", "Internal Server Error")
}

type failingScoped struct{}

func TestErrorEnvelope_ScopeResolutionFailureAnswersInternal(t *testing.T) {
	spectest.Proves(t, "go/http-services", "error-envelope", "a-scope-failure-answers-500-internal")
	cc := startedContainer(t, inject.Provide(
		inject.TokenOf[*failingScoped](),
		func(inject.Resolver) (any, error) { return nil, stderrors.New("secret provider failure") },
		inject.WithScope(inject.Scoped),
	))
	p := NewServerPlugin(ServerConfig{Port: 0})
	p.setContainerContext(cc)
	p.GET("/x", Inject(func(_ *failingScoped, _ *Context) *Response { return JSON("ok") }))
	ts := p.TestServer()
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/x")
	if err != nil {
		t.Fatal(err)
	}
	envelopeOf(t, resp, http.StatusInternalServerError, "internal", "Internal Server Error")
}

func TestErrorEnvelope_ScopeCommitFailureAnswersInternal(t *testing.T) {
	spectest.Proves(t, "go/http-services", "error-envelope", "a-scope-failure-answers-500-internal")
	probe := &boundaryProbe{finalizeErr: stderrors.New("secret commit failure")}
	p := newBoundaryServer(t, probe, func(_ *probeFinalizer, _ *Context) *Response {
		return JSON("ok")
	})
	ts := p.TestServer()
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/probe")
	if err != nil {
		t.Fatal(err)
	}
	envelopeOf(t, resp, http.StatusInternalServerError, "internal", "Internal Server Error")
}

func TestErrorEnvelope_HelpersWriteTheCanonicalEnvelope(t *testing.T) {
	spectest.Proves(t, "go/http-services", "error-envelope", "the-error-helpers-write-the-canonical-envelope")
	cases := []struct {
		name    string
		resp    *Response
		status  int
		code    string
		message string
	}{
		{"NotFound", NotFound(), http.StatusNotFound, "not_found", "The requested resource was not found"},
		{"Unauthorized", Unauthorized(), http.StatusUnauthorized, "unauthorized", "Authentication required"},
		{"Forbidden", Forbidden(), http.StatusForbidden, "forbidden", "Insufficient permissions"},
		{"InternalError", InternalError("decode failed"), http.StatusInternalServerError, "internal", "decode failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := NewServerPlugin(ServerConfig{Port: 0})
			resp := tc.resp
			p.GET("/x", func(*Context) *Response { return resp })
			ts := p.TestServer()
			defer ts.Close()

			got, err := http.Get(ts.URL + "/x")
			if err != nil {
				t.Fatal(err)
			}
			envelopeOf(t, got, tc.status, tc.code, tc.message)
		})
	}
}
