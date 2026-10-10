package runtimecli

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// fakeClock is a deterministic, monotonically-advancing clock the wait-loop
// tests thread through ioctx.Now(). Each call advances by step so a poll loop
// eventually crosses its deadline without a real time.Sleep.
type fakeClock struct {
	mu   sync.Mutex
	now  time.Time
	step time.Duration
}

func newFakeClock(step time.Duration) *fakeClock {
	return &fakeClock{now: time.Unix(0, 0).UTC(), step: step}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.now
	c.now = c.now.Add(c.step)
	return t
}

// newWaitCtx builds a deployCtx wired to srv with a fake clock so the
// publish-v2 --wait loop can be driven without real workspace/auth resolution.
func newWaitCtx(srv *httptest.Server, cap *captureIO, clock *fakeClock) *deployCtx {
	io := cap.io(srv.Client())
	io.Now = clock.Now
	return &deployCtx{
		WorkspaceContext: &clicore.WorkspaceContext{
			WorkspaceID:  "ws-acme",
			ControlPlane: srv.URL,
			AuthToken:    clicore.NewBearer("token"),
			IO:           io,
			RefreshAuth:  func() (clicore.Bearer, error) { return clicore.NewBearer("fresh"), nil },
		},
		environment: "prod",
	}
}

// waitServer answers the publish-v2 acceptance, then hands every deploy POST
// to deploy with its 1-based call number.
func waitServer(t *testing.T, deploy func(w http.ResponseWriter, call int)) (*httptest.Server, func() int) {
	t.Helper()
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/environment-definitions/accept") {
			_, _ = w.Write([]byte(`{"workspace_id":"ws-acme","revision":4,"digest":"sha256:` + strings.Repeat("e", 64) +
				`","source_revision":"` + strings.Repeat("b", 40) + `","accepted_at":"2026-09-07T06:30:00Z"}`))
			return
		}
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		deploy(w, n)
	}))
	return srv, func() int {
		mu.Lock()
		defer mu.Unlock()
		return calls
	}
}

