package jira

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
)

const (
	testEmail = "bot@example.com"
	testToken = "ATATT3xFfGF0FAKEtokenFAKEtokenFAKEtoken0000=ABCD1234"
)

func environment(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

func noPause(context.Context, int) error { return nil }

// newTestClient returns a client of a loopback server answering handler.
func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	env := environment(map[string]string{OverrideVariable: server.URL, EmailVariable: testEmail, TokenVariable: testToken})
	endpoint, err := ResolveEndpoint("acme.atlassian.net", env)
	if err != nil {
		t.Fatal(err)
	}
	return New(endpoint, NewCredential(env), Options{
		HTTP:    &http.Client{Transport: &http.Transport{DisableKeepAlives: true}},
		Timeout: 5 * time.Second, Pause: noPause,
	})
}

func TestTheOverrideReachesOnlyTheLoopbackInterface(t *testing.T) {
	spectest.Proves(t, "tooling/jira-collaboration", "credential-safety", "the-override-reaches-only-the-loopback-interface")
	endpoint, err := ResolveEndpoint("acme.atlassian.net", environment(nil))
	if err != nil || endpoint.REST.String() != "https://acme.atlassian.net/rest/api/2" {
		t.Fatalf("the default root is %v, %v", endpoint.REST, err)
	}
	for _, override := range []string{"http://127.0.0.1:8080", "http://localhost:1/", "https://[::1]:443"} {
		endpoint, err := ResolveEndpoint("acme.atlassian.net", environment(map[string]string{OverrideVariable: override}))
		if err != nil || !strings.HasSuffix(endpoint.REST.String(), "/rest/api/2") {
			t.Fatalf("loopback override %s: %v, %v", override, endpoint.REST, err)
		}
	}
	for _, override := range []string{"https://evil.example.com", "http://10.0.0.1", "http://u:p@127.0.0.1", "http://127.0.0.1/?q=1", "ftp://127.0.0.1"} {
		if _, err := ResolveEndpoint("acme.atlassian.net", environment(map[string]string{OverrideVariable: override})); err == nil {
			t.Fatalf("override %s was accepted", override)
		}
	}
	for _, site := range []string{"", "https://acme.atlassian.net", "acme.atlassian.net/x", "u@acme.atlassian.net"} {
		if _, err := ResolveEndpoint(site, environment(nil)); err == nil {
			t.Fatalf("site %q was accepted", site)
		}
	}
}

func TestTheCredentialIsBasicFromTheEnvironment(t *testing.T) {
	credential := NewCredential(environment(map[string]string{EmailVariable: " " + testEmail + "\n", TokenVariable: testToken}))
	header, err := credential.Authorization()
	if err != nil {
		t.Fatal(err)
	}
	encoded := base64.StdEncoding.EncodeToString([]byte(testEmail + ":" + testToken))
	if header != "Basic "+encoded {
		t.Fatalf("header %q", header)
	}
	secrets := credential.Secrets()
	if len(secrets) != 2 || secrets[0] != testToken || secrets[1] != encoded {
		t.Fatalf("secrets %v", secrets)
	}
	for name, env := range map[string]map[string]string{
		"no email":            {TokenVariable: testToken},
		"no token":            {EmailVariable: testEmail},
		"a colon in email":    {EmailVariable: "a:b@example.com", TokenVariable: testToken},
		"a space in token":    {EmailVariable: testEmail, TokenVariable: "abc def ghij"},
		"a quote in token":    {EmailVariable: testEmail, TokenVariable: `abcdefgh"ij`},
		"a too short a token": {EmailVariable: testEmail, TokenVariable: "abc"},
	} {
		credential := NewCredential(environment(env))
		if _, err := credential.Authorization(); err == nil || err.Kind != NoCredential {
			t.Fatalf("%s: %+v", name, err)
		} else if strings.Contains(err.Message, testToken) {
			t.Fatalf("%s: the message carries the token: %s", name, err.Message)
		}
		if len(credential.Secrets()) != 0 {
			t.Fatalf("%s: an unresolved credential lists secrets", name)
		}
	}
}

