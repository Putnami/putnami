package main

import (
	"go.putnami.dev/protocol/features/spectest"

	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	stderrors "errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	phttp "go.putnami.dev/http"
	telemetry "go.putnami.dev/protocol/telemetry"
	cliusage "go.putnami.dev/protocol/telemetry/cliusage"

	"telemetry.putnami.dev/cliagg"
)

// aggregateNow is the clock the endpoint resolves windows against in tests.
var aggregateNow = time.Date(2026, 7, 28, 15, 4, 5, 0, time.UTC)

const (
	testAudience = "telemetry-dashboard"
	testCaller   = "service-account:cloud-operator"
)

// --- test doubles ---

// stubSource returns a canned report (or error) so the HTTP contract is
// exercised without a database.
type stubSource struct {
	report cliagg.Report
	err    error
	// windows records what the handler asked for, proving the caller cannot
	// steer anything but the fixed window.
	windows []cliagg.Window
}

func (s *stubSource) Report(_ context.Context, w cliagg.Window) (cliagg.Report, error) {
	s.windows = append(s.windows, w)
	if s.err != nil {
		return cliagg.Report{}, s.err
	}
	report := s.report
	report.Window = w
	return report, nil
}

func sampleReport() cliagg.Report {
	value := func(v int64) cliagg.Count { return cliagg.Count{State: cliagg.StateAvailable, Value: &v} }
	return cliagg.Report{
		SuppressionThreshold: cliagg.SuppressionThreshold,
		Freshness: cliagg.Freshness{
			GeneratedAt: aggregateNow.Format(time.RFC3339),
			State:       cliagg.StateAvailable,
			LatestDay:   "2026-07-28",
		},
		Summary: cliagg.Summary{
			Devices:  value(42),
			Sessions: value(100),
			Commands: value(120),
			Ingest: cliagg.IngestHealth{
				AcceptedRecords: cliagg.Count{State: cliagg.StateUnavailable, Reason: cliagg.ReasonNotPersisted},
				RejectedRecords: cliagg.Count{State: cliagg.StateUnavailable, Reason: cliagg.ReasonNotPersisted},
			},
		},
		Daily:      []cliagg.DailyPoint{{Day: "2026-07-28", Partial: true, Devices: value(42), Sessions: value(100)}},
		Breakdowns: []cliagg.Breakdown{{Dimension: cliagg.DimOutcome, Buckets: []cliagg.Bucket{{Key: "success", Count: 100}}, Other: value(0)}},
	}
}

// --- fake OIDC issuer ---

type fakeIssuer struct {
	url string
	key *rsa.PrivateKey
	kid string
	// fetches counts discovery/JWKS requests, so a test can prove the anonymous
	// ingest route never drives key resolution.
	fetches atomic.Int64
}

// newFakeIssuer serves an OIDC discovery document and a JWKS over loopback,
// so the tests exercise the real JWKS verification path end to end.
func newFakeIssuer(t *testing.T) *fakeIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	issuer := &fakeIssuer{key: key, kid: "test-key-1"}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		issuer.fetches.Add(1)
		w.Header().Set("Content-Type", "application/json")
		writeJSON(t, w, map[string]any{"issuer": issuer.url, "jwks_uri": issuer.url + "/jwks"})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		issuer.fetches.Add(1)
		w.Header().Set("Content-Type", "application/json")
		writeJSON(t, w, map[string]any{"keys": []map[string]any{{
			"kty": "RSA",
			"kid": issuer.kid,
			"use": "sig",
			"alg": "RS256",
			"n":   base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes()),
			"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.PublicKey.E)).Bytes()),
		}}})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	issuer.url = srv.URL
	return issuer
}

func writeJSON(t *testing.T, w http.ResponseWriter, body any) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(body); err != nil {
		t.Errorf("write JSON: %v", err)
	}
}

// token mints an RS256 JWT with the given claims, defaulting iss/aud/sub/exp to
// the values the endpoint is configured to accept.
func (f *fakeIssuer) token(t *testing.T, claims map[string]any) string {
	t.Helper()
	full := map[string]any{
		"iss": f.url,
		"aud": testAudience,
		"sub": testCaller,
		"exp": time.Now().Add(10 * time.Minute).Unix(),
	}
	for k, v := range claims {
		if v == nil {
			delete(full, k)
			continue
		}
		full[k] = v
	}
	return f.sign(t, full, "RS256")
}

