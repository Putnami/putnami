package sourcecli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	"go.putnami.dev/cloud/extension/internal/clicore/hometest"
)

const testBaseURL = "https://control.test"

// bindingsFakeServer is a focused in-memory stand-in for the control plane's
// workspace source/github endpoints, plus the auth refresh endpoints the
// shared ActiveAuth path needs.
type bindingsFakeServer struct {
	requests             []capturedRequest
	pollStatus           string
	completeConflictCode string
	disconnected         bool
	// pullPages answers the open pull request read, one page per call, each
	// page pointing at the next. pullStatus refuses it when non-zero.
	pullPages  [][]map[string]any
	pullStatus int
}

type capturedRequest struct {
	Method string
	Path   string
	Auth   string
	Body   map[string]any
}

func (s *bindingsFakeServer) RoundTrip(req *http.Request) (*http.Response, error) {
	var bodyBytes []byte
	if req.Body != nil {
		bodyBytes, _ = io.ReadAll(req.Body)
		_ = req.Body.Close()
	}
	switch {
	case req.URL.Path == "/.well-known/openid-configuration":
		return jsonResponse(http.StatusOK, map[string]any{
			"issuer": testBaseURL, "token_endpoint": testBaseURL + "/token",
			"userinfo_endpoint": testBaseURL + "/userinfo",
		}), nil
	case req.URL.Path == "/token":
		var body map[string]any
		_ = json.Unmarshal(bodyBytes, &body)
		extra := map[string]any{}
		if workspaceID := clicore.StringValue(body["workspace_id"]); workspaceID != "" {
			extra["scope_ref"] = map[string]any{"workspace_id": workspaceID}
		}
		return jsonResponse(http.StatusOK, tokenResponse(extra)), nil
	case strings.HasPrefix(req.URL.Path, "/v1/workspaces/ws-acme/source/github"):
		var body map[string]any
		if len(bodyBytes) > 0 {
			_ = json.Unmarshal(bodyBytes, &body)
		}
		s.requests = append(s.requests, capturedRequest{Method: req.Method, Path: req.URL.Path, Auth: req.Header.Get("Authorization"), Body: body})
		base := "/v1/workspaces/ws-acme/source/github"
		switch {
		case req.Method == http.MethodPost && req.URL.Path == base+"/sessions":
			return jsonResponse(http.StatusCreated, map[string]any{
				"session_id":            "11111111-1111-4111-8111-111111111111",
				"authorize_url":         "https://github.test/login/oauth/authorize",
				"poll_interval_seconds": 1,
				"expires_at":            "2026-05-19T09:15:00Z",
			}), nil
		case req.Method == http.MethodGet && req.URL.Path == base+"/sessions/11111111-1111-4111-8111-111111111111":
			status := s.pollStatus
			if status == "" {
				status = "candidates_ready"
			}
			return jsonResponse(http.StatusOK, map[string]any{
				"session_id":            "11111111-1111-4111-8111-111111111111",
				"status":                status,
				"poll_interval_seconds": 1,
				"expires_at":            "2026-05-19T09:15:00Z",
				"candidates": []map[string]any{
					{"installation_id": 1001, "repo_id": 2002, "owner": "acme", "repo": "app"},
					{"installation_id": 1001, "repo_id": 3003, "owner": "acme", "repo": "api"},
				},
			}), nil
		case req.Method == http.MethodPost && req.URL.Path == base+"/sessions/11111111-1111-4111-8111-111111111111/complete":
			if s.completeConflictCode != "" && !clicore.Truthy(body["replace"]) {
				return jsonResponse(http.StatusConflict, map[string]any{"code": s.completeConflictCode, "error": "workspace already has an active github repository"}), nil
			}
			return jsonResponse(http.StatusOK, map[string]any{
				"provider": "github", "installation_id": 1001, "repo_id": 2002,
				"workspace_id": "ws-acme", "owner": "acme", "repo": "app", "active": true,
			}), nil
		case req.Method == http.MethodDelete && req.URL.Path == base+"/sessions/11111111-1111-4111-8111-111111111111":
			return noContentResponse(), nil
		case req.Method == http.MethodGet && req.URL.Path == base:
			return jsonResponse(http.StatusOK, map[string]any{
				"provider": "github", "connected": true,
				"binding": map[string]any{"provider": "github", "installation_id": 1001, "repo_id": 2002, "workspace_id": "ws-acme", "owner": "acme", "repo": "app", "active": true},
				"health":  map[string]any{"code": "connected", "healthy": true},
			}), nil
		case req.Method == http.MethodDelete && req.URL.Path == base:
			s.disconnected = true
			return noContentResponse(), nil
		case req.Method == http.MethodGet && req.URL.Path == base+"/pull-requests":
			if s.pullStatus != 0 {
				return jsonResponse(s.pullStatus, map[string]any{"code": "unavailable", "error": "github pull request reads unavailable"}), nil
			}
			page := 0
			if cursor := req.URL.Query().Get("cursor"); cursor != "" {
				page, _ = strconv.Atoi(strings.TrimPrefix(cursor, "page-"))
			}
			body := map[string]any{"pull_requests": []map[string]any{}}
			if page < len(s.pullPages) {
				body["pull_requests"] = s.pullPages[page]
			}
			if page+1 < len(s.pullPages) {
				body["next_cursor"] = "page-" + strconv.Itoa(page+1)
			}
			return jsonResponse(http.StatusOK, body), nil
		}
		return jsonResponse(http.StatusNotFound, map[string]any{"code": "not_found", "error": "not found"}), nil
	default:
		return jsonResponse(http.StatusNotFound, map[string]any{"error": "not found"}), nil
	}
}

