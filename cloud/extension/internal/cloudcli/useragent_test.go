package cloudcli

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCLIUserAgentEnvAndFallback(t *testing.T) {
	t.Setenv(userAgentEnvVar, "putnami-cli/2.3.4")
	if got := cliUserAgent(); got != "putnami-cli/2.3.4" {
		t.Fatalf("cliUserAgent() = %q, want the exported env value", got)
	}
	// An empty env var is treated as unset so the fallback identifies dev runs.
	t.Setenv(userAgentEnvVar, "")
	if got := cliUserAgent(); got != fallbackUserAgent {
		t.Fatalf("cliUserAgent() = %q, want fallback %q", got, fallbackUserAgent)
	}
}

func TestSetUserAgentOnlyStampsWhenAbsent(t *testing.T) {
	t.Setenv(userAgentEnvVar, "putnami-cli/1.0.0")

	// Absent → stamped with the unified identity.
	req, _ := http.NewRequest(http.MethodGet, "http://example.invalid", nil)
	setUserAgent(req)
	if got := req.Header.Get("User-Agent"); got != "putnami-cli/1.0.0" {
		t.Fatalf("absent User-Agent: got %q, want putnami-cli/1.0.0", got)
	}

	// Present (e.g. a presigned grant/transfer header) → never clobbered.
	signed, _ := http.NewRequest(http.MethodPut, "http://example.invalid", nil)
	signed.Header.Set("User-Agent", "presigned-transfer/1")
	setUserAgent(signed)
	if got := signed.Header.Get("User-Agent"); got != "presigned-transfer/1" {
		t.Fatalf("present User-Agent was clobbered: got %q, want presigned-transfer/1", got)
	}
}

// TestSendJSONStampsUserAgent covers the shared hand-built JSON request path
// the CLI's external and OIDC calls funnel through.
func TestSendJSONStampsUserAgent(t *testing.T) {
	cases := []struct{ name, env, want string }{
		{"under the CLI", "putnami-cli/9.9.9", "putnami-cli/9.9.9"},
		{"env unset falls back", "", fallbackUserAgent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(userAgentEnvVar, tc.env)
			var got string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = r.Header.Get("User-Agent")
				_, _ = w.Write([]byte(`{}`))
			}))
			defer srv.Close()

			req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := sendJSON(srv.Client(), req, nil); err != nil {
				t.Fatalf("sendJSON: %v", err)
			}
			if got != tc.want {
				t.Fatalf("User-Agent = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestClientOrDefaultHardensTransport proves the shared client stamps the UA
// even when a caller never calls setUserAgent — the regression guard for a
// future request builder.
func TestClientOrDefaultHardensTransport(t *testing.T) {
	t.Setenv(userAgentEnvVar, "putnami-cli/5.0.0")
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("User-Agent")
	}))
	defer srv.Close()

	client := clientOrDefault(srv.Client())
	req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	_ = resp.Body.Close()
	if got != "putnami-cli/5.0.0" {
		t.Fatalf("hardened transport User-Agent = %q, want putnami-cli/5.0.0", got)
	}

	// Re-resolving an already-hardened client must not double-wrap.
	if clientOrDefault(client) != client {
		t.Fatal("clientOrDefault should be idempotent on an already-wrapped client")
	}
}
