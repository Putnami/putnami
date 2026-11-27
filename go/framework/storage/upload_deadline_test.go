package storage

import (
	"context"
	stderrors "errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
)

const testPutTimeout = 50 * time.Millisecond

// slowBody delivers its chunks one by one, waiting gap before each, so the
// whole body takes longer than a short request timeout.
type slowBody struct {
	chunks []string
	gap    time.Duration
}

func (s *slowBody) Read(p []byte) (int, error) {
	if len(s.chunks) == 0 {
		return 0, io.EOF
	}
	time.Sleep(s.gap)
	n := copy(p, s.chunks[0])
	s.chunks[0] = s.chunks[0][n:]
	if s.chunks[0] == "" {
		s.chunks = s.chunks[1:]
	}
	return n, nil
}

func newSlowBody() *slowBody {
	// 5 chunks 40 ms apart: 200 ms, four times the 50 ms request timeout.
	return &slowBody{chunks: []string{"a", "b", "c", "d", "e"}, gap: 40 * time.Millisecond}
}

// zeros is an endless body of zero bytes.
type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// stallBody is larger than the socket buffers between client and server, so a
// server that stops reading it blocks the client's writes.
func stallBody() io.Reader { return io.LimitReader(zeros{}, 64<<20) }

func newTestGCSBackend(url string) *GCSBackend {
	return &GCSBackend{
		httpClient:     newStorageHTTPClient(),
		endpoint:       url,
		tokens:         staticTokenSource(""),
		requestTimeout: testPutTimeout,
	}
}

func newTestS3Backend(t *testing.T, cfg S3Config, url string) *S3Backend {
	t.Helper()
	cfg.Endpoint = url
	cfg.RequestTimeout = testPutTimeout
	b := NewS3Backend(cfg)
	t.Cleanup(func() { _ = b.Close() })
	return b
}

// bodyRead is what a test server saw of an upload body.
type bodyRead struct {
	n   int64
	err error
}

// stallingServer reads the whole body, reports it, then holds its answer
// until the test ends.
func stallingServer(t *testing.T) (*httptest.Server, <-chan bodyRead) {
	t.Helper()
	read := make(chan bodyRead, 1)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		n, err := io.Copy(io.Discard, r.Body)
		read <- bodyRead{n: n, err: err}
		<-release
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	return srv, read
}

// readStallingServer reads one byte of the body, then stops reading until the
// test ends.
func readStallingServer(t *testing.T) *httptest.Server {
	t.Helper()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, _ = r.Body.Read(make([]byte, 1))
		<-release
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	return srv
}

func TestGCSBackend_Put_SlowBodyIsNotCutByRequestTimeout(t *testing.T) {
	spectest.Proves(t, "go/object-storage", "deadlines-and-streams", "gcs-put-request-timeout-spares-a-slow-body")
	f := newFakeGCS(t)
	b := f.backend(t)
	b.requestTimeout = testPutTimeout

	res, err := b.Put(context.Background(), "bucket", "slow.bin", newSlowBody(), nil)
	if err != nil {
		t.Fatalf("Put of a body slower than RequestTimeout failed: %v", err)
	}
	if res.Size != 5 {
		t.Errorf("Size = %d, want 5", res.Size)
	}
}

func TestGCSBackend_Put_RequestTimeoutBoundsAnswerAfterBody(t *testing.T) {
	spectest.Proves(t, "go/object-storage", "deadlines-and-streams", "gcs-put-request-timeout-bounds-answer-after-body")
	srv, read := stallingServer(t)

	armed := recordUploadTimers(t)

	_, err := newTestGCSBackend(srv.URL).Put(callerBudget(t), "bucket", "stalled", newSlowBody(), nil)
	assertAnswerDeadline(t, err, read, armed, 0)
}

func TestGCSBackend_Put_RequestTimeoutBoundsAStorageStall(t *testing.T) {
	spectest.Proves(t, "go/object-storage", "deadlines-and-streams", "gcs-put-request-timeout-bounds-a-storage-stall")
	srv := readStallingServer(t)

	armed := recordUploadTimers(t)

	_, err := newTestGCSBackend(srv.URL).Put(callerBudget(t), "bucket", "stalled", stallBody(), nil)
	assertStallDeadline(t, err, armed)
}

