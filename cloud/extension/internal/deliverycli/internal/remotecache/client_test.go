package remotecache

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	cache "go.putnami.dev/protocol/cache"
)

var (
	keyBuild   = strings.Repeat("a", cache.KeyLength)
	keyCheap   = strings.Repeat("b", cache.KeyLength)
	keyPublish = strings.Repeat("c", cache.KeyLength)
)

func sampleInputs() []KeyInput {
	return []KeyInput{
		{Key: keyBuild, Extension: "@putnami/typescript", Task: "build~transpile", Project: "web", DurationMs: 60_000, SizeBytes: 5_000_000},
		{Key: keyCheap, Task: "build~transpile", Project: "web", DurationMs: 50, SizeBytes: 1000},        // below floor
		{Key: keyPublish, Task: "publish~npm", Project: "web", DurationMs: 60_000, SizeBytes: 5_000_000}, // side-effecting
	}
}

func TestBuildRequest_FiltersAndNormalizes(t *testing.T) {
	c := NewClient("https://cache.example", "tok", WithMode(cache.ModeMinimal))
	req := c.BuildRequest(sampleInputs())

	if req.ProtocolVersion != cache.ProtocolVersion {
		t.Errorf("ProtocolVersion = %d, want %d", req.ProtocolVersion, cache.ProtocolVersion)
	}
	if req.Mode != cache.ModeMinimal {
		t.Errorf("Mode = %q, want %q", req.Mode, cache.ModeMinimal)
	}
	if len(req.Keys) != 1 {
		t.Fatalf("expected 1 eligible key (cheap + publish filtered), got %d: %+v", len(req.Keys), req.Keys)
	}
	if req.Keys[0].Key != keyBuild {
		t.Errorf("eligible key = %q, want %q", req.Keys[0].Key, keyBuild)
	}
}

