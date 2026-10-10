package clicore

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"go.putnami.dev/app"
	"go.putnami.dev/client"
	"go.putnami.dev/inject"
)

// The fixture below is the shape `putnami clientgen` emits for a provider that
// declares one safe read under the forwarded user credential every Cloud
// provider the CLI calls declares. It keeps these tests free of any provider
// module.
var echoDescriptor = client.MustServiceDescriptor(
	`{"protocolVersion":1,"service":{"id":"echo-api","audience":"echo-api"},"credentials":{"user":{"kind":"forwarded-user-token"}}}`,
	`{"Echo":{"type":"object","properties":{"who":{"type":"string"}},"additionalProperties":false}}`,
)

var getEchoOperation = client.MustOperation("getEcho",
	`{"stream":"unary","transports":[{"protocol":"rest-json","path":"/echo","encoding":"json"}],"security":{"alternatives":[{"allOf":[{"profile":"user","scopes":["echo.read"]}]}]},"errors":[{"status":401,"code":"unauthorized"}],"idempotency":{"kind":"safe"}}`,
	`[{"status":200,"content":[{"mediaType":"application/json","schema":{"$ref":"#/components/schemas/Echo"}}]}]`,
)

type echo struct {
	Who *string `json:"who,omitempty"`
}

type echoClient struct{ transport *client.Client }

func registerEchoClient(module *app.Module, override ...client.ServiceBinding) *app.Module {
	return module.Provide(inject.Provide(inject.TokenOf[*echoClient](), func(inject.Resolver) (any, error) {
		transport, err := client.NewServiceClientBinding(override[0], echoDescriptor)
		if err != nil {
			return nil, err
		}
		return &echoClient{transport: transport}, nil
	}))
}

func (c *echoClient) get(ctx context.Context) (*echo, error) {
	request := &client.Request{Method: http.MethodGet, Path: "/echo", QueryValues: url.Values{}, Headers: make(http.Header)}
	result, err := client.CallOperation[echo](ctx, c.transport, &client.OperationCall{Request: request, PathParams: map[string]string{}}, getEchoOperation)
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// echoServer answers GET /echo with status and body, and records the request.
func echoServer(t *testing.T, status int, body string) (*httptest.Server, *http.Header) {
	t.Helper()
	seen := &http.Header{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server, seen
}

func newEchoClient(t *testing.T, ctx *WorkspaceContext) *echoClient {
	t.Helper()
	resolved, err := NewServiceClient[echoClient](registerEchoClient, ctx.ServiceBinding())
	if err != nil {
		t.Fatalf("NewServiceClient: %v", err)
	}
	return resolved
}

func workspaceContextFor(server *httptest.Server) *WorkspaceContext {
	return &WorkspaceContext{
		WorkspaceID:  "ws-1",
		ControlPlane: server.URL + "/",
		AuthToken:    NewBearer("user-token"),
		IO:           IO{Client: server.Client()},
	}
}

func TestGeneratedCallForwardsTheWorkspaceBearerAndCLIIdentity(t *testing.T) {
	server, seen := echoServer(t, http.StatusOK, `{"who":"me"}`)
	ctx := workspaceContextFor(server)

	got, err := newEchoClient(t, ctx).get(ctx.CallContext(context.Background()))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Who == nil || *got.Who != "me" {
		t.Fatalf("answer = %+v, want who=me", got)
	}
	if auth := seen.Get("Authorization"); auth != "Bearer user-token" {
		t.Fatalf("Authorization = %q, want the forwarded workspace bearer", auth)
	}
	if ua := seen.Get("User-Agent"); ua != CLIUserAgent() {
		t.Fatalf("User-Agent = %q, want %q", ua, CLIUserAgent())
	}
	if id := seen.Get("X-Client-Id"); id != CLIClientID {
		t.Fatalf("X-Client-Id = %q, want %q", id, CLIClientID)
	}
}

func TestServiceBindingForTrimsTheBaseAndForwardsTheUser(t *testing.T) {
	binding := ServiceBindingFor("  http://api.example.test/ ", nil)
	if binding.URL != "http://api.example.test" {
		t.Fatalf("URL = %q", binding.URL)
	}
	if !binding.AllowInsecure {
		t.Fatal("a plain-http control-plane URL must stay accepted")
	}
	if got := binding.Credentials[userCredentialProfile].Source; got != client.CredentialSourceForwardedUser {
		t.Fatalf("user credential source = %q", got)
	}
	if binding.HTTPClient == nil {
		t.Fatal("binding carries no HTTP client")
	}
}

func TestServiceCallErrorMapsAProviderRefusal(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		wantExit int
		wantLine string
	}{
		{"declared 401 exits auth", http.StatusUnauthorized, ExitAuth, "401 Unauthorized"},
		{"undeclared 500 exits api", http.StatusInternalServerError, ExitAPI, "500 Internal Server Error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server, _ := echoServer(t, tc.status, `{"code":"unauthorized","error":"unauthorized","message":"token expired"}`)
			ctx := workspaceContextFor(server)
			_, callErr := newEchoClient(t, ctx).get(ctx.CallContext(context.Background()))
			if callErr == nil {
				t.Fatal("call succeeded, want a refusal")
			}
			if got := ServiceStatus(callErr); got != tc.status {
				t.Fatalf("ServiceStatus = %d, want %d", got, tc.status)
			}
			if got := ServiceFailureReason(callErr); got != tc.wantLine {
				t.Fatalf("ServiceFailureReason = %q, want %q", got, tc.wantLine)
			}
			err := ServiceCallError(context.Background(), callErr, ServiceCallOptions{Target: "t", Prefix: "status: "})
			if err.Error() != "status: "+tc.wantLine || ExitCode(err) != tc.wantExit {
				t.Fatalf("error = %q exit %d, want %q exit %d", err.Error(), ExitCode(err), "status: "+tc.wantLine, tc.wantExit)
			}
		})
	}
}

