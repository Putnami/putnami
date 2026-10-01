package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	phttp "go.putnami.dev/http"
)

func TestHealthEndpoint(t *testing.T) {
	t.Setenv("PORT", "0")
	a, server := newApp(ServerConfig{})
	a.Run(func(context.Context) error { return nil })
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("failed to start the application: %v", err)
	}
	t.Cleanup(func() {
		if err := a.Stop(context.Background()); err != nil {
			t.Errorf("failed to stop the application: %v", err)
		}
	})

	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/_/health", nil))

	if status := rr.Code; status != http.StatusOK {
		t.Errorf("GET /_/health returned wrong status code: got %v want %v, body %s", status, http.StatusOK, rr.Body)
	}
}

func TestRootHandler(t *testing.T) {
	handler := func(_ *phttp.Context) *phttp.Response {
		return phttp.JSON(map[string]string{"Hello": "World"})
	}

	req := httptest.NewRequest("GET", "/", nil)
	rr := httptest.NewRecorder()
	ctx := phttp.NewContext(rr, req)

	resp := handler(ctx)
	if err := resp.WriteTo(rr); err != nil {
		t.Fatalf("failed to write response: %v", err)
	}

	if status := rr.Code; status != http.StatusOK {
		t.Errorf("handler returned wrong status code: got %v want %v", status, http.StatusOK)
	}

	var result map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&result); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if result["Hello"] != "World" {
		t.Errorf("handler returned unexpected body: got %v want %v", result["Hello"], "World")
	}
}
