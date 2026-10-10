package remotecache

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"

	"go.putnami.dev/client"
	cacheserverclient "go.putnami.dev/cloud/clients/cache-server/go"
)

// The control operations (negotiate, capabilities, store, commit, find-missing,
// commit-batch, download-batch, object lookup and store, run-marker lookup and
// publish) go through cache-server's generated client. The generated call owns
// the route, the request and answer schemas, the X-Client-Id identity and the
// redirect refusal. This client keeps what the contract cannot state: the
// bearer's lifecycle (doAuthed), the shared strict-v1 parsers that read the
// answer, and the redacted refusal record.
//
// The same calls also reach any third-party server that speaks protocol/cache
// at PUTNAMI_CACHE_URL. controlTransport holds the branches that keep those
// servers working; each one says when it can go.

// controlClientID is the client identity every generated call sends as
// X-Client-Id. It is the CLI's identity (cli-core CLIClientID).
const controlClientID = "putnami-cli"

// controlCredentialProfile is the cache-server credential profile a runner or
// the CLI satisfies with its own bearer: a forwarded user token.
const controlCredentialProfile = "user"

// anonymousBearer stands in for an empty bearer inside the process. The
// generated call refuses to run without a forwarded credential, while the old
// client sent such a request without an Authorization header. controlTransport
// removes the header again, so this value never reaches the network.
const anonymousBearer = "putnami-cli-anonymous-cache-request"

// maxRefusalBytes bounds the refusal body kept for StatusError and
// AuthFailure. Both read only its code and a bounded message.
const maxRefusalBytes = 1 << 20

// controlCall runs one generated operation with a context that carries the
// bearer and the success-body sink.
type controlCall func(ctx context.Context, api *cacheserverclient.CacheClient) error

// controlClients are the two generated bindings of one Client. They share the
// HTTP client, so they share its connection pool. The provenance binding adds
// the opt-in header to every call; the header-free binding keeps the
// strict-v1 negotiate request old servers and third-party servers expect.
// Neither binding caches a credential: the bearer travels in each call's
// context, so the single re-mint stays in doAuthed.
type controlClients struct {
	plain      *cacheserverclient.CacheClient
	provenance *cacheserverclient.CacheClient
	err        error
}

func newControlClients(baseURL string, hc *http.Client) controlClients {
	if hc == nil {
		hc = http.DefaultClient
	}
	// A copy, so wrapping its transport leaves the caller's client as it was.
	copied := *hc
	controlHTTP := &copied
	base := controlHTTP.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	boundURL, user := splitUserinfo(baseURL)
	controlHTTP.Transport = controlTransport{base: base, user: user}
	binding := client.ServiceBinding{
		URL:      boundURL,
		ClientID: controlClientID,
		Credentials: map[string]client.CredentialBinding{
			controlCredentialProfile: {Source: client.CredentialSourceForwardedUser},
		},
		// ValidateURL is the cache URL policy (https, loopback http, or the
		// PUTNAMI_ALLOW_INSECURE_CACHE opt-in); the binding does not repeat it.
		AllowInsecure: true,
		// The refusal message reaches the run log through StatusError and the
		// redacted AuthFailure, as it did before the generated client.
		CarryRemoteMessage: true,
		HTTPClient:         controlHTTP,
	}
	plain, err := cacheserverclient.NewCacheClientBinding(binding)
	if err != nil {
		return controlClients{err: fmt.Errorf("bind cache server client: %w", err)}
	}
	binding.Headers = map[string]string{EntryProvenanceHeader: entryProvenanceVersion}
	provenance, err := cacheserverclient.NewCacheClientBinding(binding)
	if err != nil {
		return controlClients{err: fmt.Errorf("bind cache server client: %w", err)}
	}
	return controlClients{plain: plain, provenance: provenance}
}

// splitUserinfo removes the user info from a cache URL and returns it apart.
// The service binding refuses a URL that carries user info. The old client
// accepted one, and net/http sent it as basic credentials on a request that
// had no bearer; controlTransport keeps doing that.
func splitUserinfo(raw string) (string, *url.Userinfo) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.User == nil {
		return raw, nil
	}
	user := parsed.User
	parsed.User = nil
	return parsed.String(), user
}

