package client

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
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

func TestCredentialManagerSingleflightCacheCancellationAndInvalidation(t *testing.T) {
	manager := newCredentialManager()
	request := CredentialRequest{ServiceID: "svc", ClientID: "consumer", Profile: "service", Audience: "aud", Scopes: []string{"write", "read", "read"}}
	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	source := TokenSourceFunc(func(context.Context, CredentialRequest) (Credential, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		return Credential{Value: fmt.Sprintf("token-%d", calls.Load()), Expiry: time.Now().Add(time.Hour)}, nil
	})
	binding := CredentialBinding{Provider: source}

	var wait sync.WaitGroup
	results := make(chan Credential, 12)
	for range 12 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			credential, err := manager.acquire(context.Background(), request, binding, time.Second)
			if err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			results <- credential
		}()
	}
	<-started
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := manager.acquire(canceled, request, binding, time.Second); err != context.Canceled {
		t.Fatalf("canceled waiter error = %v", err)
	}
	close(release)
	wait.Wait()
	close(results)
	for credential := range results {
		if credential.Value != "token-1" {
			t.Errorf("coalesced credential = %#v", credential)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("source calls = %d", calls.Load())
	}
	if _, err := manager.acquire(context.Background(), request, binding, time.Second); err != nil || calls.Load() != 1 {
		t.Fatalf("cache hit: calls=%d err=%v", calls.Load(), err)
	}
	manager.invalidate(request, binding, "token-1")
	release = make(chan struct{})
	close(release)
	if _, err := manager.acquire(context.Background(), request, binding, time.Second); err != nil || calls.Load() != 2 {
		t.Fatalf("refresh after invalidation: calls=%d err=%v", calls.Load(), err)
	}
}

func TestCredentialInvalidationOnlyRemovesTheRejectedValueAndExactSource(t *testing.T) {
	manager := newCredentialManager()
	request := CredentialRequest{ServiceID: "svc", ClientID: "consumer", Profile: "service", Audience: "aud"}
	var firstCalls atomic.Int32
	first := CredentialBinding{Provider: TokenSourceFunc(func(context.Context, CredentialRequest) (Credential, error) {
		value := fmt.Sprintf("first-%d", firstCalls.Add(1))
		return Credential{Value: value, Expiry: time.Now().Add(time.Hour)}, nil
	})}
	var secondCalls atomic.Int32
	second := CredentialBinding{Provider: TokenSourceFunc(func(context.Context, CredentialRequest) (Credential, error) {
		secondCalls.Add(1)
		return Credential{Value: "second", Expiry: time.Now().Add(time.Hour)}, nil
	})}

	credential, err := manager.acquire(t.Context(), request, first, time.Second)
	if err != nil || credential.Value != "first-1" {
		t.Fatalf("first acquire = %#v, %v", credential, err)
	}
	other, err := manager.acquire(t.Context(), request, second, time.Second)
	if err != nil || other.Value != "second" || secondCalls.Load() != 1 {
		t.Fatalf("second source acquire = %#v, %v calls=%d", other, err, secondCalls.Load())
	}
	manager.invalidate(request, first, "first-1")
	fresh, err := manager.acquire(t.Context(), request, first, time.Second)
	if err != nil || fresh.Value != "first-2" {
		t.Fatalf("refresh = %#v, %v", fresh, err)
	}
	// A delayed 401 for first-1 must not evict first-2, and invalidating the
	// first binding must not touch the independent second binding.
	manager.invalidate(request, first, "first-1")
	stillFresh, err := manager.acquire(t.Context(), request, first, time.Second)
	if err != nil || stillFresh.Value != "first-2" || firstCalls.Load() != 2 {
		t.Fatalf("stale rejection evicted fresh token: %#v, %v calls=%d", stillFresh, err, firstCalls.Load())
	}
	other, err = manager.acquire(t.Context(), request, second, time.Second)
	if err != nil || other.Value != "second" || secondCalls.Load() != 1 {
		t.Fatalf("unrelated source was evicted: %#v, %v calls=%d", other, err, secondCalls.Load())
	}
}

