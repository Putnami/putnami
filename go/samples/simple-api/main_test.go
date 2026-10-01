package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	phttp "go.putnami.dev/http"
)

func buildTestServer(t *testing.T) *httptest.Server {
	t.Helper()

	// Reset store between tests.
	storeMu.Lock()
	store = map[string]*Greeting{}
	seq = 0
	storeMu.Unlock()

	server := phttp.NewServerPlugin(phttp.ServerConfig{Port: 0})
	server.GET("/greetings", listGreetings)
	server.POST("/greetings", createGreeting)
	server.GET("/greetings/{id}", getGreeting)
	server.DELETE("/greetings/{id}", deleteGreeting)

	return server.TestServer()
}

// TestHealthEndpoint starts the application main serves and requests every
// operational route through its server.
func TestHealthEndpoint(t *testing.T) {
	t.Setenv("PORT", "0")
	a, server := newApp()
	a.Run(func(context.Context) error { return nil })
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("start the application: %v", err)
	}
	t.Cleanup(func() {
		if err := a.Stop(context.Background()); err != nil {
			t.Errorf("stop the application: %v", err)
		}
	})

	for _, path := range []string{"/_/health", "/livez", "/healthz", "/readyz", "/version"} {
		rr := httptest.NewRecorder()
		server.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code != http.StatusOK {
			t.Errorf("GET %s = %d %s, want 200", path, rr.Code, rr.Body)
		}
	}
}

func doReq(t *testing.T, ts *httptest.Server, method, path, body string) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, ts.URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestCreateAndGet(t *testing.T) {
	ts := buildTestServer(t)
	defer ts.Close()

	// Create
	resp := doReq(t, ts, "POST", "/greetings", `{"message":"Hello, World!"}`)
	defer resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}

	var g Greeting
	json.NewDecoder(resp.Body).Decode(&g)
	if g.Message != "Hello, World!" {
		t.Errorf("expected message 'Hello, World!', got %q", g.Message)
	}
	if g.ID == "" {
		t.Error("expected non-empty ID")
	}

	// Get
	resp2 := doReq(t, ts, "GET", "/greetings/"+g.ID, "")
	defer resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp2.StatusCode)
	}
}

func TestListGreetings(t *testing.T) {
	ts := buildTestServer(t)
	defer ts.Close()

	doReq(t, ts, "POST", "/greetings", `{"message":"One"}`).Body.Close()
	doReq(t, ts, "POST", "/greetings", `{"message":"Two"}`).Body.Close()

	resp := doReq(t, ts, "GET", "/greetings", "")
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var items []Greeting
	json.NewDecoder(resp.Body).Decode(&items)
	if len(items) != 2 {
		t.Errorf("expected 2 greetings, got %d", len(items))
	}
}

func TestDeleteGreeting(t *testing.T) {
	ts := buildTestServer(t)
	defer ts.Close()

	resp := doReq(t, ts, "POST", "/greetings", `{"message":"To delete"}`)
	var g Greeting
	json.NewDecoder(resp.Body).Decode(&g)
	resp.Body.Close()

	del := doReq(t, ts, "DELETE", "/greetings/"+g.ID, "")
	defer del.Body.Close()
	if del.StatusCode != 204 {
		t.Fatalf("expected 204, got %d", del.StatusCode)
	}

	get := doReq(t, ts, "GET", "/greetings/"+g.ID, "")
	defer get.Body.Close()
	if get.StatusCode != 404 {
		t.Errorf("expected 404 after delete, got %d", get.StatusCode)
	}
}

func TestCreateValidation(t *testing.T) {
	ts := buildTestServer(t)
	defer ts.Close()

	resp := doReq(t, ts, "POST", "/greetings", `{"message":""}`)
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestGetNotFound(t *testing.T) {
	ts := buildTestServer(t)
	defer ts.Close()

	resp := doReq(t, ts, "GET", "/greetings/nonexistent", "")
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}
