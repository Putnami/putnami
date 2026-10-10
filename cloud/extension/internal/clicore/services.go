package clicore

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"net/http"
	"strings"

	"go.putnami.dev/app"
	"go.putnami.dev/client"
	perrors "go.putnami.dev/errors"
	"go.putnami.dev/inject"
)

// CLIClientID is the client identity every generated Cloud client call sends
// as X-Client-Id. The framework refuses a call without one.
const CLIClientID = "putnami-cli"

// userCredentialProfile is the credential profile every Cloud provider the CLI
// calls declares for the signed-in user: the provider receives the caller's
// own bearer, forwarded per call (client.WithForwardedUserToken).
const userCredentialProfile = "user"

// ServiceBinding is the binding of a generated Cloud provider client for this
// workspace context: every provider the CLI calls is reached through the
// control-plane host (the load balancer routes by path), with the signed-in
// user's bearer forwarded per call.
func (ctx *WorkspaceContext) ServiceBinding() client.ServiceBinding {
	return ServiceBindingFor(ctx.ControlPlane, ctx.IO.Client)
}

// ServiceBindingFor binds a generated Cloud provider client to baseURL. The
// HTTP client keeps the IO seam (tests inject theirs) and stamps the unified
// CLI User-Agent. A plain-http base stays accepted, as it was for the
// hand-written requests: the operator chooses the control-plane URL.
func ServiceBindingFor(baseURL string, httpClient *http.Client) client.ServiceBinding {
	return ForwardedServiceBinding(baseURL, httpClient, userCredentialProfile)
}

// ForwardedServiceBinding is ServiceBindingFor for a provider that names its
// forwarded-bearer credential profile something other than "user" — put-server
// calls the registry bearer a caller forwards "reader". The bearer still
// travels per call (client.WithForwardedUserToken).
func ForwardedServiceBinding(baseURL string, httpClient *http.Client, profile string) client.ServiceBinding {
	return client.ServiceBinding{
		URL:      strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		ClientID: CLIClientID,
		Credentials: map[string]client.CredentialBinding{
			profile: {Source: client.CredentialSourceForwardedUser},
		},
		AllowInsecure: true,
		HTTPClient:    WithUserAgent(httpClient),
		// The CLI prints a refusal to the human who made the request, so it
		// opts in to the provider's free-text message;
		// the framework redacts this call's own credential from it.
		CarryRemoteMessage: true,
	}
}

// CallContext carries the workspace bearer to a generated call as the
// forwarded user credential.
func (ctx *WorkspaceContext) CallContext(parent context.Context) context.Context {
	return client.WithForwardedUserToken(parent, ctx.AuthToken.raw)
}

// RedirectRefusingHTTPClient returns a copy of base whose CheckRedirect
// refuses every redirect (http.ErrUseLastResponse) instead of following one.
// A call routed through NewServiceClient already gets this unconditionally
// from the framework (go.putnami.dev/client's own credential-transport
// hardening); this helper is for a CLI call that still builds its own
// *http.Request by hand — a redirect there could otherwise replay a bearer or
// a registry credential to an origin the caller never chose.
//
// The copy is unconditional, so base is never mutated — including when base
// is itself the result of WithUserAgent, whose idempotent fast path can
// return the same pointer it was given.
func RedirectRefusingHTTPClient(base *http.Client) *http.Client {
	if base == nil {
		base = http.DefaultClient
	}
	refusing := *base
	refusing.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &refusing
}

// Register is the shape of a generated client registration
// (Register<Name>Client).
type Register func(module *app.Module, override ...client.ServiceBinding) *app.Module

