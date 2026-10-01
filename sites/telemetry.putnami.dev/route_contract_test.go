package main

// Public route contract.
//
// A hosting platform can enforce default-deny routing on a public host: only
// what this workload's schema/http-routes.json declares reaches the workload,
// and every other path or method terminates at a shared reject backend. This
// repository owns telemetry.putnami.dev, so it owns the evidence for that
// enforcement. These tests pin both sides of the boundary:
//
//   - the committed artifact is byte-identical to what the running server
//     describes, canonical, and independent of deploy-time configuration;
//   - every declared path and method carries real traffic;
//   - every undeclared path and method is rejected before the workload is
//     invoked, and rejecting it removes no telemetry-protocol behavior.
//
// doc/public-route-contract.md records the resulting contract, the
// authentication, CORS, batching, and content-type findings, and the ownership
// split with the platform that generates its route inventory from it.

import (
	"go.putnami.dev/protocol/features/spectest"

	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"go.putnami.dev/app"
	phttp "go.putnami.dev/http"
	httproutes "go.putnami.dev/protocol/http-routes"
	telemetry "go.putnami.dev/protocol/telemetry"
	cliusage "go.putnami.dev/protocol/telemetry/cliusage"
)

// routeArtifactPath is the committed canonical inventory, relative to the
// package directory (tests run with the package directory as working dir). It is
// the framework's own describe path, so a rename upstream fails here rather than
// silently leaving an orphaned contract behind.
const routeArtifactPath = phttp.HTTPRoutesDescribePath

// edgeRuleBudget bounds how many (path, method) rules this host contributes to
// the shared Google URL map. The provider limit is far higher; the point of the
// bound is that growing this workload's public surface — especially into a
// prefix or template match — is a deliberate act that revisits the Cloud
// rollout instead of silently enlarging the enforced matcher.
const edgeRuleBudget = 8

// --- artifact helpers ---

func readRouteArtifact(t *testing.T) []byte {
	t.Helper()
	body, err := os.ReadFile(routeArtifactPath)
	if err != nil {
		t.Fatalf("read %s: %v", routeArtifactPath, err)
	}
	return body
}

func parseRouteArtifact(t *testing.T) *httproutes.Manifest {
	t.Helper()
	manifest, diags := httproutes.ParseAndValidateManifest(readRouteArtifact(t))
	if manifest == nil || len(diags) != 0 {
		t.Fatalf("%s is not a valid %s inventory: %v", routeArtifactPath, httproutes.Protocol, diags)
	}
	return manifest
}

// TestRouteEnforcementIsExplicitlyEnabled keeps the production opt-in next to
// the contract it authorizes. The Cloud deploy client reads this setting from
// the release source; absent or malformed configuration is deliberately
// default-off, so a future cleanup must not silently return this host to
// hostname-default routing.
func TestRouteEnforcementIsExplicitlyEnabled(t *testing.T) {
	data, err := os.ReadFile("putnami.json")
	if err != nil {
		t.Fatalf("read putnami.json: %v", err)
	}

	var project struct {
		Options map[string]json.RawMessage `json:"options"`
	}
	if err := json.Unmarshal(data, &project); err != nil {
		t.Fatalf("parse putnami.json: %v", err)
	}
	raw, ok := project.Options["@putnami/cloud"]
	if !ok {
		t.Fatal("putnami.json does not declare @putnami/cloud deployment intent")
	}
	var cloud struct {
		Deploy struct {
			EnforceHTTPRoutes bool `json:"enforceHttpRoutes"`
		} `json:"deploy"`
	}
	if err := json.Unmarshal(raw, &cloud); err != nil {
		t.Fatalf("parse @putnami/cloud deployment intent: %v", err)
	}
	if !cloud.Deploy.EnforceHTTPRoutes {
		t.Fatal("@putnami/cloud.deploy.enforceHttpRoutes = false, want true")
	}
}