func runSource(ioctx clicore.IO, args []string) error {
	env := ioctx.Env
	params := clicore.MergeParams(clicore.ParseFlags(args))
	workspaceRoot := clicore.EnvGet(env, "PUTNAMI_WORKSPACE_ROOT")
	if ioctx.Stdout == nil {
		ioctx.Stdout = func(string) {}
	}
	if ioctx.Stderr == nil {
		ioctx.Stderr = func(string) {}
	}
	if ioctx.Client == nil {
		ioctx.Client = http.DefaultClient
	}
	if ioctx.Now == nil {
		ioctx.Now = time.Now
	}
	return Source(params, args, workspaceRoot, env, ioctx)
}

func newSourceTestIO(t *testing.T, fake *bindingsFakeServer) clicore.IO {
	t.Helper()
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)
	return clicore.IO{
		Env: hometest.Env(home, map[string]string{
			"PUTNAMI_AUTH_URL":       testBaseURL,
			"PUTNAMI_WORKSPACE_ROOT": workspaceRoot,
		}),
		Client:      &http.Client{Transport: fake},
		Now:         func() time.Time { return time.Date(2026, 5, 19, 9, 0, 0, 0, time.UTC) },
		Context:     context.Background(),
		OpenBrowser: func(string) error { return nil },
	}
}

func TestSourceConnectHappyPathStartsPollsAndCompletes(t *testing.T) {
	fake := &bindingsFakeServer{}
	ioctx := newSourceTestIO(t, fake)
	var opened []string
	var lines []string
	ioctx.OpenBrowser = func(url string) error { opened = append(opened, url); return nil }
	ioctx.Stdout = func(line string) { lines = append(lines, line) }

	if err := runSource(ioctx, []string{"connect", "--repo", "acme/app", "--poll-interval-ms", "0"}); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if len(opened) != 1 || opened[0] != "https://github.test/login/oauth/authorize" {
		t.Fatalf("opened = %v", opened)
	}
	if got := requestMethods(fake.requests); !strings.Contains(got, "POST /v1/workspaces/ws-acme/source/github/sessions") ||
		!strings.Contains(got, "GET /v1/workspaces/ws-acme/source/github/sessions/11111111-1111-4111-8111-111111111111") ||
		!strings.Contains(got, "POST /v1/workspaces/ws-acme/source/github/sessions/11111111-1111-4111-8111-111111111111/complete") {
		t.Fatalf("requests:\n%s", got)
	}
	complete := fake.requests[len(fake.requests)-1]
	if complete.Body["installation_id"] != float64(1001) || complete.Body["repo_id"] != float64(2002) {
		t.Fatalf("complete body = %+v", complete.Body)
	}
	if !strings.Contains(strings.Join(lines, "\n"), "Connected GitHub repository acme/app") {
		t.Fatalf("output = %v", lines)
	}
}