func TestRedactRemovesSecretsAndAtlassianTokens(t *testing.T) {
	text := "sent " + testToken + " and Basic Zm9vOmJhcg== and ATATT3xOTHERtokenOTHERtokenOTHER99"
	redacted := Redact(text, []string{testToken, "Zm9vOmJhcg=="})
	if strings.Contains(redacted, "ATATT") || strings.Contains(redacted, "Zm9vOmJhcg==") || strings.Count(redacted, RedactedMarker) != 3 {
		t.Fatalf("redacted %q", redacted)
	}
}

func TestAReadIsRepeatedAndAWriteIsSentOnce(t *testing.T) {
	var reads, writes atomic.Int32
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Basic "+base64.StdEncoding.EncodeToString([]byte(testEmail+":"+testToken)) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodGet {
			if reads.Add(1) < 3 {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			_, _ = w.Write([]byte(`{"id":"10001"}`))
			return
		}
		writes.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	var issue struct {
		ID string `json:"id"`
	}
	if err := client.Get(context.Background(), Path("issue", "10001"), nil, &issue); err != nil || issue.ID != "10001" || reads.Load() != 3 {
		t.Fatalf("read: %+v %+v after %d attempts", err, issue, reads.Load())
	}
	err := client.Write(context.Background(), http.MethodPut, Path("issue", "10001"), map[string]any{}, nil)
	if err == nil || err.Kind != Uncertain || writes.Load() != 1 {
		t.Fatalf("a write answered 503 is %+v after %d sends", err, writes.Load())
	}
}

func TestARateLimitIsAnsweredAndNeverRepeated(t *testing.T) {
	var calls atomic.Int32
	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	err := client.Get(context.Background(), "/myself", nil, nil)
	if err == nil || err.Kind != Answered || !err.RateLimited || calls.Load() != 1 || !strings.Contains(err.Message, "retry after 30s") {
		t.Fatalf("a rate limit is %+v after %d sends", err, calls.Load())
	}
}

func TestJiraMessagesAreRenderedAndRedacted(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"errorMessages":["token ` + testToken + ` is wrong"],"errors":{"summary":"required","labels":"no spaces"}}`))
	})
	err := client.Write(context.Background(), http.MethodPost, "/issue", map[string]any{}, nil)
	if err == nil || err.Kind != Answered || err.Status != http.StatusBadRequest {
		t.Fatalf("%+v", err)
	}
	if strings.Contains(err.Message, testToken) || err.Message != "Jira answered 400: token [redacted] is wrong; labels: no spaces; summary: required" {
		t.Fatalf("message %q", err.Message)
	}
}

func TestAWriteNeverFollowsARedirect(t *testing.T) {
	var redirected atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected.Add(1) }))
	t.Cleanup(other.Close)
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/issue", http.StatusTemporaryRedirect)
	})
	err := client.Write(context.Background(), http.MethodPost, "/issue", map[string]any{}, nil)
	if err == nil || err.Kind != Answered || redirected.Load() != 0 {
		t.Fatalf("a redirected write: %+v, %d requests reached the other host", err, redirected.Load())
	}
	if err := client.Get(context.Background(), "/myself", nil, nil); err == nil || redirected.Load() != 0 {
		t.Fatalf("a read followed a redirect to another host: %+v", err)
	}
}

func TestAnUnsentRequestIsNotSent(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	server.Close()
	env := environment(map[string]string{OverrideVariable: server.URL, EmailVariable: testEmail, TokenVariable: testToken})
	endpoint, _ := ResolveEndpoint("acme.atlassian.net", env)
	client := New(endpoint, NewCredential(env), Options{Pause: noPause, Timeout: time.Second})
	if err := client.Write(context.Background(), http.MethodPost, "/issue", map[string]any{}, nil); err == nil || err.Kind != NotSent {
		t.Fatalf("a refused connection is %+v", err)
	}
	unauthenticated := New(endpoint, NewCredential(environment(nil)), Options{})
	if err := unauthenticated.Get(context.Background(), "/myself", nil, nil); err == nil || err.Kind != NoCredential {
		t.Fatalf("a missing credential is %+v", err)
	}
}
