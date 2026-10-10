package datacli

import (
	stderrors "errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/client"
	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	perrors "go.putnami.dev/errors"
)

func silentIO() clicore.IO {
	return clicore.IO{
		Env:    map[string]string{},
		Stdout: func(string) {},
		Stderr: func(string) {},
		Now:    time.Now,
		Client: http.DefaultClient,
	}
}

func TestDataCallError(t *testing.T) {
	authErr := dataCallError("t", "invalid", &client.RemoteError{StatusCode: http.StatusUnauthorized})
	if clicore.ExitCode(authErr) != clicore.ExitAuth || authErr.Error() != "request failed for t: 401 Unauthorized" {
		t.Fatalf("401 mapped to %q (exit %d), want ExitAuth with the status line", authErr, clicore.ExitCode(authErr))
	}
	apiErr := dataCallError("t", "invalid", &client.RemoteError{StatusCode: http.StatusInternalServerError})
	if clicore.ExitCode(apiErr) != clicore.ExitAPI || apiErr.Error() != "request failed for t: 500 Internal Server Error" {
		t.Fatalf("500 mapped to %q (exit %d), want ExitAPI with the status line", apiErr, clicore.ExitCode(apiErr))
	}
	invalidErr := dataCallError("t", "invalid grant response from t", perrors.New(client.CodeClientResponse, "bad body"))
	if clicore.ExitCode(invalidErr) != clicore.ExitAPI || invalidErr.Error() != "invalid grant response from t" {
		t.Fatalf("contract violation mapped to %q (exit %d), want the invalid-response message", invalidErr, clicore.ExitCode(invalidErr))
	}
	transportErr := dataCallError("t", "invalid", stderrors.New("dial tcp: refused"))
	if clicore.ExitCode(transportErr) != clicore.ExitAPI || transportErr.Error() != "request failed for t: dial tcp: refused" {
		t.Fatalf("transport failure mapped to %q (exit %d)", transportErr, clicore.ExitCode(transportErr))
	}
}

func TestDBUnknownSubcommand(t *testing.T) {
	err := DB(map[string]any{}, []string{"bogus"}, "", map[string]string{}, silentIO())
	if err == nil || clicore.ExitCode(err) != clicore.ExitUsage {
		t.Fatalf("unknown subcommand = %v, want ExitUsage", err)
	}
}

func TestDBGrantUnknownAction(t *testing.T) {
	err := DB(map[string]any{}, []string{"grant", "bogus"}, "", map[string]string{}, silentIO())
	if err == nil || clicore.ExitCode(err) != clicore.ExitUsage {
		t.Fatalf("unknown grant action = %v, want ExitUsage", err)
	}
}

func TestRequiredDatabase(t *testing.T) {
	if _, err := requiredDatabase(map[string]any{}); err == nil || clicore.ExitCode(err) != clicore.ExitUsage {
		t.Fatalf("missing database = %v, want ExitUsage", err)
	}
	if _, err := requiredDatabase(map[string]any{"database": "   "}); err == nil {
		t.Fatal("whitespace-only database = nil error, want ExitUsage")
	}
	got, err := requiredDatabase(map[string]any{"database": "mydb"})
	if err != nil || got != "mydb" {
		t.Fatalf("requiredDatabase = (%q, %v), want (mydb, nil)", got, err)
	}
}

func TestDBHelpStructuredOutput(t *testing.T) {
	params := clicore.MergeParams(clicore.ParseFlags([]string{"--output", "json"}))
	var lines []string
	ioctx := silentIO()
	ioctx.Stdout = func(s string) { lines = append(lines, s) }
	if err := dbHelp(params, ioctx); err != nil {
		t.Fatalf("dbHelp: %v", err)
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "db connect") || !strings.Contains(joined, "commands") {
		t.Fatalf("structured help missing expected content:\n%s", joined)
	}
}

func TestDBPositionals(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{"skips putnamiContext token and value", []string{"--putnamiContext", "ctx", "grant", "request"}, []string{"grant", "request"}},
		{"skips inline-value flag", []string{"--output=json", "list"}, []string{"list"}},
		{"skips no-prefixed boolean flag", []string{"--no-cache", "revoke", "id"}, []string{"revoke", "id"}},
		{"skips flag and its consumed value", []string{"--database", "mydb", "list"}, []string{"list"}},
		{"interleaved positionals and flags", []string{"grant", "--reason", "why", "request"}, []string{"grant", "request"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := dbPositionals(tc.args); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("dbPositionals(%v) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

func TestGrantRequestStructuredEmitsEnvelope(t *testing.T) {
	fake := &dbFakeServer{}
	ioctx, _, _ := newDBTestIO(t, fake)
	var out []string
	ioctx.Stdout = func(s string) { out = append(out, s) }
	if err := runDB(ioctx, []string{"grant", "request", "--database", testDatabase, "--reason", "debug", "--output", "json"}); err != nil {
		t.Fatalf("grant request structured: %v", err)
	}
	joined := strings.Join(out, "\n")
	if !strings.Contains(joined, "grant_ab12") {
		t.Fatalf("structured grant output missing grant id:\n%s", joined)
	}
}

func TestGrantSubcommandsRequireDatabase(t *testing.T) {
	// requiredDatabase runs before any network call, so a bare (empty) IO suffices.
	cases := [][]string{
		{"grant", "request", "--reason", "why"},
		{"grant", "list"},
		{"grant", "revoke", "grant_ab12"},
	}
	for _, args := range cases {
		if err := runDB(silentIO(), args); err == nil || clicore.ExitCode(err) != clicore.ExitUsage {
			t.Fatalf("%v without --database = %v, want ExitUsage", args, err)
		}
	}
}

// grantsErrorServer serves the auth/refresh endpoints from a real dbFakeServer
// but returns an error status for every grants call, driving the dataCallError
// failure return in each grant verb.
type grantsErrorServer struct {
	auth   *dbFakeServer
	status int
}

func (s *grantsErrorServer) RoundTrip(req *http.Request) (*http.Response, error) {
	switch req.URL.Path {
	case "/.well-known/openid-configuration", "/token", "/userinfo":
		return s.auth.RoundTrip(req)
	default:
		return jsonResponse(s.status, map[string]any{"error": "server exploded"}), nil
	}
}

func TestGrantVerbsSurfaceServerErrors(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"request", []string{"grant", "request", "--database", testDatabase, "--reason", "debug"}},
		{"list", []string{"grant", "list", "--database", testDatabase}},
		{"revoke", []string{"grant", "revoke", "grant_ab12", "--database", testDatabase}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ioctx, _, _ := newDBTestIO(t, &dbFakeServer{})
			ioctx.Client = &http.Client{Transport: &grantsErrorServer{auth: &dbFakeServer{}, status: http.StatusInternalServerError}}
			err := runDB(ioctx, tc.args)
			if err == nil {
				t.Fatalf("%s against a 500 gateway = nil error, want a surfaced failure", tc.name)
			}
			if clicore.ExitCode(err) != clicore.ExitAPI {
				t.Fatalf("%s exit code = %d, want ExitAPI", tc.name, clicore.ExitCode(err))
			}
		})
	}
}
