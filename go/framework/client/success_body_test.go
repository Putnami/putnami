package client

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	perrors "go.putnami.dev/errors"
	clientcontract "go.putnami.dev/protocol/clientcontract"
	"go.putnami.dev/protocol/features/spectest"
)

const successBodyRequirement = "success-body"

// fidelityBody is a body no encoder writes back: its fields are in neither
// declaration nor sorted order, it carries whitespace and a trailing newline,
// and it escapes a character an encoder writes as UTF-8.
const fidelityBody = "{\n  \"zeta\" : \"caf\\u00e9\",\n  \"alpha\":\"a\"\n}\n"

type fidelityValue struct {
	Zeta  string `json:"zeta"`
	Alpha string `json:"alpha"`
}

func fidelityOperation() Operation {
	operation := testOperation(clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{AllOf: []clientcontract.SecurityRequirement{}}}},
		clientcontract.Idempotency{Kind: clientcontract.IdempotencySafe})
	operation.ID = "getFidelity"
	operation.Successes = []OperationSuccess{{Status: http.StatusOK, Content: []OperationContent{{MediaType: "application/json", Schema: &clientcontract.Schema{
		Type: "object", Properties: map[string]clientcontract.Schema{"zeta": {Type: "string"}, "alpha": {Type: "string"}},
		Required: []string{"zeta", "alpha"}, AdditionalProperties: additionalForbidden(),
	}}}}}
	return operation
}

// answeringServer answers every call with status, content type and body, and
// counts the calls it saw.
func answeringServer(t *testing.T, status int, contentType, body string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		if contentType != "" {
			writer.Header().Set("Content-Type", contentType)
		}
		writer.WriteHeader(status)
		_, _ = writer.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server, &calls
}

func anonymousClient(t *testing.T, url string) *Client {
	t.Helper()
	return boundTestClient(t, url, testDescriptor(map[string]clientcontract.CredentialProfile{}), ServiceBinding{})
}

func getFidelity(ctx context.Context, client *Client) (fidelityValue, error) {
	return CallOperation[fidelityValue](ctx, client, &OperationCall{Request: &Request{Method: http.MethodGet, Path: "/items"}}, fidelityOperation())
}

func TestSuccessBodyIsTheProviderBytesAfterTheCallsOwnChecks(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", successBodyRequirement,
		"the-success-body-is-delivered-byte-for-byte-after-the-calls-own-checks")
	server, _ := answeringServer(t, http.StatusOK, "application/json; charset=utf-8", fidelityBody)
	bound := anonymousClient(t, server.URL)

	var sink SuccessBody
	value, err := getFidelity(WithSuccessBody(t.Context(), &sink), bound)
	if err != nil || value.Zeta != "café" || value.Alpha != "a" {
		t.Fatalf("value=%+v err=%v", value, err)
	}
	if got := sink.Bytes(); !bytes.Equal(got, []byte(fidelityBody)) {
		t.Fatalf("success body = %q, want the provider's %q", got, fidelityBody)
	}
	// The proof is only meaningful because marshaling the decoded value again
	// cannot give these bytes back.
	for _, remarshalled := range []any{value, map[string]string{"zeta": value.Zeta, "alpha": value.Alpha}} {
		encoded, err := json.Marshal(remarshalled)
		if err != nil || bytes.Equal(encoded, []byte(fidelityBody)) {
			t.Fatalf("re-marshaling reproduced the body: %s (%v)", encoded, err)
		}
	}

	// A call without the option is unchanged, and a nil sink is no sink.
	if _, err := getFidelity(t.Context(), bound); err != nil {
		t.Fatalf("plain call: %v", err)
	}
	if _, err := getFidelity(WithSuccessBody(t.Context(), nil), bound); err != nil {
		t.Fatalf("nil sink: %v", err)
	}
}

