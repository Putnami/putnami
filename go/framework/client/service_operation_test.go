package client

import (
	"context"
	"encoding/base64"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	perrors "go.putnami.dev/errors"
	phttp "go.putnami.dev/http"
	clientcontract "go.putnami.dev/protocol/clientcontract"
	"go.putnami.dev/protocol/features/spectest"
)

func intPointer(value int) *int       { return &value }
func int64Pointer(value int64) *int64 { return &value }
func boolPointer(value bool) *bool    { return &value }
func additionalForbidden() *clientcontract.AdditionalProperties {
	value := false
	return &clientcontract.AdditionalProperties{Allowed: &value}
}

func testDescriptor(credentials map[string]clientcontract.CredentialProfile) ServiceDescriptor {
	return ServiceDescriptor{Contract: clientcontract.DocumentV1{
		ProtocolVersion: clientcontract.ProtocolVersion,
		Service:         clientcontract.Service{ID: "inventory", Audience: "urn:inventory"},
		Credentials:     credentials,
	}}
}

func testOperation(security clientcontract.Security, idempotency clientcontract.Idempotency) Operation {
	return Operation{
		ID: "putItem",
		Contract: clientcontract.OperationV1{
			Stream:      clientcontract.StreamUnary,
			Transports:  []clientcontract.Transport{{Protocol: clientcontract.TransportRESTJSON, Path: "/items", Encoding: clientcontract.EncodingJSON}},
			Security:    security,
			Errors:      []clientcontract.DeclaredError{},
			Idempotency: idempotency,
		},
		Successes: []OperationSuccess{{Status: 200, Content: []OperationContent{{MediaType: "application/json", Schema: &clientcontract.Schema{
			Type: "object", Properties: map[string]clientcontract.Schema{"value": {Type: "string"}}, Required: []string{"value"}, AdditionalProperties: additionalForbidden(),
		}}}}},
	}
}

func boundTestClient(t *testing.T, endpoint string, descriptor ServiceDescriptor, binding ServiceBinding) *Client {
	t.Helper()
	binding.URL = endpoint
	if binding.ClientID == "" {
		binding.ClientID = "consumer.workload"
	}
	bindings, err := newServiceBindings(ServicesOptions{Services: map[string]ServiceBinding{"inventory": binding}})
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewServiceClient(bindings, descriptor)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func freshSource(value string, calls *atomic.Int32) TokenSource {
	return TokenSourceFunc(func(context.Context, CredentialRequest) (Credential, error) {
		if calls != nil {
			calls.Add(1)
		}
		return Credential{Value: value, Expiry: time.Now().Add(time.Hour)}, nil
	})
}

func TestBoundClientAppliesIdentityCredentialsRetryDeadlineAndIdempotency(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "typed-service-binding", "a-generated-client-resolves-its-endpoint-and-identity-from-the-binding")
	var serviceCalls atomic.Int32
	var tokenCalls atomic.Int32
	var mu sync.Mutex
	var idempotencyKeys []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		call := serviceCalls.Add(1)
		if request.Header.Get("Authorization") != "Bearer service-token" || request.Header.Get("X-Tenant-ID") != "tenant-42" || request.Header.Get("X-Client-Id") != "consumer.workload" {
			t.Errorf("identity headers = %#v", request.Header)
		}
		mu.Lock()
		idempotencyKeys = append(idempotencyKeys, request.Header.Get("Idempotency-Key"))
		mu.Unlock()
		if call == 1 {
			writer.WriteHeader(http.StatusServiceUnavailable)
			_, _ = writer.Write([]byte(`{"code":"errors.unavailable"}`))
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"value":"ok"}`))
	}))
	defer server.Close()
	descriptor := testDescriptor(map[string]clientcontract.CredentialProfile{
		"service": {Kind: clientcontract.CredentialServiceToken, Scopes: []string{"base"}},
		"tenant":  {Kind: clientcontract.CredentialNamedHeader, Header: "X-Tenant-ID"},
	})
	operation := testOperation(clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{AllOf: []clientcontract.SecurityRequirement{
		{Profile: "service", Scopes: []string{"write"}}, {Profile: "tenant"},
	}}}}, clientcontract.Idempotency{Kind: clientcontract.IdempotencyIdempotent, KeyHeader: "Idempotency-Key"})
	operation.Contract.Errors = []clientcontract.DeclaredError{{Status: 503, Code: "errors.unavailable", Retryable: boolPointer(true)}}
	operation.Contract.Resilience = &clientcontract.ResiliencePolicy{
		TimeoutMs: intPointer(5000), AttemptTimeoutMs: intPointer(1000), MaxResponseBytes: int64Pointer(1024),
		Retry: &clientcontract.RetryPolicy{MaxAttempts: intPointer(2), Statuses: []int{503}},
	}
	client := boundTestClient(t, server.URL, descriptor, ServiceBinding{Credentials: map[string]CredentialBinding{
		"service": {Provider: freshSource("service-token", &tokenCalls)},
		"tenant":  {Source: CredentialSourceStatic, Value: "tenant-42"},
	}})
	output, err := Call[struct {
		Value string `json:"value"`
	}](t.Context(), client, &Request{Method: http.MethodPut, Path: "/items", Body: []byte(`{"value":"input"}`)}, operation)
	if err != nil || output.Value != "ok" {
		t.Fatalf("Call output=%#v error=%v", output, err)
	}
	if serviceCalls.Load() != 2 || tokenCalls.Load() != 1 {
		t.Fatalf("calls service=%d token=%d", serviceCalls.Load(), tokenCalls.Load())
	}
	if len(idempotencyKeys) != 2 || idempotencyKeys[0] == "" || idempotencyKeys[0] != idempotencyKeys[1] {
		t.Fatalf("idempotency keys = %v", idempotencyKeys)
	}
}

func TestGeneratedRequestBodyIsValidatedBeforeDispatch(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "local-request-validation", "an-invalid-generated-json-request-is-rejected-before-dispatch")
	spectest.Proves(t, "go/typed-service-clients", "additive-responses", "a-request-property-the-contract-does-not-declare-is-still-refused")
	var serviceCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		serviceCalls.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"value":"unexpected"}`))
	}))
	defer server.Close()

	descriptor := testDescriptor(map[string]clientcontract.CredentialProfile{})
	operation := testOperation(
		clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{AllOf: []clientcontract.SecurityRequirement{}}}},
		clientcontract.Idempotency{Kind: clientcontract.IdempotencyNonIdempotent},
	)
	operation.Request = &OperationRequest{
		Required: true,
		Content: []OperationContent{{MediaType: "application/json", Schema: &clientcontract.Schema{
			Type: "object",
			Properties: map[string]clientcontract.Schema{
				"name": {Type: "string", MinLength: intPointer(2)},
			},
			Required:             []string{"name"},
			AdditionalProperties: additionalForbidden(),
		}}},
	}
	bound := boundTestClient(t, server.URL, descriptor, ServiceBinding{})

	for _, body := range []string{`{"name":"x"}`, `{}`, `{"name":"valid","extra":true}`, `null`} {
		_, err := Call[struct {
			Value string `json:"value"`
		}](t.Context(), bound, &Request{Method: http.MethodPost, Path: "/items", Headers: http.Header{"Content-Type": []string{"application/json"}}, Body: []byte(body)}, operation)
		if err == nil || !perrors.Is(err, CodeClientRequest) {
			t.Fatalf("body %s error = %v, want client.request", body, err)
		}
	}
	if serviceCalls.Load() != 0 {
		t.Fatalf("invalid generated requests dispatched %d times", serviceCalls.Load())
	}

	_, err := Call[struct {
		Value string `json:"value"`
	}](t.Context(), bound, &Request{Method: http.MethodPost, Path: "/items", Headers: http.Header{"Content-Type": []string{"application/json"}}, Body: []byte(`{"name":"valid"}`)}, operation)
	if err != nil || serviceCalls.Load() != 1 {
		t.Fatalf("valid generated request calls=%d error=%v", serviceCalls.Load(), err)
	}
}