func TestCredentialManagerRejectsEmptyExpiredAndUnboundedValues(t *testing.T) {
	tests := []Credential{
		{},
		{Value: "no-expiry"},
		{Value: "expired", Expiry: time.Now().Add(-time.Minute)},
	}
	for _, result := range tests {
		manager := newCredentialManager()
		_, err := manager.acquire(t.Context(), CredentialRequest{ServiceID: "svc", Profile: "service"}, CredentialBinding{
			Provider: TokenSourceFunc(func(context.Context, CredentialRequest) (Credential, error) { return result, nil }),
		}, time.Second)
		if err == nil {
			t.Fatalf("credential %#v was accepted", result)
		}
	}
	manager := newCredentialManager()
	secret := "provider-secret-in-error"
	_, err := manager.acquire(t.Context(), CredentialRequest{ServiceID: "svc", Profile: "service"}, CredentialBinding{
		Provider: TokenSourceFunc(func(context.Context, CredentialRequest) (Credential, error) {
			return Credential{}, fmt.Errorf("%s", secret)
		}),
	}, time.Second)
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("provider error leaked: %v", err)
	}
}

func TestOAuthCredentialSourcesUseBoundedSafeWireForms(t *testing.T) {
	var assertions atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/redirect" {
			http.Redirect(writer, request, "/token", http.StatusFound)
			return
		}
		if err := request.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if request.Form.Get("grant_type") != "client_credentials" || request.Form.Get("audience") != "aud" || request.Form.Get("scope") != "read write" {
			t.Errorf("token form = %v", request.Form)
		}
		if assertion := request.Form.Get("client_assertion"); assertion != "" {
			assertions.Add(1)
			if request.Form.Get("client_assertion_type") != clientAssertionType || request.Form.Get("client_id") != "consumer" {
				t.Errorf("assertion form = %v", request.Form)
			}
		} else {
			encodedID, encodedSecret, ok := request.BasicAuth()
			id, idErr := url.QueryUnescape(encodedID)
			secret, secretErr := url.QueryUnescape(encodedSecret)
			if !ok || idErr != nil || secretErr != nil || id != "oauth:id +%é" || secret != "a:b +%é" {
				t.Errorf("basic auth = %q %q %v (%v, %v)", encodedID, encodedSecret, ok, idErr, secretErr)
			}
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"access_token":"access","expires_in":"3600"}`))
	}))
	defer server.Close()
	request := CredentialRequest{ServiceID: "svc", ClientID: "consumer", Audience: "aud", Scopes: []string{"write", "read"}}
	credential, err := fetchOAuthToken(t.Context(), request, CredentialBinding{
		TokenURL: server.URL, ClientID: "oauth:id +%é", ClientSecret: "a:b +%é",
	}, "")
	if err != nil || credential.Value != "access" || time.Until(credential.Expiry) < 59*time.Minute {
		t.Fatalf("client credential = %#v, %v", credential, err)
	}
	if _, err := fetchOAuthToken(t.Context(), request, CredentialBinding{TokenURL: server.URL}, "assertion"); err != nil {
		t.Fatalf("assertion exchange: %v", err)
	}
	if assertions.Load() != 1 {
		t.Fatalf("assertion requests = %d", assertions.Load())
	}
	if _, err := fetchOAuthToken(t.Context(), request, CredentialBinding{TokenURL: server.URL + "/redirect", ClientID: "id", ClientSecret: "secret"}, ""); err == nil {
		t.Fatal("redirecting token endpoint must fail")
	}
}

func TestOAuthExtensionGrantSupportsFormAndJSONAndProtectsReservedFields(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		values := map[string]string{}
		switch request.URL.Path {
		case "/form":
			if request.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
				t.Errorf("form content type = %q", request.Header.Get("Content-Type"))
			}
			if err := request.ParseForm(); err != nil {
				t.Fatal(err)
			}
			for key := range request.Form {
				values[key] = request.Form.Get(key)
			}
		case "/json":
			if request.Header.Get("Content-Type") != "application/json" {
				t.Errorf("JSON content type = %q", request.Header.Get("Content-Type"))
			}
			if err := json.NewDecoder(request.Body).Decode(&values); err != nil {
				t.Fatal(err)
			}
		}
		if values["grant_type"] != "urn:putnami:params:oauth:grant-type:api-key" || values["client_id"] != "consumer" ||
			values["scope"] != "events:read events:write" || values["audience"] != "events" || values["api_key"] != "pkt_secret" ||
			values["workspace_id"] != "workspace" {
			t.Errorf("extension grant values = %#v", values)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"access_token":"exchanged","expires_in":300}`))
	}))
	defer server.Close()
	request := CredentialRequest{ServiceID: "events", ClientID: "consumer", Audience: "events", Scopes: []string{"events:write", "events:read"}}
	for _, format := range []string{"form", "json"} {
		credential, err := fetchOAuthExtensionToken(t.Context(), request, CredentialBinding{
			TokenURL:           server.URL + "/" + format,
			Source:             CredentialSourceOAuthExtensionGrant,
			GrantType:          "urn:putnami:params:oauth:grant-type:api-key",
			TokenRequestFormat: format,
			Parameters:         map[string]string{"api_key": "pkt_secret", "workspace_id": "workspace"},
		})
		if err != nil || credential.Value != "exchanged" || time.Until(credential.Expiry) < 4*time.Minute {
			t.Fatalf("%s credential = %#v, %v", format, credential, err)
		}
	}
	for _, reserved := range []string{"grant_type", "client_id", "scope", "audience"} {
		_, err := fetchOAuthExtensionToken(t.Context(), request, CredentialBinding{
			TokenURL: server.URL + "/form", GrantType: "extension", Parameters: map[string]string{reserved: "override"},
		})
		if err == nil || strings.Contains(err.Error(), "override") {
			t.Fatalf("reserved %s error = %v", reserved, err)
		}
	}
}

