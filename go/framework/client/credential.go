package client

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.putnami.dev/errors"
)

const (
	// CodeClientCredential identifies framework credential acquisition failures.
	CodeClientCredential errors.Code = "client.credential"

	defaultCredentialTimeout = 30 * time.Second
	credentialMaxBody        = 64 << 10
	gcpMetadataHost          = "http://metadata.google.internal"
	gcpIdentityPath          = "/computeMetadata/v1/instance/service-accounts/default/identity" //nolint:gosec // public metadata path, not a credential
	clientAssertionType      = "urn:ietf:params:oauth:client-assertion-type:jwt-bearer"         //nolint:gosec // public RFC 7523 identifier
)

type cachedCredential struct {
	credential Credential
	fetchedAt  time.Time
}

type credentialCall struct {
	done       chan struct{}
	cancel     context.CancelFunc
	credential Credential
	err        error
}

// credentialManager keeps service credentials per exact non-secret identity
// and collapses concurrent refreshes. Forwarded user credentials never enter it.
//
// One manager belongs to exactly one ServiceBindings registry, which belongs to
// exactly one application. The package holds no ambient manager, so two
// applications in one process never observe each other's bearer tokens, and
// closing the registry ends every cached and in-flight credential at once.
type credentialManager struct {
	mu     sync.Mutex
	cache  map[string]cachedCredential
	calls  map[string]*credentialCall
	closed bool

	// now is the manager clock. Tests drive expiry and renewal from it instead
	// of sleeping for a real credential lifetime.
	now func() time.Time
}

func newCredentialManager() *credentialManager {
	return newCredentialManagerWithClock(time.Now)
}

func newCredentialManagerWithClock(now func() time.Time) *credentialManager {
	return &credentialManager{
		cache: make(map[string]cachedCredential),
		calls: make(map[string]*credentialCall),
		now:   now,
	}
}

// close ends the manager. Every in-flight acquisition is canceled, every cached
// credential is dropped, and every later acquisition is refused with
// CodeClientClosed. It is idempotent.
func (m *credentialManager) close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	cancels := make([]context.CancelFunc, 0, len(m.calls))
	for _, call := range m.calls {
		cancels = append(cancels, call.cancel)
	}
	clear(m.cache)
	m.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

func (m *credentialManager) acquire(
	ctx context.Context,
	request CredentialRequest,
	binding CredentialBinding,
	maxDuration time.Duration,
) (Credential, error) {
	request.Scopes = sortedUnique(request.Scopes)
	key, keyErr := credentialCacheKey(request, binding)
	if keyErr != nil {
		return Credential{}, errors.New(CodeClientCredential, "credential source identity is invalid")
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return Credential{}, errClosedRegistry()
	}
	now := m.now()
	if cached, ok := m.cache[key]; ok && credentialFresh(cached, now) {
		m.mu.Unlock()
		return cached.credential, nil
	}
	if active, ok := m.calls[key]; ok {
		m.mu.Unlock()
		select {
		case <-active.done:
			return active.credential, active.err
		case <-ctx.Done():
			return Credential{}, ctx.Err()
		}
	}
	if maxDuration <= 0 {
		maxDuration = defaultCredentialTimeout
	}
	// The leader is bounded independently so one canceled waiter cannot abort a
	// refresh shared by the other callers. Every waiter still observes its own
	// total/attempt deadline while waiting on call.done. The manager keeps the
	// cancel so closing the registry ends a refresh no caller is waiting for.
	leaderCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), maxDuration)
	call := &credentialCall{done: make(chan struct{}), cancel: cancel}
	m.calls[key] = call
	m.mu.Unlock()

	go func() {
		defer cancel()
		call.credential, call.err = acquireCredential(leaderCtx, request, binding)
		if call.err != nil && leaderCtx.Err() == context.DeadlineExceeded {
			call.err = errors.New(CodeClientDeadline, "credential acquisition deadline exceeded")
		}
		if call.err == nil && strings.TrimSpace(call.credential.Value) == "" {
			call.err = errors.New(CodeClientCredential, "credential source returned an empty value")
		}
		m.mu.Lock()
		if call.err == nil && (call.credential.Expiry.IsZero() || !call.credential.Expiry.After(m.now())) {
			call.err = errors.New(CodeClientCredential, "credential source returned an expired or unbounded value")
		}
		delete(m.calls, key)
		if m.closed {
			// The registry closed while this refresh was running. Neither the
			// value nor a provider error outlives the application that asked
			// for it: the cache stays empty and every waiter sees the closed
			// registry rather than a cancellation it cannot act on.
			call.credential, call.err = Credential{}, errClosedRegistry()
		} else if call.err == nil {
			m.cache[key] = cachedCredential{credential: call.credential, fetchedAt: m.now()}
		}
		close(call.done)
		m.mu.Unlock()
	}()

	select {
	case <-call.done:
		return call.credential, call.err
	case <-ctx.Done():
		return Credential{}, ctx.Err()
	}
}