func TestSuccessBodyTravelsOnConnectJSONAndIsRefusedOnConnectProto(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", successBodyRequirement,
		"a-transport-that-rewrites-the-body-is-refused-before-dispatch")
	const connectBody = "{ \"value\" : \"connect\" }"
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		if request.URL.Path != "/inventory.v1.ApiService/GetItems" {
			t.Errorf("path = %s", request.URL.Path)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(connectBody))
	}))
	defer server.Close()
	bound := connectBoundClient(t, server.URL, connectServiceProfiles(), connectServiceBinding())

	var sink SuccessBody
	jsonOperation := connectUnaryOperation([]clientcontract.Transport{connectTransport(clientcontract.EncodingJSON)}, nil, nil)
	item, err := CallOperation[connectItem](WithSuccessBody(t.Context(), &sink), bound, newConnectCallInput(), jsonOperation)
	if err != nil || item.Value != "connect" || string(sink.Bytes()) != connectBody {
		t.Fatalf("connect json: item=%+v body=%q err=%v", item, sink.Bytes(), err)
	}

	protoOperation := connectUnaryOperation([]clientcontract.Transport{connectTransport(clientcontract.EncodingProto)}, nil, nil)
	_, err = CallOperation[connectItem](WithSuccessBody(t.Context(), &sink), bound, newConnectCallInput(), protoOperation)
	if !perrors.Is(err, CodeClientConfig) || !strings.Contains(err.Error(), "connect with the proto encoding") {
		t.Fatalf("connect proto = %v, want a refusal naming the transport", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("provider saw %d calls, want the refused one never sent", calls.Load())
	}
	// A refused call empties the sink: it delivers no bytes, and never leaves
	// the earlier call's body for the caller to mistake for its own.
	if got := sink.Bytes(); got != nil {
		t.Fatalf("a refused call left %q in the sink", got)
	}
	// Without a sink the same operation still dispatches on proto: the refusal
	// belongs to the option, not to the operation.
	if _, err := CallOperation[connectItem](t.Context(), bound, newConnectCallInput(), protoOperation); perrors.Is(err, CodeClientConfig) {
		t.Fatalf("a proto call without a sink was refused: %v", err)
	}
}

func TestSuccessBodyIsReplayedVerbatimFromTheResponseCache(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", successBodyRequirement,
		"a-cached-answer-delivers-the-bytes-the-provider-sent")
	const cachedBody = "{ \"value\" :\t\"cached\" }"
	server, calls := answeringServer(t, http.StatusOK, "application/json", cachedBody)
	clock := newCacheClock()
	cached := cachedClient(t, server.URL, clock)
	operation := cachedAccountOperation(freshStalePolicy(), nil)

	var first, second SuccessBody
	if _, err := getAccount(WithSuccessBody(t.Context(), &first), cached, operation, "a1"); err != nil {
		t.Fatal(err)
	}
	// The caller owns its copy: writing into it reaches neither the cache nor
	// the next caller.
	firstBytes := first.Bytes()
	copy(firstBytes, "XXXX")
	answer, err := getAccount(WithSuccessBody(t.Context(), &second), cached, operation, "a1")
	if err != nil || answer.Value != "cached" {
		t.Fatalf("cached answer = %+v, %v", answer, err)
	}
	if calls.Load() != 1 {
		t.Fatalf("provider saw %d calls, want the second answered from the cache", calls.Load())
	}
	if string(second.Bytes()) != cachedBody {
		t.Fatalf("cached success body = %q, want %q", second.Bytes(), cachedBody)
	}

	// A stale answer masking a provider outage is still the stored bytes.
	clock.Advance(10 * time.Second)
	server.Close()
	var stale SuccessBody
	if _, err := getAccount(WithSuccessBody(t.Context(), &stale), cached, operation, "a1"); err != nil {
		t.Fatalf("stale answer: %v", err)
	}
	if string(stale.Bytes()) != cachedBody {
		t.Fatalf("stale success body = %q, want %q", stale.Bytes(), cachedBody)
	}
}

