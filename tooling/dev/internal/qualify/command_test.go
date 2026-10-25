package qualify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	httproutes "go.putnami.dev/protocol/http-routes"
	qualifyproto "go.putnami.dev/protocol/qualify"

	"go.putnami.dev/tooling/dev/internal/compose"
)

// The verdict goldens run `putnami qualify --target local` as the dev runtime
// does and pin the verdict document byte for byte against the goldens the
// core command recorded, after normalizing what differs between runs: the
// worktree's fingerprint and commit, the composition id, ports, timestamps
// and durations.
//
// PUTNAMI_UPDATE_QUALIFY_GOLDENS=1 rewrites the goldens.

const verdictGoldenDir = "testdata/local-verdict"

var updateVerdictGoldens = os.Getenv("PUTNAMI_UPDATE_QUALIFY_GOLDENS") == "1"

// Each pattern keeps the member name and its separator, so an indented and a
// compact document normalize alike.
var (
	verdictFingerprint = regexp.MustCompile(`("fingerprint":\s*)"[^"]+"`)
	verdictHeadSHA     = regexp.MustCompile(`("headSHA":\s*)"[0-9a-f]{40}"`)
	verdictTimestamp   = regexp.MustCompile(`("(?:startedAt|finishedAt)":\s*)"[^"]+"`)
	verdictDuration    = regexp.MustCompile(`("durationMs":\s*)[0-9]+`)
	verdictComposition = regexp.MustCompile(`\b[0-9a-f]{16}\b`)
	verdictLoopback    = regexp.MustCompile(`127\.0\.0\.1:[0-9]+`)
)

func normalizeVerdict(root, document string) string {
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		document = strings.ReplaceAll(document, resolved, "<root>")
	}
	document = strings.ReplaceAll(document, root, "<root>")
	document = verdictFingerprint.ReplaceAllString(document, `${1}"<fingerprint>"`)
	document = verdictHeadSHA.ReplaceAllString(document, `${1}"<sha>"`)
	document = verdictTimestamp.ReplaceAllString(document, `${1}"<time>"`)
	document = verdictDuration.ReplaceAllString(document, `${1}"<ms>"`)
	document = verdictLoopback.ReplaceAllString(document, "127.0.0.1:<port>")
	return verdictComposition.ReplaceAllString(document, "<composition>")
}

// runCommand runs the command in ws and returns its exit code, stdout and
// stderr.
func runCommand(t *testing.T, ws *compose.Workspace, executable string, mode protocolcli.OutputMode, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run(Request{Ctx: t.Context(), Workspace: ws, Args: args, Output: mode, Executable: executable, Stdout: &stdout, Stderr: &stderr})
	return code, stdout.String(), stderr.String()
}

func assertVerdictGolden(t *testing.T, name string, code int, document string) {
	t.Helper()
	got := fmt.Sprintf("exit %d\n%s", code, document)
	if !strings.HasSuffix(got, "\n") {
		got += "\n"
	}
	path := filepath.Join(verdictGoldenDir, name+".golden")
	if updateVerdictGoldens {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (regenerate with: PUTNAMI_UPDATE_QUALIFY_GOLDENS=1 ./putnamiw test --projects /tooling/dev --run VerdictDocument --no-cache --no-enforce-coverage)", path, err)
	}
	if string(want) != got {
		t.Errorf("%s drifted from its golden.\n--- want\n%s\n--- got\n%s", path, want, got)
	}
}

func goldenRoutes() []httproutes.Route {
	provenance := httproutes.Provenance{Project: "app", SourceKind: httproutes.SourceManual}
	return []httproutes.Route{
		{Match: httproutes.MatchExact, Path: "/items", Methods: []string{"GET"}, PublicEdge: true, Provenance: provenance},
	}
}

