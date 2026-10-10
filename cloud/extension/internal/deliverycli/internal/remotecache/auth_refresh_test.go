package remotecache

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	cache "go.putnami.dev/protocol/cache"
)

// The cache bearer is minted once and memoized for a whole Putnami session, but
// every renewable source is short-lived: `putnami cloud token --for cache` mints
// a 300-second JWT and the Cloud Run metadata service mints a ~1h ID token. A
// build longer than that TTL therefore negotiates warm at the start and has
// every post-build store refused at the end — the run looks fine, persists
// nothing, and the next run is cold again.
//
// These tests pin the fix and, just as importantly, its limits: a credential the
// runner cannot re-derive is never "refreshed", a scope denial is never retried,
// and a refusal never leaks the bearer.

// mintingSource is a renewable token source under test control. Each mint
// returns a distinct bearer, so a refresh is observable on the wire; fixed makes
// it return the same bearer every time, which is how the GCP metadata server
// behaves until its cached ID token nears expiry.
type mintingSource struct {
	mu    sync.Mutex
	calls int
	fixed string
	class TokenClass
}

func (m *mintingSource) resolve(context.Context) (Bearer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	class := m.class
	if class == "" {
		class = TokenClassMetadata
	}
	if m.fixed != "" {
		return Bearer{Token: m.fixed, Class: class}, nil
	}
	return Bearer{Token: fmt.Sprintf("minted-%d", m.calls), Class: class}, nil
}

func (m *mintingSource) mints() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// authRecorder collects the redacted refusal records the client reports.
type authRecorder struct {
	mu   sync.Mutex
	seen []AuthFailure
}

func (r *authRecorder) observe(f AuthFailure) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, f)
}

func (r *authRecorder) all() []AuthFailure {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]AuthFailure(nil), r.seen...)
}

// cacheServer answers negotiate, refusing every bearer that accept rejects. It
// records the bearers it saw so a test can prove which credential was replayed.
type cacheServer struct {
	mu      sync.Mutex
	bearers []string
	accept  func(bearer string) bool
	status  int
	body    any
}

func (s *cacheServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bearer := strings.TrimPrefix(r.Header.Get(cache.AuthorizationHeader), "Bearer ")
		s.mu.Lock()
		s.bearers = append(s.bearers, bearer)
		s.mu.Unlock()
		if s.accept != nil && s.accept(bearer) {
			_ = json.NewEncoder(w).Encode(cache.NegotiateResponse{ProtocolVersion: cache.ProtocolVersion})
			return
		}
		status := s.status
		if status == 0 {
			status = http.StatusUnauthorized
		}
		body := s.body
		if body == nil {
			body = cache.ErrorResponse{
				ProtocolVersion: cache.ProtocolVersion,
				Code:            cache.CodeUnauthorized,
				Message:         "authentication with a workspace or organization scope is required",
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (s *cacheServer) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.bearers...)
}

// negotiateOnce issues one negotiate for a single eligible key.
func negotiateOnce(c *Client) error {
	_, err := c.Negotiate(context.Background(), &cache.NegotiateRequest{
		ProtocolVersion: cache.ProtocolVersion,
		Keys:            []cache.KeyRequest{{Key: keyBuild}},
	})
	return err
}

func TestTokenClass_OnlyReMintableSourcesAreRenewable(t *testing.T) {
	renewable := []TokenClass{TokenClassCommand, TokenClassURL, TokenClassMetadata}
	for _, class := range renewable {
		if !class.Renewable() {
			t.Errorf("%s.Renewable() = false; the runner can re-invoke this source", class)
		}
	}
	// A credential the runner holds as opaque bytes cannot be re-derived, so
	// "refreshing" it would resend the rejected bytes — a fake refresh.
	for _, class := range []TokenClass{TokenClassNone, TokenClassEnv, TokenClassStatic} {
		if class.Renewable() {
			t.Errorf("%s.Renewable() = true; this source cannot be re-minted by the runner", class)
		}
	}
}

func TestTokenSource_ClassFollowsResolvePrecedence(t *testing.T) {
	tests := []struct {
		name   string
		env    string
		source TokenSource
		want   TokenClass
	}{
		{name: "env wins over a configured command", env: "pkt_injected", source: TokenSource{Command: cacheTokenRecipe()}, want: TokenClassEnv},
		{name: "env wins over metadata", env: "pkt_injected", source: TokenSource{Audience: "https://cache.example"}, want: TokenClassEnv},
		{name: "command", source: TokenSource{Command: cacheTokenRecipe()}, want: TokenClassCommand},
		{name: "url", source: TokenSource{URL: "https://tokens.example"}, want: TokenClassURL},
		{name: "metadata", source: TokenSource{Audience: "https://cache.example"}, want: TokenClassMetadata},
		{name: "unconfigured", source: TokenSource{}, want: TokenClassNone},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(TokenEnv, tc.env)
			if got := tc.source.Class(); got != tc.want {
				t.Fatalf("Class() = %q, want %q", got, tc.want)
			}
		})
	}
}

