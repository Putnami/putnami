package security

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"expvar"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.putnami.dev/cache"
	phttp "go.putnami.dev/http"
	"go.putnami.dev/protocol/features/spectest"
)

// --- Test helpers ---

// newIntrospectServer starts a test introspection endpoint that runs handler
// for each request and counts the calls.
func newIntrospectServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func writeIntrospectJSON(t *testing.T, w http.ResponseWriter, payload map[string]any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		t.Fatalf("encode introspection response: %v", err)
	}
}

// runIntrospect invokes the resolver middleware for a single request carrying
// the given bearer token (empty token => no Authorization header) and returns
// the populated context.
func runIntrospect(mw phttp.Middleware, token string) *phttp.Context {
	req := httptest.NewRequest("GET", "/", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	ctx := phttp.NewContext(httptest.NewRecorder(), req)
	mw(ctx, func() *phttp.Response { return phttp.JSON("ok") })
	return ctx
}

func newTestCache(t *testing.T) cache.Cache {
	t.Helper()
	c := cache.NewMemoryCache(cache.MemoryConfig{CleanupInterval: -1})
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func futureExp(d time.Duration) float64 {
	return float64(time.Now().Add(d).Unix())
}

// activeJSON is the canonical "active token with a far-future expiry" payload the
// cache tests serve; only the subject varies.
func activeJSON(sub string) map[string]any {
	return map[string]any{"active": true, "sub": sub, "exp": futureExp(time.Hour)}
}

// --- Resolver behavior ---

func TestIntrospect_ActiveToken(t *testing.T) {
	srv, calls := newIntrospectServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		if got := r.Form.Get("token"); got != "opaque-1" {
			t.Errorf("token field = %q, want opaque-1", got)
		}
		writeIntrospectJSON(t, w, map[string]any{
			"active":    true,
			"sub":       "user-1",
			"scope":     "read write",
			"client_id": "app-x",
			"exp":       futureExp(time.Hour),
			"custom":    "val",
		})
	})

	mw := Introspect(IntrospectConfig{Endpoint: srv.URL})
	ctx := runIntrospect(mw, "opaque-1")

	if ctx.User == nil {
		t.Fatal("expected user to be set for active token")
	}
	if ctx.User.Subject != "user-1" {
		t.Errorf("subject = %q, want user-1", ctx.User.Subject)
	}
	if len(ctx.User.Scopes) != 2 || ctx.User.Scopes[0] != "read" || ctx.User.Scopes[1] != "write" {
		t.Errorf("scopes = %v, want [read write]", ctx.User.Scopes)
	}
	if ctx.User.ClientID != "app-x" {
		t.Errorf("clientID = %q, want app-x", ctx.User.ClientID)
	}
	if ctx.User.Extra["custom"] != "val" {
		t.Errorf("extra[custom] = %v, want val", ctx.User.Extra["custom"])
	}
	if _, ok := ctx.User.Extra["active"]; ok {
		t.Error("RFC 7662 active envelope flag must not leak into Claims.Extra")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("endpoint calls = %d, want 1", got)
	}
}

func TestIntrospect_InactiveToken(t *testing.T) {
	spectest.Proves(t, "go/authentication-authorization", "credential-validation", "introspection-inactive-token-yields-no-identity")
	srv, _ := newIntrospectServer(t, func(w http.ResponseWriter, _ *http.Request) {
		writeIntrospectJSON(t, w, map[string]any{"active": false})
	})

	mw := Introspect(IntrospectConfig{Endpoint: srv.URL})
	ctx := runIntrospect(mw, "revoked")

	if ctx.User != nil {
		t.Error("expected user to be nil for inactive token")
	}
}

func TestIntrospect_NoBearerToken(t *testing.T) {
	srv, calls := newIntrospectServer(t, func(w http.ResponseWriter, _ *http.Request) {
		writeIntrospectJSON(t, w, map[string]any{"active": true, "sub": "x"})
	})

	mw := Introspect(IntrospectConfig{Endpoint: srv.URL})
	ctx := runIntrospect(mw, "")

	if ctx.User != nil {
		t.Error("expected user to be nil without a bearer token")
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("endpoint must not be called without a bearer token; calls = %d", got)
	}
}

func TestIntrospect_Timeout(t *testing.T) {
	spectest.Proves(t, "go/authentication-authorization", "credential-validation", "introspection-is-deadline-bounded")
	srv, _ := newIntrospectServer(t, func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		writeIntrospectJSON(t, w, map[string]any{"active": true, "sub": "slow"})
	})

	mw := Introspect(IntrospectConfig{Endpoint: srv.URL, Timeout: 30 * time.Millisecond})
	ctx := runIntrospect(mw, "tok")

	if ctx.User != nil {
		t.Error("expected fail-closed (nil user) when the endpoint exceeds Timeout")
	}
}

func TestIntrospect_ClientCredentialsAccepted(t *testing.T) {
	srv, _ := newIntrospectServer(t, func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "rs-id" || pass != "rs-secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		writeIntrospectJSON(t, w, map[string]any{"active": true, "sub": "ok"})
	})

	mw := Introspect(IntrospectConfig{Endpoint: srv.URL, ClientID: "rs-id", ClientSecret: "rs-secret"})
	ctx := runIntrospect(mw, "tok")

	if ctx.User == nil || ctx.User.Subject != "ok" {
		t.Fatalf("expected authenticated user with correct client credentials, got %+v", ctx.User)
	}
}

func TestIntrospect_BadClientCredentials(t *testing.T) {
	srv, _ := newIntrospectServer(t, func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "rs-id" || pass != "rs-secret" {
			// Endpoint rejects THIS server's credentials (deployment misconfig).
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		writeIntrospectJSON(t, w, map[string]any{"active": true, "sub": "ok"})
	})

	mw := Introspect(IntrospectConfig{Endpoint: srv.URL, ClientID: "wrong", ClientSecret: "wrong"})
	ctx := runIntrospect(mw, "tok")

	if ctx.User != nil {
		t.Error("expected fail-closed (nil user) when the endpoint rejects client credentials")
	}
}