// describedRouteArtifact runs the framework's describe surface over a server
// wired exactly like main(), returning the canonical bytes it would write.
func describedRouteArtifact(t *testing.T, server *phttp.ServerPlugin) []byte {
	t.Helper()
	if err := server.Configure(context.Background(), app.NewModule(appName)); err != nil {
		t.Fatalf("configure server plugin: %v", err)
	}
	out := t.TempDir()
	if err := server.Describe(&app.DescribeContext{OutputDir: out}); err != nil {
		t.Fatalf("describe http routes: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(out, phttp.HTTPRoutesDescribePath))
	if err != nil {
		t.Fatalf("read described inventory: %v", err)
	}
	return body
}

// declaredRoutes projects the committed artifact into the exact-match table the
// edge enforces: path → set of admitted methods. It fails the test on any
// non-exact route, because the simulator below (like the assertions that reason
// about "undeclared paths") would no longer be a faithful model of the gateway.
func declaredRoutes(t *testing.T) map[string]map[string]bool {
	t.Helper()
	table := make(map[string]map[string]bool)
	for _, route := range parseRouteArtifact(t).Routes {
		if route.Match != httproutes.MatchExact {
			t.Fatalf("route %s uses match %q; the exact-match edge model in this file no longer applies "+
				"and the enforcement rollout must be revisited", route.Path, route.Match)
		}
		methods := table[route.Path]
		if methods == nil {
			methods = make(map[string]bool)
			table[route.Path] = methods
		}
		for _, method := range route.Methods {
			methods[method] = true
		}
	}
	return table
}

// --- default-deny edge simulator ---

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// enforcedEdge is a minimal stand-in for the Cloud default-deny URL map: it
// admits exactly what the committed artifact declares and answers everything
// else with the reject backend's 404, without ever invoking the workload.
// reached counts the requests that actually hit the workload, which is the
// local equivalent of the rollout gate's "rejected probes produce zero workload
// log entries".
type enforcedEdge struct {
	url     string
	reached *atomic.Int64
}

func newEnforcedEdge(t *testing.T, workload *httptest.Server) *enforcedEdge {
	t.Helper()
	allowed := declaredRoutes(t)
	target, err := url.Parse(workload.URL)
	if err != nil {
		t.Fatalf("parse workload URL: %v", err)
	}

	var reached atomic.Int64
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		reached.Add(1)
		return http.DefaultTransport.RoundTrip(r)
	})

	edge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Google matches the exact path; the query string never participates.
		if !allowed[r.URL.Path][r.Method] {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(edge.Close)
	return &enforcedEdge{url: edge.URL, reached: &reached}
}

// request issues one probe and returns the status, headers, and body. baseURL is
// either the edge (enforced) or the workload (unenforced) so both sides of the
// boundary are probed with the same helper.
func request(t *testing.T, baseURL, method, path string, headers map[string]string, body []byte) (*http.Response, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, baseURL+path, reader)
	if err != nil {
		t.Fatalf("build %s %s: %v", method, path, err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	// The production CLI sender refuses to follow redirects; probes must observe
	// the workload's own status, never a followed hop.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s %s body: %v", method, path, err)
	}
	return resp, payload
}

// undeclaredProbes are the requests the contract does not declare, so the edge
// must reject all of them. They cover the wrong OTLP signals, wrong methods on
// both declared paths, the trailing-slash forms, the framework health path, the
// host root, and browser preflights on both routes.
//
// workloadStatus is what the workload answers today, with no edge in front. It
// is the exact measure of what enforcement changes: a 404 changes nothing, and
// anything else is a behavior this repository is deliberately giving up.
var undeclaredProbes = []struct {
	method string
	path   string
	// bearer sends the aggregate caller's valid token, so a rejection is proven
	// to be the route contract rather than the authentication result.
	bearer         bool
	workloadStatus int
	note           string
}{
	{method: http.MethodPost, path: telemetry.PathMetrics, workloadStatus: http.StatusNotFound},
	{method: http.MethodPost, path: telemetry.PathTraces, workloadStatus: http.StatusNotFound},
	{method: http.MethodGet, path: telemetry.PathLogs, workloadStatus: http.StatusNotFound},
	{method: http.MethodDelete, path: telemetry.PathLogs, workloadStatus: http.StatusNotFound},
	{method: http.MethodPost, path: aggregatePath, bearer: true, workloadStatus: http.StatusNotFound},
	{method: http.MethodGet, path: "/_/health", workloadStatus: http.StatusNotFound,
		note: "the workload composes no health plugin, so no health route exists to declare"},
	{method: http.MethodGet, path: "/", workloadStatus: http.StatusNotFound},

	// The framework synthesizes a preflight answer for a known path with no
	// OPTIONS route. On the anonymous route it answers 204; on the authenticated
	// route the scoped auth chain still runs first, so an anonymous preflight is
	// refused rather than leaking the route's Allow header.
	{method: http.MethodOptions, path: telemetry.PathLogs, workloadStatus: http.StatusNoContent,
		note: "synthesized preflight; carries no Access-Control-* header because no CORS policy is installed"},
	{method: http.MethodOptions, path: aggregatePath, workloadStatus: http.StatusUnauthorized,
		note: "synthesized preflight, refused by the route-scoped auth chain"},

	// putnami.http-routes.v1 exact matching is byte-for-byte, so a trailing slash
	// is a distinct path the contract does not declare. The Go router splits it
	// into a trailing empty segment and still matches the registered pattern, so
	// the workload answers these today; no production client emits them.
	{method: http.MethodPost, path: telemetry.PathLogs + "/", workloadStatus: http.StatusAccepted,
		note: "trailing-slash form of the declared ingest route"},
	{method: http.MethodGet, path: aggregatePath + "/", bearer: true, workloadStatus: http.StatusOK,
		note: "trailing-slash form of the declared aggregate route"},
}

// --- the artifact is complete, canonical, and workload-owned ---

// TestCommittedRouteArtifactMatchesServedRoutes is the completeness invariant:
// the committed contract is exactly what this workload serves. A route added to
// newServer without regenerating the artifact would be served but undeclared —
// after enforcement, silently unreachable in production.
func TestCommittedRouteArtifactMatchesServedRoutes(t *testing.T) {
	spectest.Proves(t, "telemetry-putnami-dev/cli-usage-receiver", "declared-surface-is-complete", "the-committed-artifact-matches-what-the-server-describes")
	described := describedRouteArtifact(t, newServer(permissiveConfig(), &fakeEmitter{}, nil))
	if committed := readRouteArtifact(t); !bytes.Equal(committed, described) {
		t.Fatalf("%s is stale.\ncommitted:\n%s\ndescribed:\n%s", routeArtifactPath, committed, described)
	}
}

// TestRouteArtifactIsIndependentOfDeployTimeConfig proves the described contract
// is a property of the code, not of how the workload happens to be configured:
// admission limits, a collector endpoint, and a fully configured (or absent)
// aggregate reader all describe the same bytes. Without this, a deploy could
// publish a manifest narrower than the routes the instance actually serves.
func TestRouteArtifactIsIndependentOfDeployTimeConfig(t *testing.T) {
	spectest.Proves(t, "telemetry-putnami-dev/cli-usage-receiver", "declared-surface-is-complete", "the-artifact-is-independent-of-deploy-time-configuration")
	issuer := newFakeIssuer(t)
	configured := receiverConfig{
		port:              9090,
		collectorEndpoint: "https://collector.example",
		trustedProxies:    []string{"10.0.0.0/8"},
		perIPPerMinute:    7,
		globalQPS:         3,
		aggregateAuth: aggregateAuth{
			Audience: testAudience,
			Issuer:   issuer.url,
			Callers:  []string{testCaller},
		},
	}
	servers := map[string]*phttp.ServerPlugin{
		"defaults, no aggregate reader": newServer(permissiveConfig(), &fakeEmitter{}, nil),
		"configured, secured reader": newServer(configured, &fakeEmitter{},
			secureEndpoint(t, issuer, &stubSource{report: sampleReport()})),
	}

	committed := readRouteArtifact(t)
	for name, server := range servers {
		t.Run(name, func(t *testing.T) {
			if described := describedRouteArtifact(t, server); !bytes.Equal(committed, described) {
				t.Fatalf("described inventory depends on configuration:\n%s", described)
			}
		})
	}
}

// TestCommittedRouteArtifactIsCanonical pins the determinism invariant the
// gateway reconciler relies on: the committed bytes are the canonical form of
// their own content — same protocol, same ordering, same digest, byte-for-byte
// idempotent under a re-canonicalization round trip.
func TestCommittedRouteArtifactIsCanonical(t *testing.T) {
	spectest.Proves(t, "telemetry-putnami-dev/cli-usage-receiver", "declared-surface-is-complete", "the-committed-artifact-is-canonical")
	committed := readRouteArtifact(t)
	manifest := parseRouteArtifact(t)

	if manifest.Protocol != httproutes.Protocol {
		t.Fatalf("protocol = %q, want %q", manifest.Protocol, httproutes.Protocol)
	}
	if manifest.Schema != httproutes.SchemaURL {
		t.Fatalf("$schema = %q, want %q", manifest.Schema, httproutes.SchemaURL)
	}
	if !strings.HasPrefix(manifest.Digest, "sha256:") {
		t.Fatalf("digest = %q, want a sha256: digest", manifest.Digest)
	}

	// Re-canonicalizing recomputes ordering and digest from the routes alone, so
	// equality here proves the committed digest matches the committed routes.
	canonical, diags := httproutes.CanonicalJSON(manifest)
	if len(diags) != 0 {
		t.Fatalf("canonicalize committed manifest: %v", diags)
	}
	if !bytes.Equal(committed, canonical) {
		t.Fatalf("%s is not canonical.\ncommitted:\n%s\ncanonical:\n%s", routeArtifactPath, committed, canonical)
	}
}

// TestCommittedRouteArtifactIsWorkloadOwned records the ownership and shape
// facts the Cloud adoption issue needs: every route is this workload's own,
// exact-match, public-edge declaration, and the enforced matcher stays small.
func TestCommittedRouteArtifactIsWorkloadOwned(t *testing.T) {
	manifest := parseRouteArtifact(t)
	if len(manifest.Routes) == 0 {
		t.Fatal("the inventory declares no routes; a default-deny edge would reject all telemetry")
	}

	rules := 0
	for _, route := range manifest.Routes {
		if route.Match != httproutes.MatchExact {
			t.Errorf("route %s: match = %q, want %q (no prefix, template, or wildcard surface)",
				route.Path, route.Match, httproutes.MatchExact)
		}
		if strings.ContainsAny(route.Path, "{}[]:*") {
			t.Errorf("route %s: exact path carries pattern syntax", route.Path)
		}
		if !route.PublicEdge {
			t.Errorf("route %s: publicEdge = false, but the host serves it publicly", route.Path)
		}
		if route.Provenance.Project != appName {
			t.Errorf("route %s: provenance project = %q, want %q", route.Path, route.Provenance.Project, appName)
		}
		if route.Provenance.SourceKind != httproutes.SourceManual {
			t.Errorf("route %s: sourceKind = %q, want %q", route.Path, route.Provenance.SourceKind, httproutes.SourceManual)
		}
		if len(route.Methods) == 0 {
			t.Errorf("route %s: declares no methods", route.Path)
		}
		if !sort.StringsAreSorted(route.Methods) {
			t.Errorf("route %s: methods %v are not sorted", route.Path, route.Methods)
		}
		for _, method := range route.Methods {
			if method != strings.ToUpper(method) {
				t.Errorf("route %s: method %q is not upper case", route.Path, method)
			}
			rules++
		}
	}
	if rules > edgeRuleBudget {
		t.Errorf("the contract contributes %d (path, method) rules, over the %d-rule budget; "+
			"revisit the shared URL-map capacity with the platform before growing it", rules, edgeRuleBudget)
	}
}

// --- declared routes carry real traffic ---

// TestDeclaredRoutesCarryRealTraffic exercises every declared (path, method)
// pair through the enforced edge with a real telemetry payload or a real signed
// caller token, and fails if the contract declares a pair no probe covers. That
// coupling is what keeps this evidence honest as the contract evolves.
func TestDeclaredRoutesCarryRealTraffic(t *testing.T) {
	spectest.Proves(t, "telemetry-putnami-dev/cli-usage-receiver", "declared-surface-is-complete", "every-declared-path-and-method-carries-real-traffic")
	issuer := newFakeIssuer(t)
	emitter := &fakeEmitter{}
	source := &stubSource{report: sampleReport()}
	workload := newServer(permissiveConfig(), emitter, secureEndpoint(t, issuer, source)).TestServer()
	defer workload.Close()
	edge := newEnforcedEdge(t, workload)

	bearer := map[string]string{"Authorization": "Bearer " + issuer.token(t, nil)}
	otlp := map[string]string{"Content-Type": telemetry.ContentType}
	covered := make(map[string]bool)
	probe := func(method, path string, headers map[string]string, body []byte, want int) []byte {
		t.Helper()
		resp, payload := request(t, edge.url, method, path, headers, body)
		if resp.StatusCode != want {
			t.Fatalf("%s %s through the enforced edge = %d, want %d (body %s)", method, path, resp.StatusCode, want, payload)
		}
		covered[method+" "+strings.SplitN(path, "?", 2)[0]] = true
		return payload
	}

	// The anonymous CLI ingest flow, byte-for-byte the payload the production
	// sender encodes (protocols/telemetry/cliusage golden).
	probe(http.MethodPost, telemetry.PathLogs, otlp, cliusage.GoldenLogsJSON, http.StatusAccepted)
	if got := len(emitter.captured()); got != 1 {
		t.Fatalf("emitted resource logs = %d, want 1: the accepted payload did not reach the emitter", got)
	}

	// The authenticated Cloud reader flow, including the one query parameter the
	// contract accepts. The edge matches on path only, so the parameter rides the
	// same declared rule.
	body := probe(http.MethodGet, aggregatePath, bearer, nil, http.StatusOK)
	var report map[string]any
	if err := json.Unmarshal(body, &report); err != nil {
		t.Fatalf("aggregate response is not JSON: %v (%s)", err, body)
	}
	probe(http.MethodGet, aggregatePath+"?window=7d", bearer, nil, http.StatusOK)

	// HEAD is declared because the Go server answers it from the GET handler.
	if headBody := probe(http.MethodHead, aggregatePath, bearer, nil, http.StatusOK); len(headBody) != 0 {
		t.Fatalf("HEAD %s returned a body: %s", aggregatePath, headBody)
	}

	for path, methods := range declaredRoutes(t) {
		for method := range methods {
			if !covered[method+" "+path] {
				t.Errorf("declared %s %s has no traffic probe; the rollout evidence is incomplete", method, path)
			}
		}
	}
}

// --- undeclared traffic is rejected, and rejecting it costs nothing ---

// TestUndeclaredRequestsNeverReachTheWorkload is the default-deny half of the
// boundary: every undeclared probe returns 404 from the edge and none of them
// invokes the workload.
func TestUndeclaredRequestsNeverReachTheWorkload(t *testing.T) {
	spectest.Proves(t, "telemetry-putnami-dev/cli-usage-receiver", "undeclared-requests-are-rejected", "an-undeclared-request-never-reaches-workload-logic")
	issuer := newFakeIssuer(t)
	workload := newServer(permissiveConfig(), &fakeEmitter{},
		secureEndpoint(t, issuer, &stubSource{report: sampleReport()})).TestServer()
	defer workload.Close()
	edge := newEnforcedEdge(t, workload)

	token := issuer.token(t, nil)
	for _, tc := range undeclaredProbes {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			headers := map[string]string{"Content-Type": telemetry.ContentType}
			if tc.bearer {
				headers["Authorization"] = "Bearer " + token
			}
			resp, body := request(t, edge.url, tc.method, tc.path, headers, cliusage.GoldenLogsJSON)
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("%s %s through the enforced edge = %d, want 404 (body %s)", tc.method, tc.path, resp.StatusCode, body)
			}
		})
	}
	if got := edge.reached.Load(); got != 0 {
		t.Fatalf("%d rejected probe(s) reached the workload, want 0", got)
	}
}