func (f *fakeIssuer) sign(t *testing.T, claims map[string]any, alg string) string {
	t.Helper()
	headerJSON, err := json.Marshal(map[string]string{"alg": alg, "typ": "JWT", "kid": f.kid})
	if err != nil {
		t.Fatal(err)
	}
	payloadJSON, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	signingInput := base64.RawURLEncoding.EncodeToString(headerJSON) + "." + base64.RawURLEncoding.EncodeToString(payloadJSON)
	sum := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// --- harness ---

func newAggregateServer(t *testing.T, endpoint *aggregateEndpoint) *httptest.Server {
	t.Helper()
	ts := newServer(permissiveConfig(), &fakeEmitter{}, endpoint).TestServer()
	t.Cleanup(ts.Close)
	return ts
}

func secureEndpoint(t *testing.T, issuer *fakeIssuer, source cliagg.ReportSource) *aggregateEndpoint {
	t.Helper()
	return &aggregateEndpoint{
		source: source,
		auth: aggregateAuth{
			Audience: testAudience,
			Issuer:   issuer.url,
			Callers:  []string{testCaller},
		},
		now: func() time.Time { return aggregateNow },
	}
}

func get(t *testing.T, ts *httptest.Server, path, bearer string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, ts.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp, body
}

// --- authentication boundary ---

// TestAnonymousIsDeniedWhileIngestStaysAnonymous is the headline acceptance
// criterion: the read route refuses an unauthenticated caller while
// POST /v1/logs keeps answering 202 with no credential at all.
func TestAnonymousIsDeniedWhileIngestStaysAnonymous(t *testing.T) {
	spectest.Proves(t, "telemetry-putnami-dev/cli-usage-receiver", "ingest-is-anonymous", "a-bearer-token-on-ingest-is-ignored")
	issuer := newFakeIssuer(t)
	ts := newAggregateServer(t, secureEndpoint(t, issuer, &stubSource{report: sampleReport()}))

	resp, body := get(t, ts, aggregatePath, "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous aggregate read = %d, want 401 (body %s)", resp.StatusCode, body)
	}

	logsResp := post(t, ts, telemetry.PathLogs, cliusage.GoldenLogsJSON, nil)
	defer logsResp.Body.Close()
	if logsResp.StatusCode != http.StatusAccepted {
		t.Fatalf("anonymous POST %s = %d, want 202", telemetry.PathLogs, logsResp.StatusCode)
	}
}