func cacheTokenRecipe() []string { return []string{"putnami", "cloud", "token", "--for", "cache"} }

// A static PUTNAMI_CACHE_TOKEN is the control plane's injected workspace machine
// token: the runner holds bytes it cannot re-derive. It must be sent once and
// the refusal reported — never re-minted, and never replayed. This also pins the
// precedence invariant: the configured command recipe (which would fail loudly
// if it ran) must stay untouched while the env override is set.
func TestDoAuthed_StaticEnvTokenIsNeverRefreshed(t *testing.T) {
	t.Setenv(TokenEnv, "pkt_injected_machine_token")
	server := &cacheServer{}
	srv := server.start(t)

	rec := &authRecorder{}
	cfg := &Config{URL: srv.URL, Token: TokenSource{Command: []string{"/nonexistent/cache-token-command"}}}
	c := NewClient(srv.URL, "", WithBearerFunc(cfg.ResolveBearer), WithAuthObserver(rec.observe))

	if err := negotiateOnce(c); err == nil {
		t.Fatal("negotiate succeeded against a server that refuses every bearer")
	}

	sent := server.seen()
	if len(sent) != 1 {
		t.Fatalf("sent %d request(s) %v; a static token must never be replayed", len(sent), sent)
	}
	if sent[0] != "pkt_injected_machine_token" {
		t.Fatalf("bearer = %q; the env override must win over the configured command recipe", sent[0])
	}
	failures := rec.all()
	if len(failures) != 1 {
		t.Fatalf("recorded %d refusal(s), want exactly 1: %+v", len(failures), failures)
	}
	if failures[0].Source != TokenClassEnv {
		t.Errorf("Source = %q, want %q", failures[0].Source, TokenClassEnv)
	}
	if failures[0].Refreshed {
		t.Error("Refreshed = true for a static env token; there is no source to re-run")
	}
}

// The renewable path: an expired bearer is re-minted once and the SAME request
// is replayed with the fresh credential, which is what turns a run that stored
// nothing into one that stores normally.
func TestDoAuthed_RenewableSourceRefreshesAndRetriesOnce(t *testing.T) {
	t.Setenv(TokenEnv, "")
	source := &mintingSource{}
	server := &cacheServer{accept: func(bearer string) bool { return bearer == "minted-2" }}
	srv := server.start(t)

	rec := &authRecorder{}
	c := NewClient(srv.URL, "", WithBearerFunc(source.resolve), WithAuthObserver(rec.observe))

	if err := negotiateOnce(c); err != nil {
		t.Fatalf("negotiate after a bearer refresh: %v", err)
	}

	if got := server.seen(); len(got) != 2 || got[0] != "minted-1" || got[1] != "minted-2" {
		t.Fatalf("bearers on the wire = %v, want [minted-1 minted-2]", got)
	}
	if source.mints() != 2 {
		t.Fatalf("minted %d bearer(s), want exactly 2 (initial + one refresh)", source.mints())
	}
	failures := rec.all()
	if len(failures) != 1 {
		t.Fatalf("recorded %d refusal(s), want exactly 1: %+v", len(failures), failures)
	}
	if !failures[0].Refreshed || failures[0].Source != TokenClassMetadata {
		t.Fatalf("refusal = %+v, want Refreshed=true Source=metadata", failures[0])
	}
	if failures[0].Op != "negotiate" || failures[0].Status != http.StatusUnauthorized {
		t.Fatalf("refusal = %+v, want op=negotiate status=401", failures[0])
	}
}

// A credential that is genuinely revoked, not merely expired, must cost exactly
// one extra round trip — not a retry loop against a server that will never
// accept it.
func TestDoAuthed_Persistent401StopsAfterOneRetry(t *testing.T) {
	t.Setenv(TokenEnv, "")
	source := &mintingSource{}
	server := &cacheServer{accept: func(string) bool { return false }}
	srv := server.start(t)

	rec := &authRecorder{}
	c := NewClient(srv.URL, "", WithBearerFunc(source.resolve), WithAuthObserver(rec.observe))

	err := negotiateOnce(c)
	if err == nil {
		t.Fatal("negotiate succeeded against a server that refuses every bearer")
	}
	if !IsStatus(err, http.StatusUnauthorized) {
		t.Fatalf("error = %v, want a surfaced 401", err)
	}
	if got := server.seen(); len(got) != 2 {
		t.Fatalf("sent %d request(s) %v, want exactly 2 (one attempt + one replay)", len(got), got)
	}
	if source.mints() != 2 {
		t.Fatalf("minted %d bearer(s), want exactly 2", source.mints())
	}
	if len(rec.all()) != 2 {
		t.Fatalf("recorded %d refusal(s), want 2 — both attempts leave evidence: %+v", len(rec.all()), rec.all())
	}
}