// NewServiceClient resolves one generated client, T, through its generated
// registration and the given binding: the same dependency-injection path an
// application takes, without an application around it.
func NewServiceClient[T any](register Register, binding client.ServiceBinding) (*T, error) {
	module := register(app.NewModule("putnami-cloud-cli"), binding)
	container := inject.NewContainerContext("putnami-cloud-cli")
	for _, registration := range module.GetRegistrations() {
		if err := container.Register(registration); err != nil {
			return nil, NewError("configure the Cloud client: "+err.Error(), ExitAPI)
		}
	}
	if err := container.Start(); err != nil {
		return nil, NewError("configure the Cloud client: "+err.Error(), ExitAPI)
	}
	value, err := container.Get(inject.TokenOf[*T]())
	if err != nil {
		return nil, NewError("configure the Cloud client: "+err.Error(), ExitAPI)
	}
	resolved, ok := value.(*T)
	if !ok || resolved == nil {
		return nil, NewError(fmt.Sprintf("configure the Cloud client: resolved %T", value), ExitAPI)
	}
	return resolved, nil
}

// ServiceStatus returns the HTTP status of a provider's refusal, or 0 when err
// is not a provider answer (a transport failure, a timeout, a contract
// violation).
func ServiceStatus(err error) int {
	var remote *client.RemoteError
	if stderrors.As(err, &remote) {
		return remote.StatusCode
	}
	return 0
}

// ServiceMessage returns the provider's free-text message a refusal carried
// (the first-party envelope's `message`, redacted by the framework), or "" when
// err is not a provider refusal or the provider sent none.
func ServiceMessage(err error) string {
	var remote *client.RemoteError
	if stderrors.As(err, &remote) {
		return strings.TrimSpace(remote.Message)
	}
	return ""
}

// ServiceFailureReason is the short reason a failed generated call gives a
// read that must not fail its command: the status line of a provider refusal,
// "invalid response" for an answer outside the provider contract, and
// "request failed: <cause>" for anything else.
func ServiceFailureReason(err error) string {
	if status := ServiceStatus(err); status != 0 {
		return StatusLine(status)
	}
	if perrors.Is(err, client.CodeClientResponse) {
		return "invalid response"
	}
	return "request failed: " + err.Error()
}

// OpenStreamWithSession opens a generated server stream with the workspace's
// bearer and, when the provider answers 401, re-mints the workspace session
// once and opens again with the fresh bearer. It is the stream twin of
// CallWithSession: a long follow (`cloud ci logs --follow`, `cloud logs --follow`)
// outlives the access token minted at command start. The fresh bearer is kept
// on ctx so a later re-open (a reconnect after a drop) reuses it instead of
// re-minting again.
func OpenStreamWithSession[T any](reqCtx context.Context, ctx *WorkspaceContext,
	open func(context.Context) (*client.Stream[T], error)) (*client.Stream[T], error) {
	stream, err := open(ctx.CallContext(reqCtx))
	if err == nil || ctx.RefreshAuth == nil || ServiceStatus(err) != http.StatusUnauthorized {
		return stream, err
	}
	fresh, ferr := ctx.RefreshAuth()
	if ferr != nil {
		return nil, err
	}
	ctx.AuthToken = fresh
	return open(ctx.CallContext(reqCtx))
}

// CallWithSession calls a generated unary method with the workspace's bearer
// and, when the provider answers 401, re-mints the workspace session once and
// calls again with the fresh bearer. A forwarded-user credential must not
// renew itself inside the framework: only the
// consumer that owns the session — the CLI — may re-mint, so every generated
// call the CLI makes through a *WorkspaceContext goes through this wrapper
// instead of failing hard on a token minted at command start. It is the
// unary twin of OpenStreamWithSession; the fresh bearer is kept on ctx so a
// later call in the same command reuses it instead of re-minting again.
func CallWithSession[T any](reqCtx context.Context, ctx *WorkspaceContext,
	call func(context.Context) (T, error)) (T, error) {
	result, err := call(ctx.CallContext(reqCtx))
	if err == nil || ctx.RefreshAuth == nil || ServiceStatus(err) != http.StatusUnauthorized {
		return result, err
	}
	fresh, ferr := ctx.RefreshAuth()
	if ferr != nil {
		var zero T
		return zero, err
	}
	ctx.AuthToken = fresh
	return call(ctx.CallContext(reqCtx))
}