// TestIngestRouteRunsNoIdentityResolution proves the auth chain is scoped to the
// read route: a bearer token on the anonymous ingest route is ignored and never
// reaches the identity resolver, so an anonymous client cannot drive JWKS
// discovery or key fetches through POST /v1/logs.
func TestIngestRouteRunsNoIdentityResolution(t *testing.T) {
	spectest.Proves(t, "telemetry-putnami-dev/cli-usage-receiver", "ingest-is-anonymous", "the-ingest-route-never-drives-a-key-fetch")
	issuer := newFakeIssuer(t)
	ts := newAggregateServer(t, secureEndpoint(t, issuer, &stubSource{report: sampleReport()}))

	resp := post(t, ts, telemetry.PathLogs, cliusage.GoldenLogsJSON, map[string]string{
		"Authorization": "Bearer " + issuer.token(t, map[string]any{"kid": "unknown"}),
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST %s with a bearer = %d, want 202", telemetry.PathLogs, resp.StatusCode)
	}
	if got := issuer.fetches.Load(); got != 0 {
		t.Fatalf("the ingest route triggered %d key fetches, want 0", got)
	}

	// The read route does resolve keys, so the counter is a real signal.
	if _, _ = get(t, ts, aggregatePath, issuer.token(t, nil)); issuer.fetches.Load() == 0 {
		t.Fatal("the read route resolved no key; the fetch counter proves nothing")
	}
}

// TestUnconfiguredAuthFailsClosed proves an incomplete configuration denies
// rather than opens: the route is mounted (so the described route inventory is
// stable) but answers 401 to everyone, credential or not.
func TestUnconfiguredAuthFailsClosed(t *testing.T) {
	spectest.Proves(t, "telemetry-putnami-dev/cli-usage-receiver", "aggregate-read-fails-closed", "an-unconfigured-issuer-or-audience-denies-every-request")
	issuer := newFakeIssuer(t)
	source := &stubSource{report: sampleReport()}
	full := aggregateAuth{Audience: testAudience, Issuer: issuer.url, Callers: []string{testCaller}}

	tests := []struct {
		name string
		auth aggregateAuth
	}{
		{name: "nothing configured", auth: aggregateAuth{}},
		{name: "no audience", auth: aggregateAuth{Issuer: full.Issuer, Callers: full.Callers}},
		{name: "no issuer", auth: aggregateAuth{Audience: full.Audience, Callers: full.Callers}},
		{name: "no callers", auth: aggregateAuth{Audience: full.Audience, Issuer: full.Issuer}},
		{name: "blank audience", auth: aggregateAuth{Audience: "  ", Issuer: full.Issuer, Callers: full.Callers}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.auth.configured() {
				t.Fatalf("auth %+v must not count as configured", tc.auth)
			}
			ts := newAggregateServer(t, &aggregateEndpoint{
				source: source,
				auth:   tc.auth,
				now:    func() time.Time { return aggregateNow },
			})

			// Anonymous and credentialed callers are both refused.
			for _, bearer := range []string{"", issuer.token(t, nil)} {
				resp, body := get(t, ts, aggregatePath, bearer)
				if resp.StatusCode != http.StatusUnauthorized {
					t.Fatalf("aggregate read = %d, want 401 (body %s)", resp.StatusCode, body)
				}
			}
			// The anonymous ingest route is untouched by the deny.
			logsResp := post(t, ts, telemetry.PathLogs, cliusage.GoldenLogsJSON, nil)
			defer logsResp.Body.Close()
			if logsResp.StatusCode != http.StatusAccepted {
				t.Fatalf("POST %s = %d, want 202", telemetry.PathLogs, logsResp.StatusCode)
			}
		})
	}
}

// TestPinnedIdentityIsRequired covers the audience/issuer/caller pinning matrix.
// A token that fails signature, audience, or issuer validation never becomes an
// identity (401); a valid identity outside the allowlist is authenticated but
// unauthorized (403).
func TestPinnedIdentityIsRequired(t *testing.T) {
	spectest.Proves(t, "telemetry-putnami-dev/cli-usage-receiver", "aggregate-read-fails-closed", "a-pinned-identity-is-required")
	issuer := newFakeIssuer(t)
	other := newFakeIssuer(t)

	tests := []struct {
		name   string
		token  func(t *testing.T) string
		status int
	}{
		{
			name:   "valid pinned caller",
			token:  func(t *testing.T) string { return issuer.token(t, nil) },
			status: http.StatusOK,
		},
		{
			name:   "wrong audience",
			token:  func(t *testing.T) string { return issuer.token(t, map[string]any{"aud": "some-other-service"}) },
			status: http.StatusUnauthorized,
		},
		{
			name:   "missing audience",
			token:  func(t *testing.T) string { return issuer.token(t, map[string]any{"aud": nil}) },
			status: http.StatusUnauthorized,
		},
		{
			name:   "wrong issuer claim",
			token:  func(t *testing.T) string { return issuer.token(t, map[string]any{"iss": "https://evil.example"}) },
			status: http.StatusUnauthorized,
		},
		{
			name:   "token from another issuer, correct claims",
			token:  func(t *testing.T) string { return other.token(t, map[string]any{"iss": other.url}) },
			status: http.StatusUnauthorized,
		},
		{
			name: "unlisted caller",
			token: func(t *testing.T) string {
				return issuer.token(t, map[string]any{"sub": "service-account:someone-else"})
			},
			status: http.StatusForbidden,
		},
		{
			name: "expired token",
			token: func(t *testing.T) string {
				return issuer.token(t, map[string]any{"exp": time.Now().Add(-time.Minute).Unix()})
			},
			status: http.StatusUnauthorized,
		},
		{
			name:   "token without expiry",
			token:  func(t *testing.T) string { return issuer.token(t, map[string]any{"exp": nil}) },
			status: http.StatusUnauthorized,
		},
		{
			name: "algorithm confusion (HS256 header)",
			token: func(t *testing.T) string {
				return issuer.sign(t, map[string]any{"iss": issuer.url, "aud": testAudience, "sub": testCaller, "exp": time.Now().Add(time.Minute).Unix()}, "HS256")
			},
			status: http.StatusUnauthorized,
		},
		{
			name:   "garbage bearer",
			token:  func(t *testing.T) string { return "not-a-token" },
			status: http.StatusUnauthorized,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ts := newAggregateServer(t, secureEndpoint(t, issuer, &stubSource{report: sampleReport()}))
			resp, body := get(t, ts, aggregatePath, tc.token(t))
			if resp.StatusCode != tc.status {
				t.Fatalf("status = %d, want %d (body %s)", resp.StatusCode, tc.status, body)
			}
		})
	}
}