func TestIntrospect_ClientAuthPostBody(t *testing.T) {
	srv, _ := newIntrospectServer(t, func(w http.ResponseWriter, r *http.Request) {
		// client_secret_post: credentials must arrive in the form body, and the
		// Authorization header must be absent.
		if _, _, ok := r.BasicAuth(); ok {
			t.Error("ClientAuthPostBody must not send an Authorization header")
		}
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		if r.Form.Get("client_id") != "rs-id" || r.Form.Get("client_secret") != "rs-secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		writeIntrospectJSON(t, w, map[string]any{"active": true, "sub": "body-ok"})
	})

	mw := Introspect(IntrospectConfig{
		Endpoint:     srv.URL,
		ClientID:     "rs-id",
		ClientSecret: "rs-secret",
		ClientAuth:   ClientAuthPostBody,
	})
	ctx := runIntrospect(mw, "tok")

	if ctx.User == nil || ctx.User.Subject != "body-ok" {
		t.Fatalf("expected authenticated user with body-param client credentials, got %+v", ctx.User)
	}
}

func TestIntrospect_ClientAuthBasicKeepsCredentialsOutOfBody(t *testing.T) {
	srv, _ := newIntrospectServer(t, func(w http.ResponseWriter, r *http.Request) {
		// Default (client_secret_basic): credentials belong in the header, never
		// the body, so an endpoint reading body params sees none.
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		if r.Form.Has("client_id") || r.Form.Has("client_secret") {
			t.Error("ClientAuthBasic must not leak credentials into the form body")
		}
		user, pass, ok := r.BasicAuth()
		if !ok || user != "rs-id" || pass != "rs-secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		writeIntrospectJSON(t, w, map[string]any{"active": true, "sub": "basic-ok"})
	})

	mw := Introspect(IntrospectConfig{Endpoint: srv.URL, ClientID: "rs-id", ClientSecret: "rs-secret"})
	ctx := runIntrospect(mw, "tok")

	if ctx.User == nil || ctx.User.Subject != "basic-ok" {
		t.Fatalf("expected authenticated user via HTTP Basic, got %+v", ctx.User)
	}
}

// --- RFC 7523 client-assertion authentication ---

// TestIntrospect_ClientAssertionActiveToken is the assertion-path happy path:
// a secret-free resolver (no ClientID/ClientSecret) authenticates via the RFC
// 7523 body parameters, an active token maps to Claims exactly like the
// shared-secret path, and the resolver runs once per upstream call — a cache
// hit must not re-resolve the assertion.
func TestIntrospect_ClientAssertionActiveToken(t *testing.T) {
	srv, calls := newIntrospectServer(t, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		if got := r.Form.Get("client_assertion_type"); got != clientAssertionTypeJWTBearer {
			t.Errorf("client_assertion_type = %q, want %q", got, clientAssertionTypeJWTBearer)
		}
		if r.Form.Get("client_assertion") != "signed-jwt" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		writeIntrospectJSON(t, w, map[string]any{
			"active": true,
			"sub":    "assertion-user",
			"scope":  "read write",
			"exp":    futureExp(time.Hour),
		})
	})

	var resolverCalls atomic.Int64
	mw := Introspect(IntrospectConfig{
		Endpoint: srv.URL,
		Cache:    newTestCache(t),
		ClientAssertion: func(context.Context) (string, error) {
			resolverCalls.Add(1)
			return "signed-jwt", nil
		},
	})

	first := runIntrospect(mw, "tok")
	second := runIntrospect(mw, "tok")

	if first.User == nil || first.User.Subject != "assertion-user" {
		t.Fatalf("expected authenticated user via client assertion, got %+v", first.User)
	}
	if len(first.User.Scopes) != 2 || first.User.Scopes[0] != "read" || first.User.Scopes[1] != "write" {
		t.Errorf("scopes = %v, want [read write]", first.User.Scopes)
	}
	if second.User == nil || second.User.Subject != "assertion-user" {
		t.Fatalf("expected second request served from cache, got %+v", second.User)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("endpoint calls = %d, want 1 (second served from cache)", got)
	}
	if got := resolverCalls.Load(); got != 1 {
		t.Errorf("assertion resolver calls = %d, want 1 (resolved once per upstream call, not per request)", got)
	}
}

// TestIntrospect_ClientAssertionPrecedence proves that a non-nil
// ClientAssertion takes precedence over ClientID/ClientSecret for BOTH
// ClientAuth transports: the upstream request carries the RFC 7523 body
// parameters and neither an Authorization: Basic header nor
// client_id/client_secret body params, even though the shared secret is
// configured.
func TestIntrospect_ClientAssertionPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name string
		auth ClientAuthMethod
	}{
		{"over basic", ClientAuthBasic},
		{"over post body", ClientAuthPostBody},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newIntrospectServer(t, func(w http.ResponseWriter, r *http.Request) {
				if _, _, ok := r.BasicAuth(); ok {
					t.Error("client assertion must not be accompanied by an Authorization: Basic header")
				}
				if err := r.ParseForm(); err != nil {
					t.Fatalf("parse form: %v", err)
				}
				if r.Form.Has("client_id") || r.Form.Has("client_secret") {
					t.Error("client assertion must not leak client_id/client_secret into the form body")
				}
				if got := r.Form.Get("client_assertion_type"); got != clientAssertionTypeJWTBearer {
					t.Errorf("client_assertion_type = %q, want %q", got, clientAssertionTypeJWTBearer)
				}
				if r.Form.Get("client_assertion") != "signed-jwt" {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				writeIntrospectJSON(t, w, map[string]any{"active": true, "sub": "assertion-ok"})
			})

			mw := Introspect(IntrospectConfig{
				Endpoint:        srv.URL,
				ClientID:        "rs-id",     // must be ignored: the assertion takes precedence
				ClientSecret:    "rs-secret", // must be ignored: the assertion takes precedence
				ClientAuth:      tc.auth,
				ClientAssertion: func(context.Context) (string, error) { return "signed-jwt", nil },
			})
			ctx := runIntrospect(mw, "tok")

			if ctx.User == nil || ctx.User.Subject != "assertion-ok" {
				t.Fatalf("expected authenticated user via client assertion, got %+v", ctx.User)
			}
		})
	}
}