func TestAuthenticationRejectionInvalidatesExactCachedServiceToken(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "credential-alternatives", "a-rejected-service-credential-is-invalidated-and-reacquired")
	var tokenCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") == "Bearer token-1" {
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusUnauthorized)
			_, _ = writer.Write([]byte(`{"code":"unauthorized","error":"Unauthorized","message":"rejected"}`))
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"value":"accepted"}`))
	}))
	defer server.Close()
	descriptor := testDescriptor(map[string]clientcontract.CredentialProfile{"service": {Kind: clientcontract.CredentialServiceToken}})
	operation := testOperation(clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{AllOf: []clientcontract.SecurityRequirement{{Profile: "service"}}}}}, clientcontract.Idempotency{Kind: clientcontract.IdempotencyNonIdempotent})
	operation.Contract.Errors = []clientcontract.DeclaredError{{Status: 401, Code: string(perrors.CodeUnauthorized)}}
	client := boundTestClient(t, server.URL, descriptor, ServiceBinding{Credentials: map[string]CredentialBinding{
		"service": {Provider: TokenSourceFunc(func(context.Context, CredentialRequest) (Credential, error) {
			value := fmt.Sprintf("token-%d", tokenCalls.Add(1))
			return Credential{Value: value, Expiry: time.Now().Add(time.Hour)}, nil
		})},
	}})
	_, err := Call[struct {
		Value string `json:"value"`
	}](t.Context(), client, &Request{Method: http.MethodPost, Path: "/items"}, operation)
	var remote *RemoteError
	if !stderrors.As(err, &remote) || remote.Code() != string(perrors.CodeUnauthorized) {
		t.Fatalf("first error = %T %v", err, err)
	}
	output, err := Call[struct {
		Value string `json:"value"`
	}](t.Context(), client, &Request{Method: http.MethodPost, Path: "/items"}, operation)
	if err != nil || output.Value != "accepted" || tokenCalls.Load() != 2 {
		t.Fatalf("second call output=%#v err=%v tokenCalls=%d", output, err, tokenCalls.Load())
	}
}

func TestCredentialAlternativeIsDeterministicAndAcquisitionFailureDoesNotDowngrade(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "credential-alternatives", "a-declared-credential-alternative-is-selected-and-injected")
	var serviceCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		serviceCalls.Add(1)
		_, _ = writer.Write([]byte(`{"value":"unexpected"}`))
	}))
	defer server.Close()
	descriptor := testDescriptor(map[string]clientcontract.CredentialProfile{"service": {Kind: clientcontract.CredentialServiceToken}})
	operation := testOperation(clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{
		{AllOf: []clientcontract.SecurityRequirement{{Profile: "service"}}}, {AllOf: []clientcontract.SecurityRequirement{}},
	}}, clientcontract.Idempotency{Kind: clientcontract.IdempotencySafe})
	client := boundTestClient(t, server.URL, descriptor, ServiceBinding{Credentials: map[string]CredentialBinding{
		"service": {Provider: TokenSourceFunc(func(context.Context, CredentialRequest) (Credential, error) {
			return Credential{}, fmt.Errorf("do not expose this identity error")
		})},
	}})
	_, err := client.DoOperation(t.Context(), &Request{Path: "/items"}, operation)
	if err == nil || !perrors.Is(err, CodeClientCredential) || strings.Contains(err.Error(), "identity error") || serviceCalls.Load() != 0 {
		t.Fatalf("fail-closed result calls=%d err=%v", serviceCalls.Load(), err)
	}
}

func TestForwardedUserTokenRequiresBindingOptInAndStaysPerCall(t *testing.T) {
	seen := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		seen <- request.Header.Get("Authorization")
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"value":"ok"}`))
	}))
	defer server.Close()
	descriptor := testDescriptor(map[string]clientcontract.CredentialProfile{"caller": {Kind: clientcontract.CredentialForwardedUserToken}})
	operation := testOperation(clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{AllOf: []clientcontract.SecurityRequirement{{Profile: "caller"}}}}}, clientcontract.Idempotency{Kind: clientcontract.IdempotencySafe})
	without := boundTestClient(t, server.URL, descriptor, ServiceBinding{})
	ctx := WithForwardedUserToken(t.Context(), "user-token")
	if _, err := without.DoOperation(ctx, &Request{Path: "/items"}, operation); err == nil {
		t.Fatal("context token bypassed binding opt-in")
	}
	with := boundTestClient(t, server.URL, descriptor, ServiceBinding{Credentials: map[string]CredentialBinding{"caller": {Source: CredentialSourceForwardedUser}}})
	if _, err := with.DoOperation(t.Context(), &Request{Path: "/items"}, operation); err == nil {
		t.Fatal("forwarding without a per-call token succeeded")
	}
	if _, err := with.DoOperation(ctx, &Request{Path: "/items"}, operation); err != nil {
		t.Fatal(err)
	}
	if authorization := <-seen; authorization != "Bearer user-token" {
		t.Fatalf("forwarded authorization = %q", authorization)
	}
}