func TestGCPIDTokenSourceRequiresAudienceAndValidFutureJWT(t *testing.T) {
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, time.Now().Add(time.Hour).Unix())))
	token := "header." + payload + ".signature"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/custom/identity" || request.Header.Get("Metadata-Flavor") != "Google" || request.URL.Query().Get("audience") != "aud" || request.URL.Query().Get("format") != "full" {
			t.Errorf("metadata request headers/query = %v %v", request.Header, request.URL.Query())
		}
		_, _ = writer.Write([]byte(token))
	}))
	defer server.Close()
	credential, err := fetchGCPIDToken(t.Context(), "aud", CredentialBinding{MetadataURL: server.URL + "/custom/identity"})
	if err != nil || credential.Value != token || credential.Expiry.IsZero() {
		t.Fatalf("metadata credential = %#v, %v", credential, err)
	}
	if _, err := fetchGCPIDToken(t.Context(), "", CredentialBinding{MetadataURL: server.URL + "/custom/identity"}); err == nil {
		t.Fatal("empty audience accepted")
	}
	for _, malformed := range []string{"malformed", "header." + payload, "header..signature"} {
		if expiry := parseJWTExpiry(malformed); !expiry.IsZero() {
			t.Fatalf("malformed JWT %q expiry = %v", malformed, expiry)
		}
	}
}

func TestCredentialManagerAcceptsFirstUseCredentialThatIsStillValid(t *testing.T) {
	manager := newCredentialManager()
	credential, err := manager.acquire(t.Context(), CredentialRequest{ServiceID: "svc", Profile: "service"}, CredentialBinding{
		Provider: TokenSourceFunc(func(context.Context, CredentialRequest) (Credential, error) {
			return Credential{Value: "short-lived", Expiry: time.Now().Add(500 * time.Millisecond)}, nil
		}),
	}, time.Second)
	if err != nil || credential.Value != "short-lived" {
		t.Fatalf("first-use credential = %#v, %v", credential, err)
	}
}

