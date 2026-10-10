package remotecache

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	cacheserverclient "go.putnami.dev/cloud/clients/cache-server/go"
	cache "go.putnami.dev/protocol/cache"
)

// These tests pin what the generated cache-server client changes on the wire
// and what controlTransport keeps as it was, so a released server and a
// third-party protocol/cache server keep working.

// seenRequest is one request a recordingServer received.
type seenRequest struct {
	method string
	path   string
	header http.Header
	body   []byte
}

// recordingServer records every request and answers it with answer.
type recordingServer struct {
	mu   sync.Mutex
	seen []seenRequest
}

func startRecordingServer(t *testing.T, answer http.HandlerFunc) (*recordingServer, *httptest.Server) {
	t.Helper()
	rec := &recordingServer{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec.mu.Lock()
		rec.seen = append(rec.seen, seenRequest{method: r.Method, path: r.URL.Path, header: r.Header.Clone(), body: body})
		rec.mu.Unlock()
		answer(w, r)
	}))
	t.Cleanup(srv.Close)
	return rec, srv
}

func (r *recordingServer) requests() []seenRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]seenRequest(nil), r.seen...)
}

// answerJSON writes body with status and an application/json media type.
func answerJSON(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

func emptyNegotiateAnswer() string {
	return fmt.Sprintf(`{"protocolVersion":%d,"results":[]}`, cache.ProtocolVersion)
}

func TestControlCalls_SendTheProtocolRequest(t *testing.T) {
	rec, srv := startRecordingServer(t, answerJSON(http.StatusOK, emptyNegotiateAnswer()))

	c := NewClient(srv.URL, "secret-token", WithHTTPClient(srv.Client()))
	req := c.BuildRequest(sampleInputs())
	if _, err := c.Negotiate(context.Background(), req); err != nil {
		t.Fatalf("Negotiate: %v", err)
	}

	seen := rec.requests()
	if len(seen) != 1 {
		t.Fatalf("server saw %d requests, want 1", len(seen))
	}
	got := seen[0]
	if got.method != http.MethodPost || got.path != cache.NegotiatePath {
		t.Errorf("request = %s %s, want POST %s", got.method, got.path, cache.NegotiatePath)
	}
	for name, want := range map[string]string{
		cache.AuthorizationHeader: "Bearer secret-token",
		"Content-Type":            "application/json",
		"Accept":                  "application/json",
		"User-Agent":              cliUserAgent(),
		"X-Client-Id":             controlClientID,
	} {
		if value := got.header.Get(name); value != want {
			t.Errorf("%s = %q, want %q", name, value, want)
		}
	}
	if values := got.header.Values(EntryProvenanceHeader); len(values) != 0 {
		t.Errorf("Negotiate sent %s = %q; only NegotiateWithProvenance opts in", EntryProvenanceHeader, values)
	}

	// The generated input orders and omits members its own way; a strict-v1
	// server must still read the same request.
	sent, diags := cache.ParseAndValidateRequest(got.body)
	if sent == nil {
		t.Fatalf("server could not read the request %s: %v", got.body, diags)
	}
	if !reflect.DeepEqual(sent, req) {
		t.Errorf("server read %+v, want %+v", sent, req)
	}
}

func TestControlCalls_SendTheProvenanceHeaderOnlyWhenAsked(t *testing.T) {
	rec, srv := startRecordingServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == cache.CapabilitiesPath {
			answerJSON(http.StatusOK, fmt.Sprintf(`{"protocolVersion":%d}`, cache.ProtocolVersion))(w, r)
			return
		}
		answerJSON(http.StatusOK, emptyNegotiateAnswer())(w, r)
	})
	c := NewClient(srv.URL, "tok", WithHTTPClient(srv.Client()))
	ctx := context.Background()
	req := c.BuildRequest(sampleInputs())

	if _, err := c.Negotiate(ctx, req); err != nil {
		t.Fatalf("Negotiate: %v", err)
	}
	if _, err := c.NegotiateWithProvenance(ctx, req); err != nil {
		t.Fatalf("NegotiateWithProvenance: %v", err)
	}
	if _, err := c.Capabilities(ctx); err != nil {
		t.Fatalf("Capabilities: %v", err)
	}

	requests := rec.requests()
	got := make([]string, 0, len(requests))
	for _, seen := range requests {
		got = append(got, seen.path+"="+seen.header.Get(EntryProvenanceHeader))
	}
	want := []string{
		cache.NegotiatePath + "=",
		cache.NegotiatePath + "=" + entryProvenanceVersion,
		cache.CapabilitiesPath + "=",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("provenance header per request = %q, want %q", got, want)
	}
}

