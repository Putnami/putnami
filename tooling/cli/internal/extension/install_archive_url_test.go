package extension

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestOpenArtifact_EscapesTheRegistryPath pins that the namespace and the
// package travel as escaped path segments: a name that holds a '#', a '?' or a
// space reaches the registry as one route with its channel, os and arch query
// intact, and the lock URL carries the same escaped route.
func TestOpenArtifact_EscapesTheRegistryPath(t *testing.T) {
	var requested []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested = append(requested, r.RequestURI)
		w.Header().Set("X-Resolved-Version", "1.2.3")
		_, _ = w.Write([]byte("archive"))
	}))
	t.Cleanup(srv.Close)
	inst := &Installer{WorkspaceRoot: t.TempDir(), ResolverURL: srv.URL, HTTPClient: srv.Client(), AnonymousRegistry: true}

	for name, route := range map[string]string{
		"@putnami/go":      "/putnami/go/download",
		"@putnami/dev#":    "/putnami/dev%23/download",
		"@putnami/dev?x=1": "/putnami/dev%3Fx=1/download",
		"@acme/a b":        "/acme/a%20b/download",
		"go":               "/putnami/go/download",
	} {
		requested = nil
		resp, lockURL, err := inst.openArtifact(context.Background(), name, "canary", ArtifactSpec{OS: "linux", Arch: "x64"}, false)
		if err != nil {
			t.Fatalf("openArtifact(%q): %v", name, err)
		}
		resp.Body.Close()
		if want := route + "?arch=x64&channel=canary&os=linux"; len(requested) != 1 || requested[0] != want {
			t.Errorf("openArtifact(%q) requested %v, want [%s]", name, requested, want)
		}
		if want := srv.URL + route + "?channel=canary"; lockURL != want {
			t.Errorf("openArtifact(%q) lock URL = %q, want %q", name, lockURL, want)
		}
		if got, want := inst.sourceURL(name, "1.2.3", "canary"), srv.URL+route+"?channel=1.2.3"; got != want {
			t.Errorf("sourceURL(%q) = %q, want %q", name, got, want)
		}
	}

	err := inst.artifactFetchError("@putnami/dev#", "canary", map[string][]string{"channel": {"canary"}}, ArtifactSpec{OS: "linux", Arch: "x64"}, http.StatusNotFound, false, "")
	if err == nil || !strings.Contains(err.Error(), "/putnami/dev%23/download?channel=canary") {
		t.Errorf("artifactFetchError names the escaped route, got %v", err)
	}
}
