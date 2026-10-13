package deliverycli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	"go.putnami.dev/cloud/extension/internal/clicore/hometest"
	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
)

const (
	localWorkspaceID   = "6f1d2c3b-4a5e-4f60-8a7b-9c0d1e2f3a4b"
	localSessionID     = "20261013-110000-abc123"
	localRefreshToken  = "stored-refresh-token-9f2c41"
	localMachineToken  = "pkt_machine-token-withheld-7e1b"
	localRouteAccepted = "local-route-accepted"
)

// localNow is the clock of a stored login: its cached workspace token expires
// ten minutes later, and the base token an hour later.
var localNow = time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)

// localRequest is one request the local-sessions route received.
type localRequest struct {
	path        string
	auth        string
	contentType string
	session     string
	status      int
}

// localControlPlane stands in for the control plane and the auth server a
// developer's stored login talks to: OpenID discovery, the token endpoint a
// login refreshes against, and the local-sessions route of the linked
// workspace. The route accepts only the bearers in valid and answers every
// other request with the run-session receiver contract (fakeIngest).
type localControlPlane struct {
	t      *testing.T
	ingest *fakeIngest

	mu       sync.Mutex
	url      string
	valid    map[string]bool
	minted   []string
	tokenReq []map[string]any
	requests []localRequest
	// refreshFails makes the token endpoint refuse every refresh with
	// invalid_grant.
	refreshFails bool
	// revokeAfterFirst makes the route revoke the bearer of the first frame it
	// accepts and moves the clock past its expiry: the session outlived it.
	revokeAfterFirst bool
	expired          bool
	discovery        int
}

func newLocalControlPlane(t *testing.T) *localControlPlane {
	t.Helper()
	cp := &localControlPlane{t: t, ingest: &fakeIngest{token: localRouteAccepted}, valid: map[string]bool{localCachedToken: true}}
	srv := httptest.NewServer(cp)
	t.Cleanup(srv.Close)
	cp.url = srv.URL
	return cp
}

// now is the clock the stored login reads.
func (c *localControlPlane) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.expired {
		return localNow.Add(time.Hour)
	}
	return localNow
}

func (c *localControlPlane) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch r.URL.Path {
	case "/.well-known/openid-configuration":
		c.discovery++
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": c.url, "token_endpoint": c.url + "/token"})
		return
	case "/token":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		c.tokenReq = append(c.tokenReq, body)
		if c.refreshFails {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "invalid_grant", "error_description": "refresh token revoked"})
			return
		}
		minted := reportJWT(map[string]any{"sub": "dev-1", "jti": fmt.Sprintf("minted-%d", len(c.minted)+1), "scope_ref": map[string]any{"workspace_id": body["workspace_id"]}})
		c.minted = append(c.minted, minted)
		c.valid[minted] = true
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": minted, "refresh_token": localRefreshToken, "token_type": "Bearer", "expires_in": 300})
		return
	}
	prefix := "/v1/workspaces/" + localWorkspaceID + localSessionsRoute
	if r.Method != http.MethodPost || !strings.HasPrefix(r.URL.EscapedPath(), prefix) {
		c.t.Errorf("unexpected request %s %s", r.Method, r.URL.EscapedPath())
		w.WriteHeader(http.StatusNotFound)
		return
	}
	got := localRequest{
		path: r.URL.EscapedPath(), auth: r.Header.Get("Authorization"), contentType: r.Header.Get("Content-Type"),
		session: strings.TrimPrefix(r.URL.EscapedPath(), prefix),
	}
	bearer := strings.TrimPrefix(got.auth, "Bearer ")
	if !c.valid[bearer] {
		got.status = http.StatusUnauthorized
		c.requests = append(c.requests, got)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	// The route's answers are the run-session ingest's for the same frame.
	r.URL.Path = "/ingest/sessions"
	r.Header.Set("Authorization", "Bearer "+localRouteAccepted)
	recorder := httptest.NewRecorder()
	c.ingest.ServeHTTP(recorder, r)
	got.status = recorder.Code
	c.requests = append(c.requests, got)
	if got.status == http.StatusOK && c.revokeAfterFirst && !c.expired {
		delete(c.valid, bearer)
		c.expired = true
	}
	for key, values := range recorder.Header() {
		w.Header()[key] = values
	}
	w.WriteHeader(recorder.Code)
	_, _ = w.Write(recorder.Body.Bytes())
}