// TestForwardedUserCredentialRemintsOnceOn401: a forwarded-user binding that
// declares Refresh gets exactly one remint retry on 401, mirroring a hand-written client's own single-remint contract (e.g.
// WorkspaceContext.AuthedDo re-minting a stale CLI session and replaying the
// request once).
func TestForwardedUserCredentialRemintsOnceOn401(t *testing.T) {
	var seenAuthorizations []string
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mu.Lock()
		seenAuthorizations = append(seenAuthorizations, request.Header.Get("Authorization"))
		mu.Unlock()
		if request.Header.Get("Authorization") != "Bearer fresh-user-token" {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"value":"ok"}`))
	}))
	defer server.Close()
	descriptor := testDescriptor(map[string]clientcontract.CredentialProfile{"caller": {Kind: clientcontract.CredentialForwardedUserToken}})
	// Non-idempotent (the CLI's real-world POST calls) proves the remint retry
	// is independent of the provider's declared resilience policy, which would
	// otherwise cap this operation at exactly one attempt.
	operation := testOperation(
		clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{AllOf: []clientcontract.SecurityRequirement{{Profile: "caller"}}}}},
		clientcontract.Idempotency{Kind: clientcontract.IdempotencyNonIdempotent},
	)
	var refreshCalls atomic.Int32
	client := boundTestClient(t, server.URL, descriptor, ServiceBinding{Credentials: map[string]CredentialBinding{
		"caller": {
			Source: CredentialSourceForwardedUser,
			Refresh: func(context.Context) (string, error) {
				refreshCalls.Add(1)
				return "fresh-user-token", nil
			},
		},
	}})
	ctx := WithForwardedUserToken(t.Context(), "stale-user-token")
	output, err := Call[struct {
		Value string `json:"value"`
	}](ctx, client, &Request{Method: http.MethodPost, Path: "/items"}, operation)
	if err != nil || output.Value != "ok" {
		t.Fatalf("reminted call output=%#v err=%v", output, err)
	}
	if refreshCalls.Load() != 1 {
		t.Fatalf("refresh calls = %d, want exactly 1", refreshCalls.Load())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seenAuthorizations) != 2 || seenAuthorizations[0] != "Bearer stale-user-token" || seenAuthorizations[1] != "Bearer fresh-user-token" {
		t.Fatalf("authorizations = %#v", seenAuthorizations)
	}
}

// TestForwardedUserCredentialRemintsAtMostOnce proves the remint retry does
// not loop: a Refresh that still 401s surfaces the typed RemoteError instead
// of retrying forever.
func TestForwardedUserCredentialRemintsAtMostOnce(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		writer.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	descriptor := testDescriptor(map[string]clientcontract.CredentialProfile{"caller": {Kind: clientcontract.CredentialForwardedUserToken}})
	operation := testOperation(
		clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{AllOf: []clientcontract.SecurityRequirement{{Profile: "caller"}}}}},
		clientcontract.Idempotency{Kind: clientcontract.IdempotencyNonIdempotent},
	)
	var refreshCalls atomic.Int32
	client := boundTestClient(t, server.URL, descriptor, ServiceBinding{Credentials: map[string]CredentialBinding{
		"caller": {
			Source: CredentialSourceForwardedUser,
			Refresh: func(context.Context) (string, error) {
				refreshCalls.Add(1)
				return "still-stale-user-token", nil
			},
		},
	}})
	ctx := WithForwardedUserToken(t.Context(), "stale-user-token")
	_, err := client.DoOperation(ctx, &Request{Method: http.MethodPost, Path: "/items"}, operation)
	var remote *RemoteError
	if !stderrors.As(err, &remote) || remote.StatusCode != http.StatusUnauthorized {
		t.Fatalf("still-unauthorized error = %T %v", err, err)
	}
	if refreshCalls.Load() != 1 {
		t.Fatalf("refresh calls = %d, want exactly 1", refreshCalls.Load())
	}
	if attempts.Load() != 2 {
		t.Fatalf("server attempts = %d, want exactly 2 (original + one remint retry)", attempts.Load())
	}
}

// TestForwardedUserCredentialWithoutRefreshNeverRetries401 proves the remint
// retry is opt-in: a binding with no Refresh keeps today's behavior of
// surfacing the 401 on the first attempt, for every existing binding that
// never configures one.
func TestForwardedUserCredentialWithoutRefreshNeverRetries401(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		writer.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	descriptor := testDescriptor(map[string]clientcontract.CredentialProfile{"caller": {Kind: clientcontract.CredentialForwardedUserToken}})
	operation := testOperation(
		clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{AllOf: []clientcontract.SecurityRequirement{{Profile: "caller"}}}}},
		clientcontract.Idempotency{Kind: clientcontract.IdempotencySafe},
	)
	client := boundTestClient(t, server.URL, descriptor, ServiceBinding{Credentials: map[string]CredentialBinding{
		"caller": {Source: CredentialSourceForwardedUser},
	}})
	ctx := WithForwardedUserToken(t.Context(), "stale-user-token")
	_, err := client.DoOperation(ctx, &Request{Path: "/items"}, operation)
	var remote *RemoteError
	if !stderrors.As(err, &remote) || remote.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized error = %T %v", err, err)
	}
	if attempts.Load() != 1 {
		t.Fatalf("server attempts = %d, want exactly 1 (no remint hook configured)", attempts.Load())
	}
}

func TestInboundHTTPContextAutomaticallyForwardsEnabledUserAndRequestIdentity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer inbound-user" || request.Header.Get("X-Request-ID") != "request-42" {
			t.Errorf("propagated headers = %#v", request.Header)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"value":"ok"}`))
	}))
	defer server.Close()
	descriptor := testDescriptor(map[string]clientcontract.CredentialProfile{"caller": {Kind: clientcontract.CredentialForwardedUserToken}})
	operation := testOperation(clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{AllOf: []clientcontract.SecurityRequirement{{Profile: "caller"}}}}}, clientcontract.Idempotency{Kind: clientcontract.IdempotencySafe})
	bound := boundTestClient(t, server.URL, descriptor, ServiceBinding{Credentials: map[string]CredentialBinding{"caller": {Source: CredentialSourceForwardedUser}}})
	inboundRequest := httptest.NewRequest(http.MethodGet, "/consumer", nil)
	inboundRequest.Header.Set("Authorization", "Bearer inbound-user")
	inboundRequest.Header.Set("X-Request-ID", "request-42")
	inbound := phttp.NewContext(httptest.NewRecorder(), inboundRequest)
	phttp.RequestID()(inbound, func() *phttp.Response { return nil })
	output, err := Call[struct {
		Value string `json:"value"`
	}](inbound.Context(), bound, &Request{Path: "/items"}, operation)
	if err != nil || output.Value != "ok" {
		t.Fatalf("forwarded call output = %#v, %v", output, err)
	}
}