func (m *credentialManager) invalidate(request CredentialRequest, binding CredentialBinding, rejectedValue string) {
	request.Scopes = sortedUnique(request.Scopes)
	key, err := credentialCacheKey(request, binding)
	if err != nil {
		return
	}
	m.mu.Lock()
	if cached, ok := m.cache[key]; ok && cached.credential.Value == rejectedValue {
		delete(m.cache, key)
	}
	m.mu.Unlock()
}

// credentialCacheKey includes the complete source identity so two bindings for
// the same service/client/audience/scopes cannot share credentials accidentally.
// The serialized source (including secret fields) is hashed immediately and
// the resulting key is never exposed through errors or telemetry.
func credentialCacheKey(request CredentialRequest, binding CredentialBinding) (string, error) {
	bindingBytes, err := json.Marshal(binding)
	if err != nil {
		return "", err
	}
	bindingHash := sha256.Sum256(bindingBytes)
	provider := ""
	if binding.Provider != nil {
		value := reflect.ValueOf(binding.Provider)
		provider = reflect.TypeOf(binding.Provider).String()
		switch value.Kind() {
		case reflect.Chan, reflect.Func, reflect.Map, reflect.Pointer, reflect.Slice, reflect.UnsafePointer:
			provider += fmt.Sprintf(":%x", value.Pointer())
		}
	}
	keyBytes, err := json.Marshal(struct {
		Request     CredentialRequest `json:"request"`
		BindingHash [32]byte          `json:"bindingHash"`
		Provider    string            `json:"provider,omitempty"`
	}{Request: request, BindingHash: bindingHash, Provider: provider})
	if err != nil {
		return "", err
	}
	keyHash := sha256.Sum256(keyBytes)
	return string(keyHash[:]), nil
}

func credentialFresh(cached cachedCredential, now time.Time) bool {
	if cached.credential.Value == "" {
		return false
	}
	if cached.credential.Expiry.IsZero() {
		return now.Sub(cached.fetchedAt) < time.Minute
	}
	ttl := cached.credential.Expiry.Sub(cached.fetchedAt)
	leeway := ttl / 10
	if leeway > time.Minute {
		leeway = time.Minute
	}
	if leeway < time.Second {
		leeway = time.Second
	}
	return now.Add(leeway).Before(cached.credential.Expiry)
}

func acquireCredential(ctx context.Context, request CredentialRequest, binding CredentialBinding) (Credential, error) {
	if binding.Provider != nil {
		credential, err := binding.Provider.Credential(ctx, request)
		if err != nil {
			return Credential{}, errors.New(CodeClientCredential, "credential acquisition failed")
		}
		return credential, nil
	}
	switch binding.Source {
	case CredentialSourceGCPIDToken:
		return fetchGCPIDToken(ctx, request.Audience, binding)
	case CredentialSourceOAuthClientCredentials:
		return fetchOAuthToken(ctx, request, binding, "")
	case CredentialSourceOAuthExtensionGrant:
		return fetchOAuthExtensionToken(ctx, request, binding)
	case CredentialSourceOAuthClientAssertion:
		assertion := strings.TrimSpace(binding.Assertion)
		if binding.AssertionSource == CredentialSourceGCPIDToken {
			audience := binding.AssertionAudience
			if audience == "" {
				audience = binding.TokenURL
			}
			credential, err := fetchGCPIDToken(ctx, audience, binding)
			if err != nil {
				return Credential{}, err
			}
			assertion = credential.Value
		}
		if assertion == "" {
			return Credential{}, errors.New(CodeClientCredential, "client assertion is not configured")
		}
		return fetchOAuthToken(ctx, request, binding, assertion)
	default:
		return Credential{}, errors.New(CodeClientCredential, "service token source is not configured")
	}
}