func TestS3Backend_Put_SlowBodyIsNotCutByRequestTimeout(t *testing.T) {
	spectest.Proves(t, "go/object-storage", "deadlines-and-streams", "s3-put-request-timeout-spares-a-slow-body")
	for _, tc := range []struct {
		name   string
		config S3Config
	}{
		// Without credentials the body streams to the wire.
		{name: "streamed", config: S3Config{}},
		// With credentials an unsized body is spooled to a file first.
		{name: "spooled", config: S3Config{AccessKey: "AKIA", SecretKey: "secret"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stored := make(chan string, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				data, _ := io.ReadAll(r.Body)
				stored <- string(data)
				w.Header().Set("ETag", `"etag"`)
				w.WriteHeader(http.StatusOK)
			}))
			defer srv.Close()

			res, err := newTestS3Backend(t, tc.config, srv.URL).Put(context.Background(), "bucket", "slow.bin", newSlowBody(), nil)
			if err != nil {
				t.Fatalf("Put of a body slower than RequestTimeout failed: %v", err)
			}
			if got := <-stored; res.Size != 5 || got != "abcde" {
				t.Errorf("Size = %d, stored %q; want 5, abcde", res.Size, got)
			}
		})
	}
}

func TestS3Backend_Put_RequestTimeoutBoundsAnswerAfterBody(t *testing.T) {
	spectest.Proves(t, "go/object-storage", "deadlines-and-streams", "s3-put-request-timeout-bounds-answer-after-body")
	srv, read := stallingServer(t)

	armed := recordUploadTimers(t)

	_, err := newTestS3Backend(t, S3Config{}, srv.URL).Put(callerBudget(t), "bucket", "stalled", newSlowBody(), nil)
	assertAnswerDeadline(t, err, read, armed, 5)
}

func TestS3Backend_Put_RequestTimeoutBoundsAStorageStall(t *testing.T) {
	spectest.Proves(t, "go/object-storage", "deadlines-and-streams", "s3-put-request-timeout-bounds-a-storage-stall")
	srv := readStallingServer(t)

	armed := recordUploadTimers(t)

	_, err := newTestS3Backend(t, S3Config{}, srv.URL).Put(callerBudget(t), "bucket", "stalled", stallBody(), nil)
	assertStallDeadline(t, err, armed)
}

// callerBudget is a caller's context that a regression cannot outlive: it is
// canceled, never timed out, after a budget far above any scheduling delay.
// A Put it ends fails as a cancellation, which assertDeadlineError refuses, so
// only the Put's own request timeout can pass the test.
func callerBudget(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	timer := time.AfterFunc(10*time.Second, cancel)
	t.Cleanup(func() { timer.Stop(); cancel() })
	return ctx
}

// recordUploadTimers replaces uploadTimer for the test and returns the
// durations each Put's deadline is armed with: the timer's start and every
// Reset. The test replaces a package variable, so it does not run in parallel.
func recordUploadTimers(t *testing.T) func() []time.Duration {
	t.Helper()
	rec := &armRecord{}
	previous := uploadTimer
	uploadTimer = func(d time.Duration, f func()) deadlineTimer {
		rec.add(d)
		return &recordingTimer{timer: previous(d, f), rec: rec}
	}
	t.Cleanup(func() { uploadTimer = previous })
	return rec.durations
}

// armRecord holds the durations a deadline was armed with.
type armRecord struct {
	mu    sync.Mutex
	armed []time.Duration
}

func (r *armRecord) add(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.armed = append(r.armed, d)
}

func (r *armRecord) durations() []time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.armed)
}

// recordingTimer records each Reset of the deadline's timer.
type recordingTimer struct {
	timer deadlineTimer
	rec   *armRecord
}

func (r *recordingTimer) Stop() bool { return r.timer.Stop() }

func (r *recordingTimer) Reset(d time.Duration) bool {
	r.rec.add(d)
	return r.timer.Reset(d)
}

// assertArmedRequestTimeout checks that the Put armed its deadline at least
// atLeast times, and every time with RequestTimeout: at the first bytes storage
// is handed, at each later chunk, and at the end of the body.
func assertArmedRequestTimeout(t *testing.T, armed func() []time.Duration, atLeast int) {
	t.Helper()
	durations := armed()
	if len(durations) < atLeast {
		t.Fatalf("the Put armed its deadline %d times, want at least %d", len(durations), atLeast)
	}
	for _, d := range durations {
		if d != testPutTimeout {
			t.Fatalf("the Put armed a deadline of %s, want RequestTimeout (%s)", d, testPutTimeout)
		}
	}
}

