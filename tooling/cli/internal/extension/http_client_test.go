package extension

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	proto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/sdk/extension/recorded"
	"go.putnami.dev/tooling/cli/internal/useragent"
)

func TestResolveHTTPTimeout(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want time.Duration
	}{
		{"empty falls back to default", "", DefaultHTTPTimeout},
		{"duration string", "90s", 90 * time.Second},
		{"minutes", "2m", 2 * time.Minute},
		{"bare integer = seconds", "120", 120 * time.Second},
		{"invalid input falls back", "not-a-duration", DefaultHTTPTimeout},
		{"negative zero falls back", "0", DefaultHTTPTimeout},
		{"negative falls back", "-5", DefaultHTTPTimeout},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveHTTPTimeout(tt.in); got != tt.want {
				t.Errorf("resolveHTTPTimeout(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestResolveRetryBackoff(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want time.Duration
	}{
		{"empty falls back to default", "", HTTPRetryBackoff},
		{"duration string", "500ms", 500 * time.Millisecond},
		{"bare integer = seconds", "2", 2 * time.Second},
		{"zero is honored (disables sleep)", "0", 0},
		{"zero duration is honored", "0s", 0},
		{"invalid input falls back", "not-a-duration", HTTPRetryBackoff},
		{"negative falls back", "-5", HTTPRetryBackoff},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveRetryBackoff(tt.in); got != tt.want {
				t.Errorf("resolveRetryBackoff(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestIsTransientStatus(t *testing.T) {
	transient := []int{408, 429, 500, 502, 503, 504, 599}
	for _, code := range transient {
		if !isTransientStatus(code) {
			t.Errorf("%d should be transient", code)
		}
	}
	notTransient := []int{200, 301, 400, 401, 403, 404}
	for _, code := range notTransient {
		if isTransientStatus(code) {
			t.Errorf("%d should not be transient", code)
		}
	}
}

// TestDoWithRetry_RetriesOn5xx covers the headline case: a transient 5xx
// once, then 200. The fault is the 502 an archive upload received on a
// main-branch run. DoWithRetry must observe it, drain+close it, sleep the
// backoff, and return the 200.
func TestDoWithRetry_RetriesOn5xx(t *testing.T) {
	t.Setenv(HTTPRetryBackoffEnv, "0") // assert call count, not wall-clock
	srv := recorded.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}), putRegistryRecording(t, "gateway-reset-before-headers.502.http"))

	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	resp, err := DoWithRetry(context.Background(), srv.Client(), req)
	if err != nil {
		t.Fatalf("DoWithRetry: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want 200", resp.StatusCode)
	}
	if got := len(srv.Requests()); got != 2 {
		t.Errorf("expected 2 calls (1 retry), got %d", got)
	}
}

func TestDoWithRetrySetsUserAgent(t *testing.T) {
	useragent.SetVersion("v9.8.7")
	t.Cleanup(func() { useragent.SetVersion("dev") })

	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	resp, err := DoWithRetry(context.Background(), srv.Client(), req)
	if err != nil {
		t.Fatalf("DoWithRetry: %v", err)
	}
	defer resp.Body.Close()

	if gotUA != "putnami-cli/v9.8.7" {
		t.Fatalf("User-Agent = %q, want putnami-cli/v9.8.7", gotUA)
	}
}

func TestRegistryHTTPClientPropagatesOptedInMCPIdentity(t *testing.T) {
	identity := useragent.MCPIdentity("v9.8.7", "claude-cli", "1.0", "haiku")
	useragent.SetAgentIdentity(identity)
	t.Cleanup(useragent.ClearAgentIdentity)

	var gotUA, gotHarness, gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		gotHarness = r.Header.Get(proto.AgentHarnessHeader)
		gotModel = r.Header.Get(proto.AgentModelHeader)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	resp, err := NewRegistryHTTPClient().Get(srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()

	if gotUA != identity.UserAgent {
		t.Errorf("User-Agent = %q, want %q", gotUA, identity.UserAgent)
	}
	if gotHarness != identity.Harness {
		t.Errorf("%s = %q, want %q", proto.AgentHarnessHeader, gotHarness, identity.Harness)
	}
	if gotModel != identity.Model {
		t.Errorf("%s = %q, want %q", proto.AgentModelHeader, gotModel, identity.Model)
	}
}

// TestDoWithRetry_StopsAfterMaxAttempts confirms the retry budget is
// bounded — 3 attempts that all meet the recorded gateway reset return the last
// error and stop.
func TestDoWithRetry_StopsAfterMaxAttempts(t *testing.T) {
	t.Setenv(HTTPRetryBackoffEnv, "0") // assert call count, not wall-clock
	srv := recorded.NewServer(t, nil, putRegistryRecording(t, "gateway-reset-before-headers.502.http"))

	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	_, err := DoWithRetry(context.Background(), srv.Client(), req)
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	if got := len(srv.Requests()); got != HTTPRetryAttempts {
		t.Errorf("expected %d calls, got %d", HTTPRetryAttempts, got)
	}
}

// TestDoWithRetry_NoRetryOn4xx ensures non-transient responses return
// immediately — the client must not retry a 404 or auth failure.
func TestDoWithRetry_NoRetryOn4xx(t *testing.T) {
	srv := recorded.NewServer(t, nil, putRegistryRecording(t, "download-version-not-found.404.http"))

	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	resp, err := DoWithRetry(context.Background(), srv.Client(), req)
	if err != nil {
		t.Fatalf("DoWithRetry: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("StatusCode = %d, want 404", resp.StatusCode)
	}
	if got := len(srv.Requests()); got != 1 {
		t.Errorf("expected 1 call (no retries), got %d", got)
	}
}

// TestDoWithRetry_ContextCancelledStops verifies that context
// cancellation short-circuits the backoff sleep — the wrapper must not
// burn the full retry budget after the caller has bailed.
func TestDoWithRetry_ContextCancelledStops(t *testing.T) {
	srv := recorded.NewServer(t, nil, putRegistryRecording(t, "gateway-reset-before-headers.502.http"))

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel after the first attempt completes but before the backoff
	// elapses.
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	_, err := DoWithRetry(ctx, srv.Client(), req)
	if err == nil {
		t.Fatal("expected error from canceled context")
	}
}
