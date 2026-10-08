package events

import (
	"context"
	stderrors "errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
	"golang.org/x/oauth2"
)

// scriptedPubSub is an in-memory Pub/Sub REST endpoint that answers attempt n
// with answers[n-1], and every further attempt with the last answer.
type scriptedPubSub struct {
	server   *httptest.Server
	attempts atomic.Int32
	mu       sync.Mutex
	bodies   []string
	// waits holds the waits between attempts, in order.
	waits []time.Duration
}

func newScriptedPubSub(t *testing.T, answers ...http.HandlerFunc) *scriptedPubSub {
	t.Helper()
	s := &scriptedPubSub{}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(s.attempts.Add(1))
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.bodies = append(s.bodies, string(body))
		s.mu.Unlock()
		answers[min(n, len(answers))-1](w, r)
	}))
	t.Cleanup(s.server.Close)
	return s
}

// topic returns a topic whose retry policy records its waits and never sleeps.
// A nil policy stands for the default one.
func (s *scriptedPubSub) topic(policy *googlePubSubRetryPolicy) *googlePubSubRESTTopic {
	retry := googlePubSubRetryPolicy{}
	if policy != nil {
		retry = *policy
	}
	if retry.sleep == nil {
		retry.sleep = func(ctx context.Context, d time.Duration) error {
			s.waits = append(s.waits, d)
			return ctx.Err()
		}
	}
	client := &googlePubSubRESTClient{client: s.server.Client(), endpoint: s.server.URL + "/", projectID: "my-project", retry: retry}
	return client.Topic("t").(*googlePubSubRESTTopic)
}

func answerOK(w http.ResponseWriter, _ *http.Request) {
	_, _ = w.Write([]byte(`{"messageIds":["msg-1"]}`))
}

func answerStatus(code int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) }
}

func answerRetryAfter(code int, retryAfter string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", retryAfter)
		w.WriteHeader(code)
	}
}

// answerReset closes the connection with a TCP reset and no HTTP answer.
func answerReset(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		if tcp, ok := conn.(*net.TCPConn); ok {
			_ = tcp.SetLinger(0)
		}
		_ = conn.Close()
	}
}

var retryTestMessage = GooglePubSubPublishMessage{Data: []byte("x"), OrderingKey: "order-1"}

func TestDirectPubSubPublishRetriesATransientFailure(t *testing.T) {
	spectest.Proves(t, "go/event-delivery", "provider-publish-retry", "pubsub-publish-retries-a-transient-failure")
	for _, code := range []int{408, 429, 500, 502, 503, 504} {
		server := newScriptedPubSub(t, answerStatus(code), answerOK)
		if err := server.topic(nil).Publish(context.Background(), retryTestMessage); err != nil {
			t.Fatalf("status %d then 200: Publish = %v, want success", code, err)
		}
		if got := server.attempts.Load(); got != 2 {
			t.Fatalf("status %d then 200: %d attempts, want 2", code, got)
		}
		if len(server.waits) != 1 || server.waits[0] < 100*time.Millisecond || server.waits[0] > 125*time.Millisecond {
			t.Fatalf("status %d then 200: waits = %v, want one wait of 100 to 125 ms", code, server.waits)
		}
		// Every attempt sends the same bytes, so a duplicate carries the same
		// envelope id and ordering key.
		if server.bodies[0] != server.bodies[1] || server.bodies[0] != `{"messages":[{"data":"eA==","orderingKey":"order-1"}]}` {
			t.Fatalf("status %d then 200: bodies = %q, want the same message twice", code, server.bodies)
		}
	}
}

