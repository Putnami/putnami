package client

import (
	"context"
	stderrors "errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	perrors "go.putnami.dev/errors"
	clientcontract "go.putnami.dev/protocol/clientcontract"
	"go.putnami.dev/protocol/features/spectest"
)

func retryAfterResponse(status int, value string) *Response {
	headers := http.Header{}
	if value != "" {
		headers.Set("Retry-After", value)
	}
	return &Response{StatusCode: status, Headers: headers}
}

func TestRetryAfterIsReadInSecondsAndAsAnHTTPDate(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "retry-after-budget", "retry-after-is-read-in-seconds-and-as-an-http-date")
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	for name, test := range map[string]struct {
		response *Response
		want     time.Duration
		ok       bool
	}{
		"503 delta seconds":    {retryAfterResponse(http.StatusServiceUnavailable, "120"), 120 * time.Second, true},
		"429 delta seconds":    {retryAfterResponse(http.StatusTooManyRequests, "3"), 3 * time.Second, true},
		"429 zero seconds":     {retryAfterResponse(http.StatusTooManyRequests, "0"), 0, true},
		"503 http date":        {retryAfterResponse(http.StatusServiceUnavailable, now.Add(45*time.Second).Format(http.TimeFormat)), 45 * time.Second, true},
		"503 past http date":   {retryAfterResponse(http.StatusServiceUnavailable, now.Add(-time.Minute).Format(http.TimeFormat)), 0, true},
		"503 negative seconds": {retryAfterResponse(http.StatusServiceUnavailable, "-3"), 0, false},
		"503 unparsable":       {retryAfterResponse(http.StatusServiceUnavailable, "soon"), 0, false},
		"503 without header":   {retryAfterResponse(http.StatusServiceUnavailable, ""), 0, false},
		"500 is not advisory":  {retryAfterResponse(http.StatusInternalServerError, "5"), 0, false},
		"200 is not advisory":  {retryAfterResponse(http.StatusOK, "5"), 0, false},
		"absent response":      {nil, 0, false},
	} {
		t.Run(name, func(t *testing.T) {
			got, ok := retryAfterDelay(test.response, now)
			if ok != test.ok || got != test.want {
				t.Fatalf("retryAfterDelay = %v, %v; want %v, %v", got, ok, test.want, test.ok)
			}
		})
	}
}

func TestNextRetryDelayIsCappedByTheRemainingBudget(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "retry-after-budget", "a-wait-longer-than-the-remaining-budget-stops-the-retries")
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	policy := effectivePolicy{backoffBase: 200 * time.Millisecond, backoffMax: 5 * time.Second}

	if _, ok := nextRetryDelay(retryAfterResponse(http.StatusServiceUnavailable, "120"), 1, policy, 300*time.Millisecond, now); ok {
		t.Fatal("a 120s advisory was accepted inside a 300ms budget")
	}
	if _, ok := nextRetryDelay(retryAfterResponse(http.StatusTooManyRequests, "0"), 1, policy, 300*time.Millisecond, now); !ok {
		t.Fatal("a zero-second advisory was refused inside a 300ms budget")
	}
	// The advisory replaces the computed backoff rather than adding to it.
	delay, ok := nextRetryDelay(retryAfterResponse(http.StatusTooManyRequests, "1"), 4, policy, 30*time.Second, now)
	if !ok || delay != time.Second {
		t.Fatalf("advisory delay = %v, %v; want 1s", delay, ok)
	}
	// Without an advisory the exponential backoff applies, bounded by the policy.
	delay, ok = nextRetryDelay(retryAfterResponse(http.StatusInternalServerError, ""), 1, policy, 30*time.Second, now)
	if !ok || delay < 200*time.Millisecond || delay > 250*time.Millisecond {
		t.Fatalf("backoff delay = %v, %v; want 200ms plus up to 25%% jitter", delay, ok)
	}
	if _, ok := nextRetryDelay(nil, 1, policy, 0, now); ok {
		t.Fatal("an exhausted budget still planned a retry")
	}
	if _, ok := nextRetryDelay(nil, 1, policy, 100*time.Millisecond, now); ok {
		t.Fatal("a backoff longer than the remaining budget still planned a retry")
	}
}