// TestIntrospect_ClientAssertionFailClosed proves a resolver failure (an error
// or an empty assertion) fails the request closed: no identity is established
// and the endpoint is never contacted — in particular there is NO fallback to
// the configured ClientID/ClientSecret.
func TestIntrospect_ClientAssertionFailClosed(t *testing.T) {
	for _, tc := range []struct {
		name     string
		resolver func(context.Context) (string, error)
	}{
		{"resolver error", func(context.Context) (string, error) {
			return "", errors.New("metadata server unreachable")
		}},
		{"empty assertion", func(context.Context) (string, error) {
			return "", nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, calls := newIntrospectServer(t, func(w http.ResponseWriter, _ *http.Request) {
				// If reached, the resolver's failure leaked into an upstream call.
				writeIntrospectJSON(t, w, map[string]any{"active": true, "sub": "must-not-authenticate"})
			})

			mw := Introspect(IntrospectConfig{
				Endpoint:        srv.URL,
				ClientID:        "rs-id",     // must NOT be used as a fallback
				ClientSecret:    "rs-secret", // must NOT be used as a fallback
				ClientAssertion: tc.resolver,
			})
			ctx := runIntrospect(mw, "tok")

			if ctx.User != nil {
				t.Errorf("expected fail-closed (nil user) on assertion resolver failure, got %+v", ctx.User)
			}
			if got := calls.Load(); got != 0 {
				t.Errorf("endpoint must not be contacted (no secret-credential fallback) on resolver failure; calls = %d", got)
			}
		})
	}
}

// TestIntrospect_ClientAssertionTimeoutUnwedgesWaiters guards the timeout
// invariant: the singleflight leader runs the ClientAssertion resolver on a
// cancellation-detached context (context.WithoutCancel), which strips the
// request deadline. A hung resolver on that context must NOT block forever —
// the leader re-bounds it with ClientAssertionTimeout, so the resolver's ctx is
// canceled, the leader fails closed, and every waiter unblocks. Before the fix
// the resolver ran unbounded and the whole cohort wedged.
func TestIntrospect_ClientAssertionTimeoutUnwedgesWaiters(t *testing.T) {
	// The endpoint must never be reached: the resolver never returns an
	// assertion, so the leader fails closed before any HTTP request.
	srv, calls := newIntrospectServer(t, func(w http.ResponseWriter, _ *http.Request) {
		writeIntrospectJSON(t, w, activeJSON("must-not-authenticate"))
	})

	var (
		resolverEntered   sync.WaitGroup
		resolverSawCancel atomic.Bool
	)
	resolverEntered.Add(1)
	var enterOnce sync.Once
	mw := Introspect(IntrospectConfig{
		Endpoint:               srv.URL,
		ClientAssertionTimeout: 50 * time.Millisecond,
		ClientAssertion: func(ctx context.Context) (string, error) {
			enterOnce.Do(resolverEntered.Done)
			// Block on the detached-and-rebounded context. If it is honored the
			// deadline fires; a hard failsafe prevents a hung test if it is not.
			select {
			case <-ctx.Done():
				resolverSawCancel.Store(true)
				return "", ctx.Err()
			case <-time.After(10 * time.Second):
				return "", errors.New("resolver context was never bounded — it would wedge in production")
			}
		},
	})

	const n = 12
	var wg sync.WaitGroup
	results := make([]*phttp.Context, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = runIntrospect(mw, "tok")
		}(i)
	}

	// The whole cohort must finish well within the failsafe: the bounded
	// resolver expires at ~50ms, so a generous 5s ceiling proves "does not wedge
	// indefinitely" without flaking.
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("introspection waiters wedged: the ClientAssertion resolver ran unbounded on the detached context")
	}

	if !resolverSawCancel.Load() {
		t.Error("resolver did not observe context cancellation — ClientAssertionTimeout was not applied to the detached context")
	}
	for i, ctx := range results {
		if ctx.User != nil {
			t.Errorf("request %d: expected fail-closed (nil user) after resolver timeout, got %+v", i, ctx.User)
		}
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("endpoint must not be contacted when the assertion resolver times out; calls = %d", got)
	}
}

