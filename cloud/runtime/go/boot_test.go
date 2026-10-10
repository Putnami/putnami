package runtime

import (
	"bytes"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testPreparedBootReference = "pcb_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func TestPreparedBootSourcePinsOriginPathTokenAndExactIntegers(t *testing.T) {
	var requestPath, authorization, requestReference string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestPath = r.URL.RequestURI()
		authorization = r.Header.Get("Authorization")
		var body struct {
			Reference string `json:"reference"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		requestReference = body.Reference
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"config":{"maximum":9223372036854775807,"minimum":-9223372036854775808,"nested":{"values":[9007199254740993]}}}`))
	}))
	defer server.Close()

	source := NewPreparedBootSource(PreparedBootSourceConfig{
		ServerURL: server.URL + "/caller/path?redirect=https://attacker.invalid",
		Audience:  "https://config.example.test", Reference: testPreparedBootReference,
		Token: "signed-workload-token", Timeout: time.Second, RetryBudget: -1,
	})
	tree, err := source.Load()
	if err != nil {
		t.Fatal(err)
	}
	if requestPath != preparedBootPath || authorization != "Bearer signed-workload-token" || requestReference != testPreparedBootReference {
		t.Fatalf("request path=%q authorization=%q reference=%q", requestPath, authorization, requestReference)
	}
	if tree["maximum"] != int64(math.MaxInt64) || tree["minimum"] != int64(math.MinInt64) {
		t.Fatalf("integer boundaries=%#v/%#v", tree["maximum"], tree["minimum"])
	}
	nested := tree["nested"].(map[string]any)["values"].([]any)
	if nested[0] != int64(9007199254740993) {
		t.Fatalf("nested exact integer=%#v (%T)", nested[0], nested[0])
	}
}

func TestPreparedBootSourceDoesNotRedirectCredentials(t *testing.T) {
	var destinationCalls atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		destinationCalls.Add(1)
	}))
	defer destination.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", destination.URL+"/steal")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	source := NewPreparedBootSource(PreparedBootSourceConfig{
		ServerURL: origin.URL, Audience: "https://config.example.test", Reference: testPreparedBootReference,
		Token: "signed-workload-token", Timeout: time.Second, RetryBudget: -1,
	})
	if tree, err := source.Load(); tree != nil || err == nil {
		t.Fatalf("tree=%#v err=%v", tree, err)
	}
	if destinationCalls.Load() != 0 {
		t.Fatalf("redirect destination received %d credential-bearing calls", destinationCalls.Load())
	}
}

func TestPreparedBootSourceBoundsAndHidesResponseFailures(t *testing.T) {
	secret := "provider-secret-must-not-leak"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(append(bytes.Repeat([]byte("x"), maxPreparedBootBody), []byte(secret)...))
	}))
	defer server.Close()
	source := NewPreparedBootSource(PreparedBootSourceConfig{
		ServerURL: server.URL, Audience: "https://config.example.test", Reference: testPreparedBootReference,
		Token: "signed-workload-token", Timeout: time.Second, RetryBudget: -1,
	})
	tree, err := source.Load()
	if tree != nil || err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("tree=%#v err=%v", tree, err)
	}
}

func TestPreparedBootSourceRetriesOnlyOutageClassFailure(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"config":{"ready":true}}`))
	}))
	defer server.Close()
	source := NewPreparedBootSource(PreparedBootSourceConfig{
		ServerURL: server.URL, Audience: "https://config.example.test", Reference: testPreparedBootReference,
		Token: "signed-workload-token", Timeout: time.Second, RetryBudget: time.Second,
	})
	tree, err := source.Load()
	if err != nil || tree["ready"] != true || calls.Load() != 2 {
		t.Fatalf("tree=%#v calls=%d err=%v", tree, calls.Load(), err)
	}
}

func TestPreparedBootDiscoveryIsExclusiveAndPinsMetadataAudience(t *testing.T) {
	isolateDiscoveryEnv(t, false)
	t.Setenv(PreparedBootBindingEnv, testPreparedBootReference)
	t.Setenv("CONFIG_SERVER_URL", "https://config.example.test/untrusted/path")
	t.Setenv("CONFIG_SERVER_AUDIENCE", "https://config-audience.example.test")
	t.Setenv("CONFIG_SERVER_TOKEN", "signed-workload-token")
	if DiscoverRemoteSource() != nil || DiscoverRemoteSecretsSource() != nil {
		t.Fatal("ordinary remote sources remained active with a prepared boot binding")
	}
	sources := collectViaRegister()
	if len(sources) != 1 || sources[0].Name() != "prepared-config-boot" {
		t.Fatalf("sources=%#v", sources)
	}

	isolateDiscoveryEnv(t, false)
	t.Setenv(PreparedBootBindingEnv, testPreparedBootReference)
	t.Setenv("CONFIG_SERVER_URL", "https://config.example.test")
	t.Setenv("CONFIG_SERVER_AUDIENCE", "https://fixed-audience.example.test")
	t.Setenv("K_SERVICE", "orders")
	source := DiscoverPreparedBootSource()
	metadata, ok := source.config.TokenSource.(*GcpMetadataTokenSource)
	if !ok || metadata.Audience != "https://fixed-audience.example.test" {
		t.Fatalf("token source=%T audience=%q", source.config.TokenSource, metadata.Audience)
	}
}

func TestPreparedBootSourceFailsClosedWhenFixedConfigurationIsMissing(t *testing.T) {
	for _, cfg := range []PreparedBootSourceConfig{
		{Audience: "https://config.example.test", Reference: testPreparedBootReference, Token: "token", RetryBudget: -1},
		{ServerURL: "https://config.example.test", Reference: testPreparedBootReference, Token: "token", RetryBudget: -1},
		{ServerURL: "https://config.example.test", Audience: "https://config.example.test", Reference: "opaque://caller", Token: "token", RetryBudget: -1},
		{ServerURL: "https://user:password@config.example.test", Audience: "https://config.example.test", Reference: testPreparedBootReference, Token: "token", RetryBudget: -1},
	} {
		if tree, err := NewPreparedBootSource(cfg).Load(); tree != nil || err == nil {
			t.Fatalf("cfg=%+v tree=%#v err=%v", cfg, tree, err)
		}
	}
}