func (c *localControlPlane) snapshot() (requests []localRequest, minted []string, tokenRequests int, discovery int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]localRequest(nil), c.requests...), append([]string(nil), c.minted...), len(c.tokenReq), c.discovery
}

// received returns the session headers of every frame the route accepted for
// the receiver, and the stored chunks of part.
func (c *localControlPlane) received(part string) ([]string, []storedChunk) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.ingest.posts...), append([]storedChunk(nil), c.ingest.parts[part]...)
}

// grant returns the i-th body the token endpoint received.
func (c *localControlPlane) grant(i int) map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tokenReq[i]
}

// localCachedToken is the workspace-scoped token a fresh stored login caches
// for the linked workspace.
var localCachedToken = reportJWT(map[string]any{"sub": "dev-1", "jti": "cached", "scope_ref": map[string]any{"workspace_id": localWorkspaceID}})

// localBaseToken is the stored login's base access token, scoped to no
// workspace.
var localBaseToken = reportJWT(map[string]any{"sub": "dev-1", "jti": "base"})

// writeLocalLogin writes the stored login of `putnami cloud login` under
// home. A fresh login holds a workspace token cached for the linked workspace;
// an expired one holds only a base token past its expiry and a refresh token.
func writeLocalLogin(t *testing.T, home, issuer string, fresh bool) {
	t.Helper()
	login := map[string]any{
		"access_token":  localBaseToken,
		"refresh_token": localRefreshToken,
		"token_type":    "Bearer",
		"expires_at":    localNow.Add(-time.Hour).Format(time.RFC3339Nano),
		"issuer":        issuer,
		"client_id":     "putnami-cli",
	}
	if fresh {
		login["expires_at"] = localNow.Add(time.Hour).Format(time.RFC3339Nano)
		login["workspace_access"] = map[string]any{localWorkspaceID: map[string]any{
			"access_token": localCachedToken, "token_type": "Bearer",
			"expires_at": localNow.Add(10 * time.Minute).Format(time.RFC3339Nano),
		}}
	}
	file := filepath.Join(home, clicore.AuthFileRelative)
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	writeReportJSONFile(t, file, login)
}