// postControl sends req through a generated operation whose input is
// {Body: B} and returns the answer's body as the server sent it. The protocol
// request is read into the generated input strictly: a member the cache-server
// contract does not declare fails here instead of being dropped from the wire.
func postControl[I, R any](ctx context.Context, c *Client, op string, req any, method func(*cacheserverclient.CacheClient, context.Context, I) (*R, error)) ([]byte, error) {
	return postControlWith(ctx, c, op, false, req, method)
}

func postControlWith[I, R any](ctx context.Context, c *Client, op string, provenance bool, req any, method func(*cacheserverclient.CacheClient, context.Context, I) (*R, error)) ([]byte, error) {
	raw, err := json.Marshal(struct {
		Body any `json:"body"`
	}{Body: req})
	if err != nil {
		return nil, fmt.Errorf("marshal %s request: %w", op, err)
	}
	var in I
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&in); err != nil {
		return nil, fmt.Errorf("%s request does not match the cache server contract: %w", op, err)
	}
	return c.control(ctx, op, provenance, func(ctx context.Context, api *cacheserverclient.CacheClient) error {
		_, err := method(api, ctx, in)
		return err
	})
}

// control runs one generated call under doAuthed and maps a non-2xx answer to
// a StatusError.
func (c *Client) control(ctx context.Context, op string, provenance bool, call controlCall) ([]byte, error) {
	data, status, err := c.doAuthed(ctx, op, c.controlAttempt(op, provenance, call))
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, statusError(op, status, data)
	}
	return data, nil
}

// controlAttempt adapts a generated call to doAuthed: it returns the success
// body and 200, or the refusal body and its status, or a transport error.
func (c *Client) controlAttempt(op string, provenance bool, call controlCall) func(context.Context, *resolvedBearer) ([]byte, int, error) {
	return func(ctx context.Context, cred *resolvedBearer) ([]byte, int, error) {
		if c.controls.err != nil {
			return nil, 0, fmt.Errorf("%s: %w", op, c.controls.err)
		}
		api := c.controls.plain
		if provenance {
			api = c.controls.provenance
		}
		exchange := &controlExchange{}
		token := cred.Token
		if strings.TrimSpace(token) == "" {
			exchange.anonymous = true
			token = anonymousBearer
		}
		var success client.SuccessBody
		callCtx := client.WithSuccessBody(client.WithForwardedUserToken(withControlExchange(ctx, exchange), token), &success)
		err := call(callCtx, api)
		if err == nil {
			return success.Bytes(), http.StatusOK, nil
		}
		var remote *client.RemoteError
		if errors.As(err, &remote) {
			// The generated error names only declared codes, and cache-server's
			// own codes (unauthorized, invalid-request, …) are not declared, so
			// StatusError and AuthFailure read the refusal body the transport
			// kept. This can go when cache-server declares its error codes.
			return exchange.refusal, remote.StatusCode, nil
		}
		// The generated call reports a transport failure as a bare code; the
		// cause the transport saw (dial, TLS, deadline) is what a run log needs.
		// This can go when the generated call keeps the transport cause.
		if exchange.err != nil {
			return nil, 0, fmt.Errorf("%s: %w", op, exchange.err)
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, 0, fmt.Errorf("%s: %w", op, ctxErr)
		}
		return nil, 0, fmt.Errorf("%s: %w", op, err)
	}
}

// controlExchange is what controlTransport saw for one generated call.
type controlExchange struct {
	// anonymous asks the transport to drop the placeholder bearer.
	anonymous bool
	// refusal is the body of a non-2xx answer, bounded by maxRefusalBytes.
	refusal []byte
	// err is the transport failure, with the method and URL like net/http's
	// own *url.Error.
	err error
}

type controlExchangeKey struct{}

func withControlExchange(ctx context.Context, exchange *controlExchange) context.Context {
	return context.WithValue(ctx, controlExchangeKey{}, exchange)
}

func controlExchangeFrom(ctx context.Context) *controlExchange {
	exchange, _ := ctx.Value(controlExchangeKey{}).(*controlExchange)
	return exchange
}

// controlTransport sits between the generated call and the pooled transport.
// It restores the request headers the control calls always sent, and it
// reshapes a success answer that a protocol/cache server may send but the
// generated client would refuse.
type controlTransport struct {
	base http.RoundTripper
	// user is the user info of the configured cache URL, or nil.
	user *url.Userinfo
}