func TestControlCalls_ReadAThirdPartySuccessAnswer(t *testing.T) {
	hit := fmt.Sprintf(`{"protocolVersion":%d,"results":[{"key":%q,"hit":true,"result":{"status":"success","events":null},"manifest":{"files":null},"downloads":null}]}`,
		cache.ProtocolVersion, keyBuild)
	cases := []struct {
		name        string
		status      int
		contentType []string
		body        string
		wantHit     bool
	}{
		{name: "201 created", status: http.StatusCreated, contentType: []string{"application/json"}, body: emptyNegotiateAnswer()},
		{name: "no content type", status: http.StatusOK, contentType: nil, body: emptyNegotiateAnswer()},
		{name: "text plain", status: http.StatusOK, contentType: []string{"text/plain"}, body: emptyNegotiateAnswer()},
		{name: "json with charset", status: http.StatusOK, contentType: []string{"application/json; charset=utf-8"}, body: emptyNegotiateAnswer()},
		{name: "null results", status: http.StatusOK, contentType: []string{"application/json"}, body: fmt.Sprintf(`{"protocolVersion":%d,"results":null}`, cache.ProtocolVersion)},
		{name: "null members of a hit", status: http.StatusOK, contentType: []string{"application/json"}, body: hit, wantHit: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, srv := startRecordingServer(t, func(w http.ResponseWriter, _ *http.Request) {
				// A nil slice keeps net/http from sniffing a media type.
				w.Header()["Content-Type"] = tc.contentType
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			})
			c := NewClient(srv.URL, "tok", WithHTTPClient(srv.Client()))
			resp, err := c.Negotiate(context.Background(), c.BuildRequest(sampleInputs()))
			if err != nil {
				t.Fatalf("Negotiate: %v", err)
			}
			result, ok := Index(resp)[keyBuild]
			if ok != tc.wantHit || result.Hit != tc.wantHit {
				t.Fatalf("hit = %v (present %v), want %v", result.Hit, ok, tc.wantHit)
			}
			if tc.wantHit && (result.Manifest == nil || len(result.Manifest.Files) != 0) {
				t.Errorf("manifest = %+v, want an empty file set", result.Manifest)
			}
		})
	}
}

func TestControlCalls_KeepTheRefusal(t *testing.T) {
	cases := []struct {
		name string
		srv  http.HandlerFunc
		want StatusError
	}{
		{
			name: "structured 400",
			srv:  answerJSON(http.StatusBadRequest, fmt.Sprintf(`{"protocolVersion":%d,"code":"invalid-request","message":"keys must be unique"}`, cache.ProtocolVersion)),
			want: StatusError{Op: "negotiate", Status: http.StatusBadRequest, Code: "invalid-request", Message: "keys must be unique"},
		},
		{
			name: "plain 500",
			srv: func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "boom", http.StatusInternalServerError)
			},
			want: StatusError{Op: "negotiate", Status: http.StatusInternalServerError},
		},
		{
			name: "empty 404",
			srv:  func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) },
			want: StatusError{Op: "negotiate", Status: http.StatusNotFound},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, srv := startRecordingServer(t, tc.srv)
			c := NewClient(srv.URL, "tok", WithHTTPClient(srv.Client()))
			_, err := c.Negotiate(context.Background(), c.BuildRequest(sampleInputs()))
			var se *StatusError
			if !errors.As(err, &se) {
				t.Fatalf("err = %v (%T), want a *StatusError", err, err)
			}
			if *se != tc.want {
				t.Errorf("StatusError = %+v, want %+v", *se, tc.want)
			}
		})
	}
}