// writeLocalLink links workspaceRoot to the workspace on controlPlane, as
// `putnami cloud setup` does.
func writeLocalLink(t *testing.T, workspaceRoot, controlPlane string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(workspaceRoot, ".putnami"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeReportJSONFile(t, filepath.Join(workspaceRoot, clicore.LinkFileRelative), map[string]any{
		"version": 1, "control_plane_url": controlPlane, "workspace_id": localWorkspaceID,
	})
}

// localReporter is one developer machine: a home that may hold a stored
// login, a workspace that may be linked, and the reporter's environment.
type localReporter struct {
	cp   *localControlPlane
	home string
	root string
	env  map[string]string
	// resolves counts the reads of the stored login.
	resolves int
}

func newLocalReporter(t *testing.T, login string, linked bool) *localReporter {
	t.Helper()
	cp := newLocalControlPlane(t)
	r := &localReporter{cp: cp, home: t.TempDir(), root: t.TempDir()}
	switch login {
	case "fresh":
		writeLocalLogin(t, r.home, cp.url, true)
	case "expired":
		writeLocalLogin(t, r.home, cp.url, false)
	}
	if linked {
		writeLocalLink(t, r.root, cp.url)
	}
	r.env = hometest.Env(r.home, map[string]string{"NO_COLOR": "1"})
	return r
}

// serve runs the session reporter's real entrypoint (the guard, the
// configuration, the loop) with the real stored-login resolver on lines.
func (r *localReporter) serve(t *testing.T, lines ...string) reporterSession {
	t.Helper()
	var out, stderr bytes.Buffer
	ioctx := clicore.IO{Client: &http.Client{Timeout: 5 * time.Second}, Now: r.cp.now}
	resolve := storedLoginSession(map[string]any{}, r.root, r.env, ioctx, &stderr)
	counted := func() (*clicore.WorkspaceContext, error) {
		r.resolves++
		return resolve()
	}
	err := serveSessionReporter(r.env, counted, strings.NewReader(strings.Join(lines, "")), &out, &stderr)
	session := reporterSession{stderr: stderr.String(), err: err}
	if trimmed := bytes.TrimSpace(out.Bytes()); len(trimmed) > 0 {
		session.lines = bytes.Split(trimmed, []byte("\n"))
	}
	// No line the reporter writes, on either stream, carries a credential of
	// the stored login, one the login minted, or the withheld machine token.
	_, minted, _, _ := r.cp.snapshot()
	secrets := append([]string{localCachedToken, localBaseToken, localRefreshToken, localMachineToken}, minted...)
	for _, secret := range secrets {
		if strings.Contains(out.String(), secret) || strings.Contains(session.stderr, secret) || (err != nil && strings.Contains(err.Error(), secret)) {
			t.Fatalf("the reporter wrote a credential: stdout %q stderr %q err %v", out.String(), session.stderr, err)
		}
	}
	return session
}

func localFrame(t *testing.T, artifact string, offset, seq int64, data string, final bool) string {
	return frame(t, localSessionID, artifact, offset, seq, data, final)
}

func requireLocalAcks(t *testing.T, session reporterSession, codes ...string) {
	t.Helper()
	requireLines(t, session, len(codes))
	for i, code := range codes {
		ack, err := protocolcli.ParseSessionReportingAck(session.lines[i])
		if err != nil {
			t.Fatalf("ack %d %q: %v", i, session.lines[i], err)
		}
		if ack.OK != (code == "") || ack.Code != code || ack.Retryable {
			t.Fatalf("ack %d = %+v, want code %q and no retry", i, ack, code)
		}
	}
}

// Without a run credential or an ingest base, every frame goes to the linked
// workspace's local-sessions route, path-escaped session id last, with the
// run-session body and headers, under the workspace token of the stored login.
// The machine token in the environment is withheld, and a cached token that is
// still fresh is used without a refresh.
func TestSessionReporterLocalModePostsUnderTheStoredLogin(t *testing.T) {
	spectest.Proves(t, "cloud/local-session-reporting", "local-session-under-developer-login", "local-session-route-and-bearer")
	stubReporterDenyInspection(t, func() error { return nil })
	r := newLocalReporter(t, "fresh", true)
	r.env[cloudTokenEnv] = localMachineToken

	session := r.serve(t,
		localFrame(t, "events.jsonl", 0, 0, `{"record":"task:start"}`+"\n", false),
		localFrame(t, "events.jsonl", 24, 1, `{"record":"task:end"}`+"\n", false),
		localFrame(t, "session.json", 0, 0, `{"run":{"outcome":"success"}}`, false),
		localFrame(t, "session.json", 29, 1, "", true),
		localFrame(t, "events.jsonl", 46, 2, "", true),
	)
	requireLocalAcks(t, session, "", "", "", "", "")

	requests, minted, tokenRequests, _ := r.cp.snapshot()
	if len(requests) != 5 {
		t.Fatalf("local-sessions requests = %d, want one per frame: %+v", len(requests), requests)
	}
	for _, got := range requests {
		if got.path != "/v1/workspaces/"+localWorkspaceID+"/ci/local-sessions/"+localSessionID {
			t.Fatalf("path = %q", got.path)
		}
		if got.auth != "Bearer "+localCachedToken {
			t.Fatalf("authorization is not the stored login's workspace token")
		}
		if got.contentType != "application/octet-stream" || got.status != http.StatusOK {
			t.Fatalf("request = %+v", got)
		}
	}
	want := []string{"events/0/false", "events/1/false", "session/0/false", "session/1/true", "events/2/true"}
	headers, events := r.cp.received("events")
	if strings.Join(headers, ",") != strings.Join(want, ",") {
		t.Fatalf("session headers = %v, want %v", headers, want)
	}
	if got := string(events[0].body) + string(events[1].body); got != `{"record":"task:start"}`+"\n"+`{"record":"task:end"}`+"\n" {
		t.Fatalf("stored events bytes = %q", got)
	}
	if tokenRequests != 0 || len(minted) != 0 {
		t.Fatalf("a fresh cached token needs no refresh: %d token requests", tokenRequests)
	}
	if r.resolves != 1 {
		t.Fatalf("stored login read %d times, want once per process", r.resolves)
	}
}

// A stored login whose tokens expired is refreshed for the linked workspace
// the way every workspace command refreshes it: one refresh-token grant naming
// the workspace, whose token is cached in the stored login and carried by the
// frames.
func TestSessionReporterLocalModeRefreshesAnExpiredLogin(t *testing.T) {
	stubReporterDenyInspection(t, func() error { return nil })
	r := newLocalReporter(t, "expired", true)

	session := r.serve(t, localFrame(t, "events.jsonl", 0, 0, "line\n", false), localFrame(t, "events.jsonl", 5, 1, "", true))
	requireLocalAcks(t, session, "", "")

	requests, minted, tokenRequests, _ := r.cp.snapshot()
	if tokenRequests != 1 || len(minted) != 1 {
		t.Fatalf("token requests = %d, want one refresh", tokenRequests)
	}
	grant := r.cp.grant(0)
	if grant["grant_type"] != "refresh_token" || grant["refresh_token"] != localRefreshToken || grant["workspace_id"] != localWorkspaceID {
		t.Fatalf("refresh grant = %v", grant)
	}
	for _, got := range requests {
		if got.auth != "Bearer "+minted[0] {
			t.Fatal("frames do not carry the refreshed workspace token")
		}
	}
	stored, err := clicore.ReadAuth(r.env, true)
	if err != nil {
		t.Fatal(err)
	}
	if stored.WorkspaceAccess[localWorkspaceID].AccessToken != minted[0] {
		t.Fatal("the refreshed workspace token is not cached in the stored login")
	}
}

// A session that outlives its workspace token gets a 401, re-mints the bearer
// once as clicore.CallWithSession does, and sends the same frame again.
func TestSessionReporterLocalModeReMintsOnceWhenTheSessionOutlivesItsToken(t *testing.T) {
	stubReporterDenyInspection(t, func() error { return nil })
	r := newLocalReporter(t, "fresh", true)
	r.cp.revokeAfterFirst = true

	session := r.serve(t, localFrame(t, "events.jsonl", 0, 0, "a\n", false), localFrame(t, "events.jsonl", 2, 1, "b\n", false))
	requireLocalAcks(t, session, "", "")

	requests, minted, tokenRequests, _ := r.cp.snapshot()
	if tokenRequests != 1 || len(minted) != 1 {
		t.Fatalf("token requests = %d, want one re-mint", tokenRequests)
	}
	statuses := make([]int, 0, len(requests))
	for _, got := range requests {
		statuses = append(statuses, got.status)
	}
	if fmt.Sprint(statuses) != fmt.Sprint([]int{http.StatusOK, http.StatusUnauthorized, http.StatusOK}) {
		t.Fatalf("statuses = %v, want the second frame refused once, then accepted", statuses)
	}
	if requests[2].auth != "Bearer "+minted[0] {
		t.Fatal("the resent frame does not carry the re-minted token")
	}
	if _, got := r.cp.received("events"); len(got) != 2 || string(got[1].body) != "b\n" {
		t.Fatalf("stored = %+v, want both frames stored once", got)
	}
}

// Not logged in (with or without a machine token), no linked workspace, or a
// refresh the auth server refuses: every frame gets a terminal
// reporter_not_configured NACK and nothing reaches the local-sessions route.
// The stored login is read once per process, so a refused refresh is not
// retried on every frame. The diagnostic names the fix.
func TestSessionReporterLocalModeWithoutALoginRefusesEveryFrame(t *testing.T) {
	spectest.Proves(t, "cloud/local-session-reporting", "local-session-fails-closed", "local-session-not-configured")
	cases := []struct {
		name          string
		login         string
		linked        bool
		machineToken  bool
		refreshFails  bool
		tokenRequests int
		diagnostic    string
	}{
		{name: "not logged in", linked: true, diagnostic: "putnami cloud login"},
		{name: "machine token only", linked: true, machineToken: true, diagnostic: "putnami cloud login"},
		{name: "no linked workspace", login: "fresh", diagnostic: "putnami cloud setup"},
		{name: "refresh refused", login: "expired", linked: true, refreshFails: true, tokenRequests: 1, diagnostic: "putnami cloud login"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stubReporterDenyInspection(t, func() error { return nil })
			r := newLocalReporter(t, tc.login, tc.linked)
			r.cp.refreshFails = tc.refreshFails
			if tc.machineToken {
				r.env[cloudTokenEnv] = localMachineToken
			}
			session := r.serve(t,
				localFrame(t, "events.jsonl", 0, 0, "line\n", false),
				localFrame(t, "session.json", 0, 0, "{}", false),
			)
			requireLocalAcks(t, session, ackCodeNotConfigured, ackCodeNotConfigured)
			requests, _, tokenRequests, _ := r.cp.snapshot()
			if len(requests) != 0 {
				t.Fatalf("an unconfigured local mode must not post: %+v", requests)
			}
			if tokenRequests != tc.tokenRequests || r.resolves != 1 {
				t.Fatalf("token requests = %d, stored-login reads = %d; want %d and one", tokenRequests, r.resolves, tc.tokenRequests)
			}
			if !strings.Contains(session.stderr, "local mode not configured") || !strings.Contains(session.stderr, tc.diagnostic) {
				t.Fatalf("stderr = %q, want the reason and %q", session.stderr, tc.diagnostic)
			}
		})
	}
}