func TestSourceConnectRepoMismatchListsAuthorizedCandidatesAndCancels(t *testing.T) {
	fake := &bindingsFakeServer{}
	ioctx := newSourceTestIO(t, fake)
	err := runSource(ioctx, []string{"connect", "--repo", "elsewhere/missing", "--poll-interval-ms", "0"})
	if err == nil || clicore.ExitCode(err) != clicore.ExitUsage {
		t.Fatalf("error = %v", err)
	}
	for _, want := range []string{"elsewhere/missing", "acme/app", "acme/api"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q missing %q", err, want)
		}
	}
	if got := requestMethods(fake.requests); !strings.Contains(got, "DELETE /v1/workspaces/ws-acme/source/github/sessions/") {
		t.Fatalf("best-effort cancel missing:\n%s", got)
	}
}

func TestSourceConnectConflictGivesTypedReplaceGuidance(t *testing.T) {
	fake := &bindingsFakeServer{completeConflictCode: "source.github.workspace_already_bound"}
	ioctx := newSourceTestIO(t, fake)
	err := runSource(ioctx, []string{"connect", "--repo", "acme/app", "--poll-interval-ms", "0"})
	if err == nil || !strings.Contains(err.Error(), "source.github.workspace_already_bound") || !strings.Contains(err.Error(), "--replace") {
		t.Fatalf("conflict error = %v", err)
	}

	fake.requests = nil
	if err := runSource(ioctx, []string{"connect", "--repo", "acme/app", "--replace", "--poll-interval-ms", "0"}); err != nil {
		t.Fatalf("connect --replace: %v", err)
	}
	var complete capturedRequest
	for _, request := range fake.requests {
		if strings.HasSuffix(request.Path, "/complete") {
			complete = request
		}
	}
	if !clicore.Truthy(complete.Body["replace"]) {
		t.Fatalf("complete body = %+v, want replace=true", complete.Body)
	}
}

func TestSourceConnectTimeoutCancelsSession(t *testing.T) {
	fake := &bindingsFakeServer{pollStatus: "pending_auth"}
	ioctx := newSourceTestIO(t, fake)
	err := runSource(ioctx, []string{"connect", "--repo", "acme/app", "--poll-timeout-ms", "0", "--poll-interval-ms", "0"})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("timeout error = %v", err)
	}
	if got := requestMethods(fake.requests); !strings.Contains(got, "DELETE /v1/workspaces/ws-acme/source/github/sessions/") {
		t.Fatalf("best-effort cancel missing:\n%s", got)
	}
}

func TestSourceConnectCanceledContextCancelsSession(t *testing.T) {
	fake := &bindingsFakeServer{pollStatus: "pending_auth"}
	ioctx := newSourceTestIO(t, fake)
	ctx, cancel := context.WithCancel(context.Background())
	ioctx.Context = ctx
	ioctx.Sleep = func(ctx context.Context, _ time.Duration) error {
		cancel()
		return ctx.Err()
	}

	err := runSource(ioctx, []string{"connect", "--repo", "acme/app"})
	if err == nil || clicore.ExitCode(err) != clicore.ExitSignal || !strings.Contains(err.Error(), "interrupted") {
		t.Fatalf("cancel error = %v", err)
	}
	if got := requestMethods(fake.requests); !strings.Contains(got, "DELETE /v1/workspaces/ws-acme/source/github/sessions/") {
		t.Fatalf("best-effort cancel missing:\n%s", got)
	}
}

func TestSourceConnectRecognizesCanonicalCancelledStatus(t *testing.T) {
	fake := &bindingsFakeServer{pollStatus: "cancelled"} //nolint:misspell // Canonical onboarding status returned by the API.
	ioctx := newSourceTestIO(t, fake)

	err := runSource(ioctx, []string{"connect", "--repo", "acme/app", "--poll-interval-ms", "0"})
	if err == nil || clicore.ExitCode(err) != clicore.ExitAPI || err.Error() != "GitHub onboarding was canceled" {
		t.Fatalf("cancelled status error = %v", err) //nolint:misspell // Match the canonical status under test.
	}
}

func TestSourceConnectStructuredOutputEnvelope(t *testing.T) {
	fake := &bindingsFakeServer{}
	ioctx := newSourceTestIO(t, fake)
	var lines []string
	ioctx.Stdout = func(line string) { lines = append(lines, line) }
	if err := runSource(ioctx, []string{"connect", "--repo", "acme/app", "--output", "json", "--poll-interval-ms", "0"}); err != nil {
		t.Fatalf("structured connect: %v", err)
	}
	var envelope struct {
		Status string         `json:"status"`
		Data   map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(strings.Join(lines, "\n")), &envelope); err != nil {
		t.Fatalf("decode envelope from %q: %v", strings.Join(lines, "\n"), err)
	}
	if envelope.Status != "success" || envelope.Data["workspace_id"] != "ws-acme" || envelope.Data["repo"] != "app" {
		t.Fatalf("envelope = %+v", envelope)
	}
}

