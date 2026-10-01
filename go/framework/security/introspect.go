package security

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.putnami.dev/cache"
	"go.putnami.dev/ctxutil"
	"go.putnami.dev/errors"
	phttp "go.putnami.dev/http"
	identity "go.putnami.dev/protocol/identity/schema"
)

// Introspection error code and defaults.
const (
	// CodeIntrospect labels failures of the RFC 7662 introspection request.
	CodeIntrospect errors.Code = "security.introspect"

	defaultIntrospectTimeout  = 5 * time.Second
	defaultIntrospectCacheTTL = 5 * time.Minute
	maxIntrospectionSize      = 64 << 10 // 64 KiB cap on the introspection response body
	introspectCachePrefix     = "introspect:"

	// maxIntrospectRetryDelay caps the wait before the single retry of a 429
	// Too Many Requests, whatever Retry-After asks for, so backpressure costs a
	// request at most this much extra latency before it fails closed.
	maxIntrospectRetryDelay = time.Second
	// minIntrospectRetryJitter and maxIntrospectRetryJitter bound the delay
	// before that retry when the 429 carries no usable Retry-After:
	// [minIntrospectRetryJitter, maxIntrospectRetryJitter).
	minIntrospectRetryJitter = 50 * time.Millisecond
	maxIntrospectRetryJitter = 250 * time.Millisecond

	// clientAssertionTypeJWTBearer is the RFC 7523 §2.2 client_assertion_type
	// value announcing that the accompanying client_assertion parameter is a
	// signed JWT proving this resource server's identity.
	clientAssertionTypeJWTBearer = "urn:ietf:params:oauth:client-assertion-type:jwt-bearer"
)

// errMissingIntrospectionEndpoint is reported during lazy setup when neither an
// explicit Endpoint nor an Issuer (for discovery) was configured.
var errMissingIntrospectionEndpoint = &ValidationError{"introspection endpoint not configured"}

// ClientAuthMethod selects how ClientID/ClientSecret authenticate this resource
// server to the introspection endpoint. Both are permitted client
// authentication methods under RFC 6749 §2.3.1 (referenced by RFC 7662 §2.1).
type ClientAuthMethod int

const (
	// ClientAuthBasic sends the credentials in the Authorization header via HTTP
	// Basic (client_secret_basic). This is the default and the RFC-preferred
	// method.
	ClientAuthBasic ClientAuthMethod = iota

	// ClientAuthPostBody sends client_id and client_secret as form parameters in
	// the request body (client_secret_post). Use it for introspection endpoints
	// that read the credentials from the body and ignore the Authorization
	// header.
	ClientAuthPostBody
)