func TestQualifyLocal_TheVerdictDocumentKeepsItsBytes(t *testing.T) {
	spectest.Proves(t, "tooling/workload-qualification", "each-member-is-an-ordinary-serve-run",
		"the-start-exit-and-verdict-documents-keep-their-bytes")
	spectest.Proves(t, "tooling/workload-qualification", "every-non-pass-state-exits-non-zero",
		"the-verdict-rides-the-failure-envelope")
	t.Run("passed", func(t *testing.T) {
		fixture := newLocalFixture(t,
			localMember{name: "provider", mode: fixtureServe},
			localMember{name: "app", mode: fixtureServe, runsWith: []string{"provider"}, routes: goldenRoutes()},
		)
		code, stdout, stderr := runCommand(t, fixture.ws, fixture.executable, protocolcli.OutputJSON, "app", "--target", "local")
		assertVerdictGolden(t, "passed", code, normalizeVerdict(fixture.root, stdout))
		if stderr != "" {
			t.Errorf("stderr = %q, want nothing", stderr)
		}
		if got := fixture.requests(); got != "GET /items" {
			t.Errorf("requests reaching the target = %q", got)
		}
		fixture.assertStopped(t, "provider", "app")
	})
	t.Run("composition-failed", func(t *testing.T) {
		fixture := newLocalFixture(t,
			localMember{name: "provider", mode: fixtureNeverReady},
			localMember{name: "app", mode: fixtureServe, runsWith: []string{"provider"}, routes: goldenRoutes()},
		)
		code, stdout, stderr := runCommand(t, fixture.ws, fixture.executable, protocolcli.OutputJSON, "app", "--target", "local", "--ready-timeout", "2s")
		assertVerdictGolden(t, "composition-failed", code, normalizeVerdict(fixture.root, stdout))
		if stderr != "putnami: qualify /app: verdict composition_failed\n" {
			t.Errorf("stderr = %q, want the verdict error line", stderr)
		}
		fixture.assertStopped(t, "provider")
	})
}

// commandWorkspace is a workspace of one project, /app, whose committed route
// inventory declares GET /items.
func commandWorkspace(t *testing.T) *compose.Workspace {
	t.Helper()
	ws, _ := fixtureProject(t, "schema", goldenRoutes())
	return ws
}

func TestRun_RefusesAnInvocationBeforeAnythingIsDerived(t *testing.T) {
	spectest.Proves(t, "tooling/workload-qualification", "remote-verdict-binds-to-the-deployed-sha",
		"a-url-target-without-expect-sha-is-a-usage-error")
	spectest.Proves(t, "tooling/workload-qualification", "local-verdict-binds-to-the-exact-worktree",
		"expect-sha-with-a-local-target-is-a-usage-error")
	ws := commandWorkspace(t)
	cases := map[string]struct {
		args []string
		code int
		want string
	}{
		"no project":                 {nil, protocolcli.ExitUsage, "qualify takes exactly one project: putnami qualify <project> --target <local|url>"},
		"no target":                  {[]string{"/app"}, protocolcli.ExitUsage, "qualify needs --target"},
		"local with expect-sha":      {[]string{"/app", "--target", "local", "--expect-sha", "0123456"}, protocolcli.ExitUsage, "--expect-sha binds a URL target"},
		"url without expect-sha":     {[]string{"/app", "--target", "http://127.0.0.1:1"}, protocolcli.ExitUsage, "--expect-sha is required with a URL target"},
		"print-contract with target": {[]string{"/app", "--print-contract", "--target", "local"}, protocolcli.ExitUsage, "--print-contract derives and prints the contract without a target"},
		"bad platform prefix":        {[]string{"/app", "--print-contract", "--platform-prefix", "/a?b"}, protocolcli.ExitUsage, `--platform-prefix "/a?b" must be a plain path`},
		"bad ready timeout":          {[]string{"/app", "--target", "local", "--ready-timeout", "soon"}, protocolcli.ExitUsage, "--ready-timeout must be a positive Go duration such as 30s or 2m"},
		"bad request timeout":        {[]string{"/app", "--target", "local", "--request-timeout", "0s"}, protocolcli.ExitUsage, "--request-timeout must be a positive Go duration such as 30s or 2m"},
		"unknown project":            {[]string{"nope", "--target", "local"}, protocolcli.ExitUsage, "project not found: nope"},
		"unknown flag":               {[]string{"/app", "--targte", "local"}, protocolcli.ExitUsage, "unknown flag --targte for `putnami qualify`"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			code, stdout, stderr := runCommand(t, ws, "", protocolcli.OutputText, tc.args...)
			if code != tc.code || stdout != "" || !strings.HasPrefix(stderr, "putnami: "+tc.want) {
				t.Errorf("exit %d, stdout %q, stderr %q, want %d and %q", code, stdout, stderr, tc.code, tc.want)
			}
		})
	}
}

func TestRun_ResolvesTheProjectByIDNameOrPath(t *testing.T) {
	ws := commandWorkspace(t)
	for _, selector := range []string{"/app", "example", "app", "app/"} {
		if code, stdout, stderr := runCommand(t, ws, "", protocolcli.OutputText, selector, "--print-contract"); code != 0 || !strings.Contains(stdout, "GET /items") {
			t.Errorf("selector %q: exit %d, stdout %q, stderr %q", selector, code, stdout, stderr)
		}
	}
}

