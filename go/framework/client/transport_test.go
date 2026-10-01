package client

import (
	"go.putnami.dev/protocol/features/spectest"

	"context"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/errors"
)

func TestSafeErrorURL(t *testing.T) {
	cases := []struct {
		name    string
		baseURL string
		path    string
		query   map[string]string
		want    string
	}{
		{"no query", "https://api.example.com", "/v1/users", nil, "https://api.example.com/v1/users"},
		{
			"query values dropped, keys kept and sorted",
			"https://api.example.com", "/v1/users",
			map[string]string{"token": "secret", "page": "2"},
			"https://api.example.com/v1/users?page&token",
		},
		{
			"inline query in path is redacted too",
			"https://api.example.com", "/v1/users?sig=deadbeef",
			nil,
			"https://api.example.com/v1/users?sig",
		},
		{
			"inline path query and map query merge",
			"https://api.example.com", "/v1/users?sig=deadbeef",
			map[string]string{"page": "2"},
			"https://api.example.com/v1/users?page&sig",
		},
	}
	for _, c := range cases {
		if got := safeErrorURL(c.baseURL, c.path, c.query); got != c.want {
			t.Errorf("%s: safeErrorURL = %q, want %q", c.name, got, c.want)
		}
	}
}

// urlAttrValue extracts the "url" attribute from a wrapped client error.
func urlAttrValue(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	e, ok := err.(*errors.Error)
	if !ok {
		t.Fatalf("expected *errors.Error, got %T", err)
	}
	for _, a := range e.Attrs() {
		if a.Key == "url" {
			s, _ := a.Value.(string)
			return s
		}
	}
	t.Fatalf("no url attr found in error: %v", err)
	return ""
}

func TestHTTPTransport_ErrorURLRedactsQueryValues(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "error-redaction", "an-http-transport-error-never-carries-query-values")
	tr := NewHTTPTransport(HTTPTransportConfig{
		BaseURL: "http://127.0.0.1:1", // refuses fast
		Timeout: 300 * time.Millisecond,
	})
	_, err := tr.Do(context.Background(), &Request{
		Method: "GET",
		Path:   "/v1/resource",
		Query:  map[string]string{"api_key": "supersecret", "page": "2"},
	})

	got := urlAttrValue(t, err)
	if strings.Contains(got, "supersecret") {
		t.Errorf("error url attr leaked a query value: %q", got)
	}
	if want := "http://127.0.0.1:1/v1/resource?api_key&page"; got != want {
		t.Errorf("error url attr = %q, want %q", got, want)
	}
}

func TestConnectTransport_ErrorURLHasNoQueryValues(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "error-redaction", "a-connect-transport-error-never-carries-query-values")
	tr := NewConnectTransport(ConnectTransportConfig{
		BaseURL: "http://127.0.0.1:1",
		Timeout: 300 * time.Millisecond,
	})
	_, err := tr.Do(context.Background(), &Request{
		Path: "/service.v1.Service/Method",
	})

	got := urlAttrValue(t, err)
	if want := "http://127.0.0.1:1/service.v1.Service/Method"; got != want {
		t.Errorf("error url attr = %q, want %q", got, want)
	}
}
