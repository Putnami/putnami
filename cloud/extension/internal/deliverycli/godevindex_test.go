package deliverycli

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// TestReadGoDevIndexSendsTheIndexRequest pins the request go.dev answers: a
// GET of the download root with the machine-readable query, asking for JSON
// under the CLI User-Agent.
func TestReadGoDevIndexSendsTheIndexRequest(t *testing.T) {
	var method, query, accept, agent string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, query = r.Method, r.URL.RawQuery
		accept, agent = r.Header.Get("Accept"), r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"version":"go1.26.1","files":[{"filename":"go1.26.1.linux-amd64.tar.gz","sha256":"abc"}]}]`))
	}))
	defer server.Close()

	releases, err := readGoDevIndex(server.Client(), server.URL+"/"+goDevIndexQuery)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(releases) != 1 || releases[0].Version != "go1.26.1" || len(releases[0].Files) != 1 || releases[0].Files[0].SHA256 != "abc" {
		t.Fatalf("releases = %+v", releases)
	}
	if method != http.MethodGet || query != "mode=json&include=all" || accept != "application/json" || agent == "" {
		t.Fatalf("request = %s ?%s Accept %q User-Agent %q", method, query, accept, agent)
	}
}

func TestReadGoDevIndexReadsAnEmptyIndex(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer server.Close()
	releases, err := readGoDevIndex(server.Client(), server.URL+"/")
	if err != nil || len(releases) != 0 {
		t.Fatalf("read = %+v, %v; want an empty index", releases, err)
	}
}

func TestReadGoDevIndexSurfacesARefusal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "upstream unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	_, err := readGoDevIndex(server.Client(), server.URL+"/")
	if err == nil || clicore.ExitCode(err) != clicore.ExitAPI || !strings.Contains(err.Error(), "503") {
		t.Fatalf("read = %v, want an ExitAPI refusal naming the status", err)
	}
}

func TestReadGoDevIndexRefusesAnAnswerThatIsNotTheIndex(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html>moved</html>`))
	}))
	defer server.Close()
	_, err := readGoDevIndex(server.Client(), server.URL+"/")
	if err == nil || !strings.Contains(err.Error(), "invalid JSON response") {
		t.Fatalf("read = %v, want an invalid JSON response", err)
	}
}

func TestReadGoDevIndexRefusesAnUnbuildableURL(t *testing.T) {
	if _, err := readGoDevIndex(nil, "://not a url"); err == nil {
		t.Fatal("an unbuildable index URL was requested")
	}
}