// provisioningThenReady answers 202 Provisioning to the submit and Ready to
// every later call.
func provisioningThenReady(w http.ResponseWriter, call int) {
	if call == 1 {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"release_id":"rel_x","state":"Provisioning","projects":[
			{"name":"accounts/workloads/auth-server","status":"provisioning"}]}`))
		return
	}
	_, _ = w.Write([]byte(`{"release_id":"rel_x","state":"Ready","projects":[
		{"name":"accounts/workloads/auth-server","status":"ready","action":"roll","url":"https://a.example","revision":"rev-2"}]}`))
}

// TestDeployPublishV2_WaitEmitsProgress pins that the --wait loop streams
// phase transitions to stderr: a Provisioning line up front, then a Ready
// line once the poll converges.
func TestDeployPublishV2_WaitEmitsProgress(t *testing.T) {
	old := DeployPollInterval
	DeployPollInterval = time.Millisecond
	defer func() { DeployPollInterval = old }()

	srv, _ := waitServer(t, provisioningThenReady)
	defer srv.Close()

	cap := &captureIO{}
	ctx := newWaitCtx(srv, cap, newFakeClock(time.Second))
	if err := deployPublishV2(map[string]any{"wait": true}, ctx, publishV2Fixture(), ctx.IO); err != nil {
		t.Fatalf("publish-v2 --wait: %v", err)
	}
	stderr := strings.ToLower(strings.Join(cap.stderr, "\n"))
	if !strings.Contains(stderr, "provisioning") || !strings.Contains(stderr, "ready") {
		t.Errorf("stderr misses a provisioning or a ready progress line:\n%s", stderr)
	}
}

// TestDeployPublishV2_WaitProgressSilentUnderStructured pins that progress
// stays out of --output=jsonl: stdout is the single terminal object.
func TestDeployPublishV2_WaitProgressSilentUnderStructured(t *testing.T) {
	old := DeployPollInterval
	DeployPollInterval = time.Millisecond
	defer func() { DeployPollInterval = old }()

	srv, _ := waitServer(t, func(w http.ResponseWriter, _ int) { provisioningThenReady(w, 2) })
	defer srv.Close()

	cap := &captureIO{}
	ctx := newWaitCtx(srv, cap, newFakeClock(time.Second))
	if err := deployPublishV2(map[string]any{"wait": true, "output": "jsonl"}, ctx, publishV2Fixture(), ctx.IO); err != nil {
		t.Fatalf("publish-v2 --wait --output=jsonl: %v", err)
	}
	if len(cap.stderr) != 0 {
		t.Fatalf("structured mode must not emit progress; stderr=%v", cap.stderr)
	}
	if joined := strings.Join(cap.stdout, "\n"); strings.Contains(joined, "deploy: ") {
		t.Fatalf("progress leaked into stdout under structured mode:\n%s", joined)
	}
}

// TestDeployPublishV2_WaitTimeoutStillProvisioningExitsNonZero pins that a
// --wait which never converges within its budget exits non-zero after
// rendering the current state, and does not advise a re-run with --wait: a
// fresh invocation mints a new release id and starts a second rollout.
func TestDeployPublishV2_WaitTimeoutStillProvisioningExitsNonZero(t *testing.T) {
	old := DeployPollInterval
	DeployPollInterval = time.Millisecond
	defer func() { DeployPollInterval = old }()

	srv, _ := waitServer(t, func(w http.ResponseWriter, _ int) { provisioningThenReady(w, 1) })
	defer srv.Close()

	cap := &captureIO{}
	ctx := newWaitCtx(srv, cap, newFakeClock(2*time.Minute))
	err := deployPublishV2(map[string]any{"wait": true, "timeout": "3m"}, ctx, publishV2Fixture(), ctx.IO)
	if err == nil {
		t.Fatal("a timed-out --wait still Provisioning must exit non-zero")
	}
	if clicore.ExitCode(err) != clicore.ExitAPI || !strings.Contains(err.Error(), "did not reach Ready") {
		t.Fatalf("error = %v (exit %d), want the not-Ready timeout verdict with ExitAPI", err, clicore.ExitCode(err))
	}
	rendered := strings.Join(cap.stdout, "\n")
	if !strings.Contains(strings.ToLower(rendered), "still provisioning") {
		t.Fatalf("release result not rendered on timeout:\n%s", rendered)
	}
	if strings.Contains(rendered, "--wait") {
		t.Fatalf("timed-out --wait render must not advise re-running with --wait:\n%s", rendered)
	}
}

// TestDeployPublishV2_WaitPollBudgetResetAfterSubmit pins that the readiness
// poll gets a full --timeout after the submit returns, so a slow submit does
// not turn --wait into a no-wait.
func TestDeployPublishV2_WaitPollBudgetResetAfterSubmit(t *testing.T) {
	old := DeployPollInterval
	DeployPollInterval = time.Millisecond
	defer func() { DeployPollInterval = old }()

	srv, calls := waitServer(t, provisioningThenReady)
	defer srv.Close()

	cap := &captureIO{}
	// Each Now() advances 90s. A deadline shared with the submit would pass
	// before the first poll; the reset gives the poll a fresh 2m.
	ctx := newWaitCtx(srv, cap, newFakeClock(90*time.Second))
	if err := deployPublishV2(map[string]any{"wait": true, "timeout": "2m"}, ctx, publishV2Fixture(), ctx.IO); err != nil {
		t.Fatalf("publish-v2 --wait after submit budget reset: %v", err)
	}
	if got := calls(); got < 2 {
		t.Fatalf("poll never ran (calls=%d): the readiness budget was not reset after submit", got)
	}
	if !strings.Contains(strings.Join(cap.stdout, "\n"), "rev-2") {
		t.Fatalf("expected the converged Ready render, got:\n%s", strings.Join(cap.stdout, "\n"))
	}
}
