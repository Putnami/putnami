package workspacewarnings

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
	httproutes "go.putnami.dev/protocol/http-routes"
	"go.putnami.dev/tooling/cli/internal/cli/clitest"
)

const qualifySHA = "3cc91b658a1e0f7d2c4b9e8f6a5d3c2b1a0f9e8d"

// qualifyWorkspace writes a workspace whose one project, /app, commits a route
// inventory with one safe route. When declaresJobs is true, its putnami.json
// declares a `jobs` key, which the load warns about.
func qualifyWorkspace(t *testing.T, declaresJobs bool) string {
	t.Helper()
	root := t.TempDir()
	provenance := httproutes.Provenance{Project: "app", SourceKind: httproutes.SourceTypedAPI}
	manifest, diags := httproutes.Canonicalize([]httproutes.Route{
		{Match: httproutes.MatchExact, Path: "/items", Methods: []string{"GET"}, PublicEdge: true, Provenance: provenance},
		{Match: httproutes.MatchExact, Path: "/readyz", Methods: []string{"GET"}, PublicEdge: true, Provenance: provenance},
	})
	if diag.HasErrors(diags) {
		t.Fatalf("canonicalize: %v", diags)
	}
	inventory, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	project := `{"name":"app"}`
	if declaresJobs {
		project = `{"name":"app","jobs":{}}`
	}
	clitest.WriteFile(t, filepath.Join(root, "putnami.workspace.json"), `{"name":"qualify-warnings","includes":["app"]}`)
	clitest.WriteFile(t, filepath.Join(root, "app", "putnami.json"), project)
	clitest.WriteFile(t, filepath.Join(root, "app", "schema", "http-routes.json"), string(inventory))
	return root
}

// TestQualifyEnvelopeCarriesTheWarningsOfItsOwnLoad holds the contract envelope
// of --print-contract and the verdict envelope of a URL target, passed or not,
// to carrying the warnings of the command's workspace load, written once on
// stderr, and to omitting the member when the load warns of nothing.
func TestQualifyEnvelopeCarriesTheWarningsOfItsOwnLoad(t *testing.T) {
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
		name string
		args []string
		code int
	}{
		{"print-contract", []string{"qualify", "/app", "--print-contract", "--output=json"}, protocolcli.ExitSuccess},
		{"passed url target", []string{"qualify", "/app", "--target", server.URL, "--expect-sha", qualifySHA[:9], "--output=json"}, protocolcli.ExitSuccess},
		{"failing url target", []string{"qualify", "/app", "--target", server.URL, "--expect-sha", "deadbeef0", "--output=json"}, protocolcli.ExitFailure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, envelope, stdout, stderr := run(t, qualifyWorkspace(t, true), tc.args...)
			if code != tc.code || envelope.Data == nil {
				t.Fatalf("exit code = %d, want %d, with data\nstdout:\n%s\nstderr:\n%s", code, tc.code, stdout, stderr)
			}
			assertJobsIgnoredWarning(t, envelope)
			line := "putnami: warning: " + envelope.WorkspaceWarnings[0].Message
			if count := strings.Count(stderr, line); count != 1 {
				t.Errorf("stderr carries the warning %d times, want once:\n%s", count, stderr)
			}

			code, _, stdout, stderr = run(t, qualifyWorkspace(t, false), tc.args...)
			if code != tc.code {
				t.Fatalf("clean workspace exit code = %d, want %d\nstdout:\n%s\nstderr:\n%s", code, tc.code, stdout, stderr)
			}
			if strings.Contains(stdout, "workspaceWarnings") || strings.Contains(stderr, "putnami: warning: ") {
				t.Errorf("a load with no warning reported one:\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
			}
		})
	}
}