// The CLI opts in to the provider's message: a refusal
// carries it, and a non-refusal carries none.
func TestServiceMessageCarriesTheProviderMessage(t *testing.T) {
	if !ServiceBindingFor("http://api.example.test", nil).CarryRemoteMessage {
		t.Fatal("the CLI binding must opt in to the provider message")
	}
	server, _ := echoServer(t, http.StatusUnauthorized, `{"code":"unauthorized","error":"Unauthorized","message":"token expired"}`)
	ctx := workspaceContextFor(server)
	_, callErr := newEchoClient(t, ctx).get(ctx.CallContext(context.Background()))
	if got := ServiceMessage(callErr); got != "token expired" {
		t.Fatalf("ServiceMessage = %q, want the provider message", got)
	}
	if got := ServiceMessage(errors.New("dial tcp: refused")); got != "" {
		t.Fatalf("ServiceMessage of a transport failure = %q, want empty", got)
	}
}

func TestServiceCallErrorMapsAnAnswerOutsideTheContract(t *testing.T) {
	server, _ := echoServer(t, http.StatusOK, `{"who":1}`)
	ctx := workspaceContextFor(server)
	_, callErr := newEchoClient(t, ctx).get(ctx.CallContext(context.Background()))
	if callErr == nil {
		t.Fatal("call succeeded, want a contract violation")
	}
	if got := ServiceFailureReason(callErr); got != "invalid response" {
		t.Fatalf("ServiceFailureReason = %q", got)
	}
	err := ServiceCallError(context.Background(), callErr, ServiceCallOptions{Target: "https://api/echo", Prefix: "status: "})
	if err.Error() != "invalid JSON response from https://api/echo" || ExitCode(err) != ExitAPI {
		t.Fatalf("error = %q exit %d", err.Error(), ExitCode(err))
	}
}

func TestServiceCallErrorMapsCancellationAndTransportFailure(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	transport := errors.New("dial tcp: connection refused")

	err := ServiceCallError(canceled, transport, ServiceCallOptions{Target: "https://api/echo", CancelMessage: "status query canceled by user"})
	if err.Error() != "status query canceled by user" || ExitCode(err) != ExitUsage {
		t.Fatalf("canceled: error = %q exit %d", err.Error(), ExitCode(err))
	}
	err = ServiceCallError(context.Background(), transport, ServiceCallOptions{Target: "https://api/echo"})
	if err.Error() != "request failed for https://api/echo: dial tcp: connection refused" || ExitCode(err) != ExitAPI {
		t.Fatalf("transport: error = %q exit %d", err.Error(), ExitCode(err))
	}
	if got := ServiceFailureReason(transport); !strings.HasPrefix(got, "request failed: ") {
		t.Fatalf("ServiceFailureReason = %q", got)
	}
	if got := ServiceStatus(transport); got != 0 {
		t.Fatalf("ServiceStatus = %d, want 0", got)
	}
}

// TestCallWithSessionRefreshesOn401 pins the single-401 re-mint for a
// generated unary call: a 401 triggers exactly one re-mint, the replay
// carries the fresh bearer, and the context keeps it so a later call in the
// same command reuses it.
func TestCallWithSessionRefreshesOn401(t *testing.T) {
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "Bearer fresh" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":"unauthorized","error":"unauthorized","message":"token expired"}`))
			return
		}
		_, _ = w.Write([]byte(`{"who":"me"}`))
	}))
	defer server.Close()

	remints := 0
	ctx := workspaceContextFor(server)
	ctx.AuthToken = NewBearer("stale")
	ctx.RefreshAuth = func() (Bearer, error) {
		remints++
		return NewBearer("fresh"), nil
	}
	echo := newEchoClient(t, ctx)

	got, err := CallWithSession(context.Background(), ctx, echo.get)
	if err != nil {
		t.Fatalf("CallWithSession: %v", err)
	}
	if got.Who == nil || *got.Who != "me" {
		t.Fatalf("answer = %+v, want who=me", got)
	}
	if len(seen) != 2 || seen[0] != "Bearer stale" || seen[1] != "Bearer fresh" {
		t.Fatalf("authorization sequence = %v, want [Bearer stale, Bearer fresh]", seen)
	}
	if remints != 1 {
		t.Fatalf("re-mints = %d, want exactly 1 (single re-mint, not a loop)", remints)
	}
	if ctx.AuthToken.Authorization() != "Bearer fresh" {
		t.Fatal("ctx.AuthToken not updated — a later call in the same command would 401 again")
	}
}