func TestResolvePolicyBoundsBackoffByTheDeclaredResiliencePolicy(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "retry-after-budget", "backoff-is-bounded-by-the-declared-policy")
	undeclared := resolvePolicy(nil, nil, clientcontract.IdempotencySafe)
	if undeclared.backoffBase != defaultBackoffBase || undeclared.backoffMax != defaultBackoffMax {
		t.Fatalf("undeclared backoff = %v/%v", undeclared.backoffBase, undeclared.backoffMax)
	}
	short := resolvePolicy(nil, &clientcontract.ResiliencePolicy{TimeoutMs: intPointer(500)}, clientcontract.IdempotencySafe)
	if short.backoffMax != 500*time.Millisecond || short.backoffBase != defaultBackoffBase {
		t.Fatalf("500ms budget backoff = %v/%v", short.backoffBase, short.backoffMax)
	}
	tiny := resolvePolicy(nil, &clientcontract.ResiliencePolicy{TimeoutMs: intPointer(100)}, clientcontract.IdempotencySafe)
	if tiny.backoffMax != 100*time.Millisecond || tiny.backoffBase != 100*time.Millisecond {
		t.Fatalf("100ms budget backoff = %v/%v", tiny.backoffBase, tiny.backoffMax)
	}
	perAttempt := resolvePolicy(
		&clientcontract.ResiliencePolicy{TimeoutMs: intPointer(10000)},
		&clientcontract.ResiliencePolicy{AttemptTimeoutMs: intPointer(50)},
		clientcontract.IdempotencySafe,
	)
	if perAttempt.backoffBase != 50*time.Millisecond || perAttempt.backoffMax != defaultBackoffMax {
		t.Fatalf("per-attempt backoff = %v/%v", perAttempt.backoffBase, perAttempt.backoffMax)
	}
}

// retryAfterOperation declares a retryable 503 and 429 so the runtime is free to
// retry, leaving the Retry-After advisory as the only thing that stops it.
func retryAfterOperation(timeoutMs int) Operation {
	operation := testOperation(
		clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{AllOf: []clientcontract.SecurityRequirement{}}}},
		clientcontract.Idempotency{Kind: clientcontract.IdempotencySafe},
	)
	operation.Contract.Errors = []clientcontract.DeclaredError{
		{Status: http.StatusServiceUnavailable, Code: "errors.unavailable", Retryable: boolPointer(true)},
		{Status: http.StatusTooManyRequests, Code: "errors.throttled", Retryable: boolPointer(true)},
	}
	operation.Contract.Resilience = &clientcontract.ResiliencePolicy{
		TimeoutMs: intPointer(timeoutMs),
		Retry:     &clientcontract.RetryPolicy{MaxAttempts: intPointer(3)},
	}
	return operation
}

func TestRetryAfterBeyondTheBudgetReturnsTheTypedErrorInsteadOfWaiting(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "retry-after-budget", "a-wait-longer-than-the-remaining-budget-stops-the-retries")
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writer.Header().Set("Retry-After", "120")
		writer.WriteHeader(http.StatusServiceUnavailable)
		_, _ = writer.Write([]byte(`{"code":"errors.unavailable"}`))
	}))
	defer server.Close()

	client := boundTestClient(t, server.URL, testDescriptor(map[string]clientcontract.CredentialProfile{}), ServiceBinding{})
	operation := retryAfterOperation(400)
	started := time.Now()
	_, err := client.DoOperation(context.Background(), &Request{Method: http.MethodGet, Path: "/items"}, operation)
	elapsed := time.Since(started)

	var remote *RemoteError
	if !stderrors.As(err, &remote) || remote.RemoteCode != "errors.unavailable" {
		t.Fatalf("call error = %T %v", err, err)
	}
	if remote.ServiceID != "inventory" || remote.OperationID != "putItem" {
		t.Fatalf("remote identity = %q/%q", remote.ServiceID, remote.OperationID)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("attempts = %d, want 1: the 120s advisory does not fit a 400ms budget", got)
	}
	if elapsed > 300*time.Millisecond {
		t.Fatalf("call waited %v; it must return without burning the budget", elapsed)
	}
}

