package telemetry

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// unsignedJWT builds a syntactically valid, unsigned JWT whose payload carries
// the given exp. The source never verifies signatures — expiry only drives
// cache busting — so a fake two-part token is enough.
func unsignedJWT(t *testing.T, exp time.Time) string {
	t.Helper()
	payload, err := json.Marshal(map[string]int64{"exp": exp.Unix()})
	if err != nil {
		t.Fatal(err)
	}
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + "."
}

// TestGCPIDTokenSource_FetchesAudienceBoundToken proves the source asks the
// metadata server for an ID token bound to the configured audience, with the
// mandatory Metadata-Flavor header.
func TestGCPIDTokenSource_FetchesAudienceBoundToken(t *testing.T) {
	token := unsignedJWT(t, time.Now().Add(time.Hour))
	var gotAudience, gotFlavor, gotFormat string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAudience = r.URL.Query().Get("audience")
		gotFormat = r.URL.Query().Get("format")
		gotFlavor = r.Header.Get("Metadata-Flavor")
		_, _ = w.Write([]byte(token))
	}))
	t.Cleanup(srv.Close)

	source := newGCPIDTokenSource(srv.Client(), srv.URL, "https://collector.example")
	got, err := source(context.Background())
	if err != nil {
		t.Fatalf("source: %v", err)
	}
	if got != token {
		t.Errorf("token = %q, want the metadata server's body", got)
	}
	if gotAudience != "https://collector.example" {
		t.Errorf("audience = %q, want the configured collector", gotAudience)
	}
	if gotFlavor != "Google" {
		t.Errorf("Metadata-Flavor = %q, want Google", gotFlavor)
	}
	if gotFormat != "full" {
		t.Errorf("format = %q, want full", gotFormat)
	}
}

// TestGCPIDTokenSource_CachesUntilNearExpiry proves a fresh token is served
// from cache and an expiring one is refreshed — the property that makes the
// source safe to call per request.
func TestGCPIDTokenSource_CachesUntilNearExpiry(t *testing.T) {
	var fetches atomic.Int32
	longLived := unsignedJWT(t, time.Now().Add(time.Hour))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fetches.Add(1)
		_, _ = w.Write([]byte(longLived))
	}))
	t.Cleanup(srv.Close)

	source := newGCPIDTokenSource(srv.Client(), srv.URL, "aud")
	for i := range 3 {
		if _, err := source(context.Background()); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if got := fetches.Load(); got != 1 {
		t.Errorf("fetches = %d, want 1 (fresh token must be cached)", got)
	}

	// A token already inside the refresh leeway is never served from cache.
	var expFetches atomic.Int32
	expiring := unsignedJWT(t, time.Now().Add(time.Minute))
	expSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		expFetches.Add(1)
		_, _ = w.Write([]byte(expiring))
	}))
	t.Cleanup(expSrv.Close)
	expSource := newGCPIDTokenSource(expSrv.Client(), expSrv.URL, "aud")
	for i := range 2 {
		if _, err := expSource(context.Background()); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if got := expFetches.Load(); got != 2 {
		t.Errorf("fetches = %d, want 2 (near-expiry token must refresh)", got)
	}
}

// TestGCPIDTokenSource_MetadataFailureErrors proves the off-GCP behavior: the
// metadata server is unreachable or refuses, the source errors, and the caller
// (the OTLP exporter) treats it as a collector failure.
func TestGCPIDTokenSource_MetadataFailureErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not on GCP", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	source := newGCPIDTokenSource(srv.Client(), srv.URL, "aud")
	if _, err := source(context.Background()); err == nil {
		t.Fatal("expected an error from a refusing metadata server")
	}

	empty := newGCPIDTokenSource(srv.Client(), srv.URL, "")
	if _, err := empty(context.Background()); err == nil {
		t.Fatal("expected an error for an empty audience")
	}
}

// TestOTLPClientBearerTokenSource proves the client resolves the bearer per
// request, that the source wins over the static BearerToken, and that a source
// failure fails the post (the exporter then drops the batch).
func TestOTLPClientBearerTokenSource(t *testing.T) {
	var calls atomic.Int32
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(srv.Close)

	client := newOTLPClient(OTLPConfig{
		Endpoint:    srv.URL,
		BearerToken: "static-must-lose",
		BearerTokenSource: func(context.Context) (string, error) {
			return "dynamic-" + strconv.Itoa(int(calls.Add(1))), nil
		},
	})
	for want := 1; want <= 2; want++ {
		if err := client.post(context.Background(), "/v1/logs", []byte("{}")); err != nil {
			t.Fatalf("post %d: %v", want, err)
		}
		if gotAuth != "Bearer dynamic-"+strconv.Itoa(want) {
			t.Errorf("Authorization = %q, want the per-request source token %d", gotAuth, want)
		}
	}

	failing := newOTLPClient(OTLPConfig{
		Endpoint:          srv.URL,
		BearerTokenSource: func(context.Context) (string, error) { return "", &collectorError{status: 401} },
	})
	if err := failing.post(context.Background(), "/v1/logs", []byte("{}")); err == nil {
		t.Fatal("expected a source failure to fail the post")
	}
}