// TestUndeclaredRequestsMeasureWhatEnforcementChanges records the unenforced
// behavior of every undeclared probe, which is exactly what the rollout gives
// up. Most already answer 404, so enforcement changes nothing; the synthesized
// preflights and the two trailing-slash forms are the whole difference, and none
// of them is reachable by a production client. Nothing here may leak CORS
// headers, since a browser flow is the one client that would regress.
func TestUndeclaredRequestsMeasureWhatEnforcementChanges(t *testing.T) {
	issuer := newFakeIssuer(t)
	workload := newServer(permissiveConfig(), &fakeEmitter{},
		secureEndpoint(t, issuer, &stubSource{report: sampleReport()})).TestServer()
	defer workload.Close()

	token := issuer.token(t, nil)
	for _, tc := range undeclaredProbes {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			headers := map[string]string{"Content-Type": telemetry.ContentType}
			if tc.bearer {
				headers["Authorization"] = "Bearer " + token
			}
			resp, body := request(t, workload.URL, tc.method, tc.path, headers, cliusage.GoldenLogsJSON)
			if resp.StatusCode != tc.workloadStatus {
				note := tc.note
				if note == "" {
					note = "no recorded exception: this probe is expected to be rejected by the workload itself"
				}
				t.Fatalf("%s %s without the edge = %d, want %d (%s) — doc/public-route-contract.md is stale (body %s)",
					tc.method, tc.path, resp.StatusCode, tc.workloadStatus, note, body)
			}
			for header := range resp.Header {
				if strings.HasPrefix(http.CanonicalHeaderKey(header), "Access-Control-") {
					t.Fatalf("%s %s answered with %s; a browser client would regress under enforcement",
						tc.method, tc.path, header)
				}
			}
		})
	}
}

