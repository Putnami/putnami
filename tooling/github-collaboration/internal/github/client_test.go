package github

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testToken = "test-token-value"

func noPause(context.Context, int) error { return nil }

// newTestClient is a client of a test server, authenticated by testToken.
func newTestClient(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	endpoint, err := ResolveEndpoint("github.com", func(name string) string {
		if name == OverrideVariable {
			return server.URL
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	credential := NewCredential("github.com", func(name string) string {
		if name == "GH_TOKEN" {
			return testToken
		}
		return ""
	}, nil)
	client := New(endpoint, credential, Options{HTTP: &http.Client{Transport: &http.Transport{DisableKeepAlives: true}},
		Timeout: 300 * time.Millisecond, Pause: noPause})
	return client, server
}

func drop(w http.ResponseWriter) {
	connection, _, err := w.(http.Hijacker).Hijack()
	if err == nil {
		_ = connection.Close()
	}
}

func TestAReadIsSentWithTheCredentialAndDecoded(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testToken || r.Header.Get("X-GitHub-Api-Version") != apiVersion ||
			r.URL.EscapedPath() != "/repos/o/r/git/ref/heads/feature/a%20b" || r.URL.Query().Get("q") != "x" {
			http.Error(w, `{"message":"bad request"}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Link", `<https://api.github.com/x?page=2>; rel="next", <https://api.github.com/x?page=9>; rel="last"`)
		_, _ = io.WriteString(w, `{"login":"octocat"}`)
	})
	var user struct {
		Login string `json:"login"`
	}
	header, failure := client.Get(context.Background(), Path("repos", "o", "r", "git", "ref", "heads", "feature", "a b"), url.Values{"q": {"x"}}, &user)
	if failure != nil || user.Login != "octocat" || !HasNext(header) {
		t.Fatalf("read: %+v %+v", failure, user)
	}
	if HasNext(http.Header{"Link": {`<https://api.github.com/x?page=1>; rel="prev"`}}) {
		t.Error("a Link header without next names a next page")
	}
}

func TestAReadWithoutADefiniteAnswerIsRepeated(t *testing.T) {
	var calls atomic.Int32
	client, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		switch calls.Add(1) {
		case 1:
			drop(w)
		case 2:
			w.WriteHeader(http.StatusBadGateway)
		default:
			_, _ = io.WriteString(w, `{}`)
		}
	})
	if _, failure := client.Get(context.Background(), "/user", nil, &struct{}{}); failure != nil || calls.Load() != 3 {
		t.Fatalf("after %d calls: %+v", calls.Load(), failure)
	}
	calls.Store(0)
	client.options.ReadAttempts = 2
	_, failure := client.Get(context.Background(), "/user", nil, &struct{}{})
	if failure == nil || failure.Kind != Uncertain || failure.Status != http.StatusBadGateway || calls.Load() != 2 {
		t.Fatalf("a read that never got an answer: %+v after %d calls", failure, calls.Load())
	}
}

func TestADefiniteAnswerIsNeverRepeated(t *testing.T) {
	var calls atomic.Int32
	client, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", "1790000000")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"message":"API rate limit exceeded for user"}`)
	})
	_, failure := client.Get(context.Background(), "/user", nil, nil)
	if failure == nil || failure.Kind != Answered || !failure.RateLimited || calls.Load() != 1 ||
		!strings.Contains(failure.Message, "resets at 2026-") {
		t.Fatalf("a rate limit: %+v after %d calls", failure, calls.Load())
	}
}