func TestParseExpiresInRejectsInvalidAndOverflow(t *testing.T) {
	for _, raw := range []string{`0`, `-1`, `"nope"`, `9223372037`} {
		if _, err := parseExpiresIn([]byte(raw)); err == nil {
			t.Fatalf("expires_in %s accepted", raw)
		}
	}
	if seconds, err := parseExpiresIn(nil); err != nil || seconds != 0 {
		t.Fatalf("absent expires_in = %d, %v", seconds, err)
	}
}

func TestCredentialEndpointRejectsQuerySecrets(t *testing.T) {
	endpoint := (&url.URL{Scheme: "https", Host: "auth.example", RawQuery: "token=secret"}).String()
	if _, err := parseCredentialURL(endpoint, false); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("credential URL error = %v", err)
	}
}

// testClock is the controlled time source the credential lifetime tests use.
// Nothing in them waits for a real credential to age.
type testClock struct {
	mu      sync.Mutex
	instant time.Time
}

func newTestClock() *testClock {
	return &testClock{instant: time.Date(2026, time.September, 9, 12, 0, 0, 0, time.UTC)}
}

func (clock *testClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.instant
}

func (clock *testClock) advance(delta time.Duration) {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.instant = clock.instant.Add(delta)
}

func TestCredentialIsRenewedOnceWhenTheCachedValueReachesItsExpiry(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "credential-lifetime", "an-expiring-credential-is-renewed-once-for-every-concurrent-caller")
	clock := newTestClock()
	manager := newCredentialManagerWithClock(clock.Now)
	request := CredentialRequest{ServiceID: "inventory", ClientID: "consumer", Profile: "service"}
	var calls atomic.Int32
	started := make(chan struct{}, 16)
	release := make(chan struct{})
	binding := CredentialBinding{Provider: TokenSourceFunc(func(context.Context, CredentialRequest) (Credential, error) {
		value := fmt.Sprintf("token-%d", calls.Add(1))
		started <- struct{}{}
		<-release
		return Credential{Value: value, Expiry: clock.Now().Add(2 * time.Minute)}, nil
	})}

	close(release)
	first, err := manager.acquire(t.Context(), request, binding, time.Second)
	if err != nil || first.Value != "token-1" {
		t.Fatalf("first acquisition = %#v, %v", first, err)
	}
	<-started

	// Halfway through the lifetime the cached value is still fresh.
	clock.advance(time.Minute)
	cached, err := manager.acquire(t.Context(), request, binding, time.Second)
	if err != nil || cached.Value != "token-1" || calls.Load() != 1 {
		t.Fatalf("mid-life acquisition = %#v, %v calls=%d", cached, err, calls.Load())
	}

	// Inside the refresh leeway it is stale, and eight concurrent callers still
	// renew it exactly once.
	clock.advance(50 * time.Second)
	release = make(chan struct{})
	values := make(chan string, 8)
	var waiting sync.WaitGroup
	for range 8 {
		waiting.Add(1)
		go func() {
			defer waiting.Done()
			credential, acquireErr := manager.acquire(context.Background(), request, binding, time.Minute)
			if acquireErr != nil {
				t.Errorf("renewal: %v", acquireErr)
				return
			}
			values <- credential.Value
		}()
	}
	<-started
	close(release)
	waiting.Wait()
	close(values)
	for value := range values {
		if value != "token-2" {
			t.Fatalf("renewed value = %q", value)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("acquisitions across the whole lifetime = %d", calls.Load())
	}
}

