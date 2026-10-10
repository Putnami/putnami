package command

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.putnami.dev/client"
	perrors "go.putnami.dev/errors"
	"go.putnami.dev/intelligence/agent-readiness/internal/testrepo"
	"go.putnami.dev/intelligence/agent-readiness/payload"
	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
)

const submitted = `{"token":"8f3k2q","url":"https://putnami.app/r/8f3k2q","methodVersion":"0.1","level":"L2","shareBlocked":0.63}`

func fixedCollection(t *testing.T) {
	t.Helper()
	original := collect
	collect = func(ctx context.Context, opts payload.Options) (payload.Result, error) {
		opts.Now = time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
		opts.Salt = []byte("fixed test salt")
		return original(ctx, opts)
	}
	t.Cleanup(func() { collect = original })
}

func config(dir, api string, stdout, stderr io.Writer) Config {
	return Config{Dir: dir, CollectorVersion: "0.1.0", APIURL: api, Stdout: stdout, Stderr: stderr}
}

func TestPrintPayloadIsPrivateAndReadOnly(t *testing.T) {
	spectest.Proves(t, "intelligence/agent-readiness", "private-payload", "print-payload-is-private-and-read-only")
	repo := testrepo.New(t)
	before := testrepo.Status(t, repo)
	var stdout, stderr bytes.Buffer
	result, err := Run(context.Background(), Options{PrintPayload: true, Timeout: DefaultTimeout}, config(repo, "http://127.0.0.1:1", &stdout, &stderr))
	if err != nil {
		t.Fatal(err)
	}
	if result != (Result{}) || stderr.Len() != 0 {
		t.Fatalf("result=%+v stderr=%q", result, stderr.String())
	}
	if !json.Valid(stdout.Bytes()) {
		t.Fatalf("payload is not JSON: %q", stdout.String())
	}
	if err := validatePayload(bytes.TrimSpace(stdout.Bytes())); err != nil {
		t.Fatalf("payload schema: %v", err)
	}
	if after := testrepo.Status(t, repo); after != before {
		t.Fatalf("repository changed: before=%q after=%q", before, after)
	}
}

func TestSubmissionPreservesBytesAndResponse(t *testing.T) {
	spectest.Proves(t, "intelligence/agent-readiness", "anonymous-submit", "submission-preserves-bytes-and-response")
	fixedCollection(t)
	repo := testrepo.New(t)
	before := testrepo.Status(t, repo)
	var requests int
	var received []byte
	var authorization, cookie, clientID string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		received, _ = io.ReadAll(r.Body)
		authorization = r.Header.Get("Authorization")
		cookie = r.Header.Get("Cookie")
		clientID = r.Header.Get("X-Client-Id")
		if r.Method != http.MethodPost || r.URL.Path != reportsPath {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, submitted)
	}))
	defer server.Close()
	var printed, ignored bytes.Buffer
	_, err := Run(context.Background(), Options{PrintPayload: true, Timeout: DefaultTimeout}, config(repo, server.URL, &printed, &ignored))
	if err != nil {
		t.Fatal(err)
	}
	if requests != 0 {
		t.Fatalf("print-payload sent %d requests", requests)
	}
	var human, progress bytes.Buffer
	client := server.Client()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	jar.SetCookies(parsed, []*http.Cookie{{Name: "session", Value: "private"}})
	client.Jar = jar
	sendConfig := config(repo, server.URL, &human, &progress)
	sendConfig.Client = client
	result, err := Run(context.Background(), Options{Timeout: DefaultTimeout}, sendConfig)
	if err != nil {
		t.Fatal(err)
	}
	if requests != 1 || !bytes.Equal(received, bytes.TrimSuffix(printed.Bytes(), []byte("\n"))) {
		t.Fatalf("requests=%d sent bytes differ from printed payload", requests)
	}
	if authorization != "" || cookie != "" || clientID != "putnami-cli" {
		t.Fatalf("auth=%q cookie=%q client=%q", authorization, cookie, clientID)
	}
	if result.Report != "https://putnami.app/r/8f3k2q" || result.Level != "L2" || result.ShareBlocked != .63 || result.SentBytes != len(received) || result.Commits != 4 {
		t.Fatalf("result=%+v", result)
	}
	if !strings.Contains(human.String(), "Report: "+result.Report) || !strings.Contains(human.String(), "supervised agents (L2)") || !strings.Contains(progress.String(), "Reading the git history") {
		t.Fatalf("human=%q progress=%q", human.String(), progress.String())
	}
	if after := testrepo.Status(t, repo); after != before {
		t.Fatalf("repository changed: before=%q after=%q", before, after)
	}
}