func TestAWriteIsClassifiedByWhatItProves(t *testing.T) {
	var mode atomic.Value
	var calls atomic.Int32
	client, server := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		switch mode.Load() {
		case "drop":
			drop(w)
		case "slow":
			time.Sleep(time.Second)
		case "5xx":
			w.WriteHeader(http.StatusServiceUnavailable)
		case "422":
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = io.WriteString(w, `{"message":"Validation Failed","errors":[{"field":"title","code":"missing_field"},"plain",{"message":"said"},42]}`)
		case "garbage":
			_, _ = io.WriteString(w, `not json`)
		case "redirect":
			http.Redirect(w, r, "/elsewhere", http.StatusMovedPermanently)
		default:
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"number":7}`)
		}
	})
	write := func(value string) (*Error, int32) {
		mode.Store(value)
		calls.Store(0)
		var out struct {
			Number int `json:"number"`
		}
		return client.Write(context.Background(), http.MethodPost, "/repos/o/r/issues", map[string]any{"title": "t"}, &out), calls.Load()
	}
	if failure, calls := write("ok"); failure != nil || calls != 1 {
		t.Fatalf("ok: %+v", failure)
	}
	for _, uncertain := range []string{"drop", "slow", "5xx", "garbage"} {
		if failure, calls := write(uncertain); failure == nil || failure.Kind != Uncertain || calls != 1 {
			t.Fatalf("%s: %+v after %d calls; a write is sent once and its outcome is unknown", uncertain, failure, calls)
		}
	}
	failure, _ := write("422")
	if failure == nil || failure.Kind != Answered || failure.Message != "GitHub answered 422: Validation Failed (title missing_field; plain; said)" {
		t.Fatalf("422: %+v", failure)
	}
	if failure, calls := write("redirect"); failure == nil || failure.Kind != Answered || failure.Status != http.StatusMovedPermanently || calls != 1 {
		t.Fatalf("a redirected write is not replayed elsewhere: %+v after %d calls", failure, calls)
	}
	server.Close()
	if failure, _ := write("ok"); failure == nil || failure.Kind != NotSent {
		t.Fatalf("an unreachable server: %+v", failure)
	}
	if failure := client.Write(context.Background(), http.MethodPost, "/x", map[string]any{"bad": make(chan int)}, nil); failure == nil || failure.Kind != NotSent {
		t.Fatalf("an unencodable request: %+v", failure)
	}
}

func TestGraphQLErrorsAreRefusals(t *testing.T) {
	var answer atomic.Value
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/graphql" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, answer.Load().(string))
	})
	cases := map[string]int{
		`{"errors":[{"type":"FORBIDDEN","message":"no"}]}`:          http.StatusForbidden,
		`{"errors":[{"type":"NOT_FOUND","message":"gone"}]}`:        http.StatusNotFound,
		`{"errors":[{"type":"UNPROCESSABLE","message":"nope"}]}`:    http.StatusUnprocessableEntity,
		`{"data":{"pullRequest":{"isDraft":true}}}`:                 0,
		`{"data":{"pullRequest":{"isDraft":"not a boolean here"}}}`: -1,
	}
	for body, status := range cases {
		answer.Store(body)
		var out struct {
			PullRequest struct {
				IsDraft bool `json:"isDraft"`
			} `json:"pullRequest"`
		}
		failure := client.GraphQL(context.Background(), "mutation { x }", map[string]any{"id": "1"}, &out)
		switch {
		case status == 0 && (failure != nil || !out.PullRequest.IsDraft):
			t.Errorf("%s: %+v", body, failure)
		case status == -1 && (failure == nil || failure.Kind != Uncertain):
			t.Errorf("%s: %+v", body, failure)
		case status > 0 && (failure == nil || failure.Kind != Answered || failure.Status != status):
			t.Errorf("%s: %+v", body, failure)
		}
	}
	answer.Store(`{"data":{}}`)
	if failure := client.GraphQL(context.Background(), "mutation { x }", nil, nil); failure != nil {
		t.Fatal(failure)
	}
}

// A GraphQL query is a read: it is repeated after an answer that did not
// come, and GitHub's partial answer — the fields it resolved and an error for
// each it could not — reaches the caller whole.
func TestAQueryIsARepeatedReadThatKeepsFieldErrors(t *testing.T) {
	var calls atomic.Int32
	var answer atomic.Value
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/graphql" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"owner":"o"`) {
			http.Error(w, `{"message":"no variables"}`, http.StatusBadRequest)
			return
		}
		if calls.Add(1) == 1 {
			drop(w)
			return
		}
		_, _ = io.WriteString(w, answer.Load().(string))
	})
	query := func(body string) ([]GraphQLError, *Error, map[string]any) {
		calls.Store(0)
		answer.Store(body)
		var out struct {
			Repository map[string]any `json:"repository"`
		}
		errs, failure := client.Query(context.Background(), "query { x }", map[string]any{"owner": "o"}, &out)
		return errs, failure, out.Repository
	}

	errs, failure, repository := query(`{"data":{"repository":{"a":{"number":2},"b":null}},"errors":[{"type":"NOT_FOUND","path":["repository","b"],"message":"no ` + testToken + `"}]}`)
	if failure != nil || calls.Load() != 2 || repository["a"] == nil || len(errs) != 1 {
		t.Fatalf("a partial answer after a lost one: %+v %v %v after %d calls", failure, repository, errs, calls.Load())
	}
	if !errs[0].At("repository", "b") || errs[0].At("repository", "a") || errs[0].At("repository") || errs[0].Type != "NOT_FOUND" ||
		strings.Contains(errs[0].Message, testToken) {
		t.Fatalf("the field error %+v", errs[0])
	}
	if _, failure, _ := query(`{"errors":[{"type":"FORBIDDEN","message":"no"}]}`); failure == nil || failure.Kind != Answered || failure.Status != http.StatusForbidden {
		t.Fatalf("an answer without data: %+v", failure)
	}
	if _, failure, _ := query(`{"data":null}`); failure == nil || failure.Kind != Answered || failure.Status != http.StatusUnprocessableEntity {
		t.Fatalf("an answer without data or errors: %+v", failure)
	}
	if _, failure, _ := query(`{"data":null,"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded"}]}`); failure == nil || !failure.RateLimited {
		t.Fatalf("an exhausted rate limit: %+v", failure)
	}
	if _, failure, _ := query(`{"data":{"repository":"not an object"}}`); failure == nil || failure.Kind != Uncertain {
		t.Fatalf("data the caller cannot read: %+v", failure)
	}
	if _, failure := client.Query(context.Background(), "query { x }", map[string]any{"bad": make(chan int)}, nil); failure == nil || failure.Kind != NotSent {
		t.Fatalf("an unencodable query: %+v", failure)
	}
}

