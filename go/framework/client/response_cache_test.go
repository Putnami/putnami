package client

import (
	"context"
	"encoding/base64"
	"encoding/json"
	stderrors "errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	perrors "go.putnami.dev/errors"
	clientcontract "go.putnami.dev/protocol/clientcontract"
	"go.putnami.dev/protocol/features/spectest"
)

const (
	responseCacheRequirement        = "response-cache"
	responseCacheControlRequirement = "response-cache-bypass-and-field-invalidation"
)

// cacheClock is the injectable cache clock: a test moves it across the fresh
// and stale windows instead of sleeping through them.
type cacheClock struct {
	mu  sync.Mutex
	now time.Time
}

func newCacheClock() *cacheClock {
	return &cacheClock{now: time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)}
}

func (clock *cacheClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *cacheClock) Advance(duration time.Duration) {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.now = clock.now.Add(duration)
}

// cachedAccountOperation is the operation the acceptance scenarios exercise:
// a safe unary read declaring fresh 5 s / stale 5 min, keyed on its path.
func cachedAccountOperation(cache *clientcontract.CachePolicy, resilience *clientcontract.ResiliencePolicy) Operation {
	if resilience == nil {
		resilience = &clientcontract.ResiliencePolicy{}
	}
	resilience.Cache = cache
	return Operation{
		ID: "getAccount",
		Contract: clientcontract.OperationV1{
			Stream:      clientcontract.StreamUnary,
			Transports:  []clientcontract.Transport{{Protocol: clientcontract.TransportRESTJSON, Path: "/accounts/{id}", Encoding: clientcontract.EncodingJSON}},
			Security:    clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{AllOf: []clientcontract.SecurityRequirement{}}}},
			Errors:      []clientcontract.DeclaredError{{Status: http.StatusNotFound, Code: "not_found"}},
			Idempotency: clientcontract.Idempotency{Kind: clientcontract.IdempotencySafe},
			Resilience:  resilience,
		},
		Successes: []OperationSuccess{{Status: 200, Content: []OperationContent{{MediaType: "application/json", Schema: &clientcontract.Schema{
			Type: "object", Properties: map[string]clientcontract.Schema{"value": {Type: "string"}}, Required: []string{"value"}, AdditionalProperties: additionalForbidden(),
		}}}}},
	}
}

func freshStalePolicy() *clientcontract.CachePolicy {
	return &clientcontract.CachePolicy{FreshMs: 5000, StaleMs: intPointer(300000)}
}

// cachedClient binds a client whose registry runs on clock.
func cachedClient(t *testing.T, endpoint string, clock *cacheClock) *Client {
	t.Helper()
	client := boundTestClient(t, endpoint, testDescriptor(map[string]clientcontract.CredentialProfile{}), ServiceBinding{})
	client.service.registry.responses = newResponseCachesWithClock(clock.Now)
	return client
}

type account struct {
	Value string `json:"value"`
}

func getAccount(ctx context.Context, client *Client, operation Operation, id string) (account, error) {
	request := &Request{Method: http.MethodGet, Path: "/accounts/" + url.PathEscape(id), QueryValues: url.Values{}, Headers: http.Header{}}
	return CallOperation[account](ctx, client, &OperationCall{Request: request, PathParams: map[string]string{"id": id}}, operation)
}

// countingProvider answers every call with the path it was asked and counts
// the calls; a held provider keeps each answer until release closes the hold.
type countingProvider struct {
	calls  atomic.Int32
	status atomic.Int32
	mu     sync.Mutex
	hold   chan struct{}
}

func heldProvider() *countingProvider {
	return &countingProvider{hold: make(chan struct{})}
}

func (provider *countingProvider) holdAnswers() {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	provider.hold = make(chan struct{})
}

func (provider *countingProvider) release() {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if provider.hold != nil {
		close(provider.hold)
		provider.hold = nil
	}
}

func (provider *countingProvider) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	provider.calls.Add(1)
	provider.mu.Lock()
	hold := provider.hold
	provider.mu.Unlock()
	if hold != nil {
		<-hold
	}
	writer.Header().Set("Content-Type", "application/json")
	if status := int(provider.status.Load()); status != 0 {
		writer.WriteHeader(status)
		_, _ = writer.Write([]byte(`{"code":"unavailable","error":"Service Unavailable","message":"down"}`))
		return
	}
	_ = json.NewEncoder(writer).Encode(account{Value: request.URL.Path})
}

func waitUntil(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition was not reached")
		}
		runtime.Gosched()
	}
}