func TestTokenSourceOutageIsNotCachedAndRecoveryReacquiresOnce(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "credential-lifetime", "a-token-source-outage-is-never-cached-and-recovery-reacquires-once")
	clock := newTestClock()
	manager := newCredentialManagerWithClock(clock.Now)
	request := CredentialRequest{ServiceID: "inventory", ClientID: "consumer", Profile: "service"}
	var calls atomic.Int32
	binding := CredentialBinding{Provider: TokenSourceFunc(func(context.Context, CredentialRequest) (Credential, error) {
		if calls.Add(1) <= 2 {
			return Credential{}, fmt.Errorf("identity provider is down: secret-detail")
		}
		return Credential{Value: "recovered", Expiry: clock.Now().Add(time.Hour)}, nil
	})}

	for attempt := range 2 {
		_, err := manager.acquire(t.Context(), request, binding, time.Second)
		if err == nil || !perrors.Is(err, CodeClientCredential) || strings.Contains(err.Error(), "secret-detail") {
			t.Fatalf("outage %d error = %v", attempt, err)
		}
		manager.mu.Lock()
		cached := len(manager.cache)
		manager.mu.Unlock()
		if cached != 0 {
			t.Fatalf("a failed acquisition cached %d entries", cached)
		}
	}
	recovered, err := manager.acquire(t.Context(), request, binding, time.Second)
	if err != nil || recovered.Value != "recovered" {
		t.Fatalf("recovery = %#v, %v", recovered, err)
	}
	again, err := manager.acquire(t.Context(), request, binding, time.Second)
	if err != nil || again.Value != "recovered" || calls.Load() != 3 {
		t.Fatalf("post-recovery cache hit = %#v, %v calls=%d", again, err, calls.Load())
	}
}

func TestTokenResponseBoundsRejectOversizedMalformedAndUnusableCredentials(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "credential-lifetime", "a-malformed-oversized-or-immediately-expired-token-response-fails-without-leaking")
	const secret = "pkt-super-secret-value"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/oversized":
			_, _ = writer.Write([]byte(`{"access_token":"` + strings.Repeat("a", credentialMaxBody) + `"}`))
		case "/malformed":
			_, _ = writer.Write([]byte(`{"access_token":"` + secret + `"`))
		case "/unbounded":
			_, _ = writer.Write([]byte(`{"access_token":"` + secret + `"}`))
		case "/expired":
			_, _ = writer.Write([]byte(`{"access_token":"` + secret + `","expires_in":0}`))
		default:
			writer.WriteHeader(http.StatusInternalServerError)
			_, _ = writer.Write([]byte(`{"error":"` + secret + `"}`))
		}
	}))
	defer server.Close()

	clock := newTestClock()
	request := CredentialRequest{ServiceID: "inventory", ClientID: "consumer", Profile: "service"}
	for _, path := range []string{"/oversized", "/malformed", "/unbounded", "/expired", "/error"} {
		t.Run(path, func(t *testing.T) {
			manager := newCredentialManagerWithClock(clock.Now)
			endpoint := server.URL + path + "?tenant=acme"
			binding := CredentialBinding{
				Source: CredentialSourceOAuthClientCredentials, TokenURL: server.URL + path,
				ClientID: "consumer", ClientSecret: secret, AllowInsecure: true,
			}
			_, err := manager.acquire(t.Context(), request, binding, time.Second)
			if err == nil {
				t.Fatal("an unusable token response was accepted")
			}
			if !perrors.Is(err, CodeClientCredential) {
				t.Fatalf("error code = %v", err)
			}
			for _, forbidden := range []string{secret, endpoint, server.URL, "tenant=acme"} {
				if strings.Contains(err.Error(), forbidden) {
					t.Fatalf("error leaked %q: %v", forbidden, err)
				}
			}
			manager.mu.Lock()
			cached := len(manager.cache)
			manager.mu.Unlock()
			if cached != 0 {
				t.Fatalf("an unusable response cached %d entries", cached)
			}
		})
	}
}