func TestRemoteErrorProjectsDeclaredBodyAndNeverLeaksSecretsOrRawResponse(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "remote-error-identity", "a-non-success-response-never-carries-raw-response-bytes")
	secret := "active-service-token"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusNotFound)
		// ADR 0006: the declared schema describes `details`; `code`, `error` and
		// `message` are the envelope ADR 0003 fixed and are never re-declared.
		_, _ = writer.Write([]byte(`{"code":"errors.not_found","error":"Not Found","message":"no such item",` +
			`"details":{"reason":"contains active-service-token","token":"body-secret","active-service-token":"key-secret"}}`))
	}))
	defer server.Close()
	descriptor := testDescriptor(map[string]clientcontract.CredentialProfile{"service": {Kind: clientcontract.CredentialServiceToken}})
	errorSchema := clientcontract.Schema{
		Type: "object", Properties: map[string]clientcontract.Schema{"reason": {Type: "string"}},
		Required: []string{"reason"}, AdditionalProperties: &clientcontract.AdditionalProperties{Allowed: boolPointer(true)},
	}
	operation := testOperation(clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{AllOf: []clientcontract.SecurityRequirement{{Profile: "service"}}}}}, clientcontract.Idempotency{Kind: clientcontract.IdempotencySafe})
	operation.Contract.Errors = []clientcontract.DeclaredError{{Status: 404, Code: "errors.not_found", Schema: &errorSchema}}
	client := boundTestClient(t, server.URL, descriptor, ServiceBinding{Credentials: map[string]CredentialBinding{"service": {Provider: freshSource(secret, nil)}}})
	_, err := client.DoOperation(t.Context(), &Request{Path: "/items"}, operation)
	var remote *RemoteError
	if !stderrors.As(err, &remote) {
		t.Fatalf("error = %T %v", err, err)
	}
	if string(remote.Payload) != `{"reason":"[REDACTED]"}` {
		t.Fatalf("sanitized payload = %s", remote.Payload)
	}
	serialized, _ := json.Marshal(remote)
	combined := err.Error() + string(serialized) + string(remote.Payload)
	for _, forbidden := range []string{secret, "body-secret", "key-secret"} {
		if strings.Contains(combined, forbidden) {
			t.Fatalf("remote error leaked %q: %s", forbidden, combined)
		}
	}
	if !perrors.Is(err, perrors.Code("errors.not_found")) {
		t.Fatalf("framework code = %q", perrors.GetCode(err))
	}
}

func TestRemoteErrorRejectsMalformedDeclaredBodyAndOmitsUnsafeTypedDetails(t *testing.T) {
	baseSchema := clientcontract.Schema{
		Type: "object",
		Properties: map[string]clientcontract.Schema{
			"reason": {Type: "string"},
			"hint":   {Type: "string"},
		},
		Required: []string{"reason", "hint"}, AdditionalProperties: additionalForbidden(),
	}
	declared := []clientcontract.DeclaredError{{Status: http.StatusBadRequest, Code: "errors.invalid", Schema: &baseSchema}}
	malformed := decodeRemoteError(callIdentity{serviceID: "users", operationID: "getUser"}, &Response{StatusCode: http.StatusBadRequest, Body: errorEnvelope("errors.invalid", `{"reason":"quota exhausted"}`)}, declared, nil, nil, false)
	if !perrors.Is(malformed, CodeClientResponse) {
		t.Fatalf("malformed declared body = %T %v", malformed, malformed)
	}
	var malformedRemote *RemoteError
	if stderrors.As(malformed, &malformedRemote) {
		t.Fatalf("malformed body surfaced declared remote error: %+v", malformedRemote)
	}

	for name, test := range map[string]struct {
		schema  clientcontract.Schema
		details string
		want    string
	}{
		"redacted enum": {
			schema: clientcontract.Schema{
				Type: "object", Properties: map[string]clientcontract.Schema{
					"reason": {Type: "string"}, "hint": {Type: "string", Enum: []json.RawMessage{json.RawMessage(`"active-service-token"`)}},
				}, Required: []string{"reason", "hint"}, AdditionalProperties: additionalForbidden(),
			},
			details: `{"reason":"quota exhausted","hint":"active-service-token"}`,
		},
		"required secret field": {
			schema: clientcontract.Schema{
				Type: "object", Properties: map[string]clientcontract.Schema{
					"reason": {Type: "string"}, "token": {Type: "string"},
				}, Required: []string{"reason", "token"}, AdditionalProperties: additionalForbidden(),
			},
			details: `{"reason":"quota exhausted","token":"active-service-token"}`,
		},
		"optional secret field": {
			schema: clientcontract.Schema{
				Type: "object", Properties: map[string]clientcontract.Schema{
					"reason": {Type: "string"}, "token": {Type: "string"},
				}, Required: []string{"reason"}, AdditionalProperties: additionalForbidden(),
			},
			details: `{"reason":"quota exhausted","token":"active-service-token"}`,
			want:    `{"reason":"quota exhausted"}`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			declared := []clientcontract.DeclaredError{{Status: http.StatusBadRequest, Code: "errors.invalid", Schema: &test.schema}}
			err := decodeRemoteError(callIdentity{serviceID: "users", operationID: "getUser"}, &Response{StatusCode: http.StatusBadRequest, Body: errorEnvelope("errors.invalid", test.details)}, declared, nil, []string{"active-service-token"}, false)
			var remote *RemoteError
			if !stderrors.As(err, &remote) || remote.Code() != "errors.invalid" {
				t.Fatalf("declared error = %T %v", err, err)
			}
			if got := string(remote.Payload); got != test.want {
				t.Fatalf("safe payload = %q, want %q", got, test.want)
			}
		})
	}
}

type secretRoundTripper struct{ secret string }

func (transport secretRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("transport leaked %s", transport.secret)
}

func TestTransportErrorsAndDeadlinesAreNormalizedAtGeneratedBoundary(t *testing.T) {
	descriptor := testDescriptor(map[string]clientcontract.CredentialProfile{})
	operation := testOperation(clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{AllOf: []clientcontract.SecurityRequirement{}}}}, clientcontract.Idempotency{Kind: clientcontract.IdempotencySafe})
	secret := "query-and-cause-secret"
	client := boundTestClient(t, "https://service.example", descriptor, ServiceBinding{HTTPClient: &http.Client{Transport: secretRoundTripper{secret: secret}}})
	_, err := client.DoOperation(t.Context(), &Request{Path: "/items", Query: map[string]string{"token": secret}}, operation)
	if err == nil || strings.Contains(err.Error(), secret) || !perrors.Is(err, CodeClientRequest) {
		t.Fatalf("normalized transport error = %v", err)
	}

	blockingDescriptor := testDescriptor(map[string]clientcontract.CredentialProfile{"service": {Kind: clientcontract.CredentialServiceToken}})
	blockingOperation := testOperation(clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{AllOf: []clientcontract.SecurityRequirement{{Profile: "service"}}}}}, clientcontract.Idempotency{Kind: clientcontract.IdempotencySafe})
	blockingOperation.Contract.Resilience = &clientcontract.ResiliencePolicy{TimeoutMs: intPointer(20), AttemptTimeoutMs: intPointer(20)}
	blocking := boundTestClient(t, "https://service.example", blockingDescriptor, ServiceBinding{Credentials: map[string]CredentialBinding{"service": {Provider: TokenSourceFunc(func(ctx context.Context, _ CredentialRequest) (Credential, error) {
		<-ctx.Done()
		return Credential{}, ctx.Err()
	})}}})
	_, err = blocking.DoOperation(t.Context(), &Request{Path: "/items"}, blockingOperation)
	if err == nil || !perrors.Is(err, CodeClientDeadline) {
		t.Fatalf("deadline error = %v", err)
	}
}

