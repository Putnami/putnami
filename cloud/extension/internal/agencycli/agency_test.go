package agencycli

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

const (
	feature     = "cloud/agency-commands"
	workspaceID = "11111111-2222-4333-8444-555555555555"
)

// received is one request the stand-in received.
type received struct {
	method, path, authorization string
	body                        map[string]any
}

// standIn is an HTTP stand-in for the Putnami Cloud control plane: each
// route answers the status and body set for it, and every request is
// recorded.
type standIn struct {
	server   *httptest.Server
	mu       sync.Mutex
	requests []received
	answers  map[string]answer
}

type answer struct {
	status int
	body   string
}

func newStandIn(t *testing.T) *standIn {
	t.Helper()
	s := &standIn{answers: map[string]answer{}}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		request := received{method: r.Method, path: r.URL.Path, authorization: r.Header.Get("Authorization")}
		if len(data) > 0 {
			_ = json.Unmarshal(data, &request.body)
		}
		s.mu.Lock()
		s.requests = append(s.requests, request)
		found, ok := s.answers[r.Method+" "+r.URL.Path]
		s.mu.Unlock()
		if !ok {
			found = answer{http.StatusNotFound, `{"code":"not_found","error":"Not Found","message":"no route"}`}
		}
		w.Header().Set("Content-Type", "application/json")
		if found.status == 0 {
			found.status = http.StatusOK
		}
		w.WriteHeader(found.status)
		_, _ = io.WriteString(w, found.body)
	}))
	t.Cleanup(s.server.Close)
	return s
}

func (s *standIn) answer(route string, status int, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.answers[route] = answer{status, body}
}

// envelope is a first-party error envelope with the declared code of status.
func envelope(status int) string {
	codes := map[int]string{400: "http.bad_request", 401: "unauthorized", 403: "forbidden", 404: "not_found", 409: "conflict", 503: "http.service_unavailable"}
	return `{"code":"` + codes[status] + `","error":"` + http.StatusText(status) + `","message":"refused"}`
}

func (s *standIn) got() []received {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]received(nil), s.requests...)
}

// testRun is one command run against the stand-in: the linked workspace, a
// signed-in session, and the lines the command printed.
type testRun struct {
	root   string
	env    map[string]string
	ioctx  clicore.IO
	out    *[]string
	bearer string
}

func jwt(t *testing.T, workspace, nonce string) string {
	t.Helper()
	claims, err := json.Marshal(map[string]any{"scope_ref": map[string]any{"workspace_id": workspace}, "nonce": nonce})
	if err != nil {
		t.Fatal(err)
	}
	encode := base64.RawURLEncoding.EncodeToString
	return encode([]byte(`{"alg":"none"}`)) + "." + encode(claims) + ".sig"
}

func newRun(t *testing.T, s *standIn) *testRun {
	t.Helper()
	root := t.TempDir()
	link, _ := json.Marshal(map[string]any{"workspace_id": workspaceID, "control_plane_url": s.server.URL})
	if err := os.MkdirAll(filepath.Dir(clicore.LinkPath(root)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(clicore.LinkPath(root), link, 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"PUTNAMI_HOME": t.TempDir(), "HOME": t.TempDir()}
	bearer := jwt(t, workspaceID, "session")
	if err := clicore.WriteAuth(&clicore.StoredToken{
		AccessToken: jwt(t, "", "base"), RefreshToken: "refresh-secret", TokenType: "Bearer", ExpiresAt: "2999-01-01T00:00:00Z",
		WorkspaceAccess: map[string]clicore.StoredWorkspaceAccess{
			workspaceID: {AccessToken: bearer, TokenType: "Bearer", ExpiresAt: "2999-01-01T00:00:00Z"},
		},
	}, env); err != nil {
		t.Fatal(err)
	}
	out := &[]string{}
	ioctx := clicore.IO{Env: env, Client: s.server.Client(), Stdout: func(line string) { *out = append(*out, line) }, Stderr: func(string) {}}
	return &testRun{root: root, env: env, ioctx: ioctx, out: out, bearer: bearer}
}

func (r *testRun) text() string { return strings.Join(*r.out, "\n") }

// params are the parsed flags of one command.
func params(pairs ...any) map[string]any {
	out := map[string]any{}
	for i := 0; i+1 < len(pairs); i += 2 {
		out[pairs[i].(string)] = pairs[i+1]
	}
	return out
}

// stdin is a secret piped to the command.
func stdin(text string) io.Reader { return strings.NewReader(text) }

// noInput fails the test when a command reads it.
type noInput struct{ t *testing.T }

func (n noInput) Read([]byte) (int, error) {
	n.t.Fatal("the command read standard input")
	return 0, io.EOF
}