// TestIntrospect_ClientAssertionTimeoutDefaultsToTimeout proves the detached
// leader is bounded even when ClientAssertionTimeout is left zero: it defaults
// to the effective Timeout, so a hung resolver still fails closed rather than
// running unbounded on the deadline-stripped context.
func TestIntrospect_ClientAssertionTimeoutDefaultsToTimeout(t *testing.T) {
	srv, _ := newIntrospectServer(t, func(w http.ResponseWriter, _ *http.Request) {
		writeIntrospectJSON(t, w, activeJSON("must-not-authenticate"))
	})

	resolverSawCancel := make(chan struct{})
	var closeOnce sync.Once
	mw := Introspect(IntrospectConfig{
		Endpoint: srv.URL,
		Timeout:  50 * time.Millisecond, // ClientAssertionTimeout left zero => defaults to this
		ClientAssertion: func(ctx context.Context) (string, error) {
			select {
			case <-ctx.Done():
				closeOnce.Do(func() { close(resolverSawCancel) })
				return "", ctx.Err()
			case <-time.After(10 * time.Second):
				return "", errors.New("resolver context was never bounded")
			}
		},
	})

	result := make(chan *phttp.Context, 1)
	go func() { result <- runIntrospect(mw, "tok") }()

	select {
	case <-resolverSawCancel:
	case <-time.After(5 * time.Second):
		t.Fatal("resolver ran unbounded: a zero ClientAssertionTimeout did not default to Timeout")
	}

	select {
	case ctx := <-result:
		if ctx.User != nil {
			t.Errorf("expected fail-closed (nil user) after default-timeout resolver expiry, got %+v", ctx.User)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("introspection request wedged after resolver timeout")
	}
}

func TestIntrospect_CacheHit(t *testing.T) {
	srv, calls := newIntrospectServer(t, func(w http.ResponseWriter, _ *http.Request) {
		writeIntrospectJSON(t, w, activeJSON("cached"))
	})

	mw := Introspect(IntrospectConfig{Endpoint: srv.URL, Cache: newTestCache(t)})

	first := runIntrospect(mw, "tok")
	second := runIntrospect(mw, "tok")

	if first.User == nil || second.User == nil {
		t.Fatal("expected both requests to authenticate")
	}
	if first.User.Subject != "cached" || second.User.Subject != "cached" {
		t.Errorf("subjects = %q/%q, want cached", first.User.Subject, second.User.Subject)
	}
	if first.User == second.User {
		t.Error("each request should get an independently-owned Claims, not a shared pointer")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("endpoint calls = %d, want 1 (second served from cache)", got)
	}
}

func TestIntrospect_CacheScopedByEndpoint(t *testing.T) {
	shared := newTestCache(t)
	srvA, callsA := newIntrospectServer(t, func(w http.ResponseWriter, _ *http.Request) {
		writeIntrospectJSON(t, w, activeJSON("issuer-a"))
	})
	srvB, callsB := newIntrospectServer(t, func(w http.ResponseWriter, _ *http.Request) {
		writeIntrospectJSON(t, w, activeJSON("issuer-b"))
	})

	mwA := Introspect(IntrospectConfig{Endpoint: srvA.URL, Cache: shared})
	mwB := Introspect(IntrospectConfig{Endpoint: srvB.URL, Cache: shared})

	first := runIntrospect(mwA, "same-token")
	second := runIntrospect(mwB, "same-token")

	if first.User == nil || first.User.Subject != "issuer-a" {
		t.Fatalf("first resolver user = %+v, want issuer-a", first.User)
	}
	if second.User == nil || second.User.Subject != "issuer-b" {
		t.Fatalf("second resolver user = %+v, want issuer-b", second.User)
	}
	if got := callsA.Load(); got != 1 {
		t.Errorf("endpoint A calls = %d, want 1", got)
	}
	if got := callsB.Load(); got != 1 {
		t.Errorf("endpoint B calls = %d, want 1 (not served from endpoint A cache)", got)
	}
}

func TestIntrospect_CacheScopedByClientCredentials(t *testing.T) {
	shared := newTestCache(t)
	srv, calls := newIntrospectServer(t, func(w http.ResponseWriter, r *http.Request) {
		user, pass, _ := r.BasicAuth()
		switch {
		case user == "client-a" && pass == "secret-a":
			writeIntrospectJSON(t, w, activeJSON("client-a-user"))
		case user == "client-b" && pass == "secret-b":
			writeIntrospectJSON(t, w, activeJSON("client-b-user"))
		default:
			w.WriteHeader(http.StatusUnauthorized)
		}
	})

	mwA := Introspect(IntrospectConfig{Endpoint: srv.URL, ClientID: "client-a", ClientSecret: "secret-a", Cache: shared})
	mwB := Introspect(IntrospectConfig{Endpoint: srv.URL, ClientID: "client-b", ClientSecret: "secret-b", Cache: shared})

	first := runIntrospect(mwA, "same-token")
	second := runIntrospect(mwB, "same-token")

	if first.User == nil || first.User.Subject != "client-a-user" {
		t.Fatalf("first resolver user = %+v, want client-a-user", first.User)
	}
	if second.User == nil || second.User.Subject != "client-b-user" {
		t.Fatalf("second resolver user = %+v, want client-b-user", second.User)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("endpoint calls = %d, want 2 (one per client credential scope)", got)
	}
}

func TestIntrospect_InactiveNotCached(t *testing.T) {
	srv, calls := newIntrospectServer(t, func(w http.ResponseWriter, _ *http.Request) {
		writeIntrospectJSON(t, w, map[string]any{"active": false})
	})

	mw := Introspect(IntrospectConfig{Endpoint: srv.URL, Cache: newTestCache(t)})

	runIntrospect(mw, "tok")
	runIntrospect(mw, "tok")

	if got := calls.Load(); got != 2 {
		t.Errorf("inactive results must never be cached; calls = %d, want 2", got)
	}
}

func TestIntrospect_NoCacheHitsEveryRequest(t *testing.T) {
	srv, calls := newIntrospectServer(t, func(w http.ResponseWriter, _ *http.Request) {
		writeIntrospectJSON(t, w, activeJSON("x"))
	})

	mw := Introspect(IntrospectConfig{Endpoint: srv.URL}) // nil cache

	runIntrospect(mw, "tok")
	runIntrospect(mw, "tok")

	if got := calls.Load(); got != 2 {
		t.Errorf("with no cache every request hits the endpoint; calls = %d, want 2", got)
	}
}

func TestIntrospect_PastExpNotCached(t *testing.T) {
	srv, calls := newIntrospectServer(t, func(w http.ResponseWriter, _ *http.Request) {
		// active is authoritative (the endpoint decides), but a past exp means
		// the result must not be cached.
		writeIntrospectJSON(t, w, map[string]any{"active": true, "sub": "x", "exp": futureExp(-time.Minute)})
	})

	mw := Introspect(IntrospectConfig{Endpoint: srv.URL, Cache: newTestCache(t)})

	if ctx := runIntrospect(mw, "tok"); ctx.User == nil {
		t.Fatal("an active token should authenticate even if its exp has passed")
	}
	runIntrospect(mw, "tok")

	if got := calls.Load(); got != 2 {
		t.Errorf("a result whose exp has passed must not be cached; calls = %d, want 2", got)
	}
}

func TestIntrospect_TokenNeverStoredRaw(t *testing.T) {
	spectest.Proves(t, "go/authentication-authorization", "secret-non-disclosure", "introspection-cache-never-stores-a-raw-token")
	srv, _ := newIntrospectServer(t, func(w http.ResponseWriter, _ *http.Request) {
		writeIntrospectJSON(t, w, activeJSON("x"))
	})

	c := newTestCache(t)
	mw := Introspect(IntrospectConfig{Endpoint: srv.URL, Cache: c})
	runIntrospect(mw, "secret-token")

	bg := context.Background()
	if c.Has(bg, "secret-token") {
		t.Error("raw token must never be used as a cache key")
	}
	if !c.Has(bg, introspectCacheKey(srv.URL, IntrospectConfig{}, "secret-token")) {
		t.Error("expected the sha256-derived key to be present in the cache")
	}
}

func TestIntrospect_Discovery(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			writeIntrospectJSON(t, w, map[string]any{
				"introspection_endpoint": "http://" + r.Host + "/introspect",
			})
		case "/introspect":
			writeIntrospectJSON(t, w, map[string]any{"active": true, "sub": "discovered"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	mw := Introspect(IntrospectConfig{Issuer: srv.URL})
	ctx := runIntrospect(mw, "tok")

	if ctx.User == nil || ctx.User.Subject != "discovered" {
		t.Fatalf("expected user resolved via discovered introspection_endpoint, got %+v", ctx.User)
	}
}

// --- Audience validation ---

func TestIntrospect_AudienceMatch(t *testing.T) {
	srv, _ := newIntrospectServer(t, func(w http.ResponseWriter, _ *http.Request) {
		writeIntrospectJSON(t, w, map[string]any{"active": true, "sub": "u", "aud": "my-svc"})
	})

	mw := Introspect(IntrospectConfig{Endpoint: srv.URL, Audience: "my-svc"})
	ctx := runIntrospect(mw, "tok")

	if ctx.User == nil || ctx.User.Subject != "u" {
		t.Fatalf("expected user for matching audience, got %+v", ctx.User)
	}
}

func TestIntrospect_AudienceArrayMatch(t *testing.T) {
	srv, _ := newIntrospectServer(t, func(w http.ResponseWriter, _ *http.Request) {
		writeIntrospectJSON(t, w, map[string]any{"active": true, "sub": "u", "aud": []string{"a", "my-svc", "b"}})
	})

	mw := Introspect(IntrospectConfig{Endpoint: srv.URL, Audience: "my-svc"})
	ctx := runIntrospect(mw, "tok")

	if ctx.User == nil {
		t.Fatal("expected user when the configured audience is in the aud array")
	}
}

func TestIntrospect_AudienceMismatch(t *testing.T) {
	spectest.Proves(t, "go/authentication-authorization", "credential-validation", "introspection-rejects-audience-mismatch")
	srv, _ := newIntrospectServer(t, func(w http.ResponseWriter, _ *http.Request) {
		writeIntrospectJSON(t, w, map[string]any{"active": true, "sub": "u", "aud": "other-svc"})
	})

	mw := Introspect(IntrospectConfig{Endpoint: srv.URL, Audience: "my-svc"})
	ctx := runIntrospect(mw, "tok")

	if ctx.User != nil {
		t.Error("expected nil user when the token audience does not match")
	}
}

// TestIntrospect_AudienceReCheckedOnCacheHit proves the audience policy is a
// read-time check, not part of the cache key: two resolvers sharing one cache,
// endpoint, and credentials but requiring different audiences must each enforce
// their own, even when one serves the other's cached payload.
func TestIntrospect_AudienceReCheckedOnCacheHit(t *testing.T) {
	shared := newTestCache(t)
	srv, calls := newIntrospectServer(t, func(w http.ResponseWriter, _ *http.Request) {
		writeIntrospectJSON(t, w, map[string]any{"active": true, "sub": "u", "aud": "svc-a", "exp": futureExp(time.Hour)})
	})

	mwA := Introspect(IntrospectConfig{Endpoint: srv.URL, Audience: "svc-a", Cache: shared})
	mwB := Introspect(IntrospectConfig{Endpoint: srv.URL, Audience: "svc-b", Cache: shared})

	if ctx := runIntrospect(mwA, "tok"); ctx.User == nil {
		t.Fatal("resolver A should accept the matching audience")
	}
	if ctx := runIntrospect(mwB, "tok"); ctx.User != nil {
		t.Error("resolver B must reject on audience even when serving A's cached payload")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("endpoint calls = %d, want 1 (B was served from the shared cache, then rejected on audience)", got)
	}
}

// --- Concurrent request de-duplication (singleflight) ---

// TestIntrospect_SingleflightCollapsesConcurrent holds the first upstream call
// open while many identical requests pile up behind it, then proves they all
// resolve from a single endpoint call — even with no cache configured.
func TestIntrospect_SingleflightCollapsesConcurrent(t *testing.T) {
	release := make(chan struct{})
	srv, calls := newIntrospectServer(t, func(w http.ResponseWriter, _ *http.Request) {
		<-release
		writeIntrospectJSON(t, w, activeJSON("sf"))
	})

	mw := Introspect(IntrospectConfig{Endpoint: srv.URL}) // no cache: dedup must come from singleflight

	const n = 20
	var wg sync.WaitGroup
	results := make([]*phttp.Context, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = runIntrospect(mw, "tok")
		}(i)
	}
	time.Sleep(50 * time.Millisecond) // let the cohort enter the in-flight wait
	close(release)
	wg.Wait()

	for i, ctx := range results {
		if ctx.User == nil || ctx.User.Subject != "sf" {
			t.Fatalf("request %d: user = %+v, want sf", i, ctx.User)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("endpoint calls = %d, want 1 (concurrent identical tokens collapsed)", got)
	}
}

// --- 429 backpressure: one bounded retry, collapsed through singleflight ---

// TestIntrospect_ThrottledThenOKRetriesOnceAndResolves proves a 429 is retried
// once after its Retry-After (each form the leader parses) and that the retried
// 200 resolves, is cached, and leaves the breaker closed. The retry is a fresh
// upstream request, so it re-resolves the client assertion.
func TestIntrospect_ThrottledThenOKRetriesOnceAndResolves(t *testing.T) {
	for _, tc := range []struct {
		name       string
		retryAfter string
	}{
		{"delta-seconds zero", "0"},
		{"past HTTP-date", time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat)},
		{"absent header uses the jittered default", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var seen atomic.Int64
			srv, calls := newIntrospectServer(t, func(w http.ResponseWriter, r *http.Request) {
				if err := r.ParseForm(); err != nil {
					t.Errorf("parse form: %v", err)
				}
				if r.Form.Get("client_assertion") != "signed-jwt" {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				if seen.Add(1) == 1 {
					writeThrottled(w, tc.retryAfter)
					return
				}
				writeIntrospectJSON(t, w, activeJSON("after-throttle"))
			})

			var resolverCalls atomic.Int64
			mw := Introspect(IntrospectConfig{
				Endpoint: srv.URL,
				Cache:    newTestCache(t),
				Breaker:  BreakerConfig{FailureThreshold: 1, OpenDuration: time.Hour},
				ClientAssertion: func(context.Context) (string, error) {
					resolverCalls.Add(1)
					return "signed-jwt", nil
				},
			})

			beforeCalls := introspectCounterValue(t, introspectEndpointCall)
			beforeActive := introspectCounterValue(t, introspectActive)
			beforeThrottled := introspectCounterValue(t, introspectThrottled)
			beforeErrors := introspectCounterValue(t, introspectEndpointError)
			beforeOpen := introspectCounterValue(t, introspectBreakerOpen)

			ctx := runIntrospect(mw, "tok")
			if ctx.User == nil || ctx.User.Subject != "after-throttle" {
				t.Fatalf("expected the retried 200 to authenticate, got %+v", ctx.User)
			}
			if got := calls.Load(); got != 2 {
				t.Errorf("endpoint calls = %d, want 2 (the 429 and one retry)", got)
			}
			if got := resolverCalls.Load(); got != 2 {
				t.Errorf("assertion resolver calls = %d, want 2 (resolved once per upstream request)", got)
			}
			if got := introspectCounterValue(t, introspectEndpointCall) - beforeCalls; got != 2 {
				t.Errorf("endpoint_call delta = %d, want 2", got)
			}
			if got := introspectCounterValue(t, introspectActive) - beforeActive; got != 1 {
				t.Errorf("active delta = %d, want 1", got)
			}
			if got := introspectCounterValue(t, introspectThrottled) - beforeThrottled; got != 0 {
				t.Errorf("throttled delta = %d, want 0 (the retry succeeded)", got)
			}
			if got := introspectCounterValue(t, introspectEndpointError) - beforeErrors; got != 0 {
				t.Errorf("endpoint_error delta = %d, want 0", got)
			}

			// The retried result is cached like any other active 200.
			if again := runIntrospect(mw, "tok"); again.User == nil {
				t.Fatal("expected the retried result to be served from cache")
			}
			// The breaker stayed closed (FailureThreshold 1): an uncached token
			// still reaches the endpoint.
			if other := runIntrospect(mw, "other"); other.User == nil {
				t.Fatal("expected an uncached token to resolve: the breaker must stay closed")
			}
			if got := calls.Load(); got != 3 {
				t.Errorf("endpoint calls = %d, want 3 (cache hit, then one call for the uncached token)", got)
			}
			if got := introspectCounterValue(t, introspectBreakerOpen) - beforeOpen; got != 0 {
				t.Errorf("breaker_open delta = %d, want 0", got)
			}
		})
	}
}

// TestIntrospect_ThrottleRetryCollapsesConcurrent holds the first upstream call
// open while many identical requests pile up behind it, answers it 429, and
// proves the whole cohort resolves from ONE retry: exactly two upstream calls,
// not one retry per caller.
func TestIntrospect_ThrottleRetryCollapsesConcurrent(t *testing.T) {
	release := make(chan struct{})
	var seen atomic.Int64
	srv, calls := newIntrospectServer(t, func(w http.ResponseWriter, _ *http.Request) {
		if seen.Add(1) == 1 {
			<-release
			writeThrottled(w, "0")
			return
		}
		writeIntrospectJSON(t, w, activeJSON("sf-throttled"))
	})

	mw := Introspect(IntrospectConfig{Endpoint: srv.URL}) // no cache: dedup must come from singleflight

	beforeCalls := introspectCounterValue(t, introspectEndpointCall)
	beforeActive := introspectCounterValue(t, introspectActive)
	beforeThrottled := introspectCounterValue(t, introspectThrottled)

	const n = 20
	var wg sync.WaitGroup
	results := make([]*phttp.Context, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = runIntrospect(mw, "tok")
		}(i)
	}
	time.Sleep(50 * time.Millisecond) // let the cohort enter the in-flight wait
	close(release)
	wg.Wait()

	for i, ctx := range results {
		if ctx.User == nil || ctx.User.Subject != "sf-throttled" {
			t.Fatalf("request %d: user = %+v, want sf-throttled", i, ctx.User)
		}
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("endpoint calls = %d, want 2 (one first call + one retry shared by the cohort)", got)
	}
	if got := introspectCounterValue(t, introspectEndpointCall) - beforeCalls; got != 2 {
		t.Errorf("endpoint_call delta = %d, want 2", got)
	}
	if got := introspectCounterValue(t, introspectActive) - beforeActive; got != n {
		t.Errorf("active delta = %d, want %d (one outcome per request)", got, n)
	}
	if got := introspectCounterValue(t, introspectThrottled) - beforeThrottled; got != 0 {
		t.Errorf("throttled delta = %d, want 0", got)
	}
}

// TestIntrospect_ThrottleRetrySkippedWhenBudgetCannotCoverWait proves the retry
// honors the leader's budget: when Retry-After asks for longer than the budget
// left, the leader does not wait and does not retry. The request fails closed
// as throttled — not as an endpoint error, and not toward the breaker — because
// running out of our own budget is not an endpoint failure.
func TestIntrospect_ThrottleRetrySkippedWhenBudgetCannotCoverWait(t *testing.T) {
	var throttling atomic.Bool
	throttling.Store(true)
	srv, calls := newIntrospectServer(t, func(w http.ResponseWriter, _ *http.Request) {
		if throttling.Load() {
			writeThrottled(w, "1") // 1s: longer than the 500ms budget
			return
		}
		writeIntrospectJSON(t, w, activeJSON("svc"))
	})

	mw := Introspect(IntrospectConfig{
		Endpoint: srv.URL,
		Timeout:  500 * time.Millisecond, // ClientAssertionTimeout defaults to this
		Breaker:  BreakerConfig{FailureThreshold: 1, OpenDuration: time.Hour},
	})
	beforeThrottled := introspectCounterValue(t, introspectThrottled)
	beforeErrors := introspectCounterValue(t, introspectEndpointError)

	if ctx := runIntrospect(mw, "tok"); ctx.User != nil {
		t.Fatalf("a 429 authenticated %+v, want fail closed", ctx.User)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("endpoint calls = %d, want 1 (the retry must be skipped)", got)
	}
	if got := introspectCounterValue(t, introspectThrottled) - beforeThrottled; got != 1 {
		t.Errorf("throttled delta = %d, want 1", got)
	}
	if got := introspectCounterValue(t, introspectEndpointError) - beforeErrors; got != 0 {
		t.Errorf("endpoint_error delta = %d, want 0 (a retry past the budget would surface here)", got)
	}

	// The skipped retry left the breaker closed (FailureThreshold 1).
	throttling.Store(false)
	if ctx := runIntrospect(mw, "other"); ctx.User == nil {
		t.Fatal("expected an uncached token to resolve: the breaker must stay closed")
	}
}

// --- Cache pluggability (Redis / Postgres / any external cache.Cache) ---

// stubCache is a minimal external cache.Cache (the shape a Redis or Postgres
// backend would take), used to prove Introspect depends only on the cache.Cache
// interface and propagates the request context to it (which network caches need
// for cancellation and timeouts).
type stubCache struct {
	mu     sync.Mutex
	data   map[string][]byte
	sets   int
	sawCtx bool
}

func newStubCache() *stubCache { return &stubCache{data: map[string][]byte{}} }

func (s *stubCache) Get(ctx context.Context, key string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx != nil {
		s.sawCtx = true
	}
	v, ok := s.data[key]
	if !ok {
		return nil, false
	}
	return append([]byte(nil), v...), true
}

func (s *stubCache) Set(ctx context.Context, key string, value []byte, _ time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx != nil {
		s.sawCtx = true
	}
	s.data[key] = append([]byte(nil), value...)
	s.sets++
	return nil
}

func (s *stubCache) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, key)
	return nil
}

