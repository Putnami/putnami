package openapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	phttp "go.putnami.dev/http"
)

// TestRegisterOn_Handler covers:
//   - RegisterOn itself (the spec-serving endpoint registration path)
//   - the 503 "spec not yet generated" branch (handler called before Configure)
//   - the 200 success path with Cache-Control header (handler called after Configure)
func TestRegisterOn_Handler(t *testing.T) {
	plugin := NewPlugin(PluginOptions{
		Title:        "Register Test",
		Version:      "0.1.0",
		CacheControl: "public, max-age=3600",
	})

	// Build a real ServerPlugin and register the spec-serving endpoint via RegisterOn.
	// This exercises plugin.go:182-184 (RegisterOn).
	server := phttp.NewServerPlugin(phttp.ServerConfig{})
	plugin.RegisterOn(server)

	// Spin up a test HTTP server using the production routing path.
	ts := server.TestServer()
	defer ts.Close()

	route := plugin.opts.Route // "/_/openapi.json"

	// --- 503 branch: spec not yet generated (Configure not called) ---
	resp503, err := http.Get(ts.URL + route)
	if err != nil {
		t.Fatalf("GET before Configure: %v", err)
	}
	defer resp503.Body.Close()

	if resp503.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("before Configure: status = %d, want %d", resp503.StatusCode, http.StatusServiceUnavailable)
	}

	body503, err := io.ReadAll(resp503.Body)
	if err != nil {
		t.Fatalf("read 503 body: %v", err)
	}
	var errPayload map[string]string
	if err := json.Unmarshal(body503, &errPayload); err != nil {
		t.Fatalf("unmarshal 503 body %q: %v", body503, err)
	}
	if errPayload["error"] == "" {
		t.Errorf("503 body missing error field; got %v", errPayload)
	}

	// --- Generate the spec via Configure ---
	if err := plugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("Configure: %v", err)
	}

	// --- 200 success path: spec is available ---
	resp200, err := http.Get(ts.URL + route)
	if err != nil {
		t.Fatalf("GET after Configure: %v", err)
	}
	defer resp200.Body.Close()

	if resp200.StatusCode != http.StatusOK {
		t.Errorf("after Configure: status = %d, want %d", resp200.StatusCode, http.StatusOK)
	}

	// Cache-Control header must be set to the configured value.
	cc := resp200.Header.Get("Cache-Control")
	if cc != "public, max-age=3600" {
		t.Errorf("Cache-Control = %q, want %q", cc, "public, max-age=3600")
	}

	// Body must be a valid JSON OpenAPI document.
	body200, err := io.ReadAll(resp200.Body)
	if err != nil {
		t.Fatalf("read 200 body: %v", err)
	}
	var specPayload map[string]any
	if err := json.Unmarshal(body200, &specPayload); err != nil {
		t.Fatalf("unmarshal spec body %q: %v", body200, err)
	}
	if specPayload["openapi"] != "3.0.3" {
		t.Errorf("spec openapi field = %v, want 3.0.3", specPayload["openapi"])
	}
}