func TestSourceConnectStructuredModeRequiresRepoBeforeStarting(t *testing.T) {
	fake := &bindingsFakeServer{}
	ioctx := newSourceTestIO(t, fake)
	err := runSource(ioctx, []string{"connect", "--output", "json"})
	if err == nil || clicore.ExitCode(err) != clicore.ExitUsage || !strings.Contains(err.Error(), "requires --repo") {
		t.Fatalf("error = %v", err)
	}
	if len(fake.requests) != 0 {
		t.Fatalf("requests = %+v, want none", fake.requests)
	}
}

func TestSourceConnectStructuredNoOpenIsRejectedWithAuthorizationGuidance(t *testing.T) {
	fake := &bindingsFakeServer{}
	ioctx := newSourceTestIO(t, fake)
	err := runSource(ioctx, []string{"connect", "--repo", "acme/app", "--output", "json", "--no-open"})
	if err == nil || clicore.ExitCode(err) != clicore.ExitUsage || !strings.Contains(err.Error(), "authorization URL") || !strings.Contains(err.Error(), "omit --no-open") {
		t.Fatalf("error = %v", err)
	}
	if len(fake.requests) != 0 {
		t.Fatalf("requests = %+v, want none", fake.requests)
	}
}

func TestWaitSourcePollUsesInjectedCancelableSleep(t *testing.T) {
	called := false
	err := waitSourcePoll(context.Background(), 2*time.Second, func(ctx context.Context, interval time.Duration) error {
		called = true
		if ctx == nil || interval != 2*time.Second {
			t.Fatalf("sleep = (%v, %s)", ctx, interval)
		}
		return nil
	})
	if err != nil || !called {
		t.Fatalf("waitSourcePoll = %v, called=%v", err, called)
	}
}

func TestSourceConnectProposesLocalOriginAndSupportsInteractiveSelection(t *testing.T) {
	original := sourceOriginRemote
	t.Cleanup(func() { sourceOriginRemote = original })
	sourceOriginRemote = func(string) (string, error) { return "git@github.com:acme/app.git", nil }

	t.Run("origin accepted", func(t *testing.T) {
		fake := &bindingsFakeServer{}
		ioctx := newSourceTestIO(t, fake)
		ioctx.Confirm = func(question string) (string, bool) {
			if !strings.Contains(question, "acme/app") {
				t.Fatalf("question = %q", question)
			}
			return "", true
		}
		if err := runSource(ioctx, []string{"connect", "--poll-interval-ms", "0"}); err != nil {
			t.Fatalf("connect: %v", err)
		}
	})

	t.Run("numbered selection", func(t *testing.T) {
		fake := &bindingsFakeServer{}
		ioctx := newSourceTestIO(t, fake)
		ioctx.Confirm = func(string) (string, bool) { return "n", true }
		ioctx.Prompt = func(question string) (string, bool) {
			if !strings.Contains(question, "[1-2]") {
				t.Fatalf("question = %q", question)
			}
			return "2", true
		}
		if err := runSource(ioctx, []string{"connect", "--poll-interval-ms", "0"}); err != nil {
			t.Fatalf("connect: %v", err)
		}
		complete := fake.requests[len(fake.requests)-1]
		if complete.Body["repo_id"] != float64(3003) {
			t.Fatalf("complete = %+v", complete.Body)
		}
	})
}

func TestSourceStatusAndDisconnect(t *testing.T) {
	fake := &bindingsFakeServer{}
	ioctx := newSourceTestIO(t, fake)
	var lines []string
	ioctx.Stdout = func(line string) { lines = append(lines, line) }
	if err := runSource(ioctx, []string{"status"}); err != nil {
		t.Fatalf("status: %v", err)
	}
	if out := strings.Join(lines, "\n"); !strings.Contains(out, "source  ok  acme/app, 0 open pull requests") ||
		!strings.Contains(out, "connection   acme/app, installation 1001") {
		t.Fatalf("status output:\n%s", out)
	}
	ioctx.Confirm = func(string) (string, bool) { return "yes", true }
	if err := runSource(ioctx, []string{"disconnect"}); err != nil {
		t.Fatalf("disconnect: %v", err)
	}
	if !fake.disconnected {
		t.Fatal("disconnect endpoint was not called")
	}
	if err := runSource(ioctx, []string{"disconnect", "--yes"}); err != nil {
		t.Fatalf("idempotent disconnect: %v", err)
	}
}

