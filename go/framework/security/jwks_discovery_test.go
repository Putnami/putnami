package security

import (
	"crypto/rsa"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	phttp "go.putnami.dev/http"
)

// bearerUser runs mw on a request carrying token as a Bearer credential and
// returns the identity it resolved, nil when it refused the token.
func bearerUser(mw phttp.Middleware, token string) *phttp.Claims {
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	ctx := phttp.NewContext(httptest.NewRecorder(), req)
	mw(ctx, func() *phttp.Response { return phttp.JSON("ok") })
	return ctx.User
}

// oidcIssuer serves an OIDC discovery document that points at its own /jwks,
// which publishes key under kid "key-1". discovery runs before each discovery
// response and counts the reads; a non-nil error from it answers 500.
func oidcIssuer(t *testing.T, key *rsa.PrivateKey, discovery func() error) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			if err := discovery(); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"jwks_uri": "http://" + r.Host + "/jwks"})
		case "/jwks":
			_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{rsaJWKJSON("key-1", &key.PublicKey)}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestJWKSJWT_FailedDiscoveryIsRetriedAfterTheInterval(t *testing.T) {
	key := generateRSAKey(t)
	var discoveries atomic.Int64
	srv := oidcIssuer(t, key, func() error {
		if discoveries.Add(1) == 1 {
			return &ValidationError{"issuer unavailable"}
		}
		return nil
	})
	token := makeRSAJWT(t, map[string]any{
		"sub": "svc-a",
		"iss": srv.URL,
		"exp": float64(time.Now().Add(time.Hour).Unix()),
	}, "key-1", key)

	now := time.Now()
	mw := jwksJWT(JWKSJWTConfig{Issuer: srv.URL}, func() time.Time { return now })

	if user := bearerUser(mw, token); user != nil {
		t.Fatalf("token accepted while discovery fails: user=%+v", user)
	}
	if got := discoveries.Load(); got != 1 {
		t.Fatalf("discovery reads after the first token = %d, want 1", got)
	}

	now = now.Add(jwksDiscoveryRetryInterval - time.Nanosecond)
	if user := bearerUser(mw, token); user != nil {
		t.Fatalf("token accepted inside the retry interval: user=%+v", user)
	}
	if got := discoveries.Load(); got != 1 {
		t.Fatalf("discovery reads inside the retry interval = %d, want 1 (no network I/O before the interval elapses)", got)
	}

	now = now.Add(time.Nanosecond)
	user := bearerUser(mw, token)
	if user == nil || user.Subject != "svc-a" {
		t.Fatalf("token refused once the retry interval elapsed: user=%+v", user)
	}
	if got := discoveries.Load(); got != 2 {
		t.Fatalf("discovery reads after the retry = %d, want 2", got)
	}

	now = now.Add(10 * jwksDiscoveryRetryInterval)
	if user := bearerUser(mw, token); user == nil {
		t.Fatal("token refused after a successful discovery")
	}
	if got := discoveries.Load(); got != 2 {
		t.Fatalf("discovery reads after a successful discovery = %d, want 2 (the built fetcher is kept)", got)
	}
}

func TestJWKSJWT_ConcurrentFirstTokensShareOneDiscovery(t *testing.T) {
	const tokens = 20

	key := generateRSAKey(t)
	var discoveries atomic.Int64
	firstDiscovery := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseDiscovery := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseDiscovery()
	srv := oidcIssuer(t, key, func() error {
		if discoveries.Add(1) == 1 {
			close(firstDiscovery)
		}
		<-release
		return nil
	})
	token := makeRSAJWT(t, map[string]any{
		"sub": "svc-a",
		"iss": srv.URL,
		"exp": float64(time.Now().Add(time.Hour).Unix()),
	}, "key-1", key)

	mw := JWKSJWT(JWKSJWTConfig{Issuer: srv.URL})

	// Every token is in flight while the first discovery read is held open, so
	// a resolver that does not share the build sends more than one read.
	start := make(chan struct{})
	var entered, done sync.WaitGroup
	users := make([]*phttp.Claims, tokens)
	for i := range tokens {
		entered.Add(1)
		done.Add(1)
		go func() {
			defer done.Done()
			<-start
			entered.Done()
			users[i] = bearerUser(mw, token)
		}()
	}
	close(start)
	entered.Wait()
	select {
	case <-firstDiscovery:
	case <-time.After(10 * time.Second):
		t.Fatal("no discovery read within 10s of the first tokens")
	}
	releaseDiscovery()
	done.Wait()

	if got := discoveries.Load(); got != 1 {
		t.Errorf("discovery reads for %d concurrent first tokens = %d, want 1", tokens, got)
	}
	for i, user := range users {
		if user == nil || user.Subject != "svc-a" {
			t.Errorf("token %d refused: user=%+v", i, user)
		}
	}
}

func TestJWKSJWT_NoKeySourceRefusesEveryToken(t *testing.T) {
	key := generateRSAKey(t)
	// The token passes every claim check below, so only the missing key source
	// refuses it.
	token := makeRSAJWT(t, map[string]any{
		"sub": "svc-a",
		"iss": "https://issuer.example.com",
		"aud": "svc",
		"exp": float64(time.Now().Add(time.Hour).Unix()),
	}, "key-1", key)

	for name, cfg := range map[string]JWKSJWTConfig{
		"zero config":        {},
		"only claim checks":  {RequiredIssuer: "https://issuer.example.com", Audience: "svc", AllowMissingExpiration: true},
		"only cache setting": {CacheTTL: time.Minute, AllowInsecure: true},
	} {
		now := time.Now()
		mw := jwksJWT(cfg, func() time.Time { return now })
		for attempt := range 3 {
			if user := bearerUser(mw, token); user != nil {
				t.Errorf("%s: token accepted on attempt %d without a key source: user=%+v", name, attempt, user)
			}
			now = now.Add(2 * jwksDiscoveryRetryInterval)
		}
	}
}

func TestLazyJWKSFetcher_RecordsFailureAndKeepsSuccess(t *testing.T) {
	now := time.Now()
	built := NewJWKSFetcher("https://issuer.example.com/jwks", 0, false)
	failure := &ValidationError{"discovery failed"}
	var builds int
	results := []error{failure, failure, nil}
	l := &lazyJWKSFetcher{
		build: func() (*JWKSFetcher, error) {
			err := results[builds]
			builds++
			if err != nil {
				return nil, err
			}
			return built, nil
		},
		clock:         func() time.Time { return now },
		retryInterval: time.Minute,
	}

	steps := []struct {
		advance    time.Duration
		wantErr    bool
		wantBuilds int
	}{
		{0, true, 1},                // first call builds and fails
		{time.Second, true, 1},      // inside the interval: recorded error, no build
		{time.Minute, true, 2},      // interval elapsed: builds again and fails again
		{59 * time.Second, true, 2}, // the interval restarts at the second failure
		{time.Second, false, 3},     // builds and succeeds
		{time.Hour, false, 3},       // the built fetcher is kept
	}
	for i, step := range steps {
		now = now.Add(step.advance)
		f, err := l.get()
		if step.wantErr {
			if !errors.Is(err, failure) || f != nil {
				t.Fatalf("step %d: get() = (%v, %v), want (nil, %v)", i, f, err, failure)
			}
		} else if err != nil || f != built {
			t.Fatalf("step %d: get() = (%v, %v), want (%v, nil)", i, f, err, built)
		}
		if builds != step.wantBuilds {
			t.Fatalf("step %d: builds = %d, want %d", i, builds, step.wantBuilds)
		}
	}
}