// TestAuthorizationRechecksTheIssuerIndependently pins the redundancy in
// aggregateAuth.allows. The resolver already rejects a token whose issuer is not
// the pinned one, so the re-check inside allows never fires in the wired server
// — which is exactly why nothing proved it, and why removing it left the whole
// suite green. Its purpose is to make the authorization decision independent of
// resolver ordering: if a future change ever resolves an identity before pinning
// the issuer, allows must still refuse. Asserted at the unit level, because the
// wired path cannot reach the state it defends against.
func TestAuthorizationRechecksTheIssuerIndependently(t *testing.T) {
	spectest.Proves(t, "telemetry-putnami-dev/cli-usage-receiver", "aggregate-read-fails-closed", "a-pinned-identity-is-required")

	auth := aggregateAuth{
		Issuer:   "https://issuer.example",
		Audience: testAudience,
		Callers:  []string{testCaller},
	}

	if !auth.allows(&phttp.Claims{Issuer: auth.Issuer, Subject: testCaller}) {
		t.Fatal("a listed caller from the pinned issuer must be allowed; the fixture is wrong")
	}
	for _, issuer := range []string{"https://evil.example", ""} {
		if auth.allows(&phttp.Claims{Issuer: issuer, Subject: testCaller}) {
			t.Errorf("claims from issuer %q were authorized; allows must re-check the issuer rather than trust the resolver's ordering", issuer)
		}
	}
}

// TestAllowlistAcceptsAnEmailClaim documents the second identity form the
// allowlist accepts, and that the match is case-insensitive for emails only.
func TestAllowlistAcceptsAnEmailClaim(t *testing.T) {
	spectest.Proves(t, "telemetry-putnami-dev/cli-usage-receiver", "aggregate-read-fails-closed", "the-caller-allowlist-must-be-satisfied")
	issuer := newFakeIssuer(t)
	endpoint := secureEndpoint(t, issuer, &stubSource{report: sampleReport()})
	endpoint.auth.Callers = []string{"dashboard@putnami.dev"}
	ts := newAggregateServer(t, endpoint)

	resp, body := get(t, ts, aggregatePath, issuer.token(t, map[string]any{
		"sub": "service-account:unlisted", "email": "Dashboard@Putnami.dev",
	}))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", resp.StatusCode, body)
	}

	// A subject that only differs in case is NOT accepted: subjects are exact.
	endpoint2 := secureEndpoint(t, issuer, &stubSource{report: sampleReport()})
	ts2 := newAggregateServer(t, endpoint2)
	resp2, body2 := get(t, ts2, aggregatePath, issuer.token(t, map[string]any{"sub": strings.ToUpper(testCaller)}))
	if resp2.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for a case-mangled subject (body %s)", resp2.StatusCode, body2)
	}
}

// --- bounded contract ---