func (s *stubCache) Has(_ context.Context, key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.data[key]
	return ok
}

func (s *stubCache) Clear(_ context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data = map[string][]byte{}
	return nil
}

func (s *stubCache) Close() error { return nil }

func TestIntrospect_PluggableCache(t *testing.T) {
	srv, calls := newIntrospectServer(t, func(w http.ResponseWriter, _ *http.Request) {
		writeIntrospectJSON(t, w, activeJSON("ext"))
	})

	c := newStubCache()
	mw := Introspect(IntrospectConfig{Endpoint: srv.URL, Cache: c})

	first := runIntrospect(mw, "tok")
	second := runIntrospect(mw, "tok")

	if first.User == nil || second.User == nil {
		t.Fatal("expected both requests to authenticate via the external cache")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("endpoint calls = %d, want 1 (second served from the external cache)", got)
	}
	if c.sets != 1 {
		t.Errorf("external cache Set calls = %d, want 1", c.sets)
	}
	if !c.sawCtx {
		t.Error("Introspect must pass the request context to the cache (network caches rely on it)")
	}
}

// --- Observability counters ---

func TestIntrospect_Counters(t *testing.T) {
	srv, _ := newIntrospectServer(t, func(w http.ResponseWriter, _ *http.Request) {
		writeIntrospectJSON(t, w, activeJSON("c"))
	})

	mw := Introspect(IntrospectConfig{Endpoint: srv.URL})

	beforeActive := introspectCounterValue(t, introspectActive)
	beforeCalls := introspectCounterValue(t, introspectEndpointCall)

	if runIntrospect(mw, "tok").User == nil {
		t.Fatal("expected user")
	}

	if got := introspectCounterValue(t, introspectActive) - beforeActive; got != 1 {
		t.Errorf("active counter delta = %d, want 1", got)
	}
	if got := introspectCounterValue(t, introspectEndpointCall) - beforeCalls; got != 1 {
		t.Errorf("endpoint_call counter delta = %d, want 1", got)
	}
}

