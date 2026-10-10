package runtimecli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"

	"go.putnami.dev/client"
	controlapiclient "go.putnami.dev/cloud/clients/control-api/go"
	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	perrors "go.putnami.dev/errors"
)

// The deploy submit (POST .../deploy) and the two status reads (GET
// .../deploy/{release}) go through control-api's generated client. control-api
// declares the members a deploy refusal prints as error details: the
// aggregated preflight rejections (control.deploy.rejected), the Google Cloud
// refusal body (control.cloud_failure) and the 409 retry flag
// (control.deploy.release_conflict).
//
// Each call binds its own client. The client shares ctx's HTTP client and its
// connection pool, and its deployTransport records what this one call saw.
// A per-call client also keeps the framework's circuit breaker from carrying
// one poll's failures into the next: the --wait loop owns that retry.

// deployExchange is what deployTransport saw during one deploy call.
type deployExchange struct {
	// roundTrip is the last round trip's failure, as net/http's client reports
	// it (*url.Error).
	roundTrip error
	// read is the failure to read the last answer's body.
	read error
}

func (exchange *deployExchange) reset() {
	exchange.roundTrip, exchange.read = nil, nil
}

// deployControl binds control-api's generated client for one deploy call.
func deployControl(ctx *deployCtx) (*controlapiclient.ControlClient, *deployExchange, error) {
	exchange := &deployExchange{}
	binding := ctx.ServiceBinding()
	httpClient := http.DefaultClient
	if binding.HTTPClient != nil {
		httpClient = binding.HTTPClient
	}
	// A copy, so wrapping its transport leaves ctx's client as it was.
	copied := *httpClient
	base := copied.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	copied.Transport = deployTransport{base: base, exchange: exchange}
	binding.HTTPClient = &copied
	control, err := clicore.NewServiceClient[controlapiclient.ControlClient](controlapiclient.RegisterControlClient, binding)
	if err != nil {
		return nil, nil, err
	}
	return control, exchange, nil
}

// deployCall runs one generated deploy operation with ctx's bearer, and the
// single re-mint after a 401 the hand-written request had. call keeps the
// reply the generated client decoded; deployCall returns the success status.
func deployCall(reqCtx context.Context, ctx *deployCtx, call func(context.Context, *controlapiclient.ControlClient) (int, error)) (int, *deployExchange, error) {
	control, exchange, err := deployControl(ctx)
	if err != nil {
		return 0, nil, err
	}
	status, err := clicore.CallWithSession(reqCtx, ctx.WorkspaceContext, func(callCtx context.Context) (int, error) {
		exchange.reset()
		return call(callCtx, control)
	})
	if err != nil {
		return 0, exchange, err
	}
	return status, exchange, nil
}

// deployCallFailure renders a failed generated deploy call that the control
// plane did not refuse, the way the hand-written request did: a canceled
// call reads "deploy canceled by user"; a transport failure, or a call that
// outlived its declared bound, is a transportError the --wait loop retries;
// an unreadable answer reads "read response: <cause>"; an answer outside the
// contract reads "invalid JSON response from <target>".
func deployCallFailure(reqCtx context.Context, target string, exchange *deployExchange, err error) error {
	if reqCtx.Err() != nil {
		return clicore.NewError("deploy canceled by user", clicore.ExitUsage)
	}
	cause := err
	if exchange != nil && exchange.roundTrip != nil {
		cause = exchange.roundTrip
	} else if exchange != nil && exchange.read != nil {
		cause = exchange.read
	}
	switch {
	case perrors.Is(err, client.CodeClientDeadline):
		return deployTransportError(target, cause)
	case exchange != nil && exchange.roundTrip != nil:
		return deployTransportError(target, exchange.roundTrip)
	case exchange != nil && exchange.read != nil:
		return clicore.NewError("read response: "+exchange.read.Error(), clicore.ExitAPI)
	case perrors.Is(err, client.CodeClientResponse):
		return clicore.NewError("invalid JSON response from "+target, clicore.ExitAPI)
	}
	return clicore.NewError(fmt.Sprintf("request failed for %s: %s", target, err.Error()), clicore.ExitAPI)
}

// deployRefusalMessage is the text a deploy refusal prints: the control
// plane's message, else the status line, then the Google Cloud refusal body
// when the control plane sent one.
func deployRefusalMessage(err error, status int, gcpBody string) string {
	message := clicore.FirstString(clicore.ServiceMessage(err), clicore.StatusLine(status))
	if gcpBody != "" {
		message += "\n  GCP response body: " + gcpBody
	}
	return message
}

