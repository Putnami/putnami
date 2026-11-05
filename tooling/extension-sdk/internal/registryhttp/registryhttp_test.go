package registryhttp

import (
	"net/http"
	"net/url"
	"testing"

	"go.putnami.dev/sdk/extension/registryorigin"
)

func TestValidateURLKeepsHTTPSAndLoopbackHTTP(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"http://localhost:8080/", "http://localhost:8080"},
		{"http://[::1]:8080", "http://[::1]:8080"},
		{"https://registry.example/a%2Fb", "https://registry.example/a/b"},
	} {
		if got, err := ValidateURL("go", tc.raw); err != nil || got != tc.want {
			t.Errorf("ValidateURL(%q) = %q, %v; want %q", tc.raw, got, err, tc.want)
		}
	}
}

func TestValidateURLRefusesWithoutEchoingTheURL(t *testing.T) {
	for raw, want := range map[string]string{
		"":                                       "put registry URL must be an absolute HTTP(S) URL without credentials",
		"registry.example":                       "put registry URL must be an absolute HTTP(S) URL without credentials",
		"mailto:put@registry.example":            "put registry URL must be an absolute HTTP(S) URL without credentials",
		"https://user:secret@registry.example":   "put registry URL must be an absolute HTTP(S) URL without credentials, query, or fragment",
		"https://registry.example/?token=secret": "put registry URL must be an absolute HTTP(S) URL without credentials, query, or fragment",
		"https://registry.example/?":             "put registry URL must be an absolute HTTP(S) URL without credentials, query, or fragment",
		"https://registry.example/#secret":       "put registry URL must be an absolute HTTP(S) URL without credentials, query, or fragment",
		"http://registry.example":                "put registry URL must use HTTPS (HTTP is allowed only for loopback)",
		"ftp://registry.example":                 "put registry URL must use HTTP(S)",
	} {
		got, err := ValidateURL("put", raw)
		if err == nil || err.Error() != want {
			t.Errorf("ValidateURL(%q) = %q, %v; want the refusal %q", raw, got, err, want)
		}
	}
}

func TestNewClientHasAFiveMinuteTimeoutAndMapsOrigins(t *testing.T) {
	client, err := NewClient("test transport", nil)
	if err != nil {
		t.Fatal(err)
	}
	if client.Timeout.Minutes() != 5 {
		t.Fatalf("timeout %s, want five minutes", client.Timeout)
	}
	origin, err := url.Parse("https://origin.example")
	if err != nil {
		t.Fatal(err)
	}
	mapped, err := NewClient("test transport", registryorigin.Origins{"registry.example": origin})
	if err != nil {
		t.Fatal(err)
	}
	if _, plain := mapped.Transport.(*http.Transport); plain {
		t.Fatal("a mapped origin left the plain transport in place")
	}
}

func TestWithoutRedirectsCopiesTheClient(t *testing.T) {
	if _, err := WithoutRedirects(nil, "put publication"); err == nil || err.Error() != "put publication requires an HTTP client" {
		t.Fatalf("WithoutRedirects(nil) = %v", err)
	}
	follow := func(*http.Request, []*http.Request) error { return nil }
	original := &http.Client{CheckRedirect: follow}
	bounded, err := WithoutRedirects(original, "put publication")
	if err != nil || bounded == original || original.CheckRedirect == nil {
		t.Fatalf("WithoutRedirects changed its argument or failed: %v", err)
	}
}