// counterValue reads a process-wide introspection counter; tests assert on
// deltas because the counters persist across the process.
func introspectCounterValue(t *testing.T, o introspectOutcome) int64 {
	t.Helper()
	v := introspectCounters().Get(string(o))
	if v == nil {
		return 0
	}
	iv, ok := v.(*expvar.Int)
	if !ok {
		t.Fatalf("counter %s is %T, want *expvar.Int", o, v)
	}
	return iv.Value()
}

func TestIntrospect_DiscoveryRetriesAfterTransientFailure(t *testing.T) {
	var discoveryCalls atomic.Int64
	var introspectCalls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			if discoveryCalls.Add(1) == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			writeIntrospectJSON(t, w, map[string]any{
				"introspection_endpoint": "http://" + r.Host + "/introspect",
			})
		case "/introspect":
			introspectCalls.Add(1)
			writeIntrospectJSON(t, w, map[string]any{"active": true, "sub": "recovered"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	mw := Introspect(IntrospectConfig{Issuer: srv.URL})

	if ctx := runIntrospect(mw, "tok"); ctx.User != nil {
		t.Fatalf("first request should fail closed while discovery is unavailable, got %+v", ctx.User)
	}
	second := runIntrospect(mw, "tok")
	if second.User == nil || second.User.Subject != "recovered" {
		t.Fatalf("expected retry to recover discovered introspection endpoint, got %+v", second.User)
	}
	third := runIntrospect(mw, "tok-2")
	if third.User == nil || third.User.Subject != "recovered" {
		t.Fatalf("expected successful discovery to stay memoized, got %+v", third.User)
	}
	if got := discoveryCalls.Load(); got != 2 {
		t.Errorf("discovery calls = %d, want 2 (initial failure plus one retry)", got)
	}
	if got := introspectCalls.Load(); got != 2 {
		t.Errorf("introspection calls = %d, want 2", got)
	}
}

func TestIntrospect_MissingEndpoint(t *testing.T) {
	mw := Introspect(IntrospectConfig{}) // neither Endpoint nor Issuer
	ctx := runIntrospect(mw, "tok")

	if ctx.User != nil {
		t.Error("expected nil user (fail closed) when no endpoint is configured")
	}
}

func TestIntrospect_InsecureEndpointRejected(t *testing.T) {
	_, calls := newIntrospectServer(t, func(w http.ResponseWriter, _ *http.Request) {
		writeIntrospectJSON(t, w, map[string]any{"active": true, "sub": "x"})
	})

	// A plaintext http endpoint on a non-loopback host must be rejected without
	// AllowInsecure. Use a non-loopback hostname so the loopback exception does
	// not apply; the request must never reach the server.
	mw := Introspect(IntrospectConfig{Endpoint: "http://example.com/introspect"})
	if ctx := runIntrospect(mw, "tok"); ctx.User != nil {
		t.Error("expected nil user for an insecure non-loopback endpoint")
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("insecure endpoint must not be contacted; calls = %d", got)
	}
}

// --- Unit tests for helpers ---

func TestIntrospectCacheKey(t *testing.T) {
	const token = "abc.def.ghi"
	cfg := IntrospectConfig{Issuer: "https://issuer.example.com", ClientID: "client-a", ClientSecret: "secret-a"}
	key := introspectCacheKey("https://issuer.example.com/introspect", cfg, token)

	if key == token {
		t.Fatal("cache key must not equal the raw token")
	}
	if !strings.HasPrefix(key, introspectCachePrefix) {
		t.Errorf("key %q missing prefix %q", key, introspectCachePrefix)
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{
		"https://issuer.example.com/introspect",
		cfg.Issuer,
		cfg.ClientID,
		cfg.ClientSecret,
		token,
	}, "\x00")))
	if want := introspectCachePrefix + hex.EncodeToString(sum[:]); key != want {
		t.Errorf("key = %q, want %q", key, want)
	}
	if introspectCacheKey("https://issuer.example.com/introspect", cfg, token) != key {
		t.Error("cache key must be deterministic")
	}
	if introspectCacheKey("https://other.example.com/introspect", cfg, token) == key {
		t.Error("cache key must be scoped by endpoint")
	}
	otherClient := cfg
	otherClient.ClientID = "client-b"
	if introspectCacheKey("https://issuer.example.com/introspect", otherClient, token) == key {
		t.Error("cache key must be scoped by client identity")
	}
	otherSecret := cfg
	otherSecret.ClientSecret = "secret-b"
	if introspectCacheKey("https://issuer.example.com/introspect", otherSecret, token) == key {
		t.Error("cache key must be scoped by client secret")
	}
}