func TestRedirectCannotForwardGeneratedAuthorization(t *testing.T) {
	var redirected atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected.Store(true) }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Location", target.URL)
		writer.WriteHeader(http.StatusFound)
	}))
	defer server.Close()
	descriptor := testDescriptor(map[string]clientcontract.CredentialProfile{"service": {Kind: clientcontract.CredentialServiceToken}})
	operation := testOperation(clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{AllOf: []clientcontract.SecurityRequirement{{Profile: "service"}}}}}, clientcontract.Idempotency{Kind: clientcontract.IdempotencySafe})
	client := boundTestClient(t, server.URL, descriptor, ServiceBinding{Credentials: map[string]CredentialBinding{"service": {Provider: freshSource("token", nil)}}})
	_, err := client.DoOperation(t.Context(), &Request{Path: "/items"}, operation)
	if err == nil || redirected.Load() {
		t.Fatalf("redirect result redirected=%v err=%v", redirected.Load(), err)
	}
}

func TestResponseSchemaRejectsMissingTrailingAndWrongTypes(t *testing.T) {
	descriptor := testDescriptor(map[string]clientcontract.CredentialProfile{})
	operation := testOperation(clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{AllOf: []clientcontract.SecurityRequirement{}}}}, clientcontract.Idempotency{Kind: clientcontract.IdempotencySafe})
	for name, body := range map[string]string{
		"missing":          `{}`,
		"wrong":            `{"value":42}`,
		"wrong with added": `{"value":42,"added":true}`,
		"trailing":         `{"value":"ok"} {}`,
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				_, _ = writer.Write([]byte(body))
			}))
			defer server.Close()
			client := boundTestClient(t, server.URL, descriptor, ServiceBinding{})
			if _, err := Call[struct {
				Value string `json:"value"`
			}](t.Context(), client, &Request{Path: "/items"}, operation); err == nil {
				t.Fatalf("body %s passed schema validation", body)
			}
		})
	}
}

// A provider that rolls out a new optional response property before its
// consumers do keeps answering them: the generated call accepts the response,
// and the typed value it returns never carries the property.
func TestResponseSchemaDropsPropertiesTheClientDoesNotDeclare(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "additive-responses", "a-response-property-the-client-does-not-declare-is-dropped")
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"value":"ok","addedLater":{"flag":true},"addedScalar":7}`))
	}))
	defer server.Close()
	descriptor := testDescriptor(map[string]clientcontract.CredentialProfile{})
	operation := testOperation(clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{AllOf: []clientcontract.SecurityRequirement{}}}}, clientcontract.Idempotency{Kind: clientcontract.IdempotencySafe})
	client := boundTestClient(t, server.URL, descriptor, ServiceBinding{})

	// The generated type is a struct of the declared properties.
	typed, err := Call[struct {
		Value string `json:"value"`
	}](t.Context(), client, &Request{Path: "/items"}, operation)
	if err != nil || typed.Value != "ok" {
		t.Fatalf("typed call = %+v, %v", typed, err)
	}
}

func TestStrictSuccessStatusContentTypeAndVoidBody(t *testing.T) {
	descriptor := testDescriptor(map[string]clientcontract.CredentialProfile{})
	operation := testOperation(clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{AllOf: []clientcontract.SecurityRequirement{}}}}, clientcontract.Idempotency{Kind: clientcontract.IdempotencySafe})
	for name, response := range map[string]struct {
		status      int
		contentType string
		body        string
	}{
		"undeclared status":       {status: http.StatusCreated, contentType: "application/json", body: `{"value":"ok"}`},
		"undeclared content type": {status: http.StatusOK, contentType: "text/plain", body: `{"value":"ok"}`},
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", response.contentType)
				writer.WriteHeader(response.status)
				_, _ = writer.Write([]byte(response.body))
			}))
			defer server.Close()
			bound := boundTestClient(t, server.URL, descriptor, ServiceBinding{})
			_, err := Call[struct {
				Value string `json:"value"`
			}](t.Context(), bound, &Request{Path: "/items"}, operation)
			if err == nil || !perrors.Is(err, CodeClientResponse) {
				t.Fatalf("strict response error = %v", err)
			}
		})
	}

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"unexpected":true}`))
	}))
	defer server.Close()
	voidOperation := operation
	voidOperation.Successes = []OperationSuccess{{Status: http.StatusOK}}
	bound := boundTestClient(t, server.URL, descriptor, ServiceBinding{})
	response, err := bound.DoOperation(t.Context(), &Request{Path: "/items"}, voidOperation)
	if err == nil {
		err = checkVoidResponse(response, voidOperation)
	}
	if err == nil || !perrors.Is(err, CodeClientResponse) {
		t.Fatalf("unexpected void body error = %v", err)
	}
}