// IntrospectConfig configures opaque bearer token validation via RFC 7662
// OAuth2 Token Introspection.
type IntrospectConfig struct {
	// Endpoint is the RFC 7662 introspection endpoint URL. If empty and Issuer
	// is set, it is discovered from the issuer's OpenID/OAuth metadata
	// (introspection_endpoint), mirroring DiscoverJWKSURL.
	Endpoint string

	// Issuer is the OIDC/OAuth issuer URL used to discover Endpoint when it is
	// not given. Ignored when Endpoint is set.
	Issuer string

	// ClientID and ClientSecret authenticate THIS resource server to the
	// introspection endpoint (RFC 7662 §2.1). They identify the caller of
	// /introspect, not the token being introspected. The transport is selected
	// by ClientAuth.
	ClientID     string
	ClientSecret string

	// ClientAuth selects how ClientID/ClientSecret are transmitted. The zero
	// value is ClientAuthBasic (HTTP Basic); set ClientAuthPostBody for
	// endpoints that read the credentials from the request body instead.
	ClientAuth ClientAuthMethod

	// ClientAssertion, when non-nil, authenticates THIS resource server to the
	// introspection endpoint via RFC 7523: the returned token is sent as
	//   client_assertion_type=urn:ietf:params:oauth:client-assertion-type:jwt-bearer
	//   client_assertion=<token>
	// It is resolved once per upstream introspection request. When set it takes
	// precedence over ClientID/ClientSecret (which may be left empty), so a caller
	// can prove identity with a signed JWT (e.g. a GCP metadata-server ID token)
	// instead of a shared secret. A resolver error fails the introspection request
	// closed (no identity, no downgrade to ClientID/ClientSecret).
	ClientAssertion func(ctx context.Context) (string, error)

	// TokenTypeHint is the optional RFC 7662 §2.1 token_type_hint sent with the
	// request (e.g. "access_token").
	TokenTypeHint string

	// Audience, if set, requires the introspection response "aud" to contain
	// this value, so a token minted for a different resource server is rejected
	// even when the issuer reports it active. The check runs on every resolution
	// (fresh and cache-hit), so it is intentionally NOT part of the cache key —
	// a cache shared by resolvers with different audiences re-applies each one's
	// policy on read. Mirrors JWKSJWTConfig.Audience.
	Audience string

	// Cache backs successful (active) introspection results, namespaced by the
	// resolved endpoint plus this resource server's issuer/client credentials
	// and keyed by a sha256 digest that includes the token. nil disables
	// caching, so every request hits the endpoint. Inactive results are never
	// cached. Use cache.NewMemoryCache for a per-instance cache, or any
	// cache.Cache (disk/layered/external) to share results across instances.
	Cache cache.Cache

	// CacheTTL bounds how long an active result is cached. Default 5m. The
	// effective TTL is capped by the token's own exp when present, so a cached
	// result never outlives the token.
	CacheTTL time.Duration

	// Timeout bounds each introspection HTTP request. Default 5s. On timeout the
	// resolver fails closed (no identity) rather than stalling the request.
	Timeout time.Duration

	// ClientAssertionTimeout bounds the leader's detached upstream operation —
	// the RFC 7523 ClientAssertion resolver call plus the introspection HTTP
	// request, and after a 429 the Retry-After wait and the single retry (which
	// is skipped when this budget cannot cover the wait). The leader runs on a
	// cancellation-detached context (so one waiter's cancellation cannot abort
	// the call the whole singleflight cohort shares); detaching also strips the
	// request deadline, so without this bound a hung ClientAssertion resolver
	// would wedge every waiter indefinitely.
	// Default: the effective Timeout (5s). It is applied even when
	// ClientAssertion is nil, so a stalled endpoint dial before Timeout's
	// per-request bound engages still fails closed. Set a larger value only if a
	// slow assertion resolver (e.g. a metadata-server round trip) needs more
	// headroom than a single HTTP request.
	ClientAssertionTimeout time.Duration

	// AllowInsecure permits a plaintext http endpoint from non-loopback hosts.
	// By default only https (and loopback http for local development) is allowed,
	// so a network attacker cannot intercept the introspection exchange. Do not
	// enable in production.
	AllowInsecure bool

	// Breaker, when enabled (FailureThreshold > 0), wraps the upstream
	// introspection call in a circuit breaker so a sustained endpoint outage does
	// not cost every uncached request a full Timeout stall. After
	// FailureThreshold consecutive upstream failures the breaker opens for
	// OpenDuration: while open, an uncached token fails closed IMMEDIATELY without
	// an upstream call (counted as breaker_open). Cached active results still
	// serve from the cache for their TTL — the breaker only gates the network, so
	// recently introspected tokens keep working through the outage. On expiry a
	// single trial request probes the endpoint; success closes the breaker,
	// failure re-opens it. A 429 Too Many Requests is backpressure from a live
	// endpoint, not an outage: it is retried once after Retry-After (see
	// Introspect), and a 429 that persists fails only that request closed
	// (counted as throttled). It neither counts toward FailureThreshold nor
	// resets the failure run, and a throttled half-open trial frees its probe
	// slot for the next caller. The zero value disables the breaker:
	// introspection behaves exactly as before.
	Breaker BreakerConfig
}

// BreakerConfig configures the introspection circuit breaker (IntrospectConfig.Breaker).
type BreakerConfig struct {
	// FailureThreshold is the number of consecutive upstream failures (transport
	// error, timeout, or a non-200 status including rejected client credentials,
	// but never a 429 Too Many Requests) that opens the breaker. Zero or
	// negative disables the breaker entirely.
	FailureThreshold int

	// OpenDuration is how long the breaker stays open — failing uncached tokens
	// closed without an upstream call — before letting a single trial request
	// through. Defaults to 30s when FailureThreshold > 0.
	OpenDuration time.Duration
}