func TestNegotiate_HappyPath(t *testing.T) {
	var gotAuth, gotCT, gotMethod, gotPath, gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get(cache.AuthorizationHeader)
		gotCT = r.Header.Get("Content-Type")
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotUA = r.Header.Get("User-Agent")

		body, _ := io.ReadAll(r.Body)
		in, diags := cache.ParseAndValidateRequest(body)
		if in == nil {
			t.Errorf("server received invalid request: %v", diags)
		}

		resp := cache.NegotiateResponse{
			ProtocolVersion: cache.ProtocolVersion,
			Results: []cache.KeyResult{
				{Key: keyBuild, Hit: true, Result: &cache.ActionResult{Status: "success"}},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "secret-token", WithHTTPClient(srv.Client()))
	req := c.BuildRequest(sampleInputs())
	resp, err := c.Negotiate(context.Background(), req)
	if err != nil {
		t.Fatalf("Negotiate: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != cache.NegotiatePath {
		t.Errorf("path = %q, want %q", gotPath, cache.NegotiatePath)
	}
	if gotAuth != "Bearer secret-token" {
		t.Errorf("auth header = %q, want Bearer secret-token", gotAuth)
	}
	if gotCT != "application/json" {
		t.Errorf("content-type = %q, want application/json", gotCT)
	}
	// The cloud mirror of the UA helper is env-driven (PUTNAMI_CLI_USER_AGENT,
	// injected into extension jobs), unlike the framework's compiled-in version,
	// so assert against what the package itself stamps rather than a literal.
	if want := cliUserAgent(); gotUA != want {
		t.Errorf("User-Agent = %q, want %q", gotUA, want)
	}

	idx := Index(resp)
	r, ok := idx[keyBuild]
	if !ok || !r.Hit {
		t.Errorf("expected a hit for %q, got %+v", keyBuild, idx)
	}
}

func TestNegotiateWithProvenance_OptInAndLegacyFallback(t *testing.T) {
	var requested string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested = r.Header.Get(EntryProvenanceHeader)
		_ = json.NewEncoder(w).Encode(negotiateResponseWithProvenance{
			ProtocolVersion: cache.ProtocolVersion,
			Results: []keyResultWithProvenance{{
				KeyResult:        cache.KeyResult{Key: keyBuild, Hit: true, Result: &cache.ActionResult{Status: "success"}},
				Producer:         cache.ProducerCI,
				ProducerIdentity: "ci-runner@example.test",
				Channel:          cache.ChannelTrusted,
			}},
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok", WithHTTPClient(srv.Client()))
	resp, err := c.NegotiateWithProvenance(context.Background(), c.BuildRequest(sampleInputs()))
	if err != nil {
		t.Fatalf("NegotiateWithProvenance: %v", err)
	}
	if requested != entryProvenanceVersion {
		t.Errorf("%s = %q, want %q", EntryProvenanceHeader, requested, entryProvenanceVersion)
	}
	if hit := Index(resp.Response)[keyBuild]; !hit.Hit {
		t.Fatalf("shared response hit = %+v, want hit", hit)
	}
	metadata := resp.Provenance[keyBuild]
	if metadata.Producer != cache.ProducerCI || metadata.ProducerIdentity != "ci-runner@example.test" || metadata.Channel != cache.ChannelTrusted {
		t.Errorf("provenance = %+v, want trusted CI metadata", metadata)
	}
}

func TestNegotiateWithProvenance_OldServerStaysChannelLess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// An older cache server ignores the opt-in header and returns the shared
		// response shape. v2 providers must retain the channel-less fallback.
		_ = json.NewEncoder(w).Encode(cache.NegotiateResponse{
			ProtocolVersion: cache.ProtocolVersion,
			Results:         []cache.KeyResult{{Key: keyBuild, Hit: true, Result: &cache.ActionResult{Status: "success"}}},
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok", WithHTTPClient(srv.Client()))
	resp, err := c.NegotiateWithProvenance(context.Background(), c.BuildRequest(sampleInputs()))
	if err != nil {
		t.Fatalf("NegotiateWithProvenance: %v", err)
	}
	if len(resp.Provenance) != 0 {
		t.Errorf("legacy server provenance = %+v, want none", resp.Provenance)
	}
}

func TestNegotiate_EmptyKeysShortCircuits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("server must not be called when there are no eligible keys")
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok", WithHTTPClient(srv.Client()))
	// Only ineligible inputs → empty request.
	req := c.BuildRequest([]KeyInput{{Key: keyPublish, Task: "publish~npm", DurationMs: 9999}})
	resp, err := c.Negotiate(context.Background(), req)
	if err != nil {
		t.Fatalf("Negotiate: %v", err)
	}
	if len(resp.Results) != 0 {
		t.Errorf("expected empty results, got %d", len(resp.Results))
	}
}

func TestNegotiate_ServerErrorBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(cache.ErrorResponse{
			ProtocolVersion: cache.ProtocolVersion,
			Code:            cache.CodeUnauthorized,
			Message:         "token expired",
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok", WithHTTPClient(srv.Client()))
	req := c.BuildRequest(sampleInputs())
	_, err := c.Negotiate(context.Background(), req)
	if err == nil {
		t.Fatal("expected an error for 401 response")
	}
	if !strings.Contains(err.Error(), cache.CodeUnauthorized) || !strings.Contains(err.Error(), "token expired") {
		t.Errorf("error should carry the server code/message, got: %v", err)
	}
}

func TestNegotiate_InvalidResponseRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Hit without a result is contract-invalid.
		resp := cache.NegotiateResponse{
			ProtocolVersion: cache.ProtocolVersion,
			Results:         []cache.KeyResult{{Key: keyBuild, Hit: true}},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok", WithHTTPClient(srv.Client()))
	req := c.BuildRequest(sampleInputs())
	if _, err := c.Negotiate(context.Background(), req); err == nil {
		t.Fatal("expected validation error for a hit without a result")
	}
}

func TestRunMarkerLookup_HappyPath(t *testing.T) {
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get(cache.AuthorizationHeader)
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		req, diags := cache.ParseAndValidateRunMarkerRequest(body)
		if req == nil {
			t.Fatalf("server received invalid marker request: %v", diags)
		}
		if req.Branch != "main" || len(req.Commands) != 1 || req.Commands[0] != "build" {
			t.Fatalf("request = %+v, want main/build", req)
		}
		json.NewEncoder(w).Encode(cache.RunMarkerResponse{
			ProtocolVersion: cache.ProtocolVersion,
			Marker:          &cache.RunMarker{SHA: strings.Repeat("a", 40), UpdatedAt: "2026-06-18T00:00:00Z"},
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "secret-token", WithHTTPClient(srv.Client()))
	req := c.BuildRunMarkerRequest("repo:abc", "main", []string{"build"}, "")
	resp, err := c.LookupRunMarker(context.Background(), req)
	if err != nil {
		t.Fatalf("LookupRunMarker: %v", err)
	}
	if gotPath != cache.RunMarkerLookupPath {
		t.Errorf("path = %q, want %q", gotPath, cache.RunMarkerLookupPath)
	}
	if gotAuth != "Bearer secret-token" {
		t.Errorf("auth header = %q, want Bearer secret-token", gotAuth)
	}
	if resp.Marker == nil || resp.Marker.SHA != strings.Repeat("a", 40) {
		t.Fatalf("marker = %+v, want sha", resp.Marker)
	}
}

func TestRunMarkerLookup_NotFoundIsMiss(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok", WithHTTPClient(srv.Client()))
	resp, err := c.LookupRunMarker(context.Background(), c.BuildRunMarkerRequest("repo:abc", "main", []string{"build"}, ""))
	if err != nil {
		t.Fatalf("LookupRunMarker: %v", err)
	}
	if resp.Marker != nil {
		t.Fatalf("marker = %+v, want nil", resp.Marker)
	}
}

func TestPublishRunMarker_HappyPath(t *testing.T) {
	var gotPath string
	var gotObserved string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		req, diags := cache.ParseAndValidatePublishRunMarkerRequest(body)
		if req == nil {
			t.Fatalf("server received invalid publish request: %v", diags)
		}
		gotObserved = req.ObservedSHA
		json.NewEncoder(w).Encode(cache.PublishRunMarkerResponse{
			ProtocolVersion: cache.ProtocolVersion,
			Published:       true,
			Marker:          &cache.RunMarker{SHA: req.SHA},
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok", WithHTTPClient(srv.Client()))
	req := c.BuildPublishRunMarkerRequest("repo:abc", "main", []string{"build"}, "", strings.Repeat("b", 40), strings.Repeat("a", 40))
	resp, err := c.PublishRunMarker(context.Background(), req)
	if err != nil {
		t.Fatalf("PublishRunMarker: %v", err)
	}
	if gotPath != cache.RunMarkerPublishPath {
		t.Errorf("path = %q, want %q", gotPath, cache.RunMarkerPublishPath)
	}
	if gotObserved != strings.Repeat("a", 40) {
		t.Errorf("observedSha = %q, want %q", gotObserved, strings.Repeat("a", 40))
	}
	if !resp.Published {
		t.Fatal("Published = false, want true")
	}
}

func TestNewClient_PoolsConnections(t *testing.T) {
	c := NewClient("https://cache.example", "tok")

	tr, ok := c.httpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("default client Transport = %T, want *http.Transport", c.httpClient.Transport)
	}
	// The stdlib default of 2 idle conns per host would serialize the build's
	// concurrent blob uploads/downloads behind constant TLS handshakes.
	if tr.MaxIdleConnsPerHost != maxIdleConnsPerHost {
		t.Errorf("MaxIdleConnsPerHost = %d, want %d", tr.MaxIdleConnsPerHost, maxIdleConnsPerHost)
	}
	if tr.MaxIdleConns != maxIdleConns {
		t.Errorf("MaxIdleConns = %d, want %d", tr.MaxIdleConns, maxIdleConns)
	}
	if !tr.ForceAttemptHTTP2 {
		t.Error("ForceAttemptHTTP2 = false, want true")
	}
	// No blanket client timeout: it would also cap large blob upload/download
	// streams. The control-plane timeout is applied per attempt in doAuthed.
	if c.httpClient.Timeout != 0 {
		t.Errorf("Timeout = %s, want 0 (blob transfers must not be capped by a control-plane timeout)", c.httpClient.Timeout)
	}
}

func TestWithHTTPClient_OverridesDefaultTransport(t *testing.T) {
	custom := &http.Client{}
	c := NewClient("https://cache.example", "tok", WithHTTPClient(custom))
	if c.httpClient != custom {
		t.Fatal("WithHTTPClient did not override the default pooled client")
	}
}