// ServiceCallOptions names what a failed generated call renders as.
type ServiceCallOptions struct {
	// Target is the request URL the error names on a transport failure.
	Target string
	// Prefix starts the message of a provider refusal, e.g. "status: ".
	Prefix string
	// CancelMessage replaces the error when the caller's context was canceled.
	CancelMessage string
}

// ServiceCallError renders a failed generated call as the CLI's exit-coded
// error. A provider refusal reads "<prefix><status line>" and exits ExitAuth
// on 401, ExitAPI otherwise: the generated client withholds the provider's
// free-text message, so the status line is what the CLI can name. A response
// outside the provider contract reads "invalid JSON response from <target>";
// a canceled call reads CancelMessage (ExitUsage); anything else is a transport
// failure, "request failed for <target>: <cause>" (ExitAPI).
func ServiceCallError(reqCtx context.Context, err error, options ServiceCallOptions) error {
	if status := ServiceStatus(err); status != 0 {
		code := ExitAPI
		if status == http.StatusUnauthorized {
			code = ExitAuth
		}
		return NewError(options.Prefix+StatusLine(status), code)
	}
	if reqCtx != nil && reqCtx.Err() != nil && options.CancelMessage != "" {
		return NewError(options.CancelMessage, ExitUsage)
	}
	if perrors.Is(err, client.CodeClientResponse) {
		return NewError("invalid JSON response from "+options.Target, ExitAPI)
	}
	return NewError(fmt.Sprintf("request failed for %s: %s", options.Target, err.Error()), ExitAPI)
}

// RequestError renders a failed generated call the way the CLI rendered the
// same failure of a hand-written request to target, so a command moved onto a
// generated client keeps its error text and exit code: a provider
// refusal reads "request failed for <target>: <provider message, else status
// line>" as an *APIError (ExitAuth on 401, ExitAPI otherwise); an answer
// outside the provider contract reads "invalid JSON response from <target>";
// anything else is a transport failure.
func RequestError(target string, err error) error {
	var remote *client.RemoteError
	if stderrors.As(err, &remote) {
		message := FirstString(strings.TrimSpace(remote.Message), StatusLine(remote.StatusCode))
		code := ""
		if remote.RemoteCode != string(client.CodeClientRemote) {
			code = remote.RemoteCode
		}
		return unexpectedStatusError(target, remote.StatusCode, code, message)
	}
	if perrors.Is(err, client.CodeClientResponse) {
		return NewError("invalid JSON response from "+target, ExitAPI)
	}
	return NewError(fmt.Sprintf("request failed for %s: %s", target, err.Error()), ExitAPI)
}

// UnexpectedStatus is RequestError for a declared success status the command
// does not accept (a create that answered 200 where the command expects 201).
func UnexpectedStatus(target string, status int) error {
	return unexpectedStatusError(target, status, "", StatusLine(status))
}

// ResponseMap projects a generated response onto the map a command renders
// with WriteResult. The generated struct's JSON tags are the provider's own
// wire names, so the round trip reproduces the fields the provider sent.
func ResponseMap(value any) (map[string]any, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, NewError("encode the provider response: "+err.Error(), ExitAPI)
	}
	out := map[string]any{}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, NewError("encode the provider response: "+err.Error(), ExitAPI)
	}
	return out, nil
}

// Deref reads a generated client field: every optional property a generated
// Go client emits is a pointer (nil when the provider omitted it), while the
// CLI's own decoded shapes use plain values with their language zero as
// "absent". Deref bridges the two so a converter reads a generated response
// with one call per field instead of a repeated nil-check.
func Deref[T any](value *T) T {
	if value == nil {
		var zero T
		return zero
	}
	return *value
}