func TestSuccessBodyIsEmptyAfterACallThatFailed(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", successBodyRequirement, "a-refused-or-failed-call-delivers-no-bytes")
	cases := map[string]struct {
		status      int
		contentType string
		body        string
		code        perrors.Code
	}{
		"undeclared status":     {http.StatusAccepted, "application/json", `{"zeta":"z","alpha":"a"}`, CodeClientResponse},
		"undeclared media type": {http.StatusOK, "text/plain", `{"zeta":"z","alpha":"a"}`, CodeClientResponse},
		"schema mismatch":       {http.StatusOK, "application/json", `{"zeta":"z"}`, CodeClientResponse},
		"trailing data":         {http.StatusOK, "application/json", `{"zeta":"z","alpha":"a"} {}`, CodeClientResponse},
		"empty body":            {http.StatusOK, "application/json", ``, CodeClientResponse},
		"provider error":        {http.StatusInternalServerError, "application/json", `{"code":"internal","error":"Internal Server Error","message":"boom"}`, ""},
	}
	for name, answer := range cases {
		t.Run(name, func(t *testing.T) {
			good, _ := answeringServer(t, http.StatusOK, "application/json", fidelityBody)
			bad, _ := answeringServer(t, answer.status, answer.contentType, answer.body)
			var sink SuccessBody
			// The sink first holds an earlier call's body: a failed call must
			// not leave it there for the caller to mistake for its own.
			if _, err := getFidelity(WithSuccessBody(t.Context(), &sink), anonymousClient(t, good.URL)); err != nil || sink.Bytes() == nil {
				t.Fatalf("setup call: %v", err)
			}
			_, err := getFidelity(WithSuccessBody(t.Context(), &sink), anonymousClient(t, bad.URL))
			if err == nil || (answer.code != "" && !perrors.Is(err, answer.code)) {
				t.Fatalf("err = %v, want %s", err, answer.code)
			}
			if got := sink.Bytes(); got != nil {
				t.Fatalf("a failed call left %q in the sink", got)
			}
		})
	}
}

func TestSuccessBodyFollowsTheStatusAMultiSuccessCallAnswered(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", successBodyRequirement,
		"the-success-body-is-delivered-byte-for-byte-after-the-calls-own-checks")
	answers := map[string]struct {
		status int
		body   string
	}{
		"ok":         {http.StatusOK, "{ \"value\": \"done\" }"},
		"created":    {http.StatusCreated, "\"queued\""},
		"empty":      {http.StatusAccepted, ""},
		"wrong-body": {http.StatusCreated, `{"value":"done"}`},
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		answer := answers[request.URL.Query().Get("shape")]
		if answer.body != "" {
			writer.Header().Set("Content-Type", "application/json")
		}
		writer.WriteHeader(answer.status)
		_, _ = writer.Write([]byte(answer.body))
	}))
	defer server.Close()
	operation := multiSuccessOperation([]clientcontract.Transport{connectRestTransport()})
	operation.Contract.Security = clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{AllOf: []clientcontract.SecurityRequirement{}}}}
	call := func(shape string) *OperationCall {
		input := newConnectCallInput()
		input.Request.Query = map[string]string{"shape": shape}
		return input
	}
	// An unbound client validates a body only when it decodes it, so the
	// schema refusal below is the one the sink itself requires.
	unbound, err := NewBuilder().BaseURL(server.URL).Build()
	if err != nil {
		t.Fatal(err)
	}
	for _, shape := range []string{"ok", "created"} {
		var sink SuccessBody
		response, err := CallOperationResponse(WithSuccessBody(t.Context(), &sink), unbound, call(shape), operation)
		if err != nil || response.StatusCode != answers[shape].status || string(sink.Bytes()) != answers[shape].body {
			t.Fatalf("%s: response=%v body=%q err=%v", shape, response, sink.Bytes(), err)
		}
	}
	var sink SuccessBody
	if _, err := CallOperationResponse(WithSuccessBody(t.Context(), &sink), unbound, call("empty"), operation); err != nil {
		t.Fatal(err)
	}
	if got := sink.Bytes(); got == nil || len(got) != 0 {
		t.Fatalf("a declared bodyless status delivered %q, want an empty non-nil body", got)
	}
	_, err = CallOperationResponse(WithSuccessBody(t.Context(), &sink), unbound, call("wrong-body"), operation)
	if !perrors.Is(err, CodeClientResponse) || sink.Bytes() != nil {
		t.Fatalf("a body its status's schema refuses: err=%v body=%q", err, sink.Bytes())
	}
	// Without a sink the unbound call keeps its existing contract: the status
	// and media type are checked here, the schema when the method decodes.
	if _, err := CallOperationResponse(t.Context(), unbound, call("wrong-body"), operation); err != nil {
		t.Fatalf("plain unbound call: %v", err)
	}
	_, err = CallOperationResponse(WithSuccessBody(t.Context(), &sink), unbound, call("ok"),
		multiSuccessOperation([]clientcontract.Transport{connectTransport(clientcontract.EncodingProto), connectRestTransport()}))
	if !perrors.Is(err, CodeClientConfig) {
		t.Fatalf("a multi-success operation dispatching on proto: %v", err)
	}
}