func TestWindowValidationOverHTTP(t *testing.T) {
	issuer := newFakeIssuer(t)

	t.Run("accepted windows", func(t *testing.T) {
		for _, spec := range cliagg.Windows() {
			source := &stubSource{report: sampleReport()}
			ts := newAggregateServer(t, secureEndpoint(t, issuer, source))
			resp, body := get(t, ts, aggregatePath+"?window="+spec, issuer.token(t, nil))
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("window %q = %d, want 200 (body %s)", spec, resp.StatusCode, body)
			}
			var report cliagg.Report
			if err := json.Unmarshal(body, &report); err != nil {
				t.Fatalf("decode report: %v", err)
			}
			if report.Window.Spec != spec {
				t.Errorf("echoed window = %q, want %q", report.Window.Spec, spec)
			}
			if !report.Window.PartialBucket {
				t.Errorf("window %q partialBucket = false, want true", spec)
			}
			if len(source.windows) != 1 || source.windows[0].Spec != spec {
				t.Errorf("source saw %+v, want exactly one %q window", source.windows, spec)
			}
		}
	})

	t.Run("omitted window defaults", func(t *testing.T) {
		source := &stubSource{report: sampleReport()}
		ts := newAggregateServer(t, secureEndpoint(t, issuer, source))
		resp, body := get(t, ts, aggregatePath, issuer.token(t, nil))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", resp.StatusCode, body)
		}
		if len(source.windows) != 1 || source.windows[0].Spec != cliagg.DefaultWindow {
			t.Fatalf("source saw %+v, want the %q default", source.windows, cliagg.DefaultWindow)
		}
	})

	t.Run("rejected windows", func(t *testing.T) {
		rejected := []string{"2d", "90d", "1D", "0d", "all", "custom", "1d,7d", "2026-07-01..2026-07-28", "-1d"}
		for _, spec := range rejected {
			source := &stubSource{report: sampleReport()}
			ts := newAggregateServer(t, secureEndpoint(t, issuer, source))
			resp, body := get(t, ts, aggregatePath+"?window="+spec, issuer.token(t, nil))
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("window %q = %d, want 400 (body %s)", spec, resp.StatusCode, body)
			}
			if len(source.windows) != 0 {
				t.Fatalf("window %q reached the datasource: %+v", spec, source.windows)
			}
			// The rejected value is never echoed; only the fixed allowed list is.
			// ("0d" is checked in quoted form so it does not match "30d".)
			if strings.Contains(string(body), `"`+spec+`"`) {
				t.Fatalf("the rejected value %q was reflected in the response: %s", spec, body)
			}
		}
	})
}

// TestNoSteeringParameterExists proves the caller cannot aim the read at another
// datasource, workspace, project, or tenant: no parameter but the window is
// accepted at all, and the rejected name is never echoed back.
func TestNoSteeringParameterExists(t *testing.T) {
	issuer := newFakeIssuer(t)
	steering := []string{
		"datasource=other", "dsn=postgres://elsewhere", "workspace=acme", "project=secret-project",
		"tenant=other-tenant", "db=telemetry2", "table=cli_device_day", "device=abc123",
		"query=select+1", "filter=device_id", "limit=100000", "cursor=abc", "start=2020-01-01",
	}
	for _, param := range steering {
		source := &stubSource{report: sampleReport()}
		ts := newAggregateServer(t, secureEndpoint(t, issuer, source))
		resp, body := get(t, ts, aggregatePath+"?"+param, issuer.token(t, nil))
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("parameter %q = %d, want 400 (body %s)", param, resp.StatusCode, body)
		}
		if len(source.windows) != 0 {
			t.Fatalf("parameter %q reached the datasource", param)
		}
		name, value, _ := strings.Cut(param, "=")
		if strings.Contains(string(body), name) || strings.Contains(string(body), value) {
			t.Fatalf("parameter %q was reflected in the response: %s", param, body)
		}
	}

	// The same rejection applies when a steering parameter rides along with a
	// valid window, so it cannot be smuggled in.
	source := &stubSource{report: sampleReport()}
	ts := newAggregateServer(t, secureEndpoint(t, issuer, source))
	resp, _ := get(t, ts, aggregatePath+"?window=7d&datasource=other", issuer.token(t, nil))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("smuggled parameter = %d, want 400", resp.StatusCode)
	}
}

// TestOnlyGetIsMounted keeps the surface read-only: no write, export, or
// alternate verb exists on the aggregate path.
func TestOnlyGetIsMounted(t *testing.T) {
	issuer := newFakeIssuer(t)
	ts := newAggregateServer(t, secureEndpoint(t, issuer, &stubSource{report: sampleReport()}))
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		req, err := http.NewRequest(method, ts.URL+aggregatePath, strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+issuer.token(t, nil))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s %s = %d, want 404/405", method, aggregatePath, resp.StatusCode)
		}
	}
}