// Introspect returns identity-resolver middleware that validates opaque bearer
// tokens via RFC 7662 OAuth2 Token Introspection. Constructing it is the opt-in:
// a server that never calls Introspect never makes a per-request introspection
// call.
//
// On each request it extracts the bearer token, consults the cache (if any),
// and otherwise POSTs the token (plus optional token_type_hint) to the
// introspection endpoint authenticated with ClientID/ClientSecret — or, when
// ClientAssertion is set, with an RFC 7523 client assertion (a signed JWT that
// takes precedence over the shared secret). An active
// response is mapped to phttp.Claims via phttp.ClaimsFromMap (standard claims to
// typed fields, the rest to Claims.Extra) and cached; an inactive response
// yields no identity and is never cached. When Audience is set, an active token
// whose "aud" does not include it is rejected. Transport errors, timeouts, and
// non-OK responses fail closed (no identity, => 401 downstream). A 429 Too Many
// Requests is backpressure, not an outage: the upstream call is retried once
// after the endpoint's Retry-After (delta-seconds or HTTP-date, capped at 1s; a
// short jittered delay when absent), within ClientAssertionTimeout. A 429 that
// persists fails that request closed and never trips the circuit breaker.
// Concurrent requests for the same not-yet-cached token are collapsed into a
// single upstream call, including that retry.
//
// Like the other resolvers it chains first-win: if an earlier resolver already
// set ctx.User, Introspect does nothing. Resolver ordering and any
// issuer-specific fast-path (e.g. skipping tokens by prefix) belong in the
// consumer.
//
// Usage:
//
//	server.Use(security.Introspect(security.IntrospectConfig{
//	    Issuer:       "https://auth.example.com",
//	    ClientID:     os.Getenv("INTROSPECT_CLIENT_ID"),
//	    ClientSecret: os.Getenv("INTROSPECT_CLIENT_SECRET"),
//	    Cache:        cache.NewMemoryCache(cache.MemoryConfig{}),
//	}))
//
//	// Or secret-free, proving identity with a signed JWT (RFC 7523), e.g. a
//	// GCP metadata-server ID token:
//	server.Use(security.Introspect(security.IntrospectConfig{
//	    Issuer:          "https://auth.example.com",
//	    ClientAssertion: fetchIDToken, // func(ctx context.Context) (string, error)
//	    Cache:           cache.NewMemoryCache(cache.MemoryConfig{}),
//	}))
func Introspect(cfg IntrospectConfig) phttp.Middleware {
	if cfg.CacheTTL <= 0 {
		cfg.CacheTTL = defaultIntrospectCacheTTL
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultIntrospectTimeout
	}
	if cfg.ClientAssertionTimeout <= 0 {
		// Default the detached-leader bound to the per-request Timeout so the
		// ClientAssertion resolver (and any dial before Timeout engages) can
		// never run unbounded on the deadline-stripped detached context.
		cfg.ClientAssertionTimeout = cfg.Timeout
	}

	client := secureClient(cfg.Timeout, cfg.AllowInsecure)

	// Resolve the endpoint lazily so construction never blocks on the network.
	// Successful resolution is memoized; failures are intentionally left
	// retryable so a transient discovery outage does not disable the resolver
	// until process restart.
	var (
		endpointMu sync.Mutex
		endpoint   string
	)
	resolveEndpoint := func() (string, error) {
		endpointMu.Lock()
		defer endpointMu.Unlock()
		if endpoint != "" {
			return endpoint, nil
		}

		switch {
		case cfg.Endpoint != "":
			if err := requireSecureURL(cfg.Endpoint, cfg.AllowInsecure); err != nil {
				return "", err
			}
			endpoint = cfg.Endpoint
		case cfg.Issuer != "":
			discovered, err := DiscoverIntrospectionURL(cfg.Issuer, cfg.AllowInsecure)
			if err != nil {
				return "", err
			}
			endpoint = discovered
		default:
			return "", errMissingIntrospectionEndpoint
		}
		return endpoint, nil
	}

	// group collapses concurrent introspections of the same token so a burst of
	// requests for a not-yet-cached token triggers a single upstream call.
	group := &introspectGroup{}

	// breaker (nil when disabled) short-circuits the upstream call after a run of
	// consecutive failures, so a sustained endpoint outage stops costing every
	// uncached request a full Timeout. It gates only the network — the cache
	// lookup above still serves recently introspected tokens while it is open.
	breaker := newCircuitBreaker(cfg.Breaker)

	return IdentityResolver(func(ctx *phttp.Context) *phttp.Claims {
		token := extractBearerToken(ctx)
		if token == "" {
			return nil
		}

		endpoint, err := resolveEndpoint()
		if err != nil {
			decisionLogger().Error("token introspection unavailable: endpoint not resolved", nil,
				slog.String("error", err.Error()))
			return nil
		}

		key := introspectCacheKey(endpoint, cfg, token)
		reqCtx := ctx.Context()

		// Cache hit: re-map the stored payload so each request gets a fresh,
		// independently-owned Claims (the cache returns a private byte copy), and
		// re-apply the audience policy — it is deliberately not in the cache key,
		// so a cache shared across audiences enforces each resolver's on read.
		if cfg.Cache != nil {
			if raw, ok := cfg.Cache.Get(reqCtx, key); ok {
				if payload, active := parseIntrospection(raw); active {
					if checkAudience(payload, cfg.Audience) != nil {
						recordIntrospect(introspectAudienceMismatch)
						return nil
					}
					recordIntrospect(introspectCacheHit)
					return claimsFromIntrospection(payload)
				}
			}
		}

		// Circuit breaker: after a run of upstream failures, fail an uncached
		// token closed immediately instead of stalling on a Timeout against a dead
		// endpoint. This runs AFTER the cache lookup, so tokens introspected before
		// the outage keep resolving from cache for their TTL. During the half-open
		// probe window `trial` marks the single caller admitted to test the
		// endpoint; it is threaded into record() below to release the probe slot.
		allowed, trial := breaker.allow()
		if !allowed {
			recordIntrospect(introspectBreakerOpen)
			decisionLogger().Warn("token introspection short-circuited: circuit breaker open")
			return nil
		}

		// Collapse concurrent introspections of the same token. The leader runs
		// the upstream call and cache write with a cancellation-detached context
		// (one waiter's cancellation must not abort the call the cohort shares);
		// every caller then maps the shared raw result to its own Claims below.
		//
		// Detaching also strips reqCtx's deadline, so the detached context is
		// re-bounded IMMEDIATELY with ClientAssertionTimeout: otherwise a hung
		// ClientAssertion resolver (which runs on this context before any HTTP
		// request, so client.Timeout does not bound it) would wedge the whole
		// singleflight cohort indefinitely. Fail closed, not forever.
		raw, status, err := group.do(key, func() ([]byte, int, error) {
			leaderCtx, cancel := ctxutil.WithRequestTimeout(
				context.WithoutCancel(reqCtx), cfg.ClientAssertionTimeout)
			defer cancel()
			return introspectAndCache(leaderCtx, client, endpoint, token, key, cfg)
		})
		// Fold the upstream outcome into the breaker. Recorded here — after
		// group.do, per admitted request — not inside the leader closure: a trial
		// holder that collapses onto an in-flight call never runs the closure, so
		// recording there could strand its half-open probe slot and wedge the
		// breaker open. An endpoint that answered 200 (active or inactive) resets
		// it; a transport error, timeout, or any other non-200 trips it toward
		// open. A 429 that outlived the leader's single retry is neither: the
		// endpoint is alive and shedding load, so release() only frees a
		// half-open probe slot and leaves the failure run untouched —
		// backpressure never opens the breaker. Denied requests returned above
		// and never reach here, so the half-open probe (trial) always records or
		// releases. Under closed-state concurrency waiters share one call's
		// outcome, so a burst opens the breaker a little sooner — the safe
		// direction for an outage.
		if err == nil && status == http.StatusTooManyRequests {
			breaker.release(trial)
		} else {
			breaker.record(err == nil && status == http.StatusOK, trial)
		}
		if err != nil {
			// Transport error or timeout: fail closed.
			recordIntrospect(introspectEndpointError)
			decisionLogger().Warn("token introspection request failed", slog.String("error", err.Error())) //nolint:gosec // G706: error is from the framework HTTP client to the trusted introspection endpoint, not per-request user input
			return nil
		}
		switch {
		case status == http.StatusTooManyRequests:
			// The endpoint still throttled after the single bounded retry (or the
			// leader's budget could not cover the Retry-After wait). Fail THIS
			// request closed — a 429 never authenticates — under a distinct
			// outcome, so shed load is not mistaken for an outage.
			recordIntrospect(introspectThrottled)
			decisionLogger().Warn("token introspection throttled", slog.Int("status", status)) //nolint:gosec // G706: status is from the trusted introspection endpoint, not per-request user input
			return nil
		case status == http.StatusUnauthorized || status == http.StatusForbidden:
			// Distinct ERROR (vs the silent inactive path): the endpoint rejected
			// THIS server's client credentials — a deployment misconfiguration.
			recordIntrospect(introspectCredentialsRejected)
			decisionLogger().Error("token introspection rejected the resource server's client credentials (check ClientID/ClientSecret)", nil, slog.Int("status", status)) //nolint:gosec // G706: status is from the trusted introspection endpoint, not per-request user input
			return nil
		case status != http.StatusOK:
			recordIntrospect(introspectEndpointError)
			decisionLogger().Warn("token introspection endpoint returned an unexpected status", slog.Int("status", status)) //nolint:gosec // G706: status is from the trusted introspection endpoint, not per-request user input
			return nil
		}

		payload, active := parseIntrospection(raw)
		if !active {
			// Inactive results are never cached (the leader's cache write skips
			// them), so a just-minted or just-reactivated token validates on the
			// next call.
			recordIntrospect(introspectInactive)
			return nil
		}
		if checkAudience(payload, cfg.Audience) != nil {
			recordIntrospect(introspectAudienceMismatch)
			return nil
		}
		recordIntrospect(introspectActive)
		return claimsFromIntrospection(payload)
	})
}