func TestOneUpstreamCallPerKeyAndIdentityPerFreshWindowUnderConcurrentLoad(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", responseCacheRequirement, "one-upstream-call-per-key-and-identity-per-fresh-window-under-concurrent-load")
	provider := heldProvider()
	server := httptest.NewServer(provider)
	defer server.Close()
	clock := newCacheClock()
	client := cachedClient(t, server.URL, clock)
	operation := cachedAccountOperation(freshStalePolicy(), nil)

	const callers = 50
	var wg, started sync.WaitGroup
	results := make(chan error, callers)
	for range callers {
		wg.Add(1)
		started.Add(1)
		go func() {
			defer wg.Done()
			started.Done()
			got, err := getAccount(t.Context(), client, operation, "a1")
			if err == nil && got.Value != "/accounts/a1" {
				t.Errorf("answer = %+v", got)
			}
			results <- err
		}()
	}
	// The provider holds its one answer until every caller is running: each
	// either waits on the call in flight or, arriving after it landed, reads
	// the fresh answer. Without singleflight the callers already past their
	// lookup would each go upstream.
	started.Wait()
	waitUntil(t, func() bool { return provider.calls.Load() >= 1 })
	provider.release()
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatalf("concurrent call failed: %v", err)
		}
	}
	if got := provider.calls.Load(); got != 1 {
		t.Fatalf("upstream calls for %d concurrent callers = %d, want 1", callers, got)
	}
	// The per-operation work was resolved once, on the first call, and every
	// later call reused it.
	if plans := len(client.service.cachePlans); plans != 1 {
		t.Fatalf("resolved cache plans = %d, want 1", plans)
	}

	// Inside the fresh window every call is answered from memory.
	clock.Advance(4999 * time.Millisecond)
	if _, err := getAccount(t.Context(), client, operation, "a1"); err != nil || provider.calls.Load() != 1 {
		t.Fatalf("fresh call: err=%v upstream=%d", err, provider.calls.Load())
	}
	// Another key is another entry.
	if _, err := getAccount(t.Context(), client, operation, "a2"); err != nil || provider.calls.Load() != 2 {
		t.Fatalf("second key: err=%v upstream=%d", err, provider.calls.Load())
	}
	// Past the fresh window the next call goes upstream once more.
	clock.Advance(time.Millisecond)
	if _, err := getAccount(t.Context(), client, operation, "a1"); err != nil || provider.calls.Load() != 3 {
		t.Fatalf("expired call: err=%v upstream=%d", err, provider.calls.Load())
	}
}

type staleTelemetry struct {
	recordingServiceTelemetry
	mu    sync.Mutex
	stale []ServiceCallInfo
	ages  []time.Duration
}

func (telemetry *staleTelemetry) ServiceCacheStaleServed(_ context.Context, info ServiceCallInfo, age time.Duration) {
	telemetry.mu.Lock()
	defer telemetry.mu.Unlock()
	telemetry.stale = append(telemetry.stale, info)
	telemetry.ages = append(telemetry.ages, age)
}

func (telemetry *staleTelemetry) count() int {
	telemetry.mu.Lock()
	defer telemetry.mu.Unlock()
	return len(telemetry.stale)
}

func TestAProviderFailureInsideTheStaleWindowReturnsTheStoredAnswerAndIsCounted(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", responseCacheRequirement, "a-provider-failure-inside-the-stale-window-returns-the-stored-answer-and-is-counted")
	spectest.Proves(t, "go/typed-service-clients", responseCacheRequirement, "a-failure-after-the-stale-window-or-outside-the-provider-failure-classes-reaches-the-caller")
	observer := &staleTelemetry{}
	restore := InstallServiceCallTelemetry(observer)
	defer restore()

	provider := &countingProvider{}
	server := httptest.NewServer(provider)
	clock := newCacheClock()
	client := cachedClient(t, server.URL, clock)
	operation := cachedAccountOperation(freshStalePolicy(), &clientcontract.ResiliencePolicy{
		Retry: &clientcontract.RetryPolicy{MaxAttempts: intPointer(1)},
	})
	if _, err := getAccount(t.Context(), client, operation, "a1"); err != nil {
		t.Fatal(err)
	}

	// The provider goes down: every connection is refused.
	server.Close()
	clock.Advance(6 * time.Second)
	got, err := getAccount(t.Context(), client, operation, "a1")
	if err != nil || got.Value != "/accounts/a1" {
		t.Fatalf("inside the stale window: answer=%+v err=%v", got, err)
	}
	if observer.count() != 1 || observer.stale[0].OperationID != "getAccount" || observer.ages[0] != 6*time.Second {
		t.Fatalf("stale_served observations = %+v ages=%v", observer.stale, observer.ages)
	}
	clock.Advance(300*time.Second - 6*time.Second - time.Millisecond)
	if _, err := getAccount(t.Context(), client, operation, "a1"); err != nil {
		t.Fatalf("one millisecond before the stale bound: %v", err)
	}
	if observer.count() != 2 {
		t.Fatalf("stale_served count = %d, want 2", observer.count())
	}

	// At the stale bound the transport error reaches the caller.
	clock.Advance(time.Millisecond)
	if _, err := getAccount(t.Context(), client, operation, "a1"); !perrors.Is(err, CodeClientRequest) {
		t.Fatalf("after the stale window: err=%v, want the transport error", err)
	}
	if observer.count() != 2 {
		t.Fatalf("an unmasked failure was counted as stale: %d", observer.count())
	}
}