func (t controlTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	exchange := controlExchangeFrom(req.Context())
	out := req.Clone(req.Context())
	if exchange != nil && exchange.anonymous {
		// Compatibility: a token source that yields an empty bearer still sends
		// the request without credentials, as before, so a third-party server
		// that needs none keeps answering and cache-server keeps answering 401
		// into the refusal record. This can go when the cache-server contract
		// can declare an optional credential, or when the CLI refuses an empty
		// bearer itself.
		out.Header.Del("Authorization")
		if t.user != nil {
			// Compatibility: net/http sent the cache URL's user info as basic
			// credentials when the request had no Authorization header, so a
			// third-party server behind basic auth kept answering. This can go
			// when ValidateURL refuses a cache URL that carries user info.
			password, _ := t.user.Password()
			out.SetBasicAuth(t.user.Username(), password)
		}
	}
	if out.Header.Get("Accept") == "" {
		out.Header.Set("Accept", "application/json")
	}
	setUserAgent(out)

	resp, err := t.base.RoundTrip(out)
	if err != nil {
		if exchange != nil {
			exchange.err = &url.Error{Op: urlErrorOp(out.Method), URL: out.URL.Redacted(), Err: err}
		}
		return nil, err
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	closeErr := resp.Body.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil && len(body) > maxResponseBytes {
		err = fmt.Errorf("response exceeds %d bytes", maxResponseBytes)
	}
	if err != nil {
		if exchange != nil {
			exchange.err = fmt.Errorf("read response: %w", err)
		}
		return nil, err
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		body = acceptedSuccess(resp, body)
	} else if exchange != nil {
		exchange.refusal = body
		if len(exchange.refusal) > maxRefusalBytes {
			exchange.refusal = exchange.refusal[:maxRefusalBytes]
		}
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Del("Content-Length")
	return resp, nil
}

// acceptedSuccess reshapes a 2xx answer into the one the generated contract
// declares, and returns its body.
func acceptedSuccess(resp *http.Response, body []byte) []byte {
	// Compatibility: protocol/cache fixes neither the success status nor the
	// media type, and the old client read any 2xx JSON body. cache-server
	// answers 200 application/json, so this changes nothing for it; it keeps a
	// third-party server that answers 201, or omits Content-Type, working. This
	// can go when protocol/cache requires 200 application/json.
	if resp.StatusCode != http.StatusOK {
		resp.StatusCode = http.StatusOK
		resp.Status = "200 OK"
	}
	if mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type")); err != nil || !strings.EqualFold(mediaType, "application/json") {
		if resp.Header == nil {
			resp.Header = make(http.Header)
		}
		resp.Header.Set("Content-Type", "application/json")
	}
	// Compatibility: encoding/json writes a nil slice, map or []byte as null,
	// and the cache-server contract declares those members as plain arrays,
	// objects and strings, so the generated client refuses the null. Every
	// strict-v1 parser reads null and an absent member alike, so dropping the
	// null members changes no decoded value. cache-server itself sends such
	// nulls: a stored manifest without files answers "files": null. This can go
	// when the framework's OpenAPI generator declares those members nullable
	// and this client is regenerated.
	return withoutNullMembers(body)
}

// withoutNullMembers returns body with every object member whose value is null
// removed, at any depth. A body that holds no null, or is not one JSON value,
// is returned unchanged for the generated call to judge.
func withoutNullMembers(body []byte) []byte {
	if !bytes.Contains(body, []byte("null")) {
		return body
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return body
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return body
	}
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(dropNullMembers(value)); err != nil {
		return body
	}
	return bytes.TrimSuffix(out.Bytes(), []byte("\n"))
}

func dropNullMembers(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		for key, member := range typed {
			if member == nil {
				delete(typed, key)
				continue
			}
			typed[key] = dropNullMembers(member)
		}
	case []any:
		for i := range typed {
			typed[i] = dropNullMembers(typed[i])
		}
	}
	return value
}

// urlErrorOp spells a method the way net/http's *url.Error does ("Post").
func urlErrorOp(method string) string {
	if method == "" {
		return "Get"
	}
	return method[:1] + strings.ToLower(method[1:])
}
