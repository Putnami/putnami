package workspacewarnings

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
)

const qualifySHA = "3cc91b658a1e0f7d2c4b9e8f6a5d3c2b1a0f9e8d"

// TestADevCommandEnvelopeCarriesTheWarningsOfItsOwnLoad holds the envelopes
// of compose and qualify to carrying the warnings of the workspace load that
// launched them, written once on stderr, and to omitting the member when the
// load warns of nothing. For qualify, that is the contract envelope of
// --print-contract and the verdict envelope of a URL target, passed or not.
// For compose, it is a compose that fails before it starts any member, and
// with --quiet the envelope still carries the warnings while stderr stays
// silent. Both run in @putnami/dev, which reads the warnings from its job
// context.
func TestADevCommandEnvelopeCarriesTheWarningsOfItsOwnLoad(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "project-inventory", "a-command-reports-the-warnings-of-its-own-load-once")
	t.Setenv("PUTNAMI_NO_AUTO_INSTALL", "1")
	t.Setenv("DO_NOT_TRACK", "1")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/readyz":
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case "/version":
			_, _ = w.Write([]byte(`{"name":"app","version":"1.0.0","sha":"` + qualifySHA + `"}`))
		case "/items":
			_, _ = w.Write([]byte(`[]`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	for _, tc := range []struct {
		name   string
		args   []string
		code   int
		data   bool   // the envelope carries a data member
		status string // the envelope status, when the row pins one
		quiet  bool   // the row passes --quiet, so stderr carries no warning
	}{
		{"qualify print-contract", []string{"qualify", "/app", "--print-contract", "--output=json"}, protocolcli.ExitSuccess, true, "", false},
		{"qualify passed url target", []string{"qualify", "/app", "--target", server.URL, "--expect-sha", qualifySHA[:9], "--output=json"}, protocolcli.ExitSuccess, true, "", false},
		{"qualify failing url target", []string{"qualify", "/app", "--target", server.URL, "--expect-sha", "deadbeef0", "--output=json"}, protocolcli.ExitFailure, true, "", false},
		{"compose unknown project", []string{"compose", "missing", "--output=json"}, protocolcli.ExitUsage, false, protocolcli.StatusFailure, false},
		{"compose unknown project quiet", []string{"compose", "missing", "--output=json", "--quiet"}, protocolcli.ExitUsage, false, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, envelope, stdout, stderr := run(t, devWorkspace(t, true), tc.args...)
			if code != tc.code || (envelope.Data != nil) != tc.data {
				t.Fatalf("exit code = %d, want %d, with data %t\nstdout:\n%s\nstderr:\n%s", code, tc.code, tc.data, stdout, stderr)
			}
			if tc.status != "" && envelope.Status != tc.status {
				t.Fatalf("status = %q, want %q\nstdout:\n%s\nstderr:\n%s", envelope.Status, tc.status, stdout, stderr)
			}
			assertJobsIgnoredWarning(t, envelope)
			if tc.quiet {
				if strings.Contains(stderr, "putnami: warning: ") {
					t.Errorf("--quiet wrote the warning to stderr:\n%s", stderr)
				}
				return
			}
			line := "putnami: warning: " + envelope.WorkspaceWarnings[0].Message
			if count := strings.Count(stderr, line); count != 1 {
				t.Errorf("stderr carries the warning %d times, want once:\n%s", count, stderr)
			}

			code, _, stdout, stderr = run(t, devWorkspace(t, false), tc.args...)
			if code != tc.code {
				t.Fatalf("clean workspace exit code = %d, want %d\nstdout:\n%s\nstderr:\n%s", code, tc.code, stdout, stderr)
			}
			if strings.Contains(stdout, "workspaceWarnings") || strings.Contains(stderr, "putnami: warning: ") {
				t.Errorf("a load with no warning reported one:\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
			}
		})
	}
}