func TestInvalidPayloadAndInvalidResponseFailClosed(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"token":"x"}`)
	}))
	defer server.Close()
	if _, err := Submit(context.Background(), server.URL, nil, []byte(`{"unknown":true}`)); err == nil || requests != 0 {
		t.Fatalf("invalid payload sent %d requests, error=%v", requests, err)
	}
	built, err := BuildPayload(context.Background(), testrepo.New(t), "0.1.0", DefaultTimeout)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Submit(context.Background(), server.URL, nil, built.Bytes)
	if err == nil || requests != 1 {
		t.Fatalf("invalid response: requests=%d error=%v", requests, err)
	}
	if failure := sendFailure(context.Background(), err, server.URL); protocolcli.ExitCodeForError(failure) != protocolcli.ExitAPI || !strings.Contains(failure.Error(), "outside its contract") {
		t.Fatalf("failure=%v", failure)
	}
}

func TestUndeclaredResponseFieldFailsClosed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"token":"8f3k2q","url":"https://putnami.app/r/8f3k2q","methodVersion":"0.1","level":"L2","shareBlocked":0.63,"unexpected":true}`)
	}))
	defer server.Close()
	built, err := BuildPayload(context.Background(), testrepo.New(t), "0.1.0", DefaultTimeout)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Submit(context.Background(), server.URL, nil, built.Bytes); err == nil || !strings.Contains(err.Error(), "outside contract") {
		t.Fatalf("response accepted: %v", err)
	}
}

func TestServiceRefusalsAreAPIClassified(t *testing.T) {
	built, err := BuildPayload(context.Background(), testrepo.New(t), "0.1.0", DefaultTimeout)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		status       int
		body, detail string
	}{
		{http.StatusBadRequest, `{"code":"validation","error":"Bad Request","message":"bad payload"}`, "refused the payload (400): bad payload"},
		{http.StatusRequestEntityTooLarge, `{}`, "larger than it accepts"},
		{http.StatusTooManyRequests, `{}`, "try again in a minute"},
		{http.StatusServiceUnavailable, `{}`, "answered 503"},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(tc.status)
			_, _ = io.WriteString(w, tc.body)
		}))
		_, err := Submit(context.Background(), server.URL, nil, built.Bytes)
		failure := sendFailure(context.Background(), err, server.URL)
		server.Close()
		if protocolcli.ExitCodeForError(failure) != protocolcli.ExitAPI || !strings.Contains(failure.Error(), tc.detail) || !strings.Contains(failure.Error(), InspectCommand) {
			t.Errorf("status %d: %v", tc.status, failure)
		}
	}
}

func TestSubmissionDeadlineDoesNotRetry(t *testing.T) {
	release := make(chan struct{})
	ctx := newArrivalDeadline()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		ctx.expire()
		<-release
	}))
	defer server.Close()
	defer close(release)
	built, err := BuildPayload(context.Background(), testrepo.New(t), "0.1.0", DefaultTimeout)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Submit(ctx, server.URL, nil, built.Bytes)
	if !perrors.Is(err, client.CodeClientDeadline) || requests.Load() != 1 {
		t.Fatalf("deadline: requests=%d error=%v", requests.Load(), err)
	}
	if failure := sendFailure(ctx, err, server.URL); protocolcli.ExitCodeForError(failure) != protocolcli.ExitAPI || !strings.Contains(failure.Error(), "raise --timeout") {
		t.Fatalf("failure=%v", failure)
	}
}

