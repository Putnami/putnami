package gitlab

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/collabhelper"
)

const (
	feature   = "tooling/gitlab-collaboration"
	testToken = "glpat-TESTtokenTESTtoken0001"
)

func environment(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

func noPause(context.Context, int) error { return nil }

// newTestClient returns a client of a loopback server answering handler.
func newTestClient(t *testing.T, env map[string]string, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	values := map[string]string{OverrideVariable: server.URL, TokenVariable: testToken}
	for name, value := range env {
		values[name] = value
	}
	endpoint, err := ResolveEndpoint("", environment(values))
	if err != nil {
		t.Fatal(err)
	}
	return New(endpoint, NewCredential(endpoint.Host, environment(values), nil), Options{
		HTTP:    &http.Client{Transport: &http.Transport{DisableKeepAlives: true}},
		Timeout: 5 * time.Second, Pause: noPause,
	})
}

func TestTheOverrideReachesOnlyTheLoopbackInterface(t *testing.T) {
	spectest.Proves(t, feature, "credential-safety", "the-override-reaches-only-the-loopback-interface")
	endpoint, err := ResolveEndpoint("", environment(nil))
	if err != nil || endpoint.REST.String() != "https://gitlab.com/api/v4" || endpoint.Host != DefaultHost {
		t.Fatalf("the default root is %v, %v", endpoint.REST, err)
	}
	endpoint, err = ResolveEndpoint("gitlab.example.com:8443", environment(nil))
	if err != nil || endpoint.REST.String() != "https://gitlab.example.com:8443/api/v4" {
		t.Fatalf("a self-managed root is %v, %v", endpoint.REST, err)
	}
	for _, override := range []string{"http://127.0.0.1:8080", "http://localhost:1/", "https://[::1]:443"} {
		endpoint, err := ResolveEndpoint("gitlab.com", environment(map[string]string{OverrideVariable: override}))
		if err != nil || !strings.HasSuffix(endpoint.REST.String(), "/api/v4") {
			t.Fatalf("loopback override %s: %v, %v", override, endpoint.REST, err)
		}
	}
	for _, override := range []string{"https://evil.example.com", "http://10.0.0.1", "http://u:p@127.0.0.1", "http://127.0.0.1/?q=1", "ftp://127.0.0.1"} {
		if _, err := ResolveEndpoint("gitlab.com", environment(map[string]string{OverrideVariable: override})); err == nil {
			t.Fatalf("override %s was accepted", override)
		}
	}
	if _, err := ResolveEndpoint("https://gitlab.com", environment(nil)); err == nil {
		t.Fatal("a URL was accepted as a host")
	}
	if got := Path("projects", "acme/sub/app", "issues", "1"); got != "/projects/acme%2Fsub%2Fapp/issues/1" {
		t.Fatalf("path %s", got)
	}
}

func TestTheCredentialFollowsGlabDiscovery(t *testing.T) {
	spectest.Proves(t, feature, "credential-safety", "the-credential-follows-glab-discovery")
	glab := func(value string, err error) TokenCommand {
		return func(context.Context, string) (string, error) { return value, err }
	}
	cases := []struct {
		name    string
		host    string
		env     map[string]string
		command TokenCommand
		want    Token
		message string
	}{
		{"GITLAB_TOKEN serves gitlab.com", "gitlab.com", map[string]string{TokenVariable: "a"}, nil, Token{"a", Bearer}, ""},
		{"GITLAB_TOKEN serves the GITLAB_HOST instance", "gitlab.example.com",
			map[string]string{TokenVariable: "a", HostVariable: "https://GitLab.Example.com/"}, nil, Token{"a", Bearer}, ""},
		{"GITLAB_TOKEN of another instance falls to glab", "gitlab.example.com",
			map[string]string{TokenVariable: "a"}, glab("b", nil), Token{"b", Bearer}, ""},
		{"glab before the job token", "gitlab.example.com",
			map[string]string{CIVariable: "true", CIHostVariable: "gitlab.example.com", JobKeyVariable: "j"}, glab("b\n", nil), Token{"b", Bearer}, ""},
		{"the job token of the instance", "gitlab.example.com",
			map[string]string{CIVariable: "true", CIHostVariable: "gitlab.example.com", JobKeyVariable: "j"}, glab("", errNoGlab), Token{"j", JobToken}, ""},
		{"the job token by FQDN", "gitlab.example.com:8443",
			map[string]string{CIVariable: "true", CIHostVariable: "gitlab.example.com", CIFQDNVariable: "gitlab.example.com:8443", JobKeyVariable: "j"},
			glab("", nil), Token{"j", JobToken}, ""},
		{"an empty job token", "gitlab.example.com",
			map[string]string{CIVariable: "true", CIHostVariable: "gitlab.example.com"}, glab("", errors.New("glab failed")), Token{}, "CI_JOB_TOKEN (empty)"},
		{"no glab", "gitlab.com", nil, glab("", errNoGlab), Token{}, "glab (not installed)"},
		{"nothing stored", "gitlab.com", nil, glab("", nil), Token{}, "run glab auth login --hostname gitlab.com"},
		{"no command", "gitlab.com", nil, nil, Token{}, "set GITLAB_TOKEN or run glab auth login"},
		{"a token that is not one", "gitlab.com", map[string]string{TokenVariable: "a b"}, nil, Token{}, "not a single token"},
	}
	for _, c := range cases {
		credential := NewCredential(c.host, environment(c.env), c.command)
		got, err := credential.Token(context.Background())
		if c.message != "" {
			if err == nil || err.Kind != NoCredential || !strings.Contains(err.Message, c.message) {
				t.Errorf("%s: %v, %+v", c.name, got, err)
			}
			if len(credential.Secrets()) != 0 {
				t.Errorf("%s: a refused credential is listed as a secret", c.name)
			}
			continue
		}
		if err != nil || got != c.want || len(credential.Secrets()) != 1 {
			t.Errorf("%s: %+v, %+v, want %+v", c.name, got, err, c.want)
		}
	}
}

// TestGlabConfigTokenAsksGlabForTheHost runs a stand-in glab that prints its
// arguments and the token variables it was given.
func TestGlabConfigTokenAsksGlabForTheHost(t *testing.T) {
	directory := t.TempDir()
	script := "#!/bin/sh\nif [ \"$1 $2 $3 $4 $5\" = \"config get token --host gitlab.example.com\" ] && [ -z \"$GITLAB_TOKEN$GITLAB_HOST\" ]; then echo ' glab-token '; exit 0; fi\nexit 3\n"
	if err := os.WriteFile(filepath.Join(directory, "glab"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory)
	t.Setenv("GITLAB_TOKEN", "leaked")
	t.Setenv("GITLAB_HOST", "gitlab.com")
	value, err := GlabConfigToken(context.Background(), "gitlab.example.com")
	if err != nil || value != "glab-token" {
		t.Fatalf("glab answered %q, %v", value, err)
	}
	if _, err := GlabConfigToken(context.Background(), "other.example.com"); err == nil || errors.Is(err, errNoGlab) {
		t.Fatalf("a failing glab answered %v", err)
	}
	t.Setenv("PATH", t.TempDir())
	if _, err := GlabConfigToken(context.Background(), "gitlab.example.com"); !errors.Is(err, errNoGlab) {
		t.Fatalf("no glab answered %v", err)
	}
}

func TestAJobTokenServesOnlyItsOwnInstance(t *testing.T) {
	spectest.Proves(t, feature, "credential-safety", "a-job-token-serves-only-its-own-instance")
	var header atomic.Value
	client := newTestClient(t, map[string]string{TokenVariable: "", CIVariable: "true", CIHostVariable: DefaultHost, JobKeyVariable: "job-token-value"},
		func(w http.ResponseWriter, r *http.Request) {
			header.Store(r.Header.Get("JOB-TOKEN") + "|" + r.Header.Get("Authorization"))
			_, _ = w.Write([]byte(`{}`))
		})
	if _, err := client.Get(context.Background(), "/user", nil, &struct{}{}); err != nil {
		t.Fatal(err)
	}
	if got := header.Load(); got != "job-token-value|" {
		t.Fatalf("a job token was sent as %q", got)
	}
	other := NewCredential("gitlab.example.com", environment(map[string]string{CIVariable: "true", CIHostVariable: DefaultHost, JobKeyVariable: "j"}), nil)
	if _, err := other.Token(context.Background()); err == nil {
		t.Fatal("the job token of gitlab.com served another instance")
	}
}

func TestAReadIsRepeatedAndAWriteIsSentOnce(t *testing.T) {
	var reads, writes atomic.Int32
	client := newTestClient(t, nil, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testToken {
			t.Errorf("authorization %q", r.Header.Get("Authorization"))
		}
		if r.Method == http.MethodGet {
			if reads.Add(1) < 3 {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			w.Header().Set("X-Next-Page", "2")
			_, _ = w.Write([]byte(`[{"id":1}]`))
			return
		}
		writes.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	var out []struct{ ID int }
	header, err := client.Get(context.Background(), "/projects", nil, &out)
	if err != nil || len(out) != 1 || !HasNext(header) || reads.Load() != 3 {
		t.Fatalf("a read after two 502: %v, %v, %d reads", out, err, reads.Load())
	}
	err = client.Write(context.Background(), http.MethodPut, "/projects/1", map[string]any{"a": 1}, nil)
	if err == nil || err.Kind != Uncertain || writes.Load() != 1 {
		t.Fatalf("a write answered 503: %+v after %d writes", err, writes.Load())
	}
}

func TestAnswersAreClassifiedByWhatTheyProve(t *testing.T) {
	cases := []struct {
		status  int
		body    string
		header  map[string]string
		kind    Kind
		limited bool
		message string
	}{
		{404, `{"message":"404 Project Not Found"}`, nil, Answered, false, "GitLab answered 404: 404 Project Not Found"},
		{400, `{"message":{"title":["can't be blank","is too short"],"labels":["are invalid"]}}`, nil, Answered, false,
			"GitLab answered 400: labels are invalid; title can't be blank, is too short"},
		{400, `{"message":{"base":"odd"}}`, nil, Answered, false, "GitLab answered 400: base"},
		{401, `{"error":"invalid_token","error_description":"Token was revoked."}`, nil, Answered, false, "GitLab answered 401: invalid_token Token was revoked."},
		{403, `<html>forbidden</html>`, nil, Answered, false, "GitLab answered 403"},
		{429, `{"message":"Retry later"}`, map[string]string{"Retry-After": "60"}, Answered, true, "retry after 60s"},
		{500, `{"message":"500 Internal Server Error"}`, nil, Uncertain, false, "500 Internal Server Error"},
		{200, `not json`, nil, Uncertain, false, "a body this client cannot read"},
	}
	for _, c := range cases {
		client := newTestClient(t, nil, func(w http.ResponseWriter, r *http.Request) {
			for name, value := range c.header {
				w.Header().Set(name, value)
			}
			w.WriteHeader(c.status)
			_, _ = w.Write([]byte(c.body))
		})
		err := client.Write(context.Background(), http.MethodPost, "/x", nil, &struct{}{})
		if err == nil || err.Kind != c.kind || err.RateLimited != c.limited || !strings.Contains(err.Message, c.message) {
			t.Errorf("%d %s: %+v", c.status, c.body, err)
		}
	}
	if HasNext(http.Header{"X-Next-Page": {""}}) || !HasNext(http.Header{"Link": {`<https://x/?page=2>; rel="next"`}}) {
		t.Fatal("HasNext misreads the page headers")
	}
}

func TestARequestThatNeverLeftIsNotSent(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	server.Close()
	env := environment(map[string]string{OverrideVariable: server.URL, TokenVariable: testToken})
	endpoint, _ := ResolveEndpoint("", env)
	client := New(endpoint, NewCredential(DefaultHost, env, nil), Options{Pause: noPause})
	if err := client.Write(context.Background(), http.MethodPost, "/x", map[string]any{}, nil); err == nil || err.Kind != NotSent {
		t.Fatalf("a refused connection: %+v", err)
	}
	if _, err := client.Get(context.Background(), "/x", nil, nil); err == nil || err.Kind != NotSent {
		t.Fatalf("a refused connection: %+v", err)
	}
	if err := client.Write(context.Background(), http.MethodPost, "/x", map[string]any{"bad": func() {}}, nil); err == nil || err.Kind != NotSent {
		t.Fatalf("an unencodable body: %+v", err)
	}
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(200 * time.Millisecond) }))
	defer slow.Close()
	env = environment(map[string]string{OverrideVariable: slow.URL, TokenVariable: testToken})
	endpoint, _ = ResolveEndpoint("", env)
	client = New(endpoint, NewCredential(DefaultHost, env, nil), Options{Timeout: 20 * time.Millisecond, ReadAttempts: 1})
	if err := client.Write(context.Background(), http.MethodPost, "/x", nil, nil); err == nil || err.Kind != Uncertain || !strings.Contains(err.Message, "did not answer within") {
		t.Fatalf("a deadline: %+v", err)
	}
	none := New(endpoint, NewCredential(DefaultHost, environment(nil), nil), Options{})
	if _, err := none.Get(context.Background(), "/x", nil, nil); err == nil || err.Kind != NoCredential {
		t.Fatalf("no credential: %+v", err)
	}
}

func TestACredentialNeverReachesAnAnswer(t *testing.T) {
	spectest.Proves(t, feature, "credential-safety", "a-credential-never-reaches-an-answer")
	client := newTestClient(t, nil, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"message":"bad token ` + testToken + ` and glrt-RUNNERtokenRUNNER12345"}`))
	})
	err := client.Write(context.Background(), http.MethodPost, "/x", nil, nil)
	if err == nil || strings.Contains(err.Message, testToken) || strings.Contains(err.Message, "glrt-RUNNER") ||
		!strings.Contains(err.Message, collabhelper.RedactedMarker) {
		t.Fatalf("the message carries a credential: %+v", err)
	}
	if got := Redact("x glpat-abcdefghijklmnopqrstuvwx.01.0123abcd y", nil); strings.Contains(got, "glpat-") {
		t.Fatalf("a token shape survived: %s", got)
	}
	if got := withoutVariables([]string{"A=1", "GITLAB_TOKEN=x", "B=2=3"}, glabTokenVariables); strings.Join(got, " ") != "A=1 B=2=3" {
		t.Fatalf("the glab environment keeps %v", got)
	}
}