// The metadata server returns its cached ID token until the token nears expiry,
// so a re-mint often hands back the identical bearer. Replaying it is a
// guaranteed second rejection, and with hundreds of refused stores that doubles
// the failed traffic for nothing.
func TestDoAuthed_UnchangedBearerIsNotReplayed(t *testing.T) {
	t.Setenv(TokenEnv, "")
	source := &mintingSource{fixed: "same-id-token"}
	server := &cacheServer{accept: func(string) bool { return false }}
	srv := server.start(t)

	rec := &authRecorder{}
	c := NewClient(srv.URL, "", WithBearerFunc(source.resolve), WithAuthObserver(rec.observe))

	if err := negotiateOnce(c); err == nil {
		t.Fatal("negotiate succeeded against a server that refuses every bearer")
	}
	if got := server.seen(); len(got) != 1 {
		t.Fatalf("sent %d request(s) %v; an unchanged bearer must not be replayed", len(got), got)
	}
	failures := rec.all()
	if len(failures) != 1 || failures[0].Refreshed {
		t.Fatalf("refusals = %+v, want exactly one with Refreshed=false", failures)
	}
}

// A 403 is the cache server's SCOPE gate (`token is not authorized for
// cache.write`). Re-minting yields the same identity with the same grants, so a
// retry can only replay the rejection.
func TestDoAuthed_ScopeDenialIsReportedNotRetried(t *testing.T) {
	t.Setenv(TokenEnv, "")
	source := &mintingSource{}
	server := &cacheServer{
		accept: func(string) bool { return false },
		status: http.StatusForbidden,
		body: cache.ErrorResponse{
			ProtocolVersion: cache.ProtocolVersion,
			Code:            cache.CodeUnauthorized,
			Message:         "token is not authorized for cache.write",
		},
	}
	srv := server.start(t)

	rec := &authRecorder{}
	c := NewClient(srv.URL, "", WithBearerFunc(source.resolve), WithAuthObserver(rec.observe))

	if err := negotiateOnce(c); err == nil {
		t.Fatal("negotiate succeeded against a server that denies the scope")
	}
	if got := server.seen(); len(got) != 1 {
		t.Fatalf("sent %d request(s) %v; a scope denial must not be retried", len(got), got)
	}
	if source.mints() != 1 {
		t.Fatalf("minted %d bearer(s), want 1; a 403 must not trigger a re-mint", source.mints())
	}
	failures := rec.all()
	if len(failures) != 1 {
		t.Fatalf("recorded %d refusal(s), want 1: %+v", len(failures), failures)
	}
	if failures[0].Status != http.StatusForbidden || failures[0].Refreshed {
		t.Fatalf("refusal = %+v, want status=403 Refreshed=false", failures[0])
	}
	if !strings.Contains(failures[0].Message, "cache.write") {
		t.Fatalf("Message = %q; the server's rejection class must survive into the record", failures[0].Message)
	}
}

// A build refuses many keys at once (uploads run concurrently and each store is
// its own background goroutine). Every one of them holds the same stale bearer,
// so without generation-guarded invalidation each would run `putnami cloud
// token` — turning one expiry into a burst of token mints.
func TestDoAuthed_ConcurrentRefusalsReMintExactlyOnce(t *testing.T) {
	t.Setenv(TokenEnv, "")
	source := &mintingSource{}
	server := &cacheServer{accept: func(bearer string) bool { return bearer == "minted-2" }}
	srv := server.start(t)

	c := NewClient(srv.URL, "", WithBearerFunc(source.resolve))

	const callers = 24
	var wg sync.WaitGroup
	errs := make([]error, callers)
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = negotiateOnce(c)
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
	}
	if source.mints() != 2 {
		t.Fatalf("minted %d bearer(s) for %d concurrent refusals, want exactly 2 (initial + one refresh)",
			source.mints(), callers)
	}
}