func TestRetryExhaustionAndAnOpenBreakerAreMaskedButADeclaredRefusalIsNot(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", responseCacheRequirement, "a-provider-failure-inside-the-stale-window-returns-the-stored-answer-and-is-counted")
	spectest.Proves(t, "go/typed-service-clients", responseCacheRequirement, "a-failure-after-the-stale-window-or-outside-the-provider-failure-classes-reaches-the-caller")
	observer := &staleTelemetry{}
	restore := InstallServiceCallTelemetry(observer)
	defer restore()

	provider := &countingProvider{}
	server := httptest.NewServer(provider)
	defer server.Close()
	clock := newCacheClock()
	client := cachedClient(t, server.URL, clock)
	operation := cachedAccountOperation(freshStalePolicy(), &clientcontract.ResiliencePolicy{
		Retry:   &clientcontract.RetryPolicy{MaxAttempts: intPointer(1), Statuses: []int{503}},
		Circuit: &clientcontract.CircuitPolicy{FailureThreshold: intPointer(1), ResetTimeoutMs: intPointer(60000)},
	})
	if _, err := getAccount(t.Context(), client, operation, "a1"); err != nil {
		t.Fatal(err)
	}
	clock.Advance(6 * time.Second)

	// A retryable 503 left after the declared attempts is retry exhaustion.
	provider.status.Store(http.StatusServiceUnavailable)
	if got, err := getAccount(t.Context(), client, operation, "a1"); err != nil || got.Value != "/accounts/a1" {
		t.Fatalf("retry exhaustion was not masked: %+v %v", got, err)
	}
	// That failure opened the breaker; the next call never reaches the provider.
	before := provider.calls.Load()
	if got, err := getAccount(t.Context(), client, operation, "a1"); err != nil || got.Value != "/accounts/a1" {
		t.Fatalf("an open breaker was not masked: %+v %v", got, err)
	}
	if provider.calls.Load() != before {
		t.Fatal("the open breaker let a call through")
	}
	if observer.count() != 2 {
		t.Fatalf("stale_served count = %d, want 2", observer.count())
	}

	// A declared refusal is an answer, not an outage: it reaches the caller
	// even with a stored answer inside the stale window.
	var gone atomic.Bool
	refusingServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if gone.Load() {
			writer.WriteHeader(http.StatusNotFound)
			_, _ = writer.Write([]byte(`{"code":"not_found","error":"Not Found","message":"gone"}`))
			return
		}
		_ = json.NewEncoder(writer).Encode(account{Value: request.URL.Path})
	}))
	defer refusingServer.Close()
	refusingClient := cachedClient(t, refusingServer.URL, clock)
	refusing := cachedAccountOperation(freshStalePolicy(), nil)
	if _, err := getAccount(t.Context(), refusingClient, refusing, "a1"); err != nil {
		t.Fatal(err)
	}
	gone.Store(true)
	clock.Advance(6 * time.Second)
	var remote *RemoteError
	if _, err := getAccount(t.Context(), refusingClient, refusing, "a1"); !asRemote(err, &remote) || remote.StatusCode != http.StatusNotFound {
		t.Fatalf("a declared refusal was masked: %v", err)
	}
	if observer.count() != 2 {
		t.Fatalf("a declared refusal was counted as stale: %d", observer.count())
	}

	// A caller that canceled is never handed a stored answer.
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := getAccount(canceled, client, operation, "a1"); !perrors.Is(err, CodeClientCanceled) {
		t.Fatalf("canceled caller: err=%v", err)
	}
}

// expiringContext is a caller context whose end the test decides: a deadline
// or an explicit cancellation, at the moment the test chooses, with no timer.
type expiringContext struct {
	context.Context
	done chan struct{}
	mu   sync.Mutex
	err  error
}

func newExpiringContext(parent context.Context) *expiringContext {
	return &expiringContext{Context: parent, done: make(chan struct{})}
}