func TestIntrospectCacheTTL(t *testing.T) {
	const maxTTL = 5 * time.Minute

	if got := introspectCacheTTL(map[string]any{}, maxTTL); got != maxTTL {
		t.Errorf("no exp: ttl = %v, want %v", got, maxTTL)
	}
	if got := introspectCacheTTL(map[string]any{"exp": futureExp(time.Hour)}, maxTTL); got != maxTTL {
		t.Errorf("far exp: ttl = %v, want %v (capped at max)", got, maxTTL)
	}
	got := introspectCacheTTL(map[string]any{"exp": futureExp(30 * time.Second)}, maxTTL)
	if got <= 0 || got > 30*time.Second {
		t.Errorf("near exp: ttl = %v, want (0, 30s]", got)
	}
	if got := introspectCacheTTL(map[string]any{"exp": futureExp(-time.Hour)}, maxTTL); got != 0 {
		t.Errorf("past exp: ttl = %v, want 0 (do not cache)", got)
	}
}

func TestParseIntrospection(t *testing.T) {
	payload, active := parseIntrospection([]byte(`{"active":true,"sub":"x"}`))
	if !active || payload["sub"] != "x" {
		t.Errorf("active token: active=%v payload=%v", active, payload)
	}
	if _, active := parseIntrospection([]byte(`{"active":false}`)); active {
		t.Error("inactive token reported as active")
	}
	if _, active := parseIntrospection([]byte(`not json`)); active {
		t.Error("malformed body must be treated as inactive")
	}
	if _, active := parseIntrospection([]byte(`{"sub":"x"}`)); active {
		t.Error("missing active field must be treated as inactive")
	}
}