// A print-contract run and a passed verdict are the data of a success
// envelope in json and jsonl, and the bare document in cloud-logging.
func TestRun_WritesTheDocumentInEveryOutputMode(t *testing.T) {
	ws := commandWorkspace(t)
	for _, mode := range []protocolcli.OutputMode{protocolcli.OutputJSON, protocolcli.OutputJSONL} {
		code, stdout, _ := runCommand(t, ws, "", mode, "/app", "--print-contract")
		var envelope struct {
			Command string                `json:"command"`
			Status  string                `json:"status"`
			Data    qualifyproto.Contract `json:"data"`
		}
		if err := json.Unmarshal([]byte(stdout), &envelope); err != nil || code != 0 || envelope.Command != CommandPath ||
			envelope.Status != "success" || len(envelope.Data.Requests) != 1 {
			t.Errorf("%s: exit %d, stdout %q (%v)", mode, code, stdout, err)
		}
		if lines := strings.Count(strings.TrimSpace(stdout), "\n"); (mode == protocolcli.OutputJSONL) != (lines == 0) {
			t.Errorf("%s: the envelope spans %d more lines", mode, lines)
		}
	}
	code, stdout, _ := runCommand(t, ws, "", protocolcli.OutputCloudLogging, "/app", "--print-contract")
	var contract qualifyproto.Contract
	if err := json.Unmarshal([]byte(stdout), &contract); err != nil || code != 0 || len(contract.Requests) != 1 {
		t.Errorf("cloud-logging: exit %d, stdout %q (%v)", code, stdout, err)
	}
}

// A URL verdict: passed exits 0 with the verdict, and a deployment that
// reports another sha exits 1 with the verdict text and then the error.
func TestRun_AURLTargetExitsByItsVerdict(t *testing.T) {
	spectest.Proves(t, "tooling/workload-qualification", "every-non-pass-state-exits-non-zero",
		"passed-exits-zero-and-every-other-state-exits-one")
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

	code, stdout, stderr := runCommand(t, ws, "", protocolcli.OutputText, "/app", "--target", deployment.URL, "--expect-sha", sha[:12])
	if code != 0 || !strings.Contains(stdout, "verdict: passed (/app, url") || stderr != "" {
		t.Errorf("passed: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	code, stdout, stderr = runCommand(t, ws, "", protocolcli.OutputText, "/app", "--target", deployment.URL, "--expect-sha", "fedcba9876543210")
	if code != protocolcli.ExitFailure || !strings.Contains(stdout, "verdict: digest_mismatch") || stderr != "putnami: qualify /app: verdict digest_mismatch\n" {
		t.Errorf("digest mismatch: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	code, stdout, stderr = runCommand(t, ws, "", protocolcli.OutputJSONL, "/app", "--target", deployment.URL, "--expect-sha", "fedcba9876543210")
	var envelope struct {
		Status string               `json:"status"`
		Data   qualifyproto.Verdict `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil || code != protocolcli.ExitFailure || envelope.Status != "failure" ||
		envelope.Data.State != qualifyproto.StateDigestMismatch || stderr != "putnami: qualify /app: verdict digest_mismatch\n" {
		t.Errorf("jsonl digest mismatch: exit %d, stdout %q, stderr %q (%v)", code, stdout, stderr, err)
	}
	code, stdout, stderr = runCommand(t, ws, "", protocolcli.OutputCloudLogging, "/app", "--target", deployment.URL, "--expect-sha", "fedcba9876543210")
	if code != protocolcli.ExitFailure || stdout != "" || stderr != "putnami: qualify /app: verdict digest_mismatch\n" {
		t.Errorf("cloud-logging digest mismatch: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
}

// An inventory that does not decode is a configuration error, exit 2.
func TestRun_AnInvalidInventoryIsAConfigurationError(t *testing.T) {
	ws, _ := fixtureProject(t, "schema", nil)
	if err := os.MkdirAll(filepath.Join(ws.Root, "app", "schema"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws.Root, "app", "schema", "http-routes.json"), []byte(`{"protocolVersion":`), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runCommand(t, ws, "", protocolcli.OutputJSON, "/app", "--print-contract")
	var envelope protocolcli.ResultV2
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil || code != protocolcli.ExitUsage || envelope.Error == nil ||
		envelope.Error.Code != "usage" || !strings.HasPrefix(stderr, "putnami: ") {
		t.Errorf("exit %d, stdout %q, stderr %q (%v)", code, stdout, stderr, err)
	}
}