// arrivalDeadline is a context whose deadline passes when expire is called:
// Done closes and Err reports context.DeadlineExceeded. Deadline reports a fixed
// time one hour ahead, so a client keeps its own budget. A test expires it once
// its request is in flight, however long the work before the request takes.
type arrivalDeadline struct {
	context.Context
	deadline time.Time
	once     sync.Once
	done     chan struct{}
}

func newArrivalDeadline() *arrivalDeadline {
	return &arrivalDeadline{Context: context.Background(), deadline: time.Now().Add(time.Hour), done: make(chan struct{})}
}

func (c *arrivalDeadline) expire() { c.once.Do(func() { close(c.done) }) }

func (c *arrivalDeadline) Deadline() (time.Time, bool) { return c.deadline, true }

func (c *arrivalDeadline) Done() <-chan struct{} { return c.done }

func (c *arrivalDeadline) Err() error {
	select {
	case <-c.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

func TestInvalidCollectionNeverPrintsOrSends(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
	defer server.Close()
	for _, printPayload := range []bool{true, false} {
		var out, progress bytes.Buffer
		cfg := config(testrepo.New(t), server.URL, &out, &progress)
		cfg.CollectorVersion = ""
		_, err := Run(context.Background(), Options{PrintPayload: printPayload, Timeout: DefaultTimeout}, cfg)
		if err == nil || !strings.Contains(err.Error(), "schema v1") || out.Len() != 0 || requests != 0 {
			t.Fatalf("print=%v requests=%d output=%q error=%v", printPayload, requests, out.String(), err)
		}
	}
}

func TestRedirectDoesNotReplaySubmission(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Location", reportsPath)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	built, err := BuildPayload(context.Background(), testrepo.New(t), "0.1.0", DefaultTimeout)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Submit(context.Background(), server.URL, nil, built.Bytes)
	if err == nil || requests != 1 {
		t.Fatalf("redirect: requests=%d error=%v", requests, err)
	}
}

func TestParseOptionsBoundsTimeout(t *testing.T) {
	for _, value := range []string{`0`, `3601`, `1.5`, `"soon"`, `true`, `"+Inf"`} {
		_, err := ParseOptions(map[string]json.RawMessage{"timeout": json.RawMessage(value)})
		if protocolcli.ExitCodeForError(err) != protocolcli.ExitUsage {
			t.Errorf("timeout %s: %v", value, err)
		}
	}
	for _, value := range []string{`1`, `"3600"`} {
		if _, err := ParseOptions(map[string]json.RawMessage{"timeout": json.RawMessage(value)}); err != nil {
			t.Errorf("timeout %s: %v", value, err)
		}
	}
	if got, err := ParseOptions(map[string]json.RawMessage{"print-payload": json.RawMessage(`"true"`)}); err != nil || !got.PrintPayload {
		t.Fatalf("string boolean from context: %+v, %v", got, err)
	}
}

func TestPrivacyRefusalPrintsAndSendsNothing(t *testing.T) {
	original := collect
	collect = func(context.Context, payload.Options) (payload.Result, error) {
		return payload.Result{}, errors.Join(payload.ErrPrivacy, errors.New("private path"))
	}
	t.Cleanup(func() { collect = original })
	var sent int
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { sent++ }))
	defer server.Close()
	for _, printPayload := range []bool{true, false} {
		var out, progress bytes.Buffer
		_, err := Run(context.Background(), Options{PrintPayload: printPayload, Timeout: DefaultTimeout}, config(testrepo.New(t), server.URL, &out, &progress))
		if err == nil || sent != 0 || out.Len() != 0 {
			t.Fatalf("print=%v sent=%d out=%q err=%v", printPayload, sent, out.String(), err)
		}
	}
}

func TestConcurrentSubmissionsKeepTheirExactBodies(t *testing.T) {
	var mu sync.Mutex
	var received []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		received = append(received, string(body))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, submitted)
	}))
	defer server.Close()
	built, err := BuildPayload(context.Background(), testrepo.New(t), "0.1.0", DefaultTimeout)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := Submit(context.Background(), server.URL, nil, built.Bytes); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if len(received) != 4 {
		t.Fatalf("received %d", len(received))
	}
	for _, body := range received {
		if body != string(built.Bytes) {
			t.Fatal("concurrent body changed")
		}
	}
}