func (ctx *expiringContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (ctx *expiringContext) Done() <-chan struct{}       { return ctx.done }

func (ctx *expiringContext) Err() error {
	ctx.mu.Lock()
	defer ctx.mu.Unlock()
	return ctx.err
}

func (ctx *expiringContext) end(err error) {
	ctx.mu.Lock()
	defer ctx.mu.Unlock()
	ctx.err = err
	close(ctx.done)
}

func TestACallerWhoseDeadlinePassesWhileTheProviderHangsGetsTheStaleAnswer(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", responseCacheRequirement, "a-caller-deadline-that-passes-while-the-provider-hangs-returns-the-stored-answer")
	observer := &staleTelemetry{}
	restore := InstallServiceCallTelemetry(observer)
	defer restore()

	provider := &countingProvider{}
	server := httptest.NewServer(provider)
	defer server.Close()
	defer provider.release()
	clock := newCacheClock()
	client := cachedClient(t, server.URL, clock)
	operation := cachedAccountOperation(freshStalePolicy(), nil)
	if _, err := getAccount(t.Context(), client, operation, "a1"); err != nil {
		t.Fatal(err)
	}

	// The provider hangs and the caller's own deadline passes first.
	clock.Advance(6 * time.Second)
	provider.holdAnswers()
	caller := newExpiringContext(t.Context())
	answer := make(chan error, 1)
	go func() {
		got, err := getAccount(caller, client, operation, "a1")
		if err == nil && got.Value != "/accounts/a1" {
			err = stderrors.New("unexpected answer " + got.Value)
		}
		answer <- err
	}()
	waitUntil(t, func() bool { return provider.calls.Load() == 2 })
	caller.end(context.DeadlineExceeded)
	if err := <-answer; err != nil {
		t.Fatalf("a caller out of time while the provider hung: %v", err)
	}
	if observer.count() != 1 {
		t.Fatalf("stale_served count = %d, want 1", observer.count())
	}

	// The shared call kept running: a second caller joins it rather than
	// calling again, and its answer lands in the cache.
	joined := make(chan error, 1)
	go func() {
		_, err := getAccount(context.Background(), client, operation, "a1")
		joined <- err
	}()
	provider.release()
	if err := <-joined; err != nil {
		t.Fatal(err)
	}
	if _, err := getAccount(t.Context(), client, operation, "a1"); err != nil || provider.calls.Load() != 2 {
		t.Fatalf("the shared call did not keep running: err=%v upstream=%d, want 2", err, provider.calls.Load())
	}

	// An explicit cancellation is the caller's answer, never a stored one.
	clock.Advance(6 * time.Second)
	provider.holdAnswers()
	canceled := newExpiringContext(t.Context())
	answer = make(chan error, 1)
	go func() {
		_, err := getAccount(canceled, client, operation, "a1")
		answer <- err
	}()
	waitUntil(t, func() bool { return provider.calls.Load() == 3 })
	canceled.end(context.Canceled)
	if err := <-answer; !perrors.Is(err, CodeClientCanceled) {
		t.Fatalf("a canceled caller: err=%v, want client.canceled", err)
	}
	if observer.count() != 1 {
		t.Fatalf("a cancellation was answered from the cache: stale_served=%d", observer.count())
	}
}

func asRemote(err error, target **RemoteError) bool {
	return stderrors.As(err, target)
}

func TestAnInvalidationSendsTheNextCallUpstream(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", responseCacheRequirement, "an-invalidation-sends-the-next-call-upstream")
	provider := &countingProvider{}
	server := httptest.NewServer(provider)
	defer server.Close()
	clock := newCacheClock()
	client := cachedClient(t, server.URL, clock)
	operation := cachedAccountOperation(freshStalePolicy(), nil)
	for _, id := range []string{"a1", "a2"} {
		if _, err := getAccount(t.Context(), client, operation, id); err != nil {
			t.Fatal(err)
		}
	}
	if dropped := client.service.registry.InvalidateResponses("inventory", `getAccount?path.id=%22a1`); dropped != 1 {
		t.Fatalf("dropped = %d, want 1", dropped)
	}
	if _, err := getAccount(t.Context(), client, operation, "a1"); err != nil || provider.calls.Load() != 3 {
		t.Fatalf("after invalidation: err=%v upstream=%d, want 3", err, provider.calls.Load())
	}
	if _, err := getAccount(t.Context(), client, operation, "a2"); err != nil || provider.calls.Load() != 3 {
		t.Fatalf("an unmatched key was invalidated: upstream=%d", provider.calls.Load())
	}
	if dropped := client.InvalidateResponses(""); dropped != 2 {
		t.Fatalf("empty prefix dropped %d, want 2", dropped)
	}
	if dropped := client.service.registry.InvalidateResponses("other-service", ""); dropped != 0 {
		t.Fatalf("another service's invalidation dropped %d", dropped)
	}

	// An invalidation that overtakes a call in flight keeps its answer out of
	// the cache: the call that follows goes upstream again.
	provider.holdAnswers()
	done := make(chan error, 1)
	go func() {
		_, err := getAccount(context.Background(), client, operation, "a3")
		done <- err
	}()
	waitUntil(t, func() bool { return provider.calls.Load() == 4 })
	client.InvalidateResponses("getAccount?path.id=%22a3")
	provider.release()
	if err := <-done; err != nil {
		t.Fatalf("overtaken call: %v", err)
	}
	if _, err := getAccount(t.Context(), client, operation, "a3"); err != nil || provider.calls.Load() != 5 {
		t.Fatalf("an overtaken answer was stored: upstream=%d, want 5", provider.calls.Load())
	}
}

func TestEntriesNeverCrossAForwardedIdentityOrOutliveTheRegistry(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", responseCacheRequirement, "entries-never-cross-a-forwarded-identity-or-outlive-the-registry")
	provider := &countingProvider{}
	server := httptest.NewServer(provider)
	defer server.Close()
	clock := newCacheClock()
	client := cachedClient(t, server.URL, clock)
	operation := cachedAccountOperation(&clientcontract.CachePolicy{FreshMs: 5000, MaxEntries: intPointer(2)}, nil)

	alice := WithForwardedUserToken(t.Context(), "alice-token")
	bob := WithForwardedUserToken(t.Context(), "bob-token")
	for _, ctx := range []context.Context{alice, bob, alice, bob} {
		if _, err := getAccount(ctx, client, operation, "shared"); err != nil {
			t.Fatal(err)
		}
	}
	if provider.calls.Load() != 2 {
		t.Fatalf("two identities made %d upstream calls, want 2", provider.calls.Load())
	}
	// The bound holds: a third slot evicts the least recently used one.
	if _, err := getAccount(t.Context(), client, operation, "shared"); err != nil {
		t.Fatal(err)
	}
	if _, err := getAccount(alice, client, operation, "shared"); err != nil || provider.calls.Load() != 4 {
		t.Fatalf("the least recently used entry survived the bound: upstream=%d", provider.calls.Load())
	}
	for _, cache := range client.service.registry.responses.caches {
		for slot := range cache.entries {
			if strings.Contains(slot, "alice-token") || strings.Contains(slot, "bob-token") {
				t.Fatalf("a forwarded token entered the cache: %q", slot)
			}
		}
	}

	if err := client.service.registry.Close(); err != nil {
		t.Fatal(err)
	}
	if len(client.service.registry.responses.caches) != 0 {
		t.Fatal("closing the registry kept cached responses")
	}
	if _, err := getAccount(alice, client, operation, "shared"); !perrors.Is(err, CodeClientClosed) {
		t.Fatalf("late call after close: %v", err)
	}
}

func TestClosingTheRegistryCancelsACachedCallInFlight(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", responseCacheRequirement, "entries-never-cross-a-forwarded-identity-or-outlive-the-registry")
	provider := heldProvider()
	server := httptest.NewServer(provider)
	defer server.Close()
	defer provider.release()
	client := cachedClient(t, server.URL, newCacheClock())
	operation := cachedAccountOperation(freshStalePolicy(), nil)
	done := make(chan error, 1)
	go func() {
		_, err := getAccount(context.Background(), client, operation, "a1")
		done <- err
	}()
	waitUntil(t, func() bool { return provider.calls.Load() == 1 })
	if err := client.service.registry.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !perrors.Is(err, CodeClientClosed) {
		t.Fatalf("in-flight cached call after close: %v", err)
	}
}

type cacheKeyVectors struct {
	Cases []struct {
		Name                 string   `json:"name"`
		OperationID          string   `json:"operationId"`
		KeyFields            []string `json:"keyFields"`
		IdempotencyKeyHeader string   `json:"idempotencyKeyHeader"`
		Request              struct {
			Path       map[string]string   `json:"path"`
			Query      map[string][]string `json:"query"`
			Headers    map[string][]string `json:"headers"`
			Body       string              `json:"body"`
			BinaryBody *string             `json:"binaryBody"`
		} `json:"request"`
		Key string `json:"key"`
	} `json:"cases"`
}

func TestTheCacheKeyMatchesTheSharedVectors(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", responseCacheRequirement, "the-cache-key-matches-the-shared-vectors")
	data, err := os.ReadFile("../../../protocols/clientcontract/fixtures/cache/keys.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors cacheKeyVectors
	if err := json.Unmarshal(data, &vectors); err != nil {
		t.Fatal(err)
	}
	if len(vectors.Cases) == 0 {
		t.Fatal("no shared cache key vectors")
	}
	for _, vector := range vectors.Cases {
		t.Run(vector.Name, func(t *testing.T) {
			headers := http.Header{}
			for name, values := range vector.Request.Headers {
				for _, value := range values {
					headers.Add(name, value)
				}
			}
			body := []byte(vector.Request.Body)
			if vector.Request.BinaryBody != nil {
				body, err = base64.StdEncoding.DecodeString(*vector.Request.BinaryBody)
				if err != nil {
					t.Fatal(err)
				}
			}
			canonical, err := canonicalRequestBody(body, vector.Request.BinaryBody != nil)
			if err != nil {
				t.Fatal(err)
			}
			got := canonicalCacheKey(vector.OperationID, vector.KeyFields, vector.IdempotencyKeyHeader,
				vector.Request.Path, url.Values(vector.Request.Query), headers, canonical)
			if got != vector.Key {
				t.Fatalf("key drift:\n got %s\nwant %s", got, vector.Key)
			}
		})
	}
}

func TestTheRuntimeImplementsExactlyThePublishedCapabilities(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", responseCacheRequirement, "the-runtime-implements-exactly-the-published-capabilities")
	implemented := clientcontract.RuntimeCapabilitiesImplementedBy(clientcontract.GeneratedLanguageGo)
	if !reflect.DeepEqual(RuntimeCapabilities(), implemented) {
		t.Fatalf("runtime capabilities = %v, contract = %v", RuntimeCapabilities(), implemented)
	}
	if !RequireRuntimeCapabilities(string(clientcontract.RuntimeCapabilityResponseCache)) {
		t.Fatal("the implemented capability was refused")
	}
	defer func() {
		if recover() == nil {
			t.Fatal("an unimplemented capability did not stop the generated package")
		}
	}()
	RequireRuntimeCapabilities("response-cache-v2")
}

func TestABypassedCallNeitherReadsNorStoresNorJoinsACallInFlight(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", responseCacheControlRequirement, "a-bypassed-call-neither-reads-nor-stores-nor-joins-a-call-in-flight")
	provider := &countingProvider{}
	server := httptest.NewServer(provider)
	defer server.Close()
	clock := newCacheClock()
	client := cachedClient(t, server.URL, clock)
	operation := cachedAccountOperation(freshStalePolicy(), &clientcontract.ResiliencePolicy{
		Retry: &clientcontract.RetryPolicy{MaxAttempts: intPointer(1), Statuses: []int{http.StatusServiceUnavailable}},
	})
	bypassed := WithoutResponseCache(t.Context())

	// No read: a fresh stored answer does not stand in for the bypassed call.
	if _, err := getAccount(t.Context(), client, operation, "a1"); err != nil {
		t.Fatal(err)
	}
	if answer, err := getAccount(bypassed, client, operation, "a1"); err != nil || answer.Value != "/accounts/a1" || provider.calls.Load() != 2 {
		t.Fatalf("bypassed call on a stored key: answer=%+v err=%v upstream=%d, want 2", answer, err, provider.calls.Load())
	}

	// No store: the bypassed answer is not kept for the next caller.
	if _, err := getAccount(bypassed, client, operation, "b1"); err != nil || provider.calls.Load() != 3 {
		t.Fatalf("bypassed call on an empty key: err=%v upstream=%d, want 3", err, provider.calls.Load())
	}
	for _, cache := range client.service.registry.responses.caches {
		for _, entry := range cache.entries {
			if strings.Contains(entry.key, "b1") {
				t.Fatalf("a bypassed answer was stored under %q", entry.key)
			}
		}
	}
	if _, err := getAccount(t.Context(), client, operation, "b1"); err != nil || provider.calls.Load() != 4 {
		t.Fatalf("the call after a bypass: err=%v upstream=%d, want 4", err, provider.calls.Load())
	}

	// No stale answer: a bypassed call is told about the outage.
	clock.Advance(6 * time.Second)
	provider.status.Store(http.StatusServiceUnavailable)
	if _, err := getAccount(bypassed, client, operation, "a1"); err == nil {
		t.Fatal("a bypassed call was answered from the stale window")
	}
	if _, err := getAccount(t.Context(), client, operation, "a1"); err != nil {
		t.Fatalf("a cached call inside the stale window: %v", err)
	}
	provider.status.Store(0)

	// No join: a bypassed call does not wait on the shared call in flight.
	clock.Advance(time.Hour)
	provider.holdAnswers()
	calls := provider.calls.Load()
	shared := make(chan error, 1)
	go func() {
		_, err := getAccount(context.Background(), client, operation, "c1")
		shared <- err
	}()
	waitUntil(t, func() bool { return provider.calls.Load() == calls+1 })
	own := make(chan error, 1)
	go func() {
		_, err := getAccount(WithoutResponseCache(context.Background()), client, operation, "c1")
		own <- err
	}()
	waitUntil(t, func() bool { return provider.calls.Load() == calls+2 })
	provider.release()
	if err := <-shared; err != nil {
		t.Fatal(err)
	}
	if err := <-own; err != nil {
		t.Fatal(err)
	}
	if responseCacheBypassed(t.Context()) || !responseCacheBypassed(bypassed) {
		t.Fatal("the bypass does not travel with exactly the context that carries it")
	}
}

// principalOperation is a cached read whose answer names the principal it is
// about, declared as the operation's invalidation field.
func principalOperation(id, path string) Operation {
	operation := cachedAccountOperation(&clientcontract.CachePolicy{FreshMs: 5000, InvalidationFields: []string{"principalId"}}, nil)
	operation.ID = id
	operation.Contract.Transports[0].Path = path
	operation.Successes[0].Content[0].Schema = &clientcontract.Schema{Type: "object", Properties: map[string]clientcontract.Schema{
		"value": {Type: "string"}, "principalId": {Type: "string"},
	}, Required: []string{"value", "principalId"}, AdditionalProperties: additionalForbidden()}
	return operation
}

// principalProvider answers every path with the principal the path belongs
// to: /accounts/a1 and /accounts/a2 and /profiles/p1 all belong to p1.
type principalProvider struct {
	countingProvider
}

func (provider *principalProvider) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	provider.calls.Add(1)
	provider.mu.Lock()
	hold := provider.hold
	provider.mu.Unlock()
	if hold != nil {
		<-hold
	}
	principal := map[string]string{"/accounts/a1": "p1", "/accounts/a2": "p1", "/accounts/a3": "p2", "/profiles/p1": "p1", "/accounts/a4": "p1"}[request.URL.Path]
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(map[string]string{"value": request.URL.Path, "principalId": principal})
}

func getPrincipal(ctx context.Context, client *Client, operation Operation, path string) error {
	request := &Request{Method: http.MethodGet, Path: path, QueryValues: url.Values{}, Headers: http.Header{}}
	_, err := CallOperation[map[string]string](ctx, client, &OperationCall{Request: request, PathParams: map[string]string{"id": path}}, operation)
	return err
}

func TestAnInvalidationByAResponseFieldDropsEveryAnswerCarryingTheValue(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", responseCacheControlRequirement, "an-invalidation-by-a-response-field-drops-every-answer-carrying-the-value")
	provider := &principalProvider{}
	server := httptest.NewServer(provider)
	defer server.Close()
	client := cachedClient(t, server.URL, newCacheClock())
	accounts := principalOperation("getAccount", "/accounts/{id}")
	profiles := principalOperation("getProfile", "/profiles/{id}")
	alice := WithForwardedUserToken(t.Context(), "alice-token")
	store := func() {
		t.Helper()
		for _, ctx := range []context.Context{t.Context(), alice} {
			for _, path := range []string{"/accounts/a1", "/accounts/a2", "/accounts/a3"} {
				if err := getPrincipal(ctx, client, accounts, path); err != nil {
					t.Fatal(err)
				}
			}
			if err := getPrincipal(ctx, client, profiles, "/profiles/p1"); err != nil {
				t.Fatal(err)
			}
		}
	}
	store()
	if provider.calls.Load() != 8 {
		t.Fatalf("upstream = %d, want 8", provider.calls.Load())
	}

	// Values compare in canonical form: the integer 1 and the string "p1 " are
	// other values, a field no operation declares tags nothing, and another
	// service's answers are its own.
	for _, miss := range []struct {
		service, field string
		value          any
	}{{"inventory", "principalId", 1}, {"inventory", "principalId", "p1 "}, {"inventory", "value", "/accounts/a1"}, {"other-service", "principalId", "p1"}} {
		if dropped, err := client.service.registry.InvalidateResponsesByField(miss.service, miss.field, miss.value); err != nil || dropped != 0 {
			t.Fatalf("%+v dropped %d (err=%v), want 0", miss, dropped, err)
		}
	}

	// Across the service's operations and for every identity: two accounts and
	// one profile, twice.
	type principal string
	if dropped, err := client.service.registry.InvalidateResponsesByField("inventory", "principalId", principal("p1")); err != nil || dropped != 6 {
		t.Fatalf("dropped %d (err=%v), want 6", dropped, err)
	}
	store()
	if provider.calls.Load() != 14 {
		t.Fatalf("after invalidation upstream = %d, want 14: only p1's answers go upstream again", provider.calls.Load())
	}
	if dropped, err := client.InvalidateResponsesByField("principalId", "p2"); err != nil || dropped != 2 {
		t.Fatalf("client-level invalidation dropped %d (err=%v), want 2", dropped, err)
	}

	// An invalidation that overtakes a call in flight keeps its answer out of
	// the cache, whatever that answer turns out to carry.
	provider.holdAnswers()
	done := make(chan error, 1)
	go func() { done <- getPrincipal(context.Background(), client, accounts, "/accounts/a4") }()
	waitUntil(t, func() bool { return provider.calls.Load() == 15 })
	if _, err := client.InvalidateResponsesByField("principalId", "nobody"); err != nil {
		t.Fatal(err)
	}
	provider.release()
	if err := <-done; err != nil {
		t.Fatalf("overtaken call: %v", err)
	}
	if err := getPrincipal(t.Context(), client, accounts, "/accounts/a4"); err != nil || provider.calls.Load() != 16 {
		t.Fatalf("an overtaken answer was stored: err=%v upstream=%d, want 16", err, provider.calls.Load())
	}

	// A value no runtime can compare is refused, never matched against nothing.
	for _, refused := range []any{1.5, nil, []string{"p1"}, map[string]string{"id": "p1"}, json.Number("1.5"), json.Number("007")} {
		if _, err := client.InvalidateResponsesByField("principalId", refused); !perrors.Is(err, CodeClientConfig) {
			t.Fatalf("value %#v: err=%v, want client.config", refused, err)
		}
	}
	for _, field := range []string{"", " principalId", "principal\x00Id"} {
		if _, err := client.service.registry.InvalidateResponsesByField("inventory", field, "p1"); !perrors.Is(err, CodeClientConfig) {
			t.Fatalf("field %q: err=%v, want client.config", field, err)
		}
	}
	var unbound *Client
	if dropped, err := unbound.InvalidateResponsesByField("principalId", "p1"); err != nil || dropped != 0 {
		t.Fatalf("an unbound client dropped %d (err=%v)", dropped, err)
	}
	if _, err := unbound.InvalidateResponsesByField("principalId", 1.5); !perrors.Is(err, CodeClientConfig) {
		t.Fatalf("an unbound client accepted a value no runtime compares: %v", err)
	}
	var noRegistry *ServiceBindings
	if dropped, err := noRegistry.InvalidateResponsesByField("inventory", "principalId", "p1"); err != nil || dropped != 0 {
		t.Fatalf("a nil registry dropped %d (err=%v)", dropped, err)
	}
}

type invalidationVectors struct {
	Values []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
		Tag   string `json:"tag"`
	} `json:"values"`
	Refused []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	} `json:"refused"`
	Responses []struct {
		Name               string            `json:"name"`
		InvalidationFields []string          `json:"invalidationFields"`
		Body               string            `json:"body"`
		Tags               map[string]string `json:"tags"`
	} `json:"responses"`
}

