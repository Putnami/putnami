package qualify

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
)

// launchWarnings are the warnings a test hands the command as putnami hands
// them in the job context.
var launchWarnings = []protocolcli.WorkspaceWarning{
	{Code: "workspace.jobs_ignored", Message: "project /app declares jobs, which putnami ignores"},
}

// TestRun_EveryEnvelopeCarriesTheLaunchWarnings holds qualify to carrying the
// warnings of the load that launched it in each envelope it writes, in json
// and jsonl: the contract of --print-contract, a URL verdict that passed or
// not, a local verdict and a usage refusal. putnami already wrote them on
// stderr, so qualify writes none of them there. A launch without warnings
// writes an envelope without the member.
func TestRun_EveryEnvelopeCarriesTheLaunchWarnings(t *testing.T) {
	spectest.Proves(t, "tooling/workload-qualification", "compose-and-qualify-carry-their-launch-warnings",
		"every-envelope-carries-the-launch-warnings")
	sha := "0123456789abcdef0123456789abcdef01234567"
	deployment := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/readyz":
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case "/version":
			_, _ = fmt.Fprintf(w, `{"name":"app","version":"1.2.3","sha":%q}`, sha)
		case "/items":
			_, _ = w.Write([]byte(`[]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer deployment.Close()
	ws := commandWorkspace(t)

	for _, tc := range []struct {
		name string
		args []string
		code int
	}{
		{"print-contract", []string{"/app", "--print-contract"}, protocolcli.ExitSuccess},
		{"passed url target", []string{"/app", "--target", deployment.URL, "--expect-sha", sha[:12]}, protocolcli.ExitSuccess},
		{"failed url target", []string{"/app", "--target", deployment.URL, "--expect-sha", "fedcba9876543210"}, protocolcli.ExitFailure},
		{"usage refusal", []string{"/app"}, protocolcli.ExitUsage},
		{"unknown project", []string{"nope", "--target", "local"}, protocolcli.ExitUsage},
	} {
		for _, mode := range []protocolcli.OutputMode{protocolcli.OutputJSON, protocolcli.OutputJSONL} {
			t.Run(tc.name+"/"+string(mode), func(t *testing.T) {
				code, stdout, stderr := runWarnedCommand(t, launchWarnings, ws, "", mode, tc.args...)
				if code != tc.code {
					t.Fatalf("exit %d, want %d\nstdout:\n%s\nstderr:\n%s", code, tc.code, stdout, stderr)
				}
				assertCarriesLaunchWarnings(t, stdout)
				if strings.Contains(stderr, "putnami: warning: ") {
					t.Errorf("qualify wrote a launch warning on stderr: %q", stderr)
				}
				if _, stdout, _ = runCommand(t, ws, "", mode, tc.args...); strings.Contains(stdout, "workspaceWarnings") {
					t.Errorf("a launch without warnings carries the member:\n%s", stdout)
				}
			})
		}
	}

	t.Run("local target", func(t *testing.T) {
		fixture := newLocalFixture(t, localMember{name: "app", mode: fixtureServe, routes: goldenRoutes()})
		code, stdout, stderr := runWarnedCommand(t, launchWarnings, fixture.ws, fixture.executable, protocolcli.OutputJSON, "app", "--target", "local")
		if code != protocolcli.ExitSuccess {
			t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
		}
		assertCarriesLaunchWarnings(t, stdout)
		if stderr != "" {
			t.Errorf("stderr = %q, want nothing", stderr)
		}
		fixture.assertStopped(t, "app")
	})
}

// assertCarriesLaunchWarnings fails unless document is one result envelope
// whose workspaceWarnings are exactly launchWarnings.
func assertCarriesLaunchWarnings(t *testing.T, document string) {
	t.Helper()
	if violations := protocolcli.ValidateDocument(protocolcli.DocumentResultEnvelope, []byte(document)); len(violations) != 0 {
		t.Fatalf("the envelope violates the contract: %v\n%s", violations, document)
	}
	var envelope protocolcli.ResultV2
	if err := json.Unmarshal([]byte(document), &envelope); err != nil {
		t.Fatalf("decode envelope: %v\n%s", err, document)
	}
	if !reflect.DeepEqual(envelope.WorkspaceWarnings, launchWarnings) {
		t.Errorf("workspaceWarnings = %+v, want %+v\n%s", envelope.WorkspaceWarnings, launchWarnings, document)
	}
}