// introspectAndCache performs one upstream introspection call and, on an active
// 200 response, writes it to the cache. It is the unit collapsed by the
// singleflight group, so it runs at most once per concurrent burst of identical
// tokens; each caller maps the returned raw body to its own Claims.
//
// A 429 Too Many Requests is retried exactly once, after the endpoint's
// Retry-After. The retry lives here, inside the singleflight leader, so a
// throttled burst of identical tokens waits on one retry instead of each caller
// retrying. When reqCtx cannot cover the wait, the retry is skipped and the 429
// is returned as is: running out of the leader's own budget while backing off
// is not an endpoint failure. The retry's own outcome (200, 429, 5xx, transport
// error) is returned unchanged, and a second 429 is never retried.
func introspectAndCache(reqCtx context.Context, client *http.Client, endpoint, token, key string, cfg IntrospectConfig) ([]byte, int, error) {
	recordIntrospect(introspectEndpointCall)
	raw, status, retryAfter, err := introspectToken(reqCtx, client, endpoint, token, cfg)
	if err == nil && status == http.StatusTooManyRequests {
		if !waitIntrospectRetry(reqCtx, introspectRetryDelay(retryAfter, time.Now())) {
			return raw, status, nil
		}
		// The retry re-resolves the ClientAssertion: it is a fresh upstream
		// request, and assertions are resolved once per upstream request.
		recordIntrospect(introspectEndpointCall)
		raw, status, _, err = introspectToken(reqCtx, client, endpoint, token, cfg)
	}
	if err != nil || status != http.StatusOK || cfg.Cache == nil {
		return raw, status, err
	}
	if payload, active := parseIntrospection(raw); active {
		if ttl := introspectCacheTTL(payload, cfg.CacheTTL); ttl > 0 {
			if cerr := cfg.Cache.Set(reqCtx, key, raw, ttl); cerr != nil {
				decisionLogger().Warn("caching introspection result failed", slog.String("error", cerr.Error())) //nolint:gosec // G706: error is a framework cache failure, not per-request user input
			}
		}
	}
	return raw, status, err
}