// A re-mint the auth server refuses ends the local mode for the process: the
// frame that met the 401 and every later frame get reporter_not_configured,
// and no later frame reaches the route.
func TestSessionReporterLocalModeAFailedReMintRefusesTheRestOfTheSession(t *testing.T) {
	stubReporterDenyInspection(t, func() error { return nil })
	r := newLocalReporter(t, "fresh", true)
	r.cp.revokeAfterFirst = true
	r.cp.refreshFails = true

	session := r.serve(t,
		localFrame(t, "events.jsonl", 0, 0, "a\n", false),
		localFrame(t, "events.jsonl", 2, 1, "b\n", false),
		localFrame(t, "events.jsonl", 4, 2, "", true),
	)
	requireLocalAcks(t, session, "", ackCodeNotConfigured, ackCodeNotConfigured)
	requests, _, tokenRequests, _ := r.cp.snapshot()
	if len(requests) != 2 || tokenRequests != 1 {
		t.Fatalf("requests = %+v, token requests = %d; want the accepted frame, its 401, and one refused re-mint", requests, tokenRequests)
	}
}

// A run credential, from the handshake or the environment, or an ingest base
// the launcher exported, makes the reporter hosted. A hosted reporter posts to
// the ingest under its run credential, or refuses as reporter_not_configured
// when one of the two is missing, and never reads the stored login, even when
// a login and a linked workspace would make the local mode work.
func TestSessionReporterHostedCredentialWinsOverTheStoredLogin(t *testing.T) {
	spectest.Proves(t, "cloud/local-session-reporting", "hosted-credential-wins", "hosted-credential-precedence")
	cases := []struct {
		name      string
		envToken  string
		ingestURL bool
		lines     func(t *testing.T) []string
		code      string
	}{
		{name: "environment token and ingest base", envToken: handshakeEnvToken, ingestURL: true, lines: func(t *testing.T) []string { return []string{eventsFrame(t)} }},
		{name: "handshake credential and ingest base", ingestURL: true, lines: func(t *testing.T) []string {
			return []string{reporterInitializeLine(t), reporterAuthenticateLine(t, handshakeCredential), eventsFrame(t)}
		}},
		{name: "ingest base without a credential", ingestURL: true, code: ackCodeNotConfigured, lines: func(t *testing.T) []string { return []string{eventsFrame(t)} }},
		{name: "environment token without an ingest base", envToken: handshakeEnvToken, code: ackCodeNotConfigured, lines: func(t *testing.T) []string { return []string{eventsFrame(t)} }},
		{name: "handshake opened without a credential", code: ackCodeNotConfigured, lines: func(t *testing.T) []string {
			return []string{reporterInitializeLine(t), eventsFrame(t)}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stubReporterDenyInspection(t, func() error { return nil })
			r := newLocalReporter(t, "fresh", true)
			hostedBearer := handshakeEnvToken
			if tc.envToken == "" {
				hostedBearer = handshakeCredential
			}
			ingest := &fakeIngest{token: hostedBearer}
			srv := httptest.NewServer(ingest)
			defer srv.Close()
			if tc.ingestURL {
				r.env[SessionReporterIngestURLEnv] = srv.URL + "/ingest"
			}
			if tc.envToken != "" {
				r.env[protocolcli.SessionReporterTokenEnv] = tc.envToken
			}
			session := r.serve(t, tc.lines(t)...)
			if session.err != nil {
				t.Fatal(session.err)
			}
			requireAck(t, session.lines[len(session.lines)-1], tc.code)
			wantIngest := 0
			if tc.code == "" {
				wantIngest = 1
			}
			requests, _, tokenRequests, discovery := r.cp.snapshot()
			if len(ingest.posts) != wantIngest || len(requests) != 0 || tokenRequests != 0 || discovery != 0 || r.resolves != 0 {
				t.Fatalf("ingest posts %d (want %d), local requests %d, token requests %d, discovery %d, stored-login reads %d; a hosted reporter never uses the stored login",
					len(ingest.posts), wantIngest, len(requests), tokenRequests, discovery, r.resolves)
			}
		})
	}
}