func fetchOAuthExtensionToken(ctx context.Context, request CredentialRequest, binding CredentialBinding) (Credential, error) {
	endpoint, err := parseCredentialURL(binding.TokenURL, binding.AllowInsecure)
	if err != nil {
		return Credential{}, err
	}
	grantType := strings.TrimSpace(binding.GrantType)
	clientID := cmp.Or(binding.ClientID, request.ClientID)
	if !strings.HasPrefix(grantType, "urn:") || clientID == "" {
		return Credential{}, errors.New(CodeClientCredential, "OAuth extension grant identity is incomplete")
	}
	reserved := map[string]bool{"grant_type": true, "client_id": true, "scope": true, "audience": true}
	for key := range binding.Parameters {
		if reserved[key] {
			return Credential{}, errors.Newf(CodeClientCredential, "OAuth extension parameter %q is reserved", key)
		}
	}
	values := make(map[string]string, len(binding.Parameters)+4)
	values["grant_type"] = grantType
	values["client_id"] = clientID
	if request.Audience != "" {
		values["audience"] = request.Audience
	}
	if scopes := sortedUnique(request.Scopes); len(scopes) > 0 {
		values["scope"] = strings.Join(scopes, " ")
	}
	for key, value := range binding.Parameters {
		values[key] = value
	}

	var body io.Reader
	contentType := "application/x-www-form-urlencoded"
	switch binding.TokenRequestFormat {
	case "", "form":
		form := url.Values{}
		for key, value := range values {
			form.Set(key, value)
		}
		body = strings.NewReader(form.Encode())
	case "json":
		encoded, marshalErr := json.Marshal(values)
		if marshalErr != nil {
			return Credential{}, errors.New(CodeClientCredential, "encode OAuth extension request")
		}
		body = bytes.NewReader(encoded)
		contentType = "application/json"
	default:
		return Credential{}, errors.New(CodeClientCredential, "OAuth extension token request format is invalid")
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), body)
	if err != nil {
		return Credential{}, errors.New(CodeClientCredential, "build OAuth extension token request")
	}
	httpRequest.Header.Set("Content-Type", contentType)
	httpRequest.Header.Set("Accept", "application/json")
	response, err := safeCredentialHTTPClient(nil).Do(httpRequest)
	if err != nil {
		return Credential{}, errors.New(CodeClientCredential, "OAuth extension token endpoint request failed")
	}
	return parseOAuthTokenResponse(response)
}

func fetchOAuthToken(ctx context.Context, request CredentialRequest, binding CredentialBinding, assertion string) (Credential, error) {
	endpoint, err := parseCredentialURL(binding.TokenURL, binding.AllowInsecure)
	if err != nil {
		return Credential{}, err
	}
	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	if request.Audience != "" {
		form.Set("audience", request.Audience)
	}
	if scopes := sortedUnique(request.Scopes); len(scopes) > 0 {
		form.Set("scope", strings.Join(scopes, " "))
	}
	if assertion != "" {
		form.Set("client_id", cmp.Or(binding.ClientID, request.ClientID))
		form.Set("client_assertion_type", clientAssertionType)
		form.Set("client_assertion", assertion)
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), strings.NewReader(form.Encode()))
	if err != nil {
		return Credential{}, errors.Wrapf(err, CodeClientCredential, "build token request")
	}
	httpRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	httpRequest.Header.Set("Accept", "application/json")
	if assertion == "" {
		clientID := cmp.Or(binding.ClientID, request.ClientID)
		if clientID == "" || binding.ClientSecret == "" {
			return Credential{}, errors.New(CodeClientCredential, "OAuth client identity is incomplete")
		}
		// RFC 6749 section 2.3.1 applies application/x-www-form-urlencoded
		// encoding to each credential before joining them with a colon for HTTP
		// Basic authentication. Escaping first keeps colons, spaces, percent signs,
		// plus signs, and UTF-8 round-trippable at the token endpoint.
		httpRequest.SetBasicAuth(url.QueryEscape(clientID), url.QueryEscape(binding.ClientSecret))
	}
	response, err := safeCredentialHTTPClient(nil).Do(httpRequest)
	if err != nil {
		return Credential{}, errors.Wrapf(err, CodeClientCredential, "token endpoint request failed")
	}
	return parseOAuthTokenResponse(response)
}