// introspectRetryDelay returns how long a throttled introspection waits before
// its single retry, from the 429's Retry-After header (RFC 9110 §10.2.3): either
// delta-seconds or an HTTP-date resolved against now. Every result is clamped to
// [0, maxIntrospectRetryDelay], so an oversized value cannot hold a request for
// long and a past date retries at once. An absent or malformed header (a sign, a
// fraction, garbage) yields a short jittered delay instead, so a cohort of
// resource servers throttled at the same instant does not retry in lockstep.
func introspectRetryDelay(retryAfter string, now time.Time) time.Duration {
	value := strings.TrimSpace(retryAfter)
	// delta-seconds is 1*DIGIT: no sign, no fraction.
	if value != "" && strings.Trim(value, "0123456789") == "" {
		seconds, err := strconv.ParseUint(value, 10, 64)
		// All digits, so the only possible error is a value too large for uint64.
		if err != nil || seconds > uint64(maxIntrospectRetryDelay/time.Second) {
			return maxIntrospectRetryDelay
		}
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil {
		return min(max(at.Sub(now), 0), maxIntrospectRetryDelay)
	}
	// Non-cryptographic jitter: it only spreads retries, it guards no secret.
	return minIntrospectRetryJitter + rand.N(maxIntrospectRetryJitter-minIntrospectRetryJitter)
}

// waitIntrospectRetry waits delay before the single retry of a throttled
// introspection. It reports false — skip the retry and keep the 429 — when ctx
// is already done, when its deadline falls within delay (the retry could not
// run inside the budget anyway), or when ctx ends during the wait.
func waitIntrospectRetry(ctx context.Context, delay time.Duration) bool {
	if ctx.Err() != nil {
		return false
	}
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= delay {
		return false
	}
	if delay <= 0 {
		return true
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// introspectGroup collapses concurrent introspections of the same cache key into
// a single in-flight upstream call (a focused, dependency-free singleflight).
// Waiters share the leader's raw result and status; each maps it to its own
// Claims, so no claim state is shared across requests.
type introspectGroup struct {
	mu    sync.Mutex
	calls map[string]*introspectCall
}

type introspectCall struct {
	wg     sync.WaitGroup
	raw    []byte
	status int
	err    error
}

// do runs fn unless an identical key is already in flight, in which case it
// waits for and returns the in-flight call's result.
func (g *introspectGroup) do(key string, fn func() ([]byte, int, error)) ([]byte, int, error) {
	g.mu.Lock()
	if g.calls == nil {
		g.calls = make(map[string]*introspectCall)
	}
	if c, ok := g.calls[key]; ok {
		g.mu.Unlock()
		c.wg.Wait()
		return c.raw, c.status, c.err
	}
	c := &introspectCall{}
	c.wg.Add(1)
	g.calls[key] = c
	g.mu.Unlock()

	c.raw, c.status, c.err = fn()

	g.mu.Lock()
	delete(g.calls, key)
	g.mu.Unlock()
	c.wg.Done()

	return c.raw, c.status, c.err
}

// introspectToken POSTs the token to the introspection endpoint and returns the
// bounded response body, the HTTP status, and the response's raw Retry-After
// header (consulted only on a 429). The resource server authenticates with
// its ClientID/ClientSecret via the method selected by cfg.ClientAuth (RFC 7662
// §2.1): HTTP Basic by default, or form-body parameters for ClientAuthPostBody.
// When cfg.ClientAssertion is set it takes precedence: the resolved JWT is sent
// as RFC 7523 client_assertion/client_assertion_type form-body parameters
// (always the body, regardless of ClientAuth) and ClientID/ClientSecret are not
// transmitted at all. A resolver failure — an error or an empty assertion —
// aborts before any request is sent: fail closed, never downgrade to the shared
// secret.
func introspectToken(reqCtx context.Context, client *http.Client, endpoint, token string, cfg IntrospectConfig) ([]byte, int, string, error) {
	form := url.Values{}
	form.Set("token", token)
	if cfg.TokenTypeHint != "" {
		form.Set("token_type_hint", cfg.TokenTypeHint)
	}
	useAssertion := cfg.ClientAssertion != nil
	if useAssertion {
		assertion, err := cfg.ClientAssertion(reqCtx)
		if err != nil {
			return nil, 0, "", errors.Wrapf(err, CodeIntrospect, "resolve client assertion")
		}
		if assertion == "" {
			return nil, 0, "", errors.New(CodeIntrospect, "client assertion resolver returned an empty assertion")
		}
		// RFC 7523 §2.2: the assertion always travels as body parameters,
		// regardless of ClientAuth.
		form.Set("client_assertion_type", clientAssertionTypeJWTBearer)
		form.Set("client_assertion", assertion)
	}
	// client_secret_post: credentials must be in the body, so set them before
	// the body reader is built below. Skipped when a client assertion is in
	// use — the assertion replaces the shared secret entirely.
	if !useAssertion && cfg.ClientAuth == ClientAuthPostBody {
		if cfg.ClientID != "" {
			form.Set("client_id", cfg.ClientID)
		}
		if cfg.ClientSecret != "" {
			form.Set("client_secret", cfg.ClientSecret)
		}
	}
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, 0, "", errors.Wrapf(err, CodeIntrospect, "build introspection request")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	// client_secret_basic (default): credentials travel in the Authorization
	// header instead of the body. Skipped when a client assertion is in use.
	if !useAssertion && cfg.ClientAuth != ClientAuthPostBody && (cfg.ClientID != "" || cfg.ClientSecret != "") {
		req.SetBasicAuth(cfg.ClientID, cfg.ClientSecret)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, "", errors.Wrapf(err, CodeIntrospect, "introspection request")
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort close on HTTP response

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxIntrospectionSize))
	if err != nil {
		return nil, resp.StatusCode, "", errors.Wrapf(err, CodeIntrospect, "read introspection response")
	}
	return raw, resp.StatusCode, resp.Header.Get("Retry-After"), nil
}

// parseIntrospection decodes an introspection response body, returning the
// payload and whether the token is active (RFC 7662 §2.2). A malformed body is
// treated as inactive.
func parseIntrospection(raw []byte) (map[string]any, bool) {
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, false
	}
	active, ok := payload["active"].(bool)
	return payload, ok && active
}

// claimsFromIntrospection maps an active introspection payload to Claims. The
// RFC 7662 "active" envelope flag is dropped (it gates authentication; it is not
// an identity claim); everything else flows through phttp.ClaimsFromMap. The
// caller must pass a freshly-parsed map, since this mutates it.
func claimsFromIntrospection(payload map[string]any) *phttp.Claims {
	delete(payload, "active")
	return phttp.ClaimsFromMap(payload)
}

// introspectCacheTTL caps the configured TTL by the token's own exp so a cached
// result never outlives the token. It returns 0 (do not cache) when exp has
// already passed.
func introspectCacheTTL(payload map[string]any, maxTTL time.Duration) time.Duration {
	exp, ok := payload[string(identity.ClaimNameExp)].(float64)
	if !ok {
		return maxTTL
	}
	until := time.Until(time.Unix(int64(exp), 0))
	if until <= 0 {
		return 0
	}
	return min(until, maxTTL)
}

// introspectCacheKey derives the cache key from the token and the
// configuration that defines its validation authority. The raw token and client
// secret are never used directly as a key, so they never sit in a cache map or
// on disk in the clear.
//
// ClientAssertion is deliberately NOT part of the key: the assertion is a
// per-request-varying JWT (fresh exp/iat on every resolution, and a func cannot
// be hashed) that identifies the caller of /introspect, while the cached RESULT
// describes only the token being introspected — the same endpoint+token pair
// yields the same result regardless of which client identity asked, so sharing
// entries across assertion identities is safe (mirroring how Audience is
// re-checked on read instead of keyed).
func introspectCacheKey(endpoint string, cfg IntrospectConfig, token string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		endpoint,
		cfg.Issuer,
		cfg.ClientID,
		cfg.ClientSecret,
		token,
	}, "\x00")))
	return introspectCachePrefix + hex.EncodeToString(sum[:])
}

// DiscoverIntrospectionURL fetches the issuer's OpenID/OAuth metadata document
// and returns its introspection_endpoint. It mirrors DiscoverJWKSURL: the
// issuer and the discovered endpoint must use https unless allowInsecure is set
// (loopback http is always permitted for local development). RFC 8414 lets an
// authorization server advertise introspection_endpoint in the same metadata
// that carries jwks_uri.
func DiscoverIntrospectionURL(issuer string, allowInsecure bool) (string, error) {
	var doc struct {
		IntrospectionEndpoint string `json:"introspection_endpoint"`
	}
	if err := fetchOIDCDiscovery(issuer, allowInsecure, &doc); err != nil {
		return "", err
	}
	return requireDiscoveredEndpoint(doc.IntrospectionEndpoint, "introspection_endpoint", allowInsecure)
}