func TestRetryAfterWithinTheBudgetIsHonored(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "retry-after-budget", "retry-after-is-read-in-seconds-and-as-an-http-date")
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			writer.Header().Set("Retry-After", "0")
			writer.WriteHeader(http.StatusTooManyRequests)
			_, _ = writer.Write([]byte(`{"code":"errors.throttled"}`))
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"value":"ok"}`))
	}))
	defer server.Close()

	client := boundTestClient(t, server.URL, testDescriptor(map[string]clientcontract.CredentialProfile{}), ServiceBinding{})
	response, err := client.DoOperation(context.Background(), &Request{Method: http.MethodGet, Path: "/items"}, retryAfterOperation(5000))
	if err != nil {
		t.Fatalf("call error = %v", err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("attempts = %d, want 2", got)
	}
}

func TestDescriptorOnlyClientNeverSendsADeclaredCredentialCallAnonymously(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "credential-alternatives", "an-operation-that-declares-credentials-is-never-sent-anonymously")
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"value":"ok"}`))
	}))
	defer server.Close()

	base, err := NewBuilder().BaseURL(server.URL).Build()
	if err != nil {
		t.Fatal(err)
	}
	descriptorOnly := WithGeneratedServiceDescriptor(base, testDescriptor(map[string]clientcontract.CredentialProfile{
		"service": {Kind: clientcontract.CredentialServiceToken},
	}))

	for name, security := range map[string]clientcontract.Security{
		"declared service token": {Alternatives: []clientcontract.SecurityAlternative{
			{AllOf: []clientcontract.SecurityRequirement{{Profile: "service"}}},
		}},
		"no alternative at all": {Alternatives: []clientcontract.SecurityAlternative{}},
	} {
		t.Run(name, func(t *testing.T) {
			calls.Store(0)
			operation := testOperation(security, clientcontract.Idempotency{Kind: clientcontract.IdempotencySafe})
			_, err := descriptorOnly.DoOperation(context.Background(), &Request{Method: http.MethodGet, Path: "/items"}, operation)
			if !perrors.Is(err, CodeClientCredential) {
				t.Fatalf("call error = %T %v, want client.credential", err, err)
			}
			if got := calls.Load(); got != 0 {
				t.Fatalf("service received %d anonymous request(s)", got)
			}
		})
	}

	// The provider can still declare that an operation needs no credential, and
	// that declaration is the only thing that lets the call through.
	anonymous := testOperation(
		clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{AllOf: []clientcontract.SecurityRequirement{}}}},
		clientcontract.Idempotency{Kind: clientcontract.IdempotencySafe},
	)
	calls.Store(0)
	if _, err := descriptorOnly.DoOperation(context.Background(), &Request{Method: http.MethodGet, Path: "/items"}, anonymous); err != nil {
		t.Fatalf("declared anonymous operation = %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("declared anonymous operation calls = %d, want 1", got)
	}
}

func TestBoundClientRefusesAnUnsatisfiableCredentialAlternativeBeforeDispatch(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "credential-alternatives", "an-operation-that-declares-credentials-is-never-sent-anonymously")
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"value":"ok"}`))
	}))
	defer server.Close()

	// The profile is declared by the provider but the binding supplies nothing
	// for it, so no alternative is satisfiable.
	client := boundTestClient(t, server.URL, testDescriptor(map[string]clientcontract.CredentialProfile{
		"service": {Kind: clientcontract.CredentialServiceToken},
	}), ServiceBinding{})
	operation := testOperation(clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{
		{AllOf: []clientcontract.SecurityRequirement{{Profile: "service"}}},
	}}, clientcontract.Idempotency{Kind: clientcontract.IdempotencySafe})

	_, err := client.DoOperation(context.Background(), &Request{Method: http.MethodGet, Path: "/items"}, operation)
	if !perrors.Is(err, CodeClientCredential) {
		t.Fatalf("call error = %T %v, want client.credential", err, err)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("service received %d unauthenticated request(s)", got)
	}
}