func TestSuccessBodyIsRefusedByEveryCallThatCannotDeliverIt(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", successBodyRequirement, "a-refused-or-failed-call-delivers-no-bytes")
	server, calls := answeringServer(t, http.StatusOK, "application/json", `{"value":"x"}`)
	bound := anonymousClient(t, server.URL)
	// Every refusal starts from a sink that already holds a body: what it must
	// prove is that the refused call empties it.
	var sink SuccessBody
	sink.deliver(&Response{Body: []byte(`{"value":"earlier"}`)}, nil)
	sinkContext := func() context.Context { return WithSuccessBody(t.Context(), &sink) }
	request := func() *OperationCall {
		return &OperationCall{Request: &Request{Method: http.MethodGet, Path: "/items"}}
	}
	void := fidelityOperation()
	void.Successes = []OperationSuccess{{Status: http.StatusNoContent}}
	binary := binaryOperation(clientcontract.TransportRESTJSON)
	stream := serverStreamOperation(nil)
	stream.Contract.Security = clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{AllOf: []clientcontract.SecurityRequirement{}}}}
	refusals := map[string]func() error{
		"a void operation through CallOperation": func() error {
			_, err := CallOperation[fidelityValue](sinkContext(), bound, request(), void)
			return err
		},
		"a raw octet operation through CallOperation": func() error {
			_, err := CallOperation[fidelityValue](sinkContext(), bound, request(), binary)
			return err
		},
		"CallOperationVoid": func() error { return CallOperationVoid(sinkContext(), bound, request(), void) },
		"CallOperationBinary": func() error {
			_, err := CallOperationBinary(sinkContext(), bound, request(), binary)
			return err
		},
		"CallOperationBinaryStream": func() error {
			_, err := CallOperationBinaryStream(sinkContext(), bound, request(), binaryStreamOperation())
			return err
		},
		"a server stream": func() error {
			_, err := OpenOperationServerStream[fidelityValue](sinkContext(), bound, request(), stream)
			return err
		},
		"DoOperation": func() error {
			_, err := bound.DoOperation(sinkContext(), request().Request, fidelityOperation())
			return err
		},
	}
	for name, refused := range refusals {
		t.Run(name, func(t *testing.T) {
			sink.deliver(&Response{Body: []byte(`{"value":"earlier"}`)}, nil)
			if err := refused(); !perrors.Is(err, CodeClientConfig) {
				t.Fatalf("err = %v, want client.config", err)
			}
			if got := sink.Bytes(); got != nil {
				t.Fatalf("the refused call left %q in the sink", got)
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatalf("provider saw %d calls, want every refusal before dispatch", calls.Load())
	}
	// A refusal on an empty sink is the same refusal.
	if _, err := CallOperation[fidelityValue](sinkContext(), bound, request(), void); !perrors.Is(err, CodeClientConfig) || sink.Bytes() != nil {
		t.Fatalf("void on an empty sink: err=%v body=%q", err, sink.Bytes())
	}
}

func TestSuccessBodyBelongsToOneCallAtATime(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", successBodyRequirement, "a-sink-belongs-to-one-call-at-a-time")
	provider := heldProvider()
	server := httptest.NewServer(provider)
	defer server.Close()
	bound := anonymousClient(t, server.URL)
	operation := cachedAccountOperation(nil, nil)

	var sink SuccessBody
	ctx := WithSuccessBody(t.Context(), &sink)
	done := make(chan error, 1)
	go func() {
		_, err := getAccount(ctx, bound, operation, "held")
		done <- err
	}()
	deadline := time.Now().Add(10 * time.Second)
	for provider.calls.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the first call never reached the provider")
		}
		time.Sleep(time.Millisecond)
	}
	if got := sink.Bytes(); got != nil {
		t.Fatalf("the sink holds %q while its call is in flight", got)
	}
	_, err := getAccount(ctx, bound, operation, "second")
	if !perrors.Is(err, CodeClientConfig) || !strings.Contains(err.Error(), "one call at a time") {
		t.Fatalf("second call on a held sink = %v, want a refusal", err)
	}
	// A call refused for another reason while the sink is held leaves it to
	// the holder as well: the holder's answer is still delivered below.
	void := fidelityOperation()
	void.Successes = []OperationSuccess{{Status: http.StatusNoContent}}
	if _, err := CallOperation[fidelityValue](ctx, bound, &OperationCall{Request: &Request{Method: http.MethodGet, Path: "/items"}}, void); !perrors.Is(err, CodeClientConfig) {
		t.Fatalf("void call on a held sink = %v, want a refusal", err)
	}
	provider.release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if string(sink.Bytes()) != "{\"value\":\"/accounts/held\"}\n" {
		t.Fatalf("the first call delivered %q", sink.Bytes())
	}
	if provider.calls.Load() != 1 {
		t.Fatalf("provider saw %d calls, want the refused call never sent", provider.calls.Load())
	}
	// Released: the same sink serves the next call.
	if _, err := getAccount(ctx, bound, operation, "next"); err != nil || string(sink.Bytes()) != "{\"value\":\"/accounts/next\"}\n" {
		t.Fatalf("reuse after release: body=%q err=%v", sink.Bytes(), err)
	}
}