func TestSuccessProjectionPreservesCredentialLikeNamesAndNullableRefs(t *testing.T) {
	nullable := true
	descriptor := testDescriptor(map[string]clientcontract.CredentialProfile{})
	descriptor.Schemas = map[string]clientcontract.Schema{"NullableText": {Type: "string", Nullable: &nullable}}
	operation := testOperation(clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{AllOf: []clientcontract.SecurityRequirement{}}}}, clientcontract.Idempotency{Kind: clientcontract.IdempotencySafe})
	schema := clientcontract.Schema{
		Type: "object",
		Properties: map[string]clientcontract.Schema{
			"tokenCount":        {Type: "integer", Format: "int32"},
			"authorizationMode": {Type: "string"},
			"nullable":          {Ref: "#/components/schemas/NullableText"},
		},
		Required:             []string{"tokenCount", "authorizationMode", "nullable"},
		AdditionalProperties: additionalForbidden(),
	}
	operation.Successes[0].Content[0].Schema = &schema
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"tokenCount":3,"authorizationMode":"delegated","nullable":null}`))
	}))
	defer server.Close()
	bound := boundTestClient(t, server.URL, descriptor, ServiceBinding{})
	output, err := Call[struct {
		TokenCount        int32   `json:"tokenCount"`
		AuthorizationMode string  `json:"authorizationMode"`
		Nullable          *string `json:"nullable"`
	}](t.Context(), bound, &Request{Path: "/items"}, operation)
	if err != nil || output.TokenCount != 3 || output.AuthorizationMode != "delegated" || output.Nullable != nil {
		t.Fatalf("success output = %#v, %v", output, err)
	}
}

type failingResponseBody struct{ secret string }

func (body failingResponseBody) Read([]byte) (int, error) {
	return 0, fmt.Errorf("read failed %s", body.secret)
}
func (failingResponseBody) Close() error { return nil }

type responseFailureRoundTripper struct{ secret string }

func (transport responseFailureRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       failingResponseBody(transport),
	}, nil
}

func TestGeneratedBoundaryPreservesSanitizedResponseFailureCode(t *testing.T) {
	descriptor := testDescriptor(map[string]clientcontract.CredentialProfile{})
	operation := testOperation(clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{AllOf: []clientcontract.SecurityRequirement{}}}}, clientcontract.Idempotency{Kind: clientcontract.IdempotencySafe})
	secret := "response-reader-secret"
	bound := boundTestClient(t, "https://service.example", descriptor, ServiceBinding{HTTPClient: &http.Client{Transport: responseFailureRoundTripper{secret: secret}}})
	_, err := bound.DoOperation(t.Context(), &Request{Path: "/items"}, operation)
	if err == nil || !perrors.Is(err, CodeClientResponse) || strings.Contains(err.Error(), secret) {
		t.Fatalf("normalized response failure = %v", err)
	}
}

func TestDeclaredRetryabilityOverridesStatusPolicyAndNeverReplaysUnsafeCalls(t *testing.T) {
	descriptor := testDescriptor(map[string]clientcontract.CredentialProfile{})
	security := clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{AllOf: []clientcontract.SecurityRequirement{}}}}
	for _, test := range []struct {
		name       string
		kind       clientcontract.IdempotencyKind
		retryable  bool
		statuses   []int
		wantCalls  int32
		wantOutput bool
	}{
		{name: "explicit false blocks configured status", kind: clientcontract.IdempotencySafe, retryable: false, statuses: []int{503}, wantCalls: 1},
		{name: "explicit true retries safe call", kind: clientcontract.IdempotencySafe, retryable: true, statuses: []int{}, wantCalls: 2, wantOutput: true},
		{name: "explicit true does not replay unsafe call", kind: clientcontract.IdempotencyNonIdempotent, retryable: true, statuses: []int{}, wantCalls: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				if calls.Add(1) == 1 {
					writer.Header().Set("Content-Type", "application/json")
					writer.WriteHeader(http.StatusServiceUnavailable)
					_, _ = writer.Write([]byte(`{"code":"unavailable","error":"Unavailable","message":"try later"}`))
					return
				}
				writer.Header().Set("Content-Type", "application/json")
				_, _ = writer.Write([]byte(`{"value":"ok"}`))
			}))
			defer server.Close()
			operation := testOperation(security, clientcontract.Idempotency{Kind: test.kind})
			operation.Contract.Errors = []clientcontract.DeclaredError{{Status: 503, Code: string(perrors.CodeUnavailable), Retryable: boolPointer(test.retryable)}}
			operation.Contract.Resilience = &clientcontract.ResiliencePolicy{Retry: &clientcontract.RetryPolicy{MaxAttempts: intPointer(2), Statuses: test.statuses}}
			bound := boundTestClient(t, server.URL, descriptor, ServiceBinding{})
			output, err := Call[struct {
				Value string `json:"value"`
			}](t.Context(), bound, &Request{Path: "/items"}, operation)
			if test.wantOutput {
				if err != nil || output.Value != "ok" {
					t.Fatalf("output = %#v, %v", output, err)
				}
			} else if err == nil {
				t.Fatal("expected remote error")
			}
			if calls.Load() != test.wantCalls {
				t.Fatalf("calls = %d, want %d", calls.Load(), test.wantCalls)
			}
		})
	}
}

var _ io.ReadCloser = failingResponseBody{}

func TestCircuitIgnoresLocalCredentialFailureAndCountsServiceFailure(t *testing.T) {
	var serviceCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		serviceCalls.Add(1)
		writer.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	descriptor := testDescriptor(map[string]clientcontract.CredentialProfile{"service": {Kind: clientcontract.CredentialServiceToken}})
	operation := testOperation(clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{AllOf: []clientcontract.SecurityRequirement{{Profile: "service"}}}}}, clientcontract.Idempotency{Kind: clientcontract.IdempotencySafe})
	operation.Contract.Errors = []clientcontract.DeclaredError{{Status: 503, Code: "errors.unavailable"}}
	operation.Contract.Resilience = &clientcontract.ResiliencePolicy{
		Retry:   &clientcontract.RetryPolicy{MaxAttempts: intPointer(1)},
		Circuit: &clientcontract.CircuitPolicy{FailureThreshold: intPointer(1), ResetTimeoutMs: intPointer(60_000)},
	}
	client := boundTestClient(t, server.URL, descriptor, ServiceBinding{})
	for range 6 {
		if _, err := client.DoOperation(t.Context(), &Request{Path: "/items"}, operation); err == nil || perrors.Is(err, CodeCircuitOpen) {
			t.Fatalf("local config affected circuit: %v", err)
		}
	}
	client.service.binding.Credentials = map[string]CredentialBinding{"service": {Provider: freshSource("token", nil)}}
	if _, err := client.DoOperation(t.Context(), &Request{Path: "/items"}, operation); err == nil {
		t.Fatal("service failure expected")
	}
	if _, err := client.DoOperation(t.Context(), &Request{Path: "/items"}, operation); !perrors.Is(err, CodeCircuitOpen) {
		t.Fatalf("open circuit error = %v", err)
	}
	if serviceCalls.Load() != 1 {
		t.Fatalf("service calls = %d", serviceCalls.Load())
	}
}

type recordingServiceTelemetry struct {
	mu       sync.Mutex
	calls    []ServiceCallResult
	attempts []ServiceAttemptResult
}

func (telemetry *recordingServiceTelemetry) StartServiceCall(ctx context.Context, _ ServiceCallInfo) (context.Context, func(ServiceCallResult)) {
	return ctx, func(result ServiceCallResult) {
		telemetry.mu.Lock()
		defer telemetry.mu.Unlock()
		telemetry.calls = append(telemetry.calls, result)
	}
}

func (telemetry *recordingServiceTelemetry) StartServiceAttempt(ctx context.Context, _ ServiceAttemptInfo) (context.Context, func(ServiceAttemptResult)) {
	return ctx, func(result ServiceAttemptResult) {
		telemetry.mu.Lock()
		defer telemetry.mu.Unlock()
		telemetry.attempts = append(telemetry.attempts, result)
	}
}

func (*recordingServiceTelemetry) InjectServiceContext(_ context.Context, headers http.Header) {
	headers.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
}

func TestGeneratedOperationAutomaticallyObservesAuthAttemptsFinalResultAndInjectsContext(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "service-call-telemetry", "a-generated-call-reports-its-attempts-and-final-result")
	spectest.Proves(t, "go/typed-service-clients", "service-call-telemetry", "the-outgoing-request-carries-the-injected-trace-context")
	observer := &recordingServiceTelemetry{}
	restore := InstallServiceCallTelemetry(observer)
	defer restore()

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("traceparent") == "" {
			t.Error("generated call omitted telemetry context")
		}
		writer.Header().Set("Content-Type", "application/json")
		if calls.Add(1) == 1 {
			writer.WriteHeader(http.StatusServiceUnavailable)
			_, _ = writer.Write([]byte(`{"code":"unavailable"}`))
			return
		}
		_, _ = writer.Write([]byte(`{"value":"ok"}`))
	}))
	defer server.Close()
	descriptor := testDescriptor(map[string]clientcontract.CredentialProfile{
		"service": {Kind: clientcontract.CredentialServiceToken},
	})
	operation := testOperation(clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{AllOf: []clientcontract.SecurityRequirement{{Profile: "service"}}}}}, clientcontract.Idempotency{Kind: clientcontract.IdempotencySafe})
	operation.Contract.Errors = []clientcontract.DeclaredError{{Status: http.StatusServiceUnavailable, Code: string(perrors.CodeUnavailable), Retryable: boolPointer(true)}}
	operation.Contract.Resilience = &clientcontract.ResiliencePolicy{Retry: &clientcontract.RetryPolicy{MaxAttempts: intPointer(2)}}
	bound := boundTestClient(t, server.URL, descriptor, ServiceBinding{Credentials: map[string]CredentialBinding{
		"service": {Provider: freshSource("service-token", nil)},
	}})
	output, err := Call[struct {
		Value string `json:"value"`
	}](t.Context(), bound, &Request{Path: "/items"}, operation)
	if err != nil || output.Value != "ok" {
		t.Fatalf("generated call output = %#v, %v", output, err)
	}
	observer.mu.Lock()
	defer observer.mu.Unlock()
	if len(observer.calls) != 1 || observer.calls[0].Attempts != 2 || observer.calls[0].StatusCode != http.StatusOK || observer.calls[0].Code != "" {
		t.Fatalf("call results = %#v", observer.calls)
	}
	if len(observer.attempts) != 2 || observer.attempts[0].StatusCode != http.StatusServiceUnavailable || observer.attempts[0].Code != string(perrors.CodeUnavailable) || observer.attempts[1].StatusCode != http.StatusOK {
		t.Fatalf("attempt results = %#v", observer.attempts)
	}
	for _, attempt := range observer.attempts {
		if attempt.AuthDuration < 0 {
			t.Fatalf("negative auth duration: %s", attempt.AuthDuration)
		}
	}
}

func TestGeneratedOperationReportsStrictResponseFailuresBeforeFinalTelemetry(t *testing.T) {
	descriptor := testDescriptor(map[string]clientcontract.CredentialProfile{})
	security := clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{AllOf: []clientcontract.SecurityRequirement{}}}}
	for _, test := range []struct {
		name        string
		contentType string
		body        string
		void        bool
	}{
		{name: "invalid schema", contentType: "application/json", body: `{}`},
		{name: "wrong media type", contentType: "text/plain", body: `{"value":"ok"}`},
		{name: "unexpected void body", contentType: "application/json", body: `{"value":"unexpected"}`, void: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			observer := &recordingServiceTelemetry{}
			restore := InstallServiceCallTelemetry(observer)
			defer restore()
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				writer.Header().Set("Content-Type", test.contentType)
				_, _ = writer.Write([]byte(test.body))
			}))
			defer server.Close()
			operation := testOperation(security, clientcontract.Idempotency{Kind: clientcontract.IdempotencySafe})
			if test.void {
				operation.Successes = []OperationSuccess{{Status: http.StatusOK}}
			}
			bound := boundTestClient(t, server.URL, descriptor, ServiceBinding{})
			var err error
			if test.void {
				var response *Response
				response, err = bound.DoOperation(t.Context(), &Request{Path: "/items"}, operation)
				if err == nil {
					err = checkVoidResponse(response, operation)
				}
			} else {
				_, err = Call[struct {
					Value string `json:"value"`
				}](t.Context(), bound, &Request{Path: "/items"}, operation)
			}
			if err == nil || !perrors.Is(err, CodeClientResponse) || calls.Load() != 1 {
				t.Fatalf("strict failure calls=%d error=%v", calls.Load(), err)
			}
			observer.mu.Lock()
			defer observer.mu.Unlock()
			if len(observer.calls) != 1 || observer.calls[0].Code != string(CodeClientResponse) {
				t.Fatalf("final telemetry = %#v", observer.calls)
			}
			if len(observer.attempts) != 1 || observer.attempts[0].StatusCode != http.StatusOK || observer.attempts[0].Code != string(CodeClientResponse) {
				t.Fatalf("attempt telemetry = %#v", observer.attempts)
			}
		})
	}
}

// futureIDToken is an unsigned JWT-shaped token whose exp claim is one hour
// ahead, the shape fetchGCPIDToken accepts from the metadata server.
func futureIDToken() string {
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, time.Now().Add(time.Hour).Unix())))
	return "header." + payload + ".signature"
}

// fakeGCPMetadataServer answers the identity endpoint with futureIDToken and
// records the audience of every request.
func fakeGCPMetadataServer(t *testing.T, audiences *[]string, mu *sync.Mutex) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != gcpIdentityPath || request.Header.Get("Metadata-Flavor") != "Google" {
			t.Errorf("metadata request = %s %v", request.URL.Path, request.Header)
		}
		mu.Lock()
		*audiences = append(*audiences, request.URL.Query().Get("audience"))
		mu.Unlock()
		_, _ = writer.Write([]byte(futureIDToken()))
	}))
	t.Cleanup(server.Close)
	return server
}

// audienceDescriptor declares an audience on the contract and on the profile so
// a test can prove neither reaches the token source.
func audienceDescriptor() ServiceDescriptor {
	descriptor := testDescriptor(map[string]clientcontract.CredentialProfile{
		"service": {Kind: clientcontract.CredentialServiceToken, Audience: "urn:inventory:profile"},
	})
	descriptor.Contract.Service.Audience = "urn:inventory:contract"
	return descriptor
}

func TestGCPIDTokenAudienceDefaultsToTheBindingURL(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "credential-audience", "a-gcp-id-token-is-requested-for-the-binding-url-whatever-the-contract-says")
	var mu sync.Mutex
	var audiences []string
	metadata := fakeGCPMetadataServer(t, &audiences, &mu)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !strings.HasPrefix(request.Header.Get("Authorization"), "Bearer header.") {
			t.Errorf("authorization = %q", request.Header.Get("Authorization"))
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"value":"ok"}`))
	}))
	defer server.Close()
	operation := testOperation(clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{AllOf: []clientcontract.SecurityRequirement{{Profile: "service"}}}}}, clientcontract.Idempotency{Kind: clientcontract.IdempotencyIdempotent})
	// A trailing slash in the configured URL is canonicalized away: the
	// audience is the base URL the client itself calls, byte for byte.
	client := boundTestClient(t, server.URL+"/", audienceDescriptor(), ServiceBinding{Credentials: map[string]CredentialBinding{
		"service": {Source: CredentialSourceGCPIDToken, MetadataURL: metadata.URL + gcpIdentityPath},
	}})
	output, err := Call[struct {
		Value string `json:"value"`
	}](t.Context(), client, &Request{Method: http.MethodPut, Path: "/items", Body: []byte(`{"value":"input"}`)}, operation)
	if err != nil || output.Value != "ok" {
		t.Fatalf("Call output=%#v error=%v", output, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(audiences) != 1 || audiences[0] != server.URL {
		t.Fatalf("ID token audiences = %q, want [%q]", audiences, server.URL)
	}
}

func TestBindingAudienceWinsOverProfileAndContractAudience(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "credential-audience", "a-binding-audience-wins-over-the-profile-and-contract-audience")
	var mu sync.Mutex
	var idTokenAudiences []string
	metadata := fakeGCPMetadataServer(t, &idTokenAudiences, &mu)
	var oauthAudiences []string
	tokenEndpoint := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if err := request.ParseForm(); err != nil {
			t.Errorf("parse token form: %v", err)
		}
		mu.Lock()
		oauthAudiences = append(oauthAudiences, request.PostForm.Get("audience"))
		mu.Unlock()
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"access_token":"oauth-token","expires_in":3600}`))
	}))
	defer tokenEndpoint.Close()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"value":"ok"}`))
	}))
	defer server.Close()
	operation := testOperation(clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{AllOf: []clientcontract.SecurityRequirement{{Profile: "service"}}}}}, clientcontract.Idempotency{Kind: clientcontract.IdempotencyIdempotent})
	tests := []struct {
		name      string
		binding   CredentialBinding
		audiences *[]string
	}{
		{"gcp-id-token", CredentialBinding{Source: CredentialSourceGCPIDToken, Audience: "foo", MetadataURL: metadata.URL + gcpIdentityPath}, &idTokenAudiences},
		{"oauth-client-credentials", CredentialBinding{Source: CredentialSourceOAuthClientCredentials, Audience: "foo", TokenURL: tokenEndpoint.URL, ClientID: "consumer", ClientSecret: "secret"}, &oauthAudiences},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := boundTestClient(t, server.URL, audienceDescriptor(), ServiceBinding{Credentials: map[string]CredentialBinding{"service": test.binding}})
			if _, err := Call[struct {
				Value string `json:"value"`
			}](t.Context(), client, &Request{Method: http.MethodPut, Path: "/items", Body: []byte(`{"value":"input"}`)}, operation); err != nil {
				t.Fatalf("Call error=%v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if got := *test.audiences; len(got) != 1 || got[0] != "foo" {
				t.Fatalf("%s audiences = %q, want [\"foo\"]", test.name, got)
			}
		})
	}
}