// The guard runs before the stored login is read: a process that cannot deny
// inspection answers every frame inspection_guard_unavailable and reads no
// login, refreshes nothing and posts nothing.
func TestSessionReporterLocalModeReadsTheStoredLoginOnlyAfterDenyingInspection(t *testing.T) {
	t.Run("guard refused", func(t *testing.T) {
		stubReporterDenyInspection(t, func() error { return errors.New("prctl refused") })
		r := newLocalReporter(t, "expired", true)
		session := r.serve(t, localFrame(t, "events.jsonl", 0, 0, "line\n", false))
		requireLocalAcks(t, session, ackCodeInspectionGuard)
		requests, _, tokenRequests, discovery := r.cp.snapshot()
		if r.resolves != 0 || len(requests) != 0 || tokenRequests != 0 || discovery != 0 {
			t.Fatalf("a process that could not deny inspection used the stored login: reads %d, requests %d, token %d, discovery %d", r.resolves, len(requests), tokenRequests, discovery)
		}
	})
	t.Run("guard first", func(t *testing.T) {
		denied := false
		stubReporterDenyInspection(t, func() error { denied = true; return nil })
		r := newLocalReporter(t, "fresh", true)
		var out bytes.Buffer
		resolve := storedLoginSession(map[string]any{}, r.root, r.env, clicore.IO{Client: &http.Client{}, Now: r.cp.now}, io.Discard)
		guarded := func() (*clicore.WorkspaceContext, error) {
			if !denied {
				t.Fatal("the stored login was read before the process denied inspection")
			}
			return resolve()
		}
		if err := serveSessionReporter(r.env, guarded, strings.NewReader(localFrame(t, "events.jsonl", 0, 0, "line\n", false)), &out, io.Discard); err != nil {
			t.Fatal(err)
		}
		requireLocalAcks(t, reporterSession{lines: bytes.Split(bytes.TrimSpace(out.Bytes()), []byte("\n"))}, "")
	})
}