func TestClaimsFromIntrospection_DropsActiveEnvelope(t *testing.T) {
	c := claimsFromIntrospection(map[string]any{
		"active":    true,
		"sub":       "s",
		"scope":     "a b",
		"client_id": "cid",
		"aud":       "x",
	})
	if c.Subject != "s" {
		t.Errorf("subject = %q, want s", c.Subject)
	}
	if len(c.Scopes) != 2 {
		t.Errorf("scopes = %v, want 2 entries", c.Scopes)
	}
	if c.ClientID != "cid" {
		t.Errorf("clientID = %q, want cid", c.ClientID)
	}
	if _, ok := c.Extra["active"]; ok {
		t.Error("active envelope flag must be dropped, not mapped to Extra")
	}
	if c.Extra["aud"] != "x" {
		t.Errorf("non-standard claim aud should land in Extra, got %v", c.Extra["aud"])
	}
}

func TestIntrospectRetryDelay(t *testing.T) {
	// A sub-second now, so an HTTP-date (one-second resolution) just ahead of it
	// yields a delay strictly inside the cap.
	now := time.Date(2026, time.September, 11, 12, 0, 0, 400*int(time.Millisecond), time.UTC)
	httpDate := func(at time.Time) string { return at.UTC().Format(http.TimeFormat) }

	for _, tc := range []struct {
		name       string
		retryAfter string
		want       time.Duration
	}{
		{"delta-seconds zero", "0", 0},
		{"delta-seconds leading zeros", "00", 0},
		{"delta-seconds one", "1", time.Second},
		{"delta-seconds with surrounding space", " 1 ", time.Second},
		{"delta-seconds oversized is capped", "3600", maxIntrospectRetryDelay},
		{"delta-seconds beyond uint64 is capped", "99999999999999999999999", maxIntrospectRetryDelay},
		{"HTTP-date inside the cap", httpDate(now.Add(600 * time.Millisecond)), 600 * time.Millisecond},
		{"HTTP-date in obsolete RFC 850 form", now.Add(600 * time.Millisecond).UTC().Format("Monday, 02-Jan-06 15:04:05 GMT"), 600 * time.Millisecond},
		{"HTTP-date beyond the cap is capped", httpDate(now.Add(time.Hour)), maxIntrospectRetryDelay},
		{"HTTP-date in the past retries at once", httpDate(now.Add(-time.Hour)), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := introspectRetryDelay(tc.retryAfter, now); got != tc.want {
				t.Errorf("introspectRetryDelay(%q) = %v, want %v", tc.retryAfter, got, tc.want)
			}
		})
	}

	// Absent or malformed headers fall back to the jittered default, never to
	// an immediate retry and never beyond the cap.
	for _, retryAfter := range []string{"", "   ", "soon", "-5", "+5", "1.5", "0x10"} {
		t.Run("jittered default for "+strconv.Quote(retryAfter), func(t *testing.T) {
			for range 100 {
				got := introspectRetryDelay(retryAfter, now)
				if got < minIntrospectRetryJitter || got >= maxIntrospectRetryJitter {
					t.Fatalf("introspectRetryDelay(%q) = %v, want within [%v, %v)", retryAfter, got, minIntrospectRetryJitter, maxIntrospectRetryJitter)
				}
			}
		})
	}
	if maxIntrospectRetryJitter > maxIntrospectRetryDelay {
		t.Errorf("jitter bound %v exceeds the retry cap %v", maxIntrospectRetryJitter, maxIntrospectRetryDelay)
	}
}

func TestWaitIntrospectRetry(t *testing.T) {
	// within fails the test instead of hanging when wait ignores its context.
	within := func(t *testing.T, wait func() bool) bool {
		t.Helper()
		done := make(chan bool, 1)
		go func() { done <- wait() }()
		select {
		case ok := <-done:
			return ok
		case <-time.After(5 * time.Second):
			t.Fatal("waitIntrospectRetry ignored its context and kept waiting")
			return false
		}
	}

	t.Run("zero delay retries at once", func(t *testing.T) {
		if !waitIntrospectRetry(context.Background(), 0) {
			t.Error("want true")
		}
	})
	t.Run("short delay inside the budget retries", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
		defer cancel()
		if !waitIntrospectRetry(ctx, time.Millisecond) {
			t.Error("want true")
		}
	})
	t.Run("done context skips", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if waitIntrospectRetry(ctx, 0) {
			t.Error("want false: a done context cannot run the retry")
		}
	})
	t.Run("deadline inside the delay skips without waiting", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancel()
		if within(t, func() bool { return waitIntrospectRetry(ctx, time.Hour) }) {
			t.Error("want false: the budget cannot cover the wait")
		}
	})
	t.Run("context ending during the wait skips", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background()) // no deadline: only cancellation ends it
		time.AfterFunc(10*time.Millisecond, cancel)
		defer cancel()
		if within(t, func() bool { return waitIntrospectRetry(ctx, time.Hour) }) {
			t.Error("want false: the context ended during the wait")
		}
	})
}