// The refusal record is printed into a CI log, so it must carry classification
// only. A server that echoes the bearer back in its error body must not be able
// to launder the credential into that log.
func TestAuthFailure_NeverCarriesTheBearer(t *testing.T) {
	t.Setenv(TokenEnv, "")
	const secret = "pkt_super_secret_bearer_value"
	server := &cacheServer{
		accept: func(string) bool { return false },
		body: cache.ErrorResponse{
			ProtocolVersion: cache.ProtocolVersion,
			Code:            cache.CodeUnauthorized,
			Message:         "rejected bearer " + secret,
		},
	}
	srv := server.start(t)

	rec := &authRecorder{}
	c := NewClient(srv.URL, secret, WithAuthObserver(rec.observe))

	if err := negotiateOnce(c); err == nil {
		t.Fatal("negotiate succeeded against a server that refuses every bearer")
	}
	failures := rec.all()
	if len(failures) != 1 {
		t.Fatalf("recorded %d refusal(s), want 1: %+v", len(failures), failures)
	}
	if strings.Contains(failures[0].Message, secret) || strings.Contains(failures[0].String(), secret) {
		t.Fatalf("the refusal record leaked the bearer: %q", failures[0].String())
	}
	if failures[0].Source != TokenClassStatic {
		t.Errorf("Source = %q, want %q for a literal token", failures[0].Source, TokenClassStatic)
	}
}

func TestAuthFailure_BoundsTheServerReason(t *testing.T) {
	t.Setenv(TokenEnv, "")
	server := &cacheServer{
		accept: func(string) bool { return false },
		body: cache.ErrorResponse{
			ProtocolVersion: cache.ProtocolVersion,
			Code:            cache.CodeUnauthorized,
			Message:         strings.Repeat("x", maxReasonBytes*4),
		},
	}
	srv := server.start(t)

	rec := &authRecorder{}
	c := NewClient(srv.URL, "tok", WithAuthObserver(rec.observe))
	if err := negotiateOnce(c); err == nil {
		t.Fatal("negotiate succeeded against a server that refuses every bearer")
	}
	got := rec.all()[0].Message
	if len([]rune(got)) > maxReasonBytes+1 {
		t.Fatalf("reason is %d runes, want it bounded at %d (+ ellipsis)", len([]rune(got)), maxReasonBytes)
	}
}

// WithTokenFunc cannot report a class, so its credential is treated as static:
// a source that is not PROVEN renewable is never re-minted.
func TestWithTokenFunc_IsTreatedAsStaticAndNeverRefreshed(t *testing.T) {
	t.Setenv(TokenEnv, "")
	var calls int
	server := &cacheServer{accept: func(string) bool { return false }}
	srv := server.start(t)

	rec := &authRecorder{}
	c := NewClient(srv.URL, "", WithAuthObserver(rec.observe),
		WithTokenFunc(func(context.Context) (string, error) {
			calls++
			return fmt.Sprintf("legacy-%d", calls), nil
		}))

	if err := negotiateOnce(c); err == nil {
		t.Fatal("negotiate succeeded against a server that refuses every bearer")
	}
	if calls != 1 {
		t.Fatalf("resolver ran %d times, want 1; an opaque resolver must not be re-run", calls)
	}
	if got := server.seen(); len(got) != 1 {
		t.Fatalf("sent %d request(s) %v, want 1", len(got), got)
	}
	if rec.all()[0].Source != TokenClassStatic {
		t.Fatalf("Source = %q, want %q", rec.all()[0].Source, TokenClassStatic)
	}
}

// A refused read is worth reporting but leaves nothing broken behind; a refused
// WRITE is what strands the cache cold. The op label is what lets the runner
// tell them apart, so it must survive onto the record.
func TestAuthFailure_LabelsTheRefusedExchange(t *testing.T) {
	t.Setenv(TokenEnv, "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(cache.ErrorResponse{
			ProtocolVersion: cache.ProtocolVersion,
			Code:            cache.CodeUnauthorized,
			Message:         "expired",
		})
	}))
	defer srv.Close()

	rec := &authRecorder{}
	c := NewClient(srv.URL, "tok", WithAuthObserver(rec.observe))
	_, _ = c.Commit(context.Background(), &cache.CommitRequest{
		ProtocolVersion: cache.ProtocolVersion,
		Key:             keyBuild,
		Result:          &cache.ActionResult{Status: "success"},
		Manifest:        &cache.Manifest{Files: []cache.FileEntry{{Path: "out", Digest: cache.DigestOf([]byte("o")), Size: 1}}},
	})

	failures := rec.all()
	if len(failures) != 1 || failures[0].Op != "commit" {
		t.Fatalf("refusals = %+v, want exactly one labeled op=commit", failures)
	}
	if failures[0].Code != cache.CodeUnauthorized {
		t.Errorf("Code = %q, want %q", failures[0].Code, cache.CodeUnauthorized)
	}
}