func TestOAuthAudienceStillFallsBackToTheProfileThenTheContract(t *testing.T) {
	var mu sync.Mutex
	var audiences []string
	tokenEndpoint := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_ = request.ParseForm()
		mu.Lock()
		audiences = append(audiences, request.PostForm.Get("audience"))
		mu.Unlock()
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"access_token":"oauth-token","expires_in":3600}`))
	}))
	defer tokenEndpoint.Close()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"value":"ok"}`))
	}))
	defer server.Close()
	operation := testOperation(clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{AllOf: []clientcontract.SecurityRequirement{{Profile: "service"}}}}}, clientcontract.Idempotency{Kind: clientcontract.IdempotencyIdempotent})
	binding := ServiceBinding{Credentials: map[string]CredentialBinding{
		"service": {Source: CredentialSourceOAuthClientCredentials, TokenURL: tokenEndpoint.URL, ClientID: "consumer", ClientSecret: "secret"},
	}}
	withProfile := audienceDescriptor()
	withoutProfile := audienceDescriptor()
	withoutProfile.Contract.Credentials["service"] = clientcontract.CredentialProfile{Kind: clientcontract.CredentialServiceToken}
	for _, descriptor := range []ServiceDescriptor{withProfile, withoutProfile} {
		client := boundTestClient(t, server.URL, descriptor, binding)
		if _, err := Call[struct {
			Value string `json:"value"`
		}](t.Context(), client, &Request{Method: http.MethodPut, Path: "/items", Body: []byte(`{"value":"input"}`)}, operation); err != nil {
			t.Fatalf("Call error=%v", err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"urn:inventory:profile", "urn:inventory:contract"}; len(audiences) != 2 || audiences[0] != want[0] || audiences[1] != want[1] {
		t.Fatalf("OAuth audiences = %q, want %q", audiences, want)
	}
}

// TestServiceBindingCarriesTheProviderMessageOnlyWhenItOptsIn proves the
// CarryRemoteMessage option reaches the live decode path from the binding a
// deployment declares, not just from decodeRemoteError's argument: the same
// provider answer yields no prose on a default binding and the provider's own
// text on a binding that sets CarryRemoteMessage.
func TestServiceBindingCarriesTheProviderMessageOnlyWhenItOptsIn(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusNotFound)
		_, _ = writer.Write([]byte(`{"code":"not_found","error":"Not Found","message":"widget 4f0c8f4e does not exist"}`))
	}))
	defer server.Close()
	operation := testOperation(
		clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{AllOf: []clientcontract.SecurityRequirement{}}}},
		clientcontract.Idempotency{Kind: clientcontract.IdempotencyNonIdempotent},
	)
	operation.Contract.Errors = []clientcontract.DeclaredError{{Status: http.StatusNotFound, Code: "not_found"}}

	for _, test := range []struct {
		name    string
		binding ServiceBinding
		want    string
	}{
		{name: "default binding", binding: ServiceBinding{}, want: ""},
		{name: "opted-in binding", binding: ServiceBinding{CarryRemoteMessage: true}, want: "widget 4f0c8f4e does not exist"},
	} {
		t.Run(test.name, func(t *testing.T) {
			bound := boundTestClient(t, server.URL, testDescriptor(map[string]clientcontract.CredentialProfile{}), test.binding)
			_, err := Call[struct {
				Value string `json:"value"`
			}](t.Context(), bound, &Request{Method: http.MethodPost, Path: "/items"}, operation)
			var remote *RemoteError
			if !stderrors.As(err, &remote) || remote.Code() != "not_found" {
				t.Fatalf("error = %T %v", err, err)
			}
			if remote.Message != test.want {
				t.Fatalf("message = %q, want %q", remote.Message, test.want)
			}
		})
	}
}