// --- failure modes ---

func TestDatasourceUnavailableIsExplicit(t *testing.T) {
	issuer := newFakeIssuer(t)
	ts := newAggregateServer(t, secureEndpoint(t, issuer, &stubSource{err: cliagg.ErrNoDatasource}))
	resp, body := get(t, ts, aggregatePath, issuer.token(t, nil))
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body %s)", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "aggregate_store_unavailable") {
		t.Fatalf("body = %s, want the fixed unavailable code", body)
	}
}

func TestOverflowedProjectionIsUnavailableNotPartial(t *testing.T) {
	spectest.Proves(t, "telemetry-putnami-dev/cli-usage-receiver", "exhausted-dimension-is-reported-unavailable", "an-exhausted-dimension-is-unavailable-not-a-partial-count")
	issuer := newFakeIssuer(t)
	ts := newAggregateServer(t, secureEndpoint(t, issuer, &stubSource{err: cliagg.ErrProjectionOverflow}))
	resp, body := get(t, ts, aggregatePath, issuer.token(t, nil))
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body %s)", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "aggregate_store_unavailable") {
		t.Fatalf("body = %s, want the fixed unavailable code", body)
	}
}

// TestReadErrorsNeverLeakDetail proves a failure answers with a fixed code: no
// driver text, statement, connection string, or identifier reaches the caller.
func TestReadErrorsNeverLeakDetail(t *testing.T) {
	issuer := newFakeIssuer(t)
	secret := "postgres://user:hunter2@db.internal/telemetry device_id=abc-123"
	ts := newAggregateServer(t, secureEndpoint(t, issuer, &stubSource{err: stderrors.New(secret)}))

	resp, body := get(t, ts, aggregatePath, issuer.token(t, nil))
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body %s)", resp.StatusCode, body)
	}
	for _, fragment := range []string{"hunter2", "db.internal", "device_id", "abc-123", "postgres"} {
		if strings.Contains(string(body), fragment) {
			t.Fatalf("error body leaked %q: %s", fragment, body)
		}
	}
}

// TestNilSourceIsUnavailableNotAPanic guards the wiring: an endpoint built
// without a source answers 503 rather than crashing the workload.
func TestNilSourceIsUnavailableNotAPanic(t *testing.T) {
	issuer := newFakeIssuer(t)
	endpoint := secureEndpoint(t, issuer, nil)
	ts := newAggregateServer(t, endpoint)
	resp, body := get(t, ts, aggregatePath, issuer.token(t, nil))
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body %s)", resp.StatusCode, body)
	}
}

// TestResponseCarriesExplicitStates checks the served payload keeps the
// available/unavailable/suppressed contract intact through JSON.
func TestResponseCarriesExplicitStates(t *testing.T) {
	issuer := newFakeIssuer(t)
	ts := newAggregateServer(t, secureEndpoint(t, issuer, &stubSource{report: sampleReport()}))
	resp, body := get(t, ts, aggregatePath, issuer.token(t, nil))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", resp.StatusCode, body)
	}

	var decoded struct {
		Summary struct {
			Devices struct {
				State string `json:"state"`
				Value *int64 `json:"value"`
			} `json:"devices"`
			Ingest struct {
				AcceptedRecords struct {
					State  string `json:"state"`
					Value  *int64 `json:"value"`
					Reason string `json:"reason"`
				} `json:"acceptedRecords"`
			} `json:"ingest"`
		} `json:"summary"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store: the report is scoped to one caller", got)
	}
	if decoded.Summary.Devices.State != cliagg.StateAvailable || decoded.Summary.Devices.Value == nil {
		t.Errorf("devices = %+v, want an available value", decoded.Summary.Devices)
	}
	ingest := decoded.Summary.Ingest.AcceptedRecords
	if ingest.State != cliagg.StateUnavailable || ingest.Value != nil || ingest.Reason != cliagg.ReasonNotPersisted {
		t.Errorf("ingest.acceptedRecords = %+v, want unavailable with no value", ingest)
	}
}