// assertAnswerDeadline checks that a Put whose body the server read in full
// failed on its own deadline while the server held the answer. want is the
// body size the server must have read; 0 accepts any size, for a multipart
// body that wraps the object.
func assertAnswerDeadline(t *testing.T, err error, read <-chan bodyRead, armed func() []time.Duration, want int64) {
	t.Helper()
	assertDeadlineError(t, err)
	// The slow body hands its bytes in chunks, then ends: at least the first
	// chunk's arm and the arm after the body.
	assertArmedRequestTimeout(t, armed, 2)
	select {
	case got := <-read:
		if got.err != nil || got.n == 0 || (want > 0 && got.n != want) {
			t.Fatalf("server read %d bytes, err %v; want the whole body", got.n, got.err)
		}
	case <-time.After(10 * time.Second):
		// A safety net, not a margin: the server reports its read of the body
		// whether the read ended or failed.
		t.Fatal("the server never reported reading the body")
	}
}

// assertStallDeadline checks that a Put whose server stopped reading the body
// failed on its own deadline, before the caller's budget and the test server's
// release.
func assertStallDeadline(t *testing.T, err error, armed func() []time.Duration) {
	t.Helper()
	assertDeadlineError(t, err)
	assertArmedRequestTimeout(t, armed, 1)
}

func assertDeadlineError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected Put to fail on its request timeout")
	}
	if !stderrors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want it to wrap context.DeadlineExceeded", err)
	}
	if stderrors.Is(err, context.Canceled) {
		t.Errorf("err = %v, must not read as the caller's cancellation", err)
	}
}

func TestUploadDeadline_CallerCancelStaysACancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	reqCtx, deadline := withUploadDeadline(ctx, time.Hour)
	defer deadline.stop()
	cancel()
	<-reqCtx.Done()
	err := deadline.cause(reqCtx, reqCtx.Err())
	if !stderrors.Is(err, context.Canceled) || stderrors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want the caller's cancellation", err)
	}
}

func TestUploadDeadline_PausesWhileTheCallerReads(t *testing.T) {
	reqCtx, deadline := withUploadDeadline(context.Background(), 50*time.Millisecond)
	defer deadline.stop()
	body := &sentReader{r: &slowBody{chunks: []string{"a", "b", "c"}, gap: 150 * time.Millisecond}, deadline: deadline}
	buf := make([]byte, 1)
	for range 3 {
		if _, err := body.Read(buf); err != nil {
			t.Fatalf("Read: %v", err)
		}
		// The transport takes the byte at once and asks for the next one.
	}
	if reqCtx.Err() != nil {
		t.Fatalf("the deadline ran while the caller's reader was producing bytes: %v", context.Cause(reqCtx))
	}
}

func TestUploadDeadline_StartsOnCloseAndNeverAfterStop(t *testing.T) {
	reqCtx, deadline := withUploadDeadline(context.Background(), 10*time.Millisecond)
	body := &sentReader{r: strings.NewReader("abc"), deadline: deadline}
	if err := body.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-reqCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("closing the body did not start the deadline")
	}
	if !stderrors.Is(context.Cause(reqCtx), errUploadDeadline) {
		t.Errorf("cause = %v, want errUploadDeadline", context.Cause(reqCtx))
	}
	deadline.stop()

	stopped, late := withUploadDeadline(context.Background(), time.Millisecond)
	late.stop()
	late.handed(true)
	if late.timer != nil {
		t.Error("handed after stop started a timer")
	}
	if stderrors.Is(context.Cause(stopped), errUploadDeadline) {
		t.Error("a stopped deadline ended its context with errUploadDeadline")
	}
}

func TestUploadDeadline_NoTimeoutSetsNoDeadline(t *testing.T) {
	reqCtx, deadline := withUploadDeadline(context.Background(), -1)
	defer deadline.stop()
	deadline.handed(true)
	if deadline.timer != nil {
		t.Error("a negative timeout started a timer")
	}
	if reqCtx.Err() != nil {
		t.Errorf("ctx ended: %v", reqCtx.Err())
	}
}