func requestMethods(requests []capturedRequest) string {
	lines := make([]string, 0, len(requests))
	for _, request := range requests {
		lines = append(lines, request.Method+" "+request.Path)
	}
	return strings.Join(lines, "\n")
}

func TestSourceHelpListsCommands(t *testing.T) {
	var lines []string
	ioctx := clicore.IO{
		Env:    map[string]string{},
		Stdout: func(s string) { lines = append(lines, s) },
		Now:    time.Now,
		Client: http.DefaultClient,
	}
	if err := Source(map[string]any{}, nil, "", ioctx.Env, ioctx); err != nil {
		t.Fatalf("help: %v", err)
	}
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"source connect", "source status", "source disconnect"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("help output missing %q:\n%s", want, joined)
		}
	}
	for _, moved := range []string{"break-glass", "source bind", "source unbind"} {
		if strings.Contains(joined, moved) {
			t.Fatalf("public help lists the operator command %q:\n%s", moved, joined)
		}
	}
}

func TestSourceRefusesOperatorBinding(t *testing.T) {
	for _, verb := range []string{"bind", "unbind"} {
		ioctx := clicore.IO{Env: map[string]string{}, Stdout: func(string) {}, Stderr: func(string) {}, Now: time.Now, Client: http.DefaultClient}
		err := Source(map[string]any{}, []string{verb, "--installation", "1001", "--repo", "2002"}, "", ioctx.Env, ioctx)
		if err == nil || clicore.ExitCode(err) != clicore.ExitUsage {
			t.Fatalf("Source(%s) err = %v, want a usage error", verb, err)
		}
	}
}

func writeTestAuth(t *testing.T, home string) {
	t.Helper()
	file := filepath.Join(home, clicore.AuthFileRelative)
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeJSONFile(t, file, map[string]any{
		"access_token":  tokenResponse(nil)["access_token"],
		"refresh_token": "refresh-token",
		"token_type":    "Bearer",
		"expires_at":    "2026-04-30T13:00:00.000Z",
		"issuer":        testBaseURL,
		"client_id":     "putnami-cli",
	})
}

func writeLinkFile(t *testing.T, workspaceRoot string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(workspaceRoot, ".putnami"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeJSONFile(t, filepath.Join(workspaceRoot, clicore.LinkFileRelative), map[string]any{
		"version":           1,
		"control_plane_url": testBaseURL,
		"workspace_id":      "ws-acme",
		"environment":       "prod",
	})
}

func tokenResponse(extra map[string]any) map[string]any {
	claims := map[string]any{
		"sub":   "user-1",
		"email": "dev@example.com",
		"scope": "openid profile email",
	}
	for k, v := range extra {
		claims[k] = v
	}
	return map[string]any{
		"access_token":  jwt(claims),
		"refresh_token": "refresh-token",
		"token_type":    "Bearer",
		"expires_in":    300,
	}
}

func jwt(payload map[string]any) string {
	return base64url(map[string]any{"alg": "none", "typ": "JWT"}) + "." + base64url(payload) + ".sig"
}

func base64url(value map[string]any) string {
	data, _ := json.Marshal(value)
	return base64.RawURLEncoding.EncodeToString(data)
}

// noContentResponse is a 204 exactly as the provider writes it: no body.
func noContentResponse() *http.Response {
	return &http.Response{
		StatusCode: http.StatusNoContent,
		Status:     http.StatusText(http.StatusNoContent),
		Header:     http.Header{},
		Body:       io.NopCloser(bytes.NewReader(nil)),
	}
}

func jsonResponse(status int, body any) *http.Response {
	data, _ := json.Marshal(body)
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(data)),
	}
}

func writeJSONFile(t *testing.T, file string, data any) {
	t.Helper()
	encoded, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("encode json: %v", err)
	}
	if err := os.WriteFile(file, encoded, 0o644); err != nil {
		t.Fatalf("write %s: %v", file, err)
	}
}