// No diagnostic and no answer carries a credential on any local-mode path, and
// the configuration renders without its bearer for every verb.
func TestSessionReporterLocalModeNeverPrintsACredential(t *testing.T) {
	spectest.Proves(t, "cloud/local-session-reporting", "local-session-fails-closed", "local-session-token-never-printed")
	paths := []struct {
		name  string
		login string
		setup func(cp *localControlPlane)
	}{
		{name: "accepted", login: "fresh"},
		{name: "refreshed", login: "expired"},
		{name: "re-minted", login: "fresh", setup: func(cp *localControlPlane) { cp.revokeAfterFirst = true }},
		{name: "re-mint refused", login: "fresh", setup: func(cp *localControlPlane) { cp.revokeAfterFirst, cp.refreshFails = true, true }},
		{name: "refresh refused", login: "expired", setup: func(cp *localControlPlane) { cp.refreshFails = true }},
		{name: "not logged in"},
	}
	for _, path := range paths {
		t.Run(path.name, func(t *testing.T) {
			stubReporterDenyInspection(t, func() error { return nil })
			r := newLocalReporter(t, path.login, true)
			r.env[cloudTokenEnv] = localMachineToken
			if path.setup != nil {
				path.setup(r.cp)
			}
			// serve fails the test when stdout, stderr or the error holds any
			// credential of the login, any token minted for it, or the machine token.
			session := r.serve(t, localFrame(t, "events.jsonl", 0, 0, "a\n", false), localFrame(t, "events.jsonl", 2, 1, "b\n", false))
			if session.err != nil {
				t.Fatal(session.err)
			}
		})
	}
	cfg := sessionReporterConfig{token: localCachedToken, local: func() (*clicore.WorkspaceContext, error) { return nil, nil }}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
		if rendered := fmt.Sprintf(verb, cfg); strings.Contains(rendered, localCachedToken) {
			t.Fatalf("%s rendered the bearer: %s", verb, rendered)
		}
	}
}

func TestLocalSessionURLEscapesTheSessionID(t *testing.T) {
	session := &clicore.WorkspaceContext{WorkspaceID: localWorkspaceID, ControlPlane: "https://api.example.test/"}
	got := localSessionURL(session, "a b/c?d")
	want := "https://api.example.test/v1/workspaces/" + localWorkspaceID + "/ci/local-sessions/a%20b%2Fc%3Fd"
	if got != want {
		t.Fatalf("localSessionURL = %q, want %q", got, want)
	}
}