// nativeVectorValue reads a vector's JSON text into the Go value a consumer
// holds: a string, a bool, or the narrowest of int64 and uint64 that keeps
// every digit. What fits neither stays the json.Number it was read as.
func nativeVectorValue(t *testing.T, text string) any {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		t.Fatal(err)
	}
	if number, ok := value.(json.Number); ok {
		if integer, err := number.Int64(); err == nil {
			return integer
		}
		if unsigned, err := strconv.ParseUint(number.String(), 10, 64); err == nil {
			return unsigned
		}
	}
	return value
}

func TestTheResponseFieldTagsMatchTheSharedVectors(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", responseCacheControlRequirement, "the-response-field-tags-match-the-shared-vectors")
	data, err := os.ReadFile("../../../protocols/clientcontract/fixtures/cache/invalidation.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors invalidationVectors
	if err := json.Unmarshal(data, &vectors); err != nil {
		t.Fatal(err)
	}
	if len(vectors.Values) == 0 || len(vectors.Refused) == 0 || len(vectors.Responses) == 0 {
		t.Fatal("the shared invalidation vectors are empty")
	}
	for _, vector := range vectors.Values {
		t.Run("value/"+vector.Name, func(t *testing.T) {
			tag, err := invalidationTag("field", nativeVectorValue(t, vector.Value))
			if err != nil || tag != vector.Tag {
				t.Fatalf("tag = %q (err=%v), want %q", tag, err, vector.Tag)
			}
			// A consumer holding the raw number renders the same tag.
			if !strings.HasPrefix(vector.Value, `"`) && vector.Value != "true" && vector.Value != "false" {
				if tag, err := invalidationTag("field", json.Number(vector.Value)); err != nil || tag != vector.Tag {
					t.Fatalf("json.Number tag = %q (err=%v), want %q", tag, err, vector.Tag)
				}
			}
		})
	}
	for _, vector := range vectors.Refused {
		t.Run("refused/"+vector.Name, func(t *testing.T) {
			if tag, err := invalidationTag("field", nativeVectorValue(t, vector.Value)); err == nil {
				t.Fatalf("value %s rendered %q, want a refusal", vector.Value, tag)
			}
		})
	}
	for _, vector := range vectors.Responses {
		t.Run("response/"+vector.Name, func(t *testing.T) {
			got := responseTags(&Response{Body: []byte(vector.Body)}, vector.InvalidationFields)
			if len(got) != len(vector.Tags) || (len(got) > 0 && !reflect.DeepEqual(got, vector.Tags)) {
				t.Fatalf("tags = %v, want %v", got, vector.Tags)
			}
		})
	}
}

func TestAnOperationWithoutACacheDeclarationIsNeverCached(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", responseCacheRequirement, "one-upstream-call-per-key-and-identity-per-fresh-window-under-concurrent-load")
	provider := &countingProvider{}
	server := httptest.NewServer(provider)
	defer server.Close()
	client := cachedClient(t, server.URL, newCacheClock())
	operation := cachedAccountOperation(nil, nil)
	operation.Contract.Resilience = nil
	for range 3 {
		if _, err := getAccount(t.Context(), client, operation, "a1"); err != nil {
			t.Fatal(err)
		}
	}
	if provider.calls.Load() != 3 {
		t.Fatalf("an undeclared operation was cached: upstream=%d", provider.calls.Load())
	}
}