// TestCallWithSessionSurfacesOriginal401WhenRefreshFails pins fail-closed
// behavior: when the re-mint itself fails, the original 401 RemoteError is
// returned untouched.
func TestCallWithSessionSurfacesOriginal401WhenRefreshFails(t *testing.T) {
	server, _ := echoServer(t, http.StatusUnauthorized, `{"code":"unauthorized","error":"unauthorized","message":"token expired"}`)
	ctx := workspaceContextFor(server)
	ctx.RefreshAuth = func() (Bearer, error) {
		return Bearer{}, NewError("session is not refreshable; run putnami cloud login", ExitAuth)
	}
	echo := newEchoClient(t, ctx)

	_, err := CallWithSession(context.Background(), ctx, echo.get)
	if err == nil {
		t.Fatal("call succeeded, want the original 401")
	}
	if got := ServiceStatus(err); got != http.StatusUnauthorized {
		t.Fatalf("ServiceStatus = %d, want 401", got)
	}
}

// TestCallWithSessionSkipsRemintWhenNotUnauthorized pins that a non-401
// refusal never triggers a re-mint attempt.
func TestCallWithSessionSkipsRemintWhenNotUnauthorized(t *testing.T) {
	server, _ := echoServer(t, http.StatusInternalServerError, `{"code":"internal","error":"internal","message":"boom"}`)
	ctx := workspaceContextFor(server)
	remints := 0
	ctx.RefreshAuth = func() (Bearer, error) {
		remints++
		return NewBearer("fresh"), nil
	}
	echo := newEchoClient(t, ctx)

	_, err := CallWithSession(context.Background(), ctx, echo.get)
	if got := ServiceStatus(err); got != http.StatusInternalServerError {
		t.Fatalf("ServiceStatus = %d, want 500", got)
	}
	if remints != 0 {
		t.Fatalf("re-mints = %d, want 0 for a non-401 refusal", remints)
	}
}

// TestRedirectRefusingHTTPClientRefusesWithoutMutatingTheCaller pins the
// invariant every hand-written transport this helper backs
// (review.go/operator.go's redirectRefusingIntelligenceClient) enforces: the
// derived client refuses a redirect, and the caller's own client — including
// its own (opposite) CheckRedirect policy — is never touched.
func TestRedirectRefusingHTTPClientRefusesWithoutMutatingTheCaller(t *testing.T) {
	var targetHits int
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { targetHits++ }))
	defer target.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	caller := redirector.Client()
	followCalls := 0
	caller.CheckRedirect = func(*http.Request, []*http.Request) error {
		followCalls++
		return nil // the caller's own policy: follow every redirect.
	}

	refusing := RedirectRefusingHTTPClient(caller)
	if refusing == caller {
		t.Fatal("RedirectRefusingHTTPClient returned the caller's own client instead of a copy")
	}
	resp, err := refusing.Get(redirector.URL + "/probe")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want the unfollowed redirect", resp.StatusCode)
	}
	if targetHits != 0 {
		t.Fatalf("redirect target hits = %d, want 0", targetHits)
	}
	if err := caller.CheckRedirect(nil, nil); err != nil || followCalls != 1 {
		t.Fatalf("caller's own redirect policy was mutated: err=%v calls=%d", err, followCalls)
	}
}

// TestRedirectRefusingHTTPClientTreatsANilBaseAsTheDefaultClient pins the nil
// fallback so a caller that has not built its own *http.Client yet still gets
// a refusing one instead of a nil-pointer panic.
func TestRedirectRefusingHTTPClientTreatsANilBaseAsTheDefaultClient(t *testing.T) {
	refusing := RedirectRefusingHTTPClient(nil)
	if refusing == nil || refusing.CheckRedirect == nil {
		t.Fatal("a nil base must still yield a redirect-refusing client")
	}
	if http.DefaultClient.CheckRedirect != nil {
		t.Fatal("the package default client must stay untouched")
	}
}

func TestNewServiceClientReportsARegistrationThatCannotBind(t *testing.T) {
	broken := ServiceBindingFor("not a url", nil)
	_, err := NewServiceClient[echoClient](registerEchoClient, broken)
	if err == nil || ExitCode(err) != ExitAPI || !strings.HasPrefix(err.Error(), "configure the Cloud client: ") {
		t.Fatalf("error = %v, want a configure failure", err)
	}
}