func TestMessagesNeverCarryTheCredential(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"no such thing for `+testToken+`"}`)
	})
	_, failure := client.Get(context.Background(), "/user", nil, nil)
	if failure == nil || strings.Contains(failure.Message, testToken) || !strings.Contains(failure.Message, RedactedMarker) {
		t.Fatalf("read failure %+v", failure)
	}
	failure = client.Write(context.Background(), http.MethodPatch, "/user", map[string]any{}, nil)
	if failure == nil || strings.Contains(failure.Message, testToken) {
		t.Fatalf("write failure %+v", failure)
	}
	if got := Redact("a ghp_"+strings.Repeat("a", 36)+" and github_pat_"+strings.Repeat("b", 40)+" and x", nil); got != "a [redacted] and [redacted] and x" {
		t.Fatalf("known token shapes: %q", got)
	}
	if failure.Error() != failure.Message {
		t.Fatal("Error() is the message")
	}
}

func TestEndpointsAndPaths(t *testing.T) {
	none := func(string) string { return "" }
	endpoint, err := ResolveEndpoint("", none)
	if err != nil || endpoint.REST.String() != "https://api.github.com" || endpoint.GraphQL.String() != "https://api.github.com/graphql" || endpoint.Host != "github.com" {
		t.Fatalf("github.com: %+v %v", endpoint, err)
	}
	endpoint, err = ResolveEndpoint("GitHub.com", none)
	if err != nil || endpoint.Host != "github.com" {
		t.Fatalf("case: %+v", endpoint)
	}
	endpoint, err = ResolveEndpoint("ghe.example.com:8443", none)
	if err != nil || endpoint.REST.String() != "https://ghe.example.com:8443/api/v3" || endpoint.GraphQL.String() != "https://ghe.example.com:8443/api/graphql" {
		t.Fatalf("enterprise: %+v %v", endpoint, err)
	}
	for _, host := range []string{"https://github.com", "github.com/x", "user@github.com", "-bad"} {
		if _, err := ResolveEndpoint(host, none); err == nil {
			t.Errorf("host %q was accepted", host)
		}
	}
	override := func(value string) func(string) string {
		return func(name string) string {
			if name == OverrideVariable {
				return value
			}
			return ""
		}
	}
	for _, value := range []string{"http://127.0.0.1:9/", "https://[::1]:9", "http://localhost:9/api"} {
		if _, err := ResolveEndpoint("github.com", override(value)); err != nil {
			t.Errorf("loopback override %s: %v", value, err)
		}
	}
	for _, value := range []string{"http://10.0.0.1", "https://api.github.com", "http://u:p@127.0.0.1", "http://127.0.0.1/#x", "::"} {
		if _, err := ResolveEndpoint("github.com", override(value)); err == nil {
			t.Errorf("override %s was accepted", value)
		}
	}
	if got := Path("repos", "o", "r", "issues", "1"); got != "/repos/o/r/issues/1" {
		t.Errorf("path %s", got)
	}
	if got := Path("a/b", "c d"); got != "/a%2Fb/c%20d" {
		t.Errorf("escaped path %s", got)
	}
}

func TestCredentialResolution(t *testing.T) {
	env := map[string]string{}
	getenv := func(name string) string { return env[name] }
	var asked []string
	command := func(_ context.Context, host string) (string, error) {
		asked = append(asked, host)
		if env["gh"] == "fail" {
			return "", errors.New("the stored token could not be read")
		}
		return env["gh"], nil
	}
	resolve := func(host string) (string, *Error) {
		return NewCredential(host, getenv, command).Token(context.Background())
	}
	env["GITHUB_TOKEN"] = "second"
	env["GH_TOKEN"] = "first"
	if token, failure := resolve("github.com"); token != "first" || failure != nil {
		t.Fatalf("GH_TOKEN first: %q %+v", token, failure)
	}
	delete(env, "GH_TOKEN")
	if token, _ := resolve("github.com"); token != "second" {
		t.Fatalf("GITHUB_TOKEN second: %q", token)
	}
	delete(env, "GITHUB_TOKEN")
	env["gh"] = " stored-token \n"
	if token, _ := resolve("github.com"); token != "stored-token" || len(asked) != 1 {
		t.Fatalf("gh third: %q %v", token, asked)
	}
	env["gh"] = ""
	if _, failure := resolve("github.com"); failure == nil || failure.Kind != NoCredential {
		t.Fatal("gh with no stored token")
	}
	env["gh"] = "fail"
	if _, failure := resolve("github.com"); failure == nil || failure.Message != "the stored token could not be read" {
		t.Fatalf("a failing gh: %+v", failure)
	}
	env["GH_ENTERPRISE_TOKEN"] = "enterprise"
	env["gh"] = "stored"
	if token, _ := resolve("ghe.example.com"); token != "stored" {
		t.Fatalf("an enterprise host GH_HOST does not name: %q", token)
	}
	env["GH_HOST"] = "GHE.example.com"
	if token, _ := resolve("ghe.example.com"); token != "enterprise" {
		t.Fatalf("the enterprise host GH_HOST names: %q", token)
	}
	credential := NewCredential("github.com", getenv, func(context.Context, string) (string, error) { return "", errNoGH })
	if _, failure := credential.Token(context.Background()); failure == nil || !strings.Contains(failure.Message, "install gh") {
		t.Fatalf("no gh: %+v", failure)
	}
	if credential.Secrets() != nil {
		t.Fatal("an unresolved credential has secrets")
	}
	if _, failure := NewCredential("github.com", getenv, nil).Token(context.Background()); failure == nil {
		t.Fatal("no command")
	}
	env["GH_TOKEN"] = "has space"
	if _, failure := resolve("github.com"); failure == nil || strings.Contains(failure.Message, "has space") {
		t.Fatalf("a malformed token: %+v", failure)
	}
}

func TestGHAuthTokenAsksTheInstalledGH(t *testing.T) {
	bin := t.TempDir()
	script := "#!/bin/sh\n" +
		"if [ \"$1 $2 $3 $4\" = \"auth token --hostname github.com\" ]; then echo stored-token; exit 0; fi\n" +
		"echo \"no token for $4, secret-looking noise\" >&2; exit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	if token, err := GHAuthToken(context.Background(), "github.com"); err != nil || token != "stored-token" {
		t.Fatalf("gh answered %q, %v", token, err)
	}
	_, err := GHAuthToken(context.Background(), "ghe.example.com")
	if err == nil || strings.Contains(err.Error(), "noise") || !strings.Contains(err.Error(), "gh auth login --hostname ghe.example.com") {
		t.Fatalf("a failing gh: %v", err)
	}
}

func TestGHAuthTokenWithoutGH(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if _, err := GHAuthToken(context.Background(), "github.com"); !errors.Is(err, errNoGH) {
		t.Fatalf("without gh on PATH: %v", err)
	}
}