func TestSuccessBodyIsNotCarriedIntoANestedGeneratedCall(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", successBodyRequirement, "a-sink-belongs-to-one-call-at-a-time")
	nested, _ := answeringServer(t, http.StatusOK, "application/json", `{"value":"nested"}`)
	nestedClient := anonymousClient(t, nested.URL)
	var nestedOwn SuccessBody
	provider := TokenSourceFunc(func(ctx context.Context, _ CredentialRequest) (Credential, error) {
		// A nested call made with the context the callback received neither
		// fills the outer sink nor is refused because of it...
		if _, err := getAccount(ctx, nestedClient, cachedAccountOperation(nil, nil), "n"); err != nil {
			return Credential{}, err
		}
		// ...and a callback that wants its own body passes its own sink.
		if _, err := getAccount(WithSuccessBody(ctx, &nestedOwn), nestedClient, cachedAccountOperation(nil, nil), "n"); err != nil {
			return Credential{}, err
		}
		return Credential{Value: "outer-token", Expiry: time.Now().Add(time.Hour)}, nil
	})
	outer, _ := answeringServer(t, http.StatusOK, "application/json", fidelityBody)
	operation := fidelityOperation()
	operation.Contract.Security = clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{AllOf: []clientcontract.SecurityRequirement{{Profile: "service"}}}}}
	bound := boundTestClient(t, outer.URL, testDescriptor(map[string]clientcontract.CredentialProfile{
		"service": {Kind: clientcontract.CredentialServiceToken},
	}), ServiceBinding{Credentials: map[string]CredentialBinding{"service": {Provider: provider}}})

	var sink SuccessBody
	if _, err := CallOperation[fidelityValue](WithSuccessBody(t.Context(), &sink), bound,
		&OperationCall{Request: &Request{Method: http.MethodGet, Path: "/items"}}, operation); err != nil {
		t.Fatal(err)
	}
	if string(sink.Bytes()) != fidelityBody {
		t.Fatalf("outer sink = %q, want the outer call's body", sink.Bytes())
	}
	if string(nestedOwn.Bytes()) != `{"value":"nested"}` {
		t.Fatalf("nested sink = %q, want the nested call's body", nestedOwn.Bytes())
	}
}