// The generated client opens a breaker per operation after five consecutive
// 5xx answers and fails fast without a request. This pins that behavior so a
// change to it is a visible decision.
func TestControlCalls_OpenTheBreakerAfterConsecutiveServerFailures(t *testing.T) {
	rec, srv := startRecordingServer(t, answerJSON(http.StatusServiceUnavailable, `{}`))
	c := NewClient(srv.URL, "tok", WithHTTPClient(srv.Client()))
	req := c.BuildRequest(sampleInputs())

	for i := range 5 {
		if _, err := c.Negotiate(context.Background(), req); !IsStatus(err, http.StatusServiceUnavailable) {
			t.Fatalf("call %d: err = %v, want HTTP 503", i+1, err)
		}
	}
	_, err := c.Negotiate(context.Background(), req)
	if err == nil || IsStatus(err, http.StatusServiceUnavailable) {
		t.Fatalf("sixth call: err = %v, want the open-breaker error", err)
	}
	if got := len(rec.requests()); got != 5 {
		t.Errorf("server saw %d requests, want 5", got)
	}
	// The breaker belongs to the operation: another operation still reaches
	// the server.
	if _, err := c.Capabilities(context.Background()); err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if got := len(rec.requests()); got != 6 {
		t.Errorf("server saw %d requests, want 6", got)
	}
}

func TestControlCalls_SendNoCredentialForAnEmptyBearer(t *testing.T) {
	t.Setenv(TokenEnv, "")
	rec, srv := startRecordingServer(t, answerJSON(http.StatusOK, emptyNegotiateAnswer()))

	c := NewClient(srv.URL, "", WithHTTPClient(srv.Client()))
	if _, err := c.Negotiate(context.Background(), c.BuildRequest(sampleInputs())); err != nil {
		t.Fatalf("Negotiate: %v", err)
	}
	seen := rec.requests()
	if len(seen) != 1 {
		t.Fatalf("server saw %d requests, want 1", len(seen))
	}
	if values := seen[0].header.Values(cache.AuthorizationHeader); len(values) != 0 {
		t.Errorf("Authorization = %q, want none", values)
	}
	for name, values := range seen[0].header {
		for _, value := range values {
			if strings.Contains(value, anonymousBearer) {
				t.Errorf("header %s leaks the placeholder bearer", name)
			}
		}
	}
}

func TestControlCalls_SendTheURLUserInfoOnlyWithoutABearer(t *testing.T) {
	t.Setenv(TokenEnv, "")
	rec, srv := startRecordingServer(t, answerJSON(http.StatusOK, emptyNegotiateAnswer()))
	withUser, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	withUser.User = url.UserPassword("cache-user", "cache-pass")

	anonymous := NewClient(withUser.String(), "", WithHTTPClient(srv.Client()))
	if _, err := anonymous.Negotiate(context.Background(), anonymous.BuildRequest(sampleInputs())); err != nil {
		t.Fatalf("anonymous Negotiate: %v", err)
	}
	bearer := NewClient(withUser.String(), "tok", WithHTTPClient(srv.Client()))
	if _, err := bearer.Negotiate(context.Background(), bearer.BuildRequest(sampleInputs())); err != nil {
		t.Fatalf("bearer Negotiate: %v", err)
	}

	seen := rec.requests()
	if len(seen) != 2 {
		t.Fatalf("server saw %d requests, want 2", len(seen))
	}
	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte("cache-user:cache-pass"))
	if got := seen[0].header.Get(cache.AuthorizationHeader); got != basic {
		t.Errorf("anonymous Authorization = %q, want %q", got, basic)
	}
	if got := seen[1].header.Get(cache.AuthorizationHeader); got != "Bearer tok" {
		t.Errorf("bearer Authorization = %q, want Bearer tok", got)
	}
}