func parseOAuthTokenResponse(response *http.Response) (Credential, error) {
	defer response.Body.Close() //nolint:errcheck // best effort after bounded read
	body, err := io.ReadAll(io.LimitReader(response.Body, credentialMaxBody+1))
	if err != nil {
		return Credential{}, errors.Wrapf(err, CodeClientCredential, "read token response")
	}
	if len(body) > credentialMaxBody {
		return Credential{}, errors.New(CodeClientCredential, "token response exceeds maximum size")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return Credential{}, errors.Newf(CodeClientCredential, "token endpoint returned status %d", response.StatusCode)
	}
	var payload struct {
		AccessToken string          `json:"access_token"`
		ExpiresIn   json.RawMessage `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return Credential{}, errors.New(CodeClientCredential, "token endpoint returned invalid JSON")
	}
	seconds, err := parseExpiresIn(payload.ExpiresIn)
	if err != nil {
		return Credential{}, err
	}
	credential := Credential{Value: strings.TrimSpace(payload.AccessToken)}
	if seconds > 0 {
		credential.Expiry = time.Now().Add(time.Duration(seconds) * time.Second)
	}
	return credential, nil
}

func parseExpiresIn(raw json.RawMessage) (int64, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, nil
	}
	var number json.Number
	if raw[0] == '"' {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return 0, errors.New(CodeClientCredential, "token expiry is invalid")
		}
		number = json.Number(text)
	} else {
		number = json.Number(raw)
	}
	seconds, err := strconv.ParseInt(number.String(), 10, 64)
	if err != nil || seconds <= 0 || seconds > math.MaxInt64/int64(time.Second) {
		return 0, errors.New(CodeClientCredential, "token expiry is invalid")
	}
	return seconds, nil
}

func fetchGCPIDToken(ctx context.Context, audience string, binding CredentialBinding) (Credential, error) {
	if strings.TrimSpace(audience) == "" {
		return Credential{}, errors.New(CodeClientCredential, "GCP ID token audience is empty")
	}
	rawEndpoint := strings.TrimSpace(binding.MetadataURL)
	allowInsecure := binding.AllowInsecure
	if rawEndpoint == "" {
		rawEndpoint = gcpMetadataHost + gcpIdentityPath
		allowInsecure = true
	}
	endpoint, err := parseCredentialURL(rawEndpoint, allowInsecure)
	if err != nil {
		return Credential{}, err
	}
	query := url.Values{"audience": []string{audience}, "format": []string{"full"}}
	endpoint.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return Credential{}, errors.New(CodeClientCredential, "build metadata token request")
	}
	request.Header.Set("Metadata-Flavor", "Google")
	response, err := safeCredentialHTTPClient(nil).Do(request)
	if err != nil {
		return Credential{}, errors.New(CodeClientCredential, "metadata token request failed")
	}
	defer response.Body.Close() //nolint:errcheck // best effort after bounded read
	if response.StatusCode != http.StatusOK {
		return Credential{}, errors.Newf(CodeClientCredential, "metadata server returned status %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, credentialMaxBody+1))
	if err != nil {
		return Credential{}, errors.New(CodeClientCredential, "read metadata token response")
	}
	if len(body) > credentialMaxBody {
		return Credential{}, errors.New(CodeClientCredential, "metadata token response exceeds maximum size")
	}
	token := strings.TrimSpace(string(body))
	expiry := parseJWTExpiry(token)
	if token == "" || expiry.IsZero() || !expiry.After(time.Now()) {
		return Credential{}, errors.New(CodeClientCredential, "metadata server returned an invalid ID token")
	}
	return Credential{Value: token, Expiry: expiry}, nil
}

func safeCredentialHTTPClient(base *http.Client) *http.Client {
	var client http.Client
	if base != nil {
		client = *base
	}
	if client.Timeout == 0 {
		client.Timeout = defaultCredentialTimeout
	}
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	return &client
}

func parseCredentialURL(raw string, allowInsecure bool) (*url.URL, error) {
	parsed, err := parseBoundURL(raw, allowInsecure)
	if err != nil {
		return nil, errors.Wrapf(err, CodeClientCredential, "invalid credential endpoint")
	}
	return parsed, nil
}

func parseJWTExpiry(token string) time.Time {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[1] == "" {
		return time.Time{}
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil || claims.Exp <= 0 {
		return time.Time{}
	}
	return time.Unix(claims.Exp, 0)
}

func sortedUnique(values []string) []string {
	out := append([]string(nil), values...)
	sort.Strings(out)
	write := 0
	for _, value := range out {
		if value == "" || (write > 0 && out[write-1] == value) {
			continue
		}
		out[write] = value
		write++
	}
	return out[:write]
}