func TestDirectPubSubPublishStopsAfterFourAttempts(t *testing.T) {
	spectest.Proves(t, "go/event-delivery", "provider-publish-retry", "pubsub-publish-stops-after-four-attempts")
	server := newScriptedPubSub(t, answerStatus(http.StatusInternalServerError))
	err := server.topic(nil).Publish(context.Background(), retryTestMessage)
	var publishErr *GooglePubSubPublishError
	if !stderrors.As(err, &publishErr) || publishErr.StatusCode != http.StatusInternalServerError {
		t.Fatalf("Publish = %v, want the 500 answer of the last attempt", err)
	}
	if got := server.attempts.Load(); got != 4 {
		t.Fatalf("%d attempts, want exactly 4", got)
	}
	if got := classifyDirectPubSubPublishError(err); got != PublishOutcomeAmbiguous {
		t.Fatalf("outcome = %q, want ambiguous", got)
	}
	// 100 ms, 200 ms, 400 ms, each with 0 to 25% of jitter.
	if len(server.waits) != 3 {
		t.Fatalf("waits = %v, want 3", server.waits)
	}
	for i, base := range []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond} {
		if server.waits[i] < base || server.waits[i] > base+base/4 {
			t.Errorf("wait %d = %v, want %v to %v", i+1, server.waits[i], base, base+base/4)
		}
	}

	// The default backoff never exceeds 2 s plus jitter.
	backoff := googlePubSubRetryPolicy{}.withDefaults().backoff
	if got := backoff(20); got < 2*time.Second || got > 2500*time.Millisecond {
		t.Errorf("backoff(20) = %v, want 2 s to 2.5 s", got)
	}

	// Four refused attempts stay retryable: nothing was published.
	refused := newScriptedPubSub(t, answerStatus(http.StatusServiceUnavailable))
	err = refused.topic(nil).Publish(context.Background(), retryTestMessage)
	if got := refused.attempts.Load(); got != 4 {
		t.Fatalf("503: %d attempts, want exactly 4", got)
	}
	if got := classifyDirectPubSubPublishError(err); got != PublishOutcomeRetryable {
		t.Fatalf("503 outcome = %q, want retryable", got)
	}
}