func TestControlCalls_KeepTheTransportCause(t *testing.T) {
	t.Run("refused connection", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		base := srv.URL
		srv.Close()

		c := NewClient(base, "tok")
		_, err := c.Negotiate(context.Background(), c.BuildRequest(sampleInputs()))
		var urlErr *url.Error
		if !errors.As(err, &urlErr) {
			t.Fatalf("err = %v (%T), want a *url.Error", err, err)
		}
		if urlErr.Op != "Post" || urlErr.URL != base+cache.NegotiatePath {
			t.Errorf("url.Error = %s %s, want Post %s", urlErr.Op, urlErr.URL, base+cache.NegotiatePath)
		}
		var opErr *net.OpError
		if !errors.As(err, &opErr) {
			t.Errorf("err = %v; the dial failure must stay readable", err)
		}
		if !strings.HasPrefix(err.Error(), "negotiate: ") {
			t.Errorf("err = %q, want it labeled with the operation", err)
		}
	})
	t.Run("deadline", func(t *testing.T) {
		_, srv := startRecordingServer(t, func(_ http.ResponseWriter, r *http.Request) {
			<-r.Context().Done()
		})
		c := NewClient(srv.URL, "tok", WithHTTPClient(srv.Client()))
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		_, err := c.Negotiate(ctx, c.BuildRequest(sampleInputs()))
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want context.DeadlineExceeded", err)
		}
	})
}

// spaceReader yields JSON whitespace forever.
type spaceReader struct{}

func (spaceReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = ' '
	}
	return len(p), nil
}

func answerPastTheCap(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.Copy(w, io.LimitReader(spaceReader{}, maxResponseBytes+1))
}

func TestControlCalls_RefuseAnAnswerPastTheCap(t *testing.T) {
	_, srv := startRecordingServer(t, answerPastTheCap)
	c := NewClient(srv.URL, "tok", WithHTTPClient(srv.Client()))
	_, err := c.Negotiate(context.Background(), c.BuildRequest(sampleInputs()))
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("response exceeds %d bytes", maxResponseBytes)) {
		t.Fatalf("err = %v, want the response cap error", err)
	}
}

func TestPostControl_RefusesAMemberTheContractDoesNotDeclare(t *testing.T) {
	rec, srv := startRecordingServer(t, answerJSON(http.StatusOK, emptyNegotiateAnswer()))
	c := NewClient(srv.URL, "tok", WithHTTPClient(srv.Client()))
	req := struct {
		ProtocolVersion int    `json:"protocolVersion"`
		Surprise        string `json:"surprise"`
	}{ProtocolVersion: cache.ProtocolVersion, Surprise: "dropped"}

	_, err := postControl(context.Background(), c, "negotiate", req, (*cacheserverclient.CacheClient).CreateV1CacheNegotiate)
	if err == nil || !strings.Contains(err.Error(), "does not match the cache server contract") {
		t.Fatalf("err = %v, want the contract mismatch", err)
	}
	if got := len(rec.requests()); got != 0 {
		t.Errorf("server saw %d requests, want 0", got)
	}
}

func TestControlCalls_ReportABindingFailure(t *testing.T) {
	c := NewClient("https://cache.example/base?query=1", "tok")
	_, err := c.Negotiate(context.Background(), c.BuildRequest(sampleInputs()))
	if err == nil || !strings.Contains(err.Error(), "bind cache server client") {
		t.Fatalf("err = %v, want the binding failure", err)
	}
}

