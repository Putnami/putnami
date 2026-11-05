package linear

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.putnami.dev/sdk/extension/collabhelper"
)

const testKey = "lin_api_testKEYtestKEYtestKEYtestKEY0000"

func noPause(context.Context, int) error { return nil }

// newTestClient is a client of a test server, authenticated by key.
func newTestClient(t *testing.T, key string, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	endpoint, err := ResolveEndpoint(func(name string) string {
		if name == OverrideVariable {
			return server.URL
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	credential := NewCredential(func(name string) string {
		if name == KeyVariable {
			return key
		}
		return ""
	})
	return New(endpoint, credential, Options{HTTP: &http.Client{Transport: &http.Transport{DisableKeepAlives: true}},
		Timeout: 300 * time.Millisecond, Pause: noPause})
}

func TestAQueryCarriesTheKeyAndItsOperationName(t *testing.T) {
	client := newTestClient(t, testKey, func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			OperationName string         `json:"operationName"`
			Variables     map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		if r.Header.Get("Authorization") != testKey || r.Method != http.MethodPost || request.OperationName != "Probe" || request.Variables["x"] != "y" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = io.WriteString(w, `{"data":{"viewer":{"id":"me"}}}`)
	})
	var data struct {
		Viewer struct {
			ID string `json:"id"`
		} `json:"viewer"`
	}
	if err := client.Query(context.Background(), "Probe", "query Probe { viewer { id } }", map[string]any{"x": "y"}, &data); err != nil || data.Viewer.ID != "me" {
		t.Fatalf("query: %+v %+v", err, data)
	}
}

func TestAnOAuthTokenIsABearerToken(t *testing.T) {
	if authorization("lin_oauth_abc") != "Bearer lin_oauth_abc" || authorization(testKey) != testKey {
		t.Fatal("the Authorization header does not follow the key's kind")
	}
}

func TestAQueryWithoutADefiniteAnswerIsRepeatedAndAMutationIsNot(t *testing.T) {
	var calls atomic.Int32
	client := newTestClient(t, testKey, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = io.WriteString(w, `{"data":{"ok":true}}`)
	})
	if err := client.Query(context.Background(), "Q", "query Q { ok }", nil, nil); err != nil || calls.Load() != 3 {
		t.Fatalf("a query after two 502s: %+v after %d calls", err, calls.Load())
	}
	calls.Store(0)
	err := client.Mutate(context.Background(), "M", "mutation M { ok }", nil, nil)
	if err == nil || err.Kind != Uncertain || calls.Load() != 1 {
		t.Fatalf("a mutation answered 502: %+v after %d calls", err, calls.Load())
	}
}

func TestAServerSideErrorIsUncertainWhateverItsBody(t *testing.T) {
	answers := []struct {
		status int
		body   string
	}{
		{502, `{"errors":[{"message":"upstream","extensions":{"code":"INTERNAL_ERROR"}}]}`},
		{503, `{"errors":[{"message":"Invalid input","extensions":{"code":"INVALID_INPUT"}}]}`},
		{200, `{"errors":[{"message":"lock timeout","extensions":{"code":"LOCK_TIMEOUT"}}]}`},
	}
	for _, answer := range answers {
		var calls atomic.Int32
		client := newTestClient(t, testKey, func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			w.WriteHeader(answer.status)
			_, _ = io.WriteString(w, answer.body)
		})
		err := client.Mutate(context.Background(), "M", "mutation M { ok }", nil, nil)
		if err == nil || err.Kind != Uncertain || calls.Load() != 1 {
			t.Errorf("a mutation answered %d %s: %+v after %d calls", answer.status, answer.body, err, calls.Load())
		}
	}
}

func TestARefusalIsDefiniteAndRedacted(t *testing.T) {
	var calls atomic.Int32
	client := newTestClient(t, testKey, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"errors":[{"message":"bad key `+testKey+`","extensions":{"code":"AUTHENTICATION_ERROR"}}]}`)
	})
	err := client.Query(context.Background(), "Q", "query Q { ok }", nil, nil)
	if err == nil || err.Kind != Answered || err.Code != "AUTHENTICATION_ERROR" || calls.Load() != 1 {
		t.Fatalf("a refusal: %+v after %d calls", err, calls.Load())
	}
	if strings.Contains(err.Message, testKey) || !strings.Contains(err.Message, collabhelper.RedactedMarker) {
		t.Fatalf("the message carries the key: %q", err.Message)
	}
}

func TestARateLimitAndAMissingEntityAreNamed(t *testing.T) {
	answers := []string{
		`{"errors":[{"message":"Rate limit exceeded","extensions":{"code":"RATELIMITED"}}]}`,
		`{"errors":[{"message":"Entity not found: Issue","extensions":{"code":"INPUT_ERROR"}}]}`,
	}
	var calls atomic.Int32
	client := newTestClient(t, testKey, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, answers[calls.Add(1)-1])
	})
	if err := client.Query(context.Background(), "Q", "query Q { ok }", nil, nil); err == nil || !err.RateLimited {
		t.Fatalf("a rate limit: %+v", err)
	}
	if err := client.Query(context.Background(), "Q", "query Q { ok }", nil, nil); err == nil || !err.NotFound {
		t.Fatalf("a missing entity: %+v", err)
	}
}

func TestNoKeySendsNothing(t *testing.T) {
	var calls atomic.Int32
	client := newTestClient(t, "", func(http.ResponseWriter, *http.Request) { calls.Add(1) })
	err := client.Query(context.Background(), "Q", "query Q { ok }", nil, nil)
	if err == nil || err.Kind != NoCredential || calls.Load() != 0 || !strings.Contains(err.Message, KeyVariable) {
		t.Fatalf("without a key: %+v after %d calls", err, calls.Load())
	}
}

func TestRedactRemovesKeyShapedStrings(t *testing.T) {
	text := Redact("mine secret-value, theirs lin_api_0123456789abcdefABCDEF and lin_oauth_0123456789abcdefABCDEF", []string{"secret-value"})
	if strings.Contains(text, "secret-value") || strings.Contains(text, "lin_api_") || strings.Contains(text, "lin_oauth_") {
		t.Fatalf("redacted %q", text)
	}
}