func TestDirectPubSubPublishDoesNotRetryAPermanentFailure(t *testing.T) {
	tests := []struct {
		name    string
		answer  http.HandlerFunc
		outcome string
	}{
		{"400", answerStatus(http.StatusBadRequest), PublishOutcomePermanent},
		{"403", answerStatus(http.StatusForbidden), PublishOutcomePermanent},
		{"404", answerStatus(http.StatusNotFound), PublishOutcomePermanent},
		{"409", answerStatus(http.StatusConflict), PublishOutcomeAmbiguous},
		{"499", answerStatus(499), PublishOutcomeAmbiguous},
		{"501", answerStatus(http.StatusNotImplemented), PublishOutcomeAmbiguous},
		// Pub/Sub accepted the publish: sending it again would duplicate it.
		{"unreadable 2xx answer", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("not json")) }, PublishOutcomeAmbiguous},
		{"2xx answer without a message id", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{}`)) }, PublishOutcomeAmbiguous},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := newScriptedPubSub(t, tc.answer, answerOK)
			err := server.topic(nil).Publish(context.Background(), retryTestMessage)
			if err == nil {
				t.Fatal("Publish succeeded, want the error of the first attempt")
			}
			if got := server.attempts.Load(); got != 1 {
				t.Fatalf("%d attempts, want 1", got)
			}
			if got := classifyDirectPubSubPublishError(err); got != tc.outcome {
				t.Fatalf("outcome = %q, want %q", got, tc.outcome)
			}
		})
	}
}

func TestDirectPubSubPublishStopsWhenTheCallerContextEnds(t *testing.T) {
	// The caller cancels during the wait before the first retry. The real
	// sleeper returns at once and no second attempt is made.
	ctx, cancel := context.WithCancel(context.Background())
	server := newScriptedPubSub(t, answerStatus(http.StatusServiceUnavailable), answerOK)
	policy := &googlePubSubRetryPolicy{sleep: func(ctx context.Context, _ time.Duration) error {
		cancel()
		return sleepWithContext(ctx, time.Hour)
	}}
	start := time.Now()
	err := server.topic(policy).Publish(ctx, retryTestMessage)
	var publishErr *GooglePubSubPublishError
	if !stderrors.As(err, &publishErr) || publishErr.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("Publish = %v, want the 503 answer of the only attempt", err)
	}
	if got := server.attempts.Load(); got != 1 {
		t.Fatalf("%d attempts, want 1", got)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Publish took %v, want it to stop at once", elapsed)
	}

	// A context that ended before the publish allows one attempt and no retry.
	done, cancelDone := context.WithCancel(context.Background())
	cancelDone()
	server = newScriptedPubSub(t, answerOK)
	err = server.topic(nil).Publish(done, retryTestMessage)
	if !stderrors.Is(err, context.Canceled) || len(server.waits) != 0 || server.attempts.Load() > 1 {
		t.Fatalf("Publish = %v after %d attempts and waits %v, want context.Canceled and no retry", err, server.attempts.Load(), server.waits)
	}
}

func TestDirectPubSubPublishHonoursRetryAfter(t *testing.T) {
	for _, code := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		server := newScriptedPubSub(t, answerRetryAfter(code, "2"), answerOK)
		if err := server.topic(nil).Publish(context.Background(), retryTestMessage); err != nil {
			t.Fatalf("status %d: Publish = %v, want success", code, err)
		}
		if len(server.waits) != 1 || server.waits[0] != 2*time.Second {
			t.Fatalf("status %d: waits = %v, want [2s]", code, server.waits)
		}
	}

	// A Retry-After shorter than the backoff does not shorten the wait.
	short := newScriptedPubSub(t, answerRetryAfter(http.StatusServiceUnavailable, "0"), answerOK)
	if err := short.topic(nil).Publish(context.Background(), retryTestMessage); err != nil || len(short.waits) != 1 || short.waits[0] < 100*time.Millisecond {
		t.Fatalf("Retry-After 0: Publish = %v with waits %v, want success after the backoff", err, short.waits)
	}

	// Only 429 and 503 answers carry a wait the transport reads.
	other := newScriptedPubSub(t, answerRetryAfter(http.StatusInternalServerError, "2"), answerOK)
	if err := other.topic(nil).Publish(context.Background(), retryTestMessage); err != nil || len(other.waits) != 1 || other.waits[0] > 125*time.Millisecond {
		t.Fatalf("500 with Retry-After: Publish = %v with waits %v, want success after the backoff", err, other.waits)
	}

	// A Retry-After that does not fit in the publish limit ends the publish
	// with the answer that carried it.
	long := newScriptedPubSub(t, answerRetryAfter(http.StatusTooManyRequests, "30"), answerOK)
	err := long.topic(nil).Publish(context.Background(), retryTestMessage)
	if got := classifyDirectPubSubPublishError(err); got != PublishOutcomeRetryable || long.attempts.Load() != 1 || len(long.waits) != 0 {
		t.Fatalf("Retry-After 30: Publish = %v after %d attempts and waits %v, want one retryable attempt", err, long.attempts.Load(), long.waits)
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	tests := []struct {
		value string
		want  time.Duration
	}{
		{"", 0},
		{"3", 3 * time.Second},
		{" 7 ", 7 * time.Second},
		{"-1", 0},
		{"soon", 0},
		{now.Add(4 * time.Second).Format(http.TimeFormat), 4 * time.Second},
		{now.Add(-time.Minute).Format(http.TimeFormat), 0},
	}
	for _, tc := range tests {
		if got := parseRetryAfter(tc.value, now); got != tc.want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", tc.value, got, tc.want)
		}
	}
}

func TestDirectPubSubPublishRetriesAConnectionReset(t *testing.T) {
	server := newScriptedPubSub(t, answerReset(t), answerOK)
	if err := server.topic(nil).Publish(context.Background(), retryTestMessage); err != nil {
		t.Fatalf("Publish = %v, want success on the second attempt", err)
	}
	if got := server.attempts.Load(); got != 2 {
		t.Fatalf("%d attempts, want 2", got)
	}

	// A refused connection is retried too: nothing listens on a closed server.
	closed := newScriptedPubSub(t, answerOK)
	topic := closed.topic(nil)
	closed.server.Close()
	err := topic.Publish(context.Background(), retryTestMessage)
	if err == nil || len(closed.waits) != 3 {
		t.Fatalf("closed server: Publish = %v with waits %v, want an error after 3 retries", err, closed.waits)
	}
	if got := classifyDirectPubSubPublishError(err); got != PublishOutcomeAmbiguous {
		t.Fatalf("closed server outcome = %q, want ambiguous", got)
	}
}

func TestDirectPubSubPublishRespectsTheOverallBudget(t *testing.T) {
	// A fake clock that only the waits move: each attempt answers 503 with
	// Retry-After 4. Attempts run at 0 s, 4 s and 8 s. The fourth would start at
	// 12 s, after the 10 s limit, so it is never made.
	now := time.Unix(1700000000, 0)
	server := newScriptedPubSub(t, answerRetryAfter(http.StatusServiceUnavailable, "4"))
	var waited time.Duration
	policy := &googlePubSubRetryPolicy{
		now: func() time.Time { return now.Add(waited) },
		sleep: func(ctx context.Context, d time.Duration) error {
			waited += d
			return ctx.Err()
		},
	}
	err := server.topic(policy).Publish(context.Background(), retryTestMessage)
	if got := server.attempts.Load(); got != 3 || waited != 8*time.Second {
		t.Fatalf("%d attempts and %v of waits, want 3 attempts and 8s", got, waited)
	}
	if got := classifyDirectPubSubPublishError(err); got != PublishOutcomeRetryable {
		t.Fatalf("Publish = %v (%q), want the retryable 503 answer and not a deadline error", err, got)
	}

	// With the real clock and a server that never answers, the publish ends
	// within its limit, and each attempt within its own.
	release := make(chan struct{})
	hang := newScriptedPubSub(t, func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	t.Cleanup(func() { close(release) })
	policy = &googlePubSubRetryPolicy{
		attemptTimeout: 40 * time.Millisecond,
		budget:         100 * time.Millisecond,
		backoff:        func(int) time.Duration { return time.Millisecond },
		sleep:          sleepWithContext,
	}
	start := time.Now()
	err = hang.topic(policy).Publish(context.Background(), retryTestMessage)
	elapsed := time.Since(start)
	if !stderrors.Is(err, context.DeadlineExceeded) || classifyDirectPubSubPublishError(err) != PublishOutcomeAmbiguous {
		t.Fatalf("Publish = %v, want an ambiguous deadline error", err)
	}
	if got := hang.attempts.Load(); got < 2 || got > 4 {
		t.Fatalf("%d attempts, want 2 to 4", got)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("Publish took %v, want about 100ms", elapsed)
	}
}

func TestDirectPubSubPublishRetriesAnAttemptTimeout(t *testing.T) {
	release := make(chan struct{})
	server := newScriptedPubSub(t, func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}, answerOK)
	t.Cleanup(func() { close(release) })
	policy := &googlePubSubRetryPolicy{attemptTimeout: 50 * time.Millisecond}
	if err := server.topic(policy).Publish(context.Background(), retryTestMessage); err != nil {
		t.Fatalf("Publish = %v, want success on the second attempt", err)
	}
	if got := server.attempts.Load(); got != 2 {
		t.Fatalf("%d attempts, want 2", got)
	}
}

func TestDirectPubSubPublishKeepsAnUnknownOutcomeAmbiguous(t *testing.T) {
	spectest.Proves(t, "go/event-delivery", "provider-publish-retry", "pubsub-publish-keeps-an-unknown-outcome-ambiguous")
	// The first attempt may have published; the last one was refused. The
	// error is the last one, and the outcome is still ambiguous.
	server := newScriptedPubSub(t, answerStatus(http.StatusInternalServerError), answerStatus(http.StatusServiceUnavailable))
	err := server.topic(nil).Publish(context.Background(), retryTestMessage)
	var publishErr *GooglePubSubPublishError
	if !stderrors.As(err, &publishErr) || publishErr.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("Publish = %v, want the 503 answer of the last attempt", err)
	}
	if got := server.attempts.Load(); got != 4 {
		t.Fatalf("%d attempts, want 4", got)
	}
	if got := classifyDirectPubSubPublishError(err); got != PublishOutcomeAmbiguous {
		t.Fatalf("outcome = %q, want ambiguous", got)
	}
	if want := publishErr.Error() + "; an earlier attempt may have published the message"; err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}

	// The same holds when the last attempt is a permanent refusal.
	server = newScriptedPubSub(t, answerReset(t), answerStatus(http.StatusForbidden))
	err = server.topic(nil).Publish(context.Background(), retryTestMessage)
	if got := classifyDirectPubSubPublishError(err); got != PublishOutcomeAmbiguous || server.attempts.Load() != 2 {
		t.Fatalf("reset then 403: outcome = %q after %d attempts, want ambiguous after 2", got, server.attempts.Load())
	}
}

// scriptedTokenSource fails lookup n with errs[n-1], then returns a token.
type scriptedTokenSource struct {
	errs  []error
	calls int
}

func (s *scriptedTokenSource) Token() (*oauth2.Token, error) {
	s.calls++
	if s.calls <= len(s.errs) {
		return nil, s.errs[s.calls-1]
	}
	return &oauth2.Token{AccessToken: "access-1", TokenType: "Bearer"}, nil
}

func TestDirectPubSubPublishRetriesATransientTokenFailure(t *testing.T) {
	retrieve := func(code int) error {
		return &oauth2.RetrieveError{Response: &http.Response{StatusCode: code}}
	}
	tests := []struct {
		name     string
		tokens   *scriptedTokenSource
		lookups  int
		requests int32
		outcome  string
	}{
		{"network failure", &scriptedTokenSource{errs: []error{stderrors.New("dial tcp: connection refused")}}, 2, 1, ""},
		{"token endpoint unavailable", &scriptedTokenSource{errs: []error{retrieve(503), retrieve(429)}}, 3, 1, ""},
		{"token endpoint refuses the credentials", &scriptedTokenSource{errs: []error{retrieve(400)}}, 1, 0, PublishOutcomeRetryable},
		{"every lookup fails", &scriptedTokenSource{errs: []error{retrieve(500), retrieve(500), retrieve(500), retrieve(500)}}, 4, 0, PublishOutcomeRetryable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := newScriptedPubSub(t, answerOK)
			topic := server.topic(nil)
			topic.client.tokens = tc.tokens
			err := topic.Publish(context.Background(), retryTestMessage)
			if got := classifyDirectPubSubPublishError(err); got != tc.outcome {
				t.Fatalf("Publish = %v (%q), want outcome %q", err, got, tc.outcome)
			}
			if tc.tokens.calls != tc.lookups || server.attempts.Load() != tc.requests {
				t.Fatalf("%d token lookups and %d requests, want %d and %d", tc.tokens.calls, server.attempts.Load(), tc.lookups, tc.requests)
			}
		})
	}

	// A token lookup that outlives its attempt is retried while the caller
	// still waits.
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	server := newScriptedPubSub(t, answerOK)
	topic := server.topic(&googlePubSubRetryPolicy{attemptTimeout: 20 * time.Millisecond, maxAttempts: 2})
	topic.client.tokens = &fakeTokenSource{block: block}
	err := topic.Publish(context.Background(), retryTestMessage)
	var credentialErr *GooglePubSubCredentialError
	if !stderrors.As(err, &credentialErr) || len(server.waits) != 1 || server.attempts.Load() != 0 {
		t.Fatalf("Publish = %v with waits %v and %d requests, want a credential error after one retry and no request", err, server.waits, server.attempts.Load())
	}
}

func TestSleepWithContext(t *testing.T) {
	if err := sleepWithContext(context.Background(), time.Millisecond); err != nil {
		t.Fatalf("sleepWithContext = %v, want nil", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleepWithContext(ctx, time.Hour); !stderrors.Is(err, context.Canceled) {
		t.Fatalf("sleepWithContext = %v, want context.Canceled", err)
	}
}