func TestWithoutNullMembers(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "no null", in: `{"b":1,"a":2}`, want: `{"b":1,"a":2}`},
		{name: "nested members", in: `{"a":null,"b":{"c":null,"d":[null,{"e":null,"f":1}]}}`, want: `{"b":{"d":[null,{"f":1}]}}`},
		{name: "null inside a string", in: `{"name":"null"}`, want: `{"name":"null"}`},
		{name: "html and big numbers", in: `{"html":"<a>&</a>","n":12345678901234567890123,"x":null}`, want: `{"html":"<a>&</a>","n":12345678901234567890123}`},
		{name: "top-level null", in: `null`, want: `null`},
		{name: "not json", in: `not json null`, want: `not json null`},
		{name: "two values", in: `{"a":null} {"b":1}`, want: `{"a":null} {"b":1}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(withoutNullMembers([]byte(tc.in))); got != tc.want {
				t.Errorf("withoutNullMembers(%s) = %s, want %s", tc.in, got, tc.want)
			}
		})
	}
}

func TestAcceptedSuccess_LabelsAnUnlabeledAnswer(t *testing.T) {
	resp := &http.Response{StatusCode: http.StatusAccepted}
	body := acceptedSuccess(resp, []byte(`{"a":null}`))
	if resp.StatusCode != http.StatusOK || resp.Status != "200 OK" {
		t.Errorf("status = %d %q, want 200", resp.StatusCode, resp.Status)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if string(body) != `{}` {
		t.Errorf("body = %s, want {}", body)
	}
}

func TestURLErrorOp(t *testing.T) {
	for method, want := range map[string]string{"": "Get", http.MethodGet: "Get", http.MethodPost: "Post"} {
		if got := urlErrorOp(method); got != want {
			t.Errorf("urlErrorOp(%q) = %q, want %q", method, got, want)
		}
	}
}

func TestSplitUserinfo(t *testing.T) {
	bound, user := splitUserinfo("https://u:p@cache.example/base")
	if bound != "https://cache.example/base" || user == nil || user.Username() != "u" {
		t.Errorf("splitUserinfo = %q, %v", bound, user)
	}
	for _, raw := range []string{"https://cache.example", "://bad"} {
		if bound, user := splitUserinfo(raw); bound != raw || user != nil {
			t.Errorf("splitUserinfo(%q) = %q, %v; want it unchanged", raw, bound, user)
		}
	}
}

// --- protocol routes cache-server does not serve ---

func validUploadGrant(r *http.Request) string {
	return fmt.Sprintf(`{"protocolVersion":%d,"urlTemplate":"http://%s/dput/%s","method":"PUT","conditionalExistsStatus":412}`,
		cache.ProtocolVersion, r.Host, cache.DigestPlaceholder)
}

func TestProtocolRoutes_ReMintOnceOnA401(t *testing.T) {
	t.Setenv(TokenEnv, "")
	source := &mintingSource{}
	rec, srv := startRecordingServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(cache.AuthorizationHeader) != "Bearer minted-2" {
			answerJSON(http.StatusUnauthorized, fmt.Sprintf(`{"protocolVersion":%d,"code":"unauthorized","message":"expired"}`, cache.ProtocolVersion))(w, r)
			return
		}
		answerJSON(http.StatusOK, validUploadGrant(r))(w, r)
	})
	auth := &authRecorder{}
	c := NewClient(srv.URL, "", WithHTTPClient(srv.Client()), WithBearerFunc(source.resolve), WithAuthObserver(auth.observe))

	grant, err := c.UploadGrant(context.Background())
	if err != nil {
		t.Fatalf("UploadGrant: %v", err)
	}
	if grant.Method != cache.TransferPut {
		t.Errorf("grant = %+v", grant)
	}
	seen := rec.requests()
	if len(seen) != 2 || seen[0].method != http.MethodGet || seen[0].path != cache.UploadGrantPath {
		t.Fatalf("requests = %+v, want two GET %s", seen, cache.UploadGrantPath)
	}
	if got := seen[1].header.Get("Accept"); got != "application/json" {
		t.Errorf("Accept = %q", got)
	}
	if got := seen[1].header.Get("Content-Type"); got != "" {
		t.Errorf("a GET sent Content-Type %q", got)
	}
	failures := auth.all()
	if len(failures) != 1 || failures[0].Op != "upload-grant" || !failures[0].Refreshed || failures[0].Code != "unauthorized" {
		t.Errorf("refusals = %+v, want one refreshed upload-grant refusal", failures)
	}
}

func TestProtocolRoutes_MapARefusalToAStatusError(t *testing.T) {
	_, srv := startRecordingServer(t, answerJSON(http.StatusServiceUnavailable, fmt.Sprintf(`{"protocolVersion":%d,"code":"unavailable","message":"later"}`, cache.ProtocolVersion)))
	c := NewClient(srv.URL, "tok", WithHTTPClient(srv.Client()))

	_, err := c.UploadGrant(context.Background())
	var se *StatusError
	if !errors.As(err, &se) || se.Status != http.StatusServiceUnavailable || se.Code != "unavailable" {
		t.Errorf("UploadGrant err = %v, want HTTP 503 unavailable", err)
	}

	content := []byte("small blob")
	_, err = c.UploadBatch(context.Background(), &cache.UploadBatchRequest{
		ProtocolVersion: cache.ProtocolVersion,
		Blobs:           []cache.InlineBlob{{Digest: cache.DigestOf(content), Data: content}},
	})
	if !IsStatus(err, http.StatusServiceUnavailable) {
		t.Errorf("UploadBatch err = %v, want HTTP 503", err)
	}
}

func TestProtocolRoutes_SendTheBatchWithoutABearerWhenNoneIsSet(t *testing.T) {
	t.Setenv(TokenEnv, "")
	rec, srv := startRecordingServer(t, answerJSON(http.StatusOK, fmt.Sprintf(`{"protocolVersion":%d,"stored":[]}`, cache.ProtocolVersion)))
	c := NewClient(srv.URL, "", WithHTTPClient(srv.Client()))

	content := []byte("small blob")
	if _, err := c.UploadBatch(context.Background(), &cache.UploadBatchRequest{
		ProtocolVersion: cache.ProtocolVersion,
		Blobs:           []cache.InlineBlob{{Digest: cache.DigestOf(content), Data: content}},
	}); err != nil {
		t.Fatalf("UploadBatch: %v", err)
	}
	seen := rec.requests()
	if len(seen) != 1 || seen[0].method != http.MethodPost || seen[0].path != cache.UploadBatchPath {
		t.Fatalf("requests = %+v, want one POST %s", seen, cache.UploadBatchPath)
	}
	if got := seen[0].header.Get(cache.AuthorizationHeader); got != "" {
		t.Errorf("Authorization = %q, want none", got)
	}
	if got := seen[0].header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
}

func TestProtocolRoutes_RefuseAnAnswerPastTheCap(t *testing.T) {
	_, srv := startRecordingServer(t, answerPastTheCap)
	c := NewClient(srv.URL, "tok", WithHTTPClient(srv.Client()))
	_, err := c.UploadGrant(context.Background())
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("response exceeds %d bytes", maxResponseBytes)) {
		t.Fatalf("err = %v, want the response cap error", err)
	}
}

func TestProtocolRoutes_ReportTransportFailures(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	base := srv.URL
	srv.Close()

	c := NewClient(base, "tok")
	if _, err := c.UploadGrant(context.Background()); err == nil || !strings.HasPrefix(err.Error(), "upload-grant: ") {
		t.Errorf("err = %v, want an upload-grant transport error", err)
	}

	bad := NewClient("http://cache.example\x7f", "tok")
	if _, err := bad.UploadGrant(context.Background()); err == nil || !strings.Contains(err.Error(), "build upload-grant request") {
		t.Errorf("err = %v, want the request build error", err)
	}
}