// registryClient exposes the registry behind a bound client so a test can
// assert what the application-scoped credential cache holds.
func registryClient(t *testing.T, endpoint string, descriptor ServiceDescriptor, binding ServiceBinding) (*ServiceBindings, *Client) {
	t.Helper()
	binding.URL = endpoint
	if binding.ClientID == "" {
		binding.ClientID = "consumer.workload"
	}
	registry, err := newServiceBindings(ServicesOptions{Services: map[string]ServiceBinding{"inventory": binding}})
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewServiceClient(registry, descriptor)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = registry.Close() })
	return registry, client
}

func cachedCredentials(registry *ServiceBindings) int {
	registry.credentials.mu.Lock()
	defer registry.credentials.mu.Unlock()
	return len(registry.credentials.cache)
}

func TestForwardedUserIdentityComesOnlyFromAnInboundContextAndNeverFromTheServiceCache(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "credential-placement", "a-forwarded-user-identity-is-read-from-the-inbound-context-and-never-from-the-service-cache")
	authorizations := make(chan string, 8)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		authorizations <- request.Header.Get("Authorization")
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"value":"ok"}`))
	}))
	defer server.Close()

	descriptor := testDescriptor(map[string]clientcontract.CredentialProfile{
		"service": {Kind: clientcontract.CredentialServiceToken},
		"caller":  {Kind: clientcontract.CredentialForwardedUserToken},
	})
	serviceOperation := testOperation(clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{
		{AllOf: []clientcontract.SecurityRequirement{{Profile: "service"}}},
	}}, clientcontract.Idempotency{Kind: clientcontract.IdempotencySafe})
	callerOperation := testOperation(clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{
		{AllOf: []clientcontract.SecurityRequirement{{Profile: "caller"}}},
	}}, clientcontract.Idempotency{Kind: clientcontract.IdempotencySafe})
	registry, client := registryClient(t, server.URL, descriptor, ServiceBinding{Credentials: map[string]CredentialBinding{
		"service": {Provider: freshSource("service-token", nil)},
		"caller":  {Source: CredentialSourceForwardedUser},
	}})

	if _, err := client.DoOperation(t.Context(), &Request{Path: "/items"}, serviceOperation); err != nil {
		t.Fatal(err)
	}
	if authorization := <-authorizations; authorization != "Bearer service-token" {
		t.Fatalf("service authorization = %q", authorization)
	}

	// The cached service bearer must never stand in for a missing caller
	// identity: without a real inbound token the call is refused before dispatch.
	if _, err := client.DoOperation(t.Context(), &Request{Path: "/items"}, callerOperation); !perrors.Is(err, CodeClientCredential) {
		t.Fatalf("call without an inbound identity = %v", err)
	}
	select {
	case authorization := <-authorizations:
		t.Fatalf("a request was dispatched with %q", authorization)
	default:
	}

	inboundRequest := httptest.NewRequest(http.MethodGet, "/consumer", nil)
	inboundRequest.Header.Set("Authorization", "Bearer inbound-user")
	inbound := phttp.NewContext(httptest.NewRecorder(), inboundRequest)
	if _, err := client.DoOperation(inbound.Context(), &Request{Path: "/items"}, callerOperation); err != nil {
		t.Fatal(err)
	}
	if authorization := <-authorizations; authorization != "Bearer inbound-user" {
		t.Fatalf("forwarded authorization = %q", authorization)
	}
	if cached := cachedCredentials(registry); cached != 1 {
		t.Fatalf("cache entries after a forwarded call = %d, want only the service token", cached)
	}
}

func TestCredentialAcquisitionIsSpentInsideTheDeclaredCallBudget(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "credential-lifetime", "credential-acquisition-is-spent-inside-the-declared-call-budget")
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"value":"ok"}`))
	}))
	defer server.Close()

	descriptor := testDescriptor(map[string]clientcontract.CredentialProfile{"service": {Kind: clientcontract.CredentialServiceToken}})
	operation := testOperation(clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{
		{AllOf: []clientcontract.SecurityRequirement{{Profile: "service"}}},
	}}, clientcontract.Idempotency{Kind: clientcontract.IdempotencySafe})
	operation.Contract.Resilience = &clientcontract.ResiliencePolicy{TimeoutMs: intPointer(150), AttemptTimeoutMs: intPointer(80)}
	_, client := registryClient(t, server.URL, descriptor, ServiceBinding{Credentials: map[string]CredentialBinding{
		"service": {Provider: TokenSourceFunc(func(ctx context.Context, _ CredentialRequest) (Credential, error) {
			<-ctx.Done()
			return Credential{}, ctx.Err()
		})},
	}})

	started := time.Now()
	_, err := client.DoOperation(t.Context(), &Request{Path: "/items"}, operation)
	if !perrors.Is(err, CodeClientDeadline) {
		t.Fatalf("budget exhausted during acquisition = %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("the acquisition outlived the declared budget: %s", elapsed)
	}
	if requests.Load() != 0 {
		t.Fatalf("a request was dispatched without a credential: %d", requests.Load())
	}
}