// TestTrailingSlashFormKeepsTheAuthenticationBoundary is the security half of
// the trailing-slash finding: the router matches /v1/cli-usage/aggregate/ onto
// the registered pattern, and the route-scoped identity resolver and guard key
// off that pattern, so the variant form is authenticated exactly like the
// declared path. A variant that skipped the chain would be an anonymous read of
// the private aggregate route, with or without edge enforcement.
func TestTrailingSlashFormKeepsTheAuthenticationBoundary(t *testing.T) {
	spectest.Proves(t, "telemetry-putnami-dev/cli-usage-receiver", "aggregate-read-fails-closed", "the-trailing-slash-form-authenticates-like-the-declared-path")
	issuer := newFakeIssuer(t)
	workload := newServer(permissiveConfig(), &fakeEmitter{},
		secureEndpoint(t, issuer, &stubSource{report: sampleReport()})).TestServer()
	defer workload.Close()

	for _, path := range []string{aggregatePath, aggregatePath + "/"} {
		resp, body := request(t, workload.URL, http.MethodGet, path, nil, nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("anonymous GET %s = %d, want 401 (body %s)", path, resp.StatusCode, body)
		}
	}
}

// TestTelemetryProtocolBehaviorSurvivesEnforcement pins the protocol behavior
// that must be identical whether or not the edge enforces the contract: the
// status matrix, batching in a single request, content-type independence, the
// absence of redirects (the CLI sender does not follow them), and the absence of
// CORS headers on the declared routes.
func TestTelemetryProtocolBehaviorSurvivesEnforcement(t *testing.T) {
	spectest.Proves(t, "telemetry-putnami-dev/cli-usage-receiver", "undeclared-requests-are-rejected", "the-receiver-never-answers-with-a-redirect")
	emitter := &fakeEmitter{}
	workload := newServer(permissiveConfig(), emitter, nil).TestServer()
	defer workload.Close()
	edge := newEnforcedEdge(t, workload)
	otlp := map[string]string{"Content-Type": telemetry.ContentType}

	t.Run("status matrix is unchanged end to end", func(t *testing.T) {
		oversize := bytes.Repeat([]byte("a"), maxBodyBytes+4096)
		cases := []struct {
			name string
			body []byte
			want int
		}{
			{name: "accepted", body: cliusage.GoldenLogsJSON, want: http.StatusAccepted},
			{name: "malformed", body: []byte("not json{"), want: http.StatusBadRequest},
			{name: "oversize", body: oversize, want: http.StatusRequestEntityTooLarge},
		}
		for _, tc := range cases {
			resp, body := request(t, edge.url, http.MethodPost, telemetry.PathLogs, otlp, tc.body)
			if resp.StatusCode != tc.want {
				t.Errorf("%s payload = %d, want %d (body %s)", tc.name, resp.StatusCode, tc.want, body)
			}
			if resp.StatusCode >= 300 && resp.StatusCode < 400 {
				t.Errorf("%s payload answered %d; the CLI sender never follows redirects", tc.name, resp.StatusCode)
			}
			for header := range resp.Header {
				if strings.HasPrefix(http.CanonicalHeaderKey(header), "Access-Control-") {
					t.Errorf("%s payload answered with %s; no CORS policy is configured on this workload", tc.name, header)
				}
			}
		}
	})

	t.Run("a batch rides one declared request", func(t *testing.T) {
		const batch = 25
		before := len(emitter.captured())
		resp, body := request(t, edge.url, http.MethodPost, telemetry.PathLogs, otlp, batchedGoldenLogs(t, batch))
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("batched payload = %d, want 202 (body %s)", resp.StatusCode, body)
		}
		captured := emitter.captured()[before:]
		records := 0
		for _, rl := range captured {
			for _, sl := range rl.ScopeLogs {
				records += len(sl.LogRecords)
			}
		}
		if records != batch {
			t.Fatalf("emitted %d records from a %d-record batch", records, batch)
		}
	})

	t.Run("content type is not part of the contract", func(t *testing.T) {
		// Neither the receiver nor an exact-match edge rule inspects Content-Type,
		// so enforcement cannot change this. Recorded so the enforcement rollout does not
		// assume a content-type predicate is available to tighten.
		for _, contentType := range []string{"", "text/plain", "application/x-www-form-urlencoded"} {
			headers := map[string]string{}
			if contentType != "" {
				headers["Content-Type"] = contentType
			}
			resp, body := request(t, edge.url, http.MethodPost, telemetry.PathLogs, headers, cliusage.GoldenLogsJSON)
			if resp.StatusCode != http.StatusAccepted {
				t.Errorf("Content-Type %q = %d, want 202 (body %s)", contentType, resp.StatusCode, body)
			}
		}
	})
}

// batchedGoldenLogs repeats the golden CLI-usage record n times inside a single
// OTLP request, which is how the production sender drains a buffered run.
func batchedGoldenLogs(t *testing.T, n int) []byte {
	t.Helper()
	req, diags := telemetry.ParseLogsRequest(cliusage.GoldenLogsJSON)
	if req == nil || len(diags) != 0 {
		t.Fatalf("parse golden logs: %v", diags)
	}
	record := req.ResourceLogs[0].ScopeLogs[0].LogRecords[0]
	records := make([]telemetry.LogRecord, 0, n)
	for range n {
		records = append(records, record)
	}
	req.ResourceLogs[0].ScopeLogs[0].LogRecords = records

	body, err := telemetry.MarshalCanonical(*req)
	if err != nil {
		t.Fatalf("marshal batched logs: %v", err)
	}
	return body
}