// deployPostBody reads the deploy request the CLI built into the declared
// request. Every member the CLI sends must survive the conversion: a member
// the contract does not declare, or a value it would change, fails here
// instead of being dropped from the wire.
func deployPostBody(body map[string]any) (controlapiclient.DeployPostRequest, error) {
	var out controlapiclient.DeployPostRequest
	sent, err := json.Marshal(body)
	if err != nil {
		return out, clicore.NewError("marshal deploy body: "+err.Error(), clicore.ExitAPI)
	}
	if err := json.Unmarshal(sent, &out); err != nil {
		return out, clicore.NewError("deploy body does not match the control-api contract: "+err.Error(), clicore.ExitAPI)
	}
	declared, err := json.Marshal(out)
	if err != nil {
		return out, clicore.NewError("marshal deploy body: "+err.Error(), clicore.ExitAPI)
	}
	if !sameJSON(sent, declared) {
		return out, clicore.NewError("deploy body does not match the control-api contract: "+string(sent), clicore.ExitAPI)
	}
	return out, nil
}

// sameJSON reports whether a and b hold the same JSON value.
func sameJSON(a, b []byte) bool {
	left, lerr := decodeJSONValue(a)
	right, rerr := decodeJSONValue(b)
	return lerr == nil && rerr == nil && reflect.DeepEqual(left, right)
}

func decodeJSONValue(data []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return value, nil
}

// deployTransport sits between the generated call and ctx's HTTP transport.
// It records a failed round trip, and reshapes an answer the generated call
// would otherwise misread.
type deployTransport struct {
	base     http.RoundTripper
	exchange *deployExchange
}

func (t deployTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// The exchange describes the last attempt: an earlier attempt's failure
	// that a retry got past is not this call's.
	t.exchange.reset()
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		// The generated call reports a transport failure without its cause
		// (dial, TLS, reset, deadline). The deploy error names it, as it did
		// before the generated client. This can go when the generated call
		// keeps the transport cause.
		t.exchange.roundTrip = &url.Error{Op: urlErrorOp(req.Method), URL: req.URL.Redacted(), Err: err}
		return nil, err
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.exchange.read = err
		return nil, err
	}
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		body = withoutNullMembers(body)
	case resp.StatusCode >= 400:
		body = deployRefusalEnvelope(body)
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Del("Content-Length")
	return resp, nil
}

// urlErrorOp spells a method the way net/http's *url.Error does ("Post").
func urlErrorOp(method string) string {
	if method == "" {
		return "Get"
	}
	return method[:1] + strings.ToLower(method[1:])
}

// withoutNullMembers returns a success body with every null object member
// removed, at any depth.
//
// Compatibility: encoding/json writes a nil slice or map as null, and the
// control-api contract declares those members as plain arrays and objects, so
// the generated call refuses the null. The deploy answers have such members
// (an attempt's unset worker revision, for one). The CLI reads null and an absent member
// alike, as it always has, so dropping them changes no value it reads. This can
// go when the framework's OpenAPI generator declares nil-able slices and maps
// nullable and this client is regenerated. A body that holds no null, or is
// not one JSON value, is returned unchanged for the generated call to judge.
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
	out, err := json.Marshal(dropNullMembers(value))
	if err != nil {
		return body
	}
	return out
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

// deployRefusalEnvelope returns a deploy refusal as the first-party envelope
// ({code, message, details}) the generated call reads.
//
// Compatibility: a control plane answers many deploy refusals with the human
// text only in `error` (the engine's BadRequest). This copies the text the CLI
// printed (`message`, else `error`) into `message`. This can go once every
// control plane the CLI supports writes message on each deploy refusal.
func deployRefusalEnvelope(body []byte) []byte {
	value, err := decodeJSONValue(body)
	if err != nil {
		return body
	}
	envelope, ok := value.(map[string]any)
	if !ok {
		return body
	}
	changed := false
	if text := clicore.ServerErrorMessage(envelope, "", "message", "error"); text != "" {
		if current, isString := envelope["message"].(string); !isString || current != text {
			envelope["message"] = text
			changed = true
		}
	}
	if !changed {
		return body
	}
	out, err := json.Marshal(envelope)
	if err != nil {
		return body
	}
	return out
}