func TestRejectedCredentialIsReacquiredWithoutReplayingAnUnsafeOperation(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "credential-placement", "a-rejected-credential-is-reacquired-without-replaying-an-unsafe-operation")
	var requests atomic.Int32
	var tokens atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		if request.Header.Get("Authorization") == "Bearer token-1" {
			writer.WriteHeader(http.StatusUnauthorized)
			_, _ = writer.Write([]byte(`{"code":"unauthorized","error":"Unauthorized","message":"rejected"}`))
			return
		}
		_, _ = writer.Write([]byte(`{"value":"accepted"}`))
	}))
	defer server.Close()

	descriptor := testDescriptor(map[string]clientcontract.CredentialProfile{"service": {Kind: clientcontract.CredentialServiceToken}})
	operation := testOperation(clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{
		{AllOf: []clientcontract.SecurityRequirement{{Profile: "service"}}},
	}}, clientcontract.Idempotency{Kind: clientcontract.IdempotencyNonIdempotent})
	// The provider even declares the rejection retryable: a non-idempotent
	// operation must still be sent exactly once.
	operation.Contract.Errors = []clientcontract.DeclaredError{{Status: 401, Code: string(perrors.CodeUnauthorized), Retryable: boolPointer(true)}}
	registry, client := registryClient(t, server.URL, descriptor, ServiceBinding{Credentials: map[string]CredentialBinding{
		"service": {Provider: TokenSourceFunc(func(context.Context, CredentialRequest) (Credential, error) {
			return Credential{Value: fmt.Sprintf("token-%d", tokens.Add(1)), Expiry: time.Now().Add(time.Hour)}, nil
		})},
	}})

	if _, err := client.DoOperation(t.Context(), &Request{Method: http.MethodPost, Path: "/items"}, operation); err == nil {
		t.Fatal("the rejected call succeeded")
	}
	if requests.Load() != 1 {
		t.Fatalf("an unsafe operation was replayed: %d requests", requests.Load())
	}
	if cached := cachedCredentials(registry); cached != 0 {
		t.Fatalf("the rejected credential stayed cached: %d entries", cached)
	}
	if _, err := client.DoOperation(t.Context(), &Request{Method: http.MethodPost, Path: "/items"}, operation); err != nil {
		t.Fatalf("call after reacquisition = %v", err)
	}
	if tokens.Load() != 2 || requests.Load() != 2 {
		t.Fatalf("tokens=%d requests=%d", tokens.Load(), requests.Load())
	}
}

func TestAlternativeAppliesEveryRequirementAndIsNeverDowngraded(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "credential-placement", "every-requirement-of-the-chosen-alternative-is-applied-and-a-failed-choice-is-never-downgraded")
	headers := make(chan http.Header, 4)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		headers <- request.Header.Clone()
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"value":"ok"}`))
	}))
	defer server.Close()

	descriptor := testDescriptor(map[string]clientcontract.CredentialProfile{
		"service": {Kind: clientcontract.CredentialServiceToken},
		"tenant":  {Kind: clientcontract.CredentialNamedHeader, Header: "X-Tenant-ID"},
		"apiKey":  {Kind: clientcontract.CredentialAPIKey, Header: "X-Api-Key"},
	})
	both := []clientcontract.SecurityRequirement{{Profile: "service"}, {Profile: "tenant"}}

	t.Run("every requirement of the chosen alternative is applied", func(t *testing.T) {
		operation := testOperation(clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{AllOf: both}}},
			clientcontract.Idempotency{Kind: clientcontract.IdempotencySafe})
		_, client := registryClient(t, server.URL, descriptor, ServiceBinding{Credentials: map[string]CredentialBinding{
			"service": {Provider: freshSource("service-token", nil)},
			"tenant":  {Source: CredentialSourceStatic, Value: "tenant-42"},
		}})
		if _, err := client.DoOperation(t.Context(), &Request{Path: "/items"}, operation); err != nil {
			t.Fatal(err)
		}
		sent := <-headers
		if sent.Get("Authorization") != "Bearer service-token" || sent.Get("X-Tenant-ID") != "tenant-42" {
			t.Fatalf("applied credentials = %#v", sent)
		}
	})

	t.Run("a partially bound alternative is refused before dispatch", func(t *testing.T) {
		before := requests.Load()
		operation := testOperation(clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{AllOf: both}}},
			clientcontract.Idempotency{Kind: clientcontract.IdempotencySafe})
		_, client := registryClient(t, server.URL, descriptor, ServiceBinding{Credentials: map[string]CredentialBinding{
			"service": {Provider: freshSource("service-token", nil)},
		}})
		if _, err := client.DoOperation(t.Context(), &Request{Path: "/items"}, operation); !perrors.Is(err, CodeClientCredential) {
			t.Fatalf("partially bound alternative = %v", err)
		}
		if requests.Load() != before {
			t.Fatal("a request was dispatched without every declared requirement")
		}
	})

	t.Run("an unsatisfiable first alternative selects the next declared one", func(t *testing.T) {
		operation := testOperation(clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{
			{AllOf: []clientcontract.SecurityRequirement{{Profile: "service"}}},
			{AllOf: []clientcontract.SecurityRequirement{{Profile: "apiKey"}}},
		}}, clientcontract.Idempotency{Kind: clientcontract.IdempotencySafe})
		_, client := registryClient(t, server.URL, descriptor, ServiceBinding{Credentials: map[string]CredentialBinding{
			"apiKey": {Source: CredentialSourceStatic, Value: "key-7"},
		}})
		if _, err := client.DoOperation(t.Context(), &Request{Path: "/items"}, operation); err != nil {
			t.Fatal(err)
		}
		sent := <-headers
		if sent.Get("X-Api-Key") != "key-7" || sent.Get("Authorization") != "" {
			t.Fatalf("secondary credential placement = %#v", sent)
		}
	})

	t.Run("a chosen alternative that fails to acquire is never downgraded", func(t *testing.T) {
		before := requests.Load()
		operation := testOperation(clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{
			{AllOf: []clientcontract.SecurityRequirement{{Profile: "service"}}},
			{AllOf: []clientcontract.SecurityRequirement{{Profile: "apiKey"}}},
		}}, clientcontract.Idempotency{Kind: clientcontract.IdempotencySafe})
		_, client := registryClient(t, server.URL, descriptor, ServiceBinding{Credentials: map[string]CredentialBinding{
			"service": {Provider: TokenSourceFunc(func(context.Context, CredentialRequest) (Credential, error) {
				return Credential{}, fmt.Errorf("identity provider is down")
			})},
			"apiKey": {Source: CredentialSourceStatic, Value: "key-7"},
		}})
		if _, err := client.DoOperation(t.Context(), &Request{Path: "/items"}, operation); !perrors.Is(err, CodeClientCredential) {
			t.Fatalf("failed acquisition = %v", err)
		}
		if requests.Load() != before {
			t.Fatal("the call fell back to the weaker alternative after the chosen one failed")
		}
	})
}
