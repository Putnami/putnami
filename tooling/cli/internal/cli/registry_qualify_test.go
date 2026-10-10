package cli

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
	httproutes "go.putnami.dev/protocol/http-routes"
	qualifyproto "go.putnami.dev/protocol/qualify"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commandmeta"
)

const qualifyTestSHA = "3cc91b658a1e0f7d2c4b9e8f6a5d3c2b1a0f9e8d"

// qualifyWorkspace writes a workspace with two projects: /app, whose committed
// route inventory holds one safe route among unsafe ones, and /bare, which has
// no inventory at all.
func qualifyWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write := func(rel string, data []byte) {
		t.Helper()
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	provenance := httproutes.Provenance{Project: "example", SourceKind: httproutes.SourceTypedAPI}
	manifest, diags := httproutes.Canonicalize([]httproutes.Route{
		{Match: httproutes.MatchExact, Path: "/items", Methods: []string{"GET", "POST"}, PublicEdge: true, Provenance: provenance},
		{Match: httproutes.MatchExact, Path: "/readyz", Methods: []string{"GET"}, PublicEdge: true, Provenance: provenance},
		{Match: httproutes.MatchTemplate, Path: "/items/{id}", Methods: []string{"GET"}, PublicEdge: true, Provenance: provenance},
	})
	if diag.HasErrors(diags) {
		t.Fatalf("canonicalize: %v", diags)
	}
	inventory, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	write("putnami.workspace.json", []byte(`{"name":"fixture","includes":["app","bare","prefixed"]}`))
	write("app/putnami.json", []byte(`{"name":"example"}`))
	write("app/schema/http-routes.json", inventory)
	write("bare/putnami.json", []byte(`{"name":"bare"}`))
	// /prefixed mounts its platform endpoints under "/_" and says so in its
	// inventory, the way a web workload that serves them at /_/readyz does.
	prefixed, diags := httproutes.Canonicalize([]httproutes.Route{
		{Match: httproutes.MatchExact, Path: "/items", Methods: []string{"GET"}, PublicEdge: true, Provenance: provenance},
		{Match: httproutes.MatchExact, Path: "/_/readyz", Methods: []string{"GET"}, PublicEdge: false, Provenance: provenance},
		{Match: httproutes.MatchExact, Path: "/_/version", Methods: []string{"GET"}, PublicEdge: false, Provenance: provenance},
	})
	if diag.HasErrors(diags) {
		t.Fatalf("canonicalize: %v", diags)
	}
	prefixedInventory, err := json.Marshal(prefixed)
	if err != nil {
		t.Fatal(err)
	}
	write("prefixed/putnami.json", []byte(`{"name":"prefixed"}`))
	write("prefixed/.gen/schema/http-routes.json", prefixedInventory)
	return root
}

// runQualify drives `putnami qualify` through the real argument parser and the
// structured dispatcher, returning the exit code and stdout.
func runQualify(t *testing.T, root string, args ...string) (int, string) {
	t.Helper()
	parsed := ParseArgs(append([]string{"qualify"}, args...), nil, nil)
	if parsed.Err != nil {
		return exitCodeForError(parsed.Err), ""
	}
	code := -1
	app := &App{}
	out := captureStdout(t, func() {
		code = app.runStructuredCommand(context.Background(), parsed, &wsproto.Config{}, root)
	})
	return code, out
}

func decodeEnvelope(t *testing.T, out string) protocolcli.ResultV2 {
	t.Helper()
	var envelope protocolcli.ResultV2
	if err := json.Unmarshal([]byte(out), &envelope); err != nil {
		t.Fatalf("stdout is not one result envelope: %v\n%s", err, out)
	}
	if envelope.Command != "qualify" {
		t.Errorf("envelope command = %q, want qualify (the project must not leak into the command path)", envelope.Command)
	}
	return envelope
}

// verdictFromEnvelope reads data as a verdict through the strict protocol parser.
func verdictFromEnvelope(t *testing.T, envelope protocolcli.ResultV2) *qualifyproto.Verdict {
	t.Helper()
	data, err := json.Marshal(envelope.Data)
	if err != nil {
		t.Fatal(err)
	}
	verdict, diags := qualifyproto.ParseAndValidateVerdict(data)
	if diag.HasErrors(diags) {
		t.Fatalf("envelope data is not a valid verdict: %v\n%s", diags, data)
	}
	return verdict
}

func qualifyDeployment(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/readyz":
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case "/version":
			_, _ = w.Write([]byte(`{"name":"example","version":"1.0.0","sha":"` + qualifyTestSHA + `"}`))
		case "/items":
			_, _ = w.Write([]byte(`[]`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func TestCmdQualify_PrintContractListsTheDerivedContract(t *testing.T) {
	root := qualifyWorkspace(t)

	code, out := runQualify(t, root, "/app", "--print-contract", "--output=json")
	if code != ExitSuccess {
		t.Fatalf("exit = %d\n%s", code, out)
	}
	envelope := decodeEnvelope(t, out)
	data, err := json.Marshal(envelope.Data)
	if err != nil {
		t.Fatal(err)
	}
	contract, diags := qualifyproto.ParseAndValidateContract(data)
	if diag.HasErrors(diags) {
		t.Fatalf("data is not a valid contract: %v\n%s", diags, data)
	}
	if len(contract.Requests) != 1 || contract.Requests[0].ID != "GET /items" || contract.Requests[0].Provenance != "typed-api" {
		t.Errorf("requests = %+v, want only GET /items with its provenance", contract.Requests)
	}
	if len(contract.DerivedFrom) != 1 || contract.DerivedFrom[0].Path != "app/schema/http-routes.json" {
		t.Errorf("derivedFrom = %+v", contract.DerivedFrom)
	}

	// Selected by name, in human mode.
	code, out = runQualify(t, root, "example", "--print-contract")
	if code != ExitSuccess || !strings.Contains(out, "GET /items  maxStatus 499  provenance typed-api") || !strings.Contains(out, contract.Digest) {
		t.Errorf("human contract: exit %d\n%s", code, out)
	}

	// An empty contract still prints and exits 0: the verdict, not the contract, is unsupported.
	code, out = runQualify(t, root, "/bare", "--print-contract")
	if code != ExitSuccess || !strings.Contains(out, "requests: 0") || !strings.Contains(out, "putnami build --projects /bare") {
		t.Errorf("empty contract: exit %d\n%s", code, out)
	}
}

func TestCmdQualify_URLTargetVerdictDrivesTheExitCode(t *testing.T) {
	spectest.Proves(t, "cli/workload-qualification", "every-non-pass-state-exits-non-zero", "passed-exits-zero-and-every-other-state-exits-one")
	spectest.Proves(t, "cli/workload-qualification", "every-non-pass-state-exits-non-zero", "the-verdict-rides-the-failure-envelope")
	root := qualifyWorkspace(t)
	server := qualifyDeployment(t)

	code, out := runQualify(t, root, "/app", "--target", server.URL, "--expect-sha", qualifyTestSHA[:9], "--output=json")
	if code != ExitSuccess {
		t.Fatalf("passed exit = %d\n%s", code, out)
	}
	envelope := decodeEnvelope(t, out)
	if verdict := verdictFromEnvelope(t, envelope); verdict.State != qualifyproto.StatePassed || envelope.Status != protocolcli.StatusSuccess {
		t.Errorf("state %s status %s", verdict.State, envelope.Status)
	}

	// A deployment that serves its platform endpoints only under "/_" passes
	// with no --platform-prefix: the inventory names the mount.
	spectest.Proves(t, "cli/workload-qualification", "smoke-is-derived-from-the-route-inventory", "the-platform-prefix-is-read-from-the-inventory")
	underscore := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/_/readyz":
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case "/_/version":
			_, _ = w.Write([]byte(`{"name":"prefixed","version":"1.0.0","sha":"` + qualifyTestSHA + `"}`))
		case "/items":
			_, _ = w.Write([]byte(`[]`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(underscore.Close)
	code, out = runQualify(t, root, "/prefixed", "--target", underscore.URL, "--expect-sha", qualifyTestSHA[:9], "--output=json")
	if code != ExitSuccess {
		t.Fatalf("prefixed deployment exit = %d\n%s", code, out)
	}
	if verdict := verdictFromEnvelope(t, decodeEnvelope(t, out)); verdict.State != qualifyproto.StatePassed {
		t.Fatalf("prefixed deployment state = %s, want passed", verdict.State)
	}

	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedURL := "http://" + closed.Addr().String()
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		args  []string
		state qualifyproto.State
	}{
		{"wrong sha", []string{"/app", "--target", server.URL, "--expect-sha", "deadbeef0"}, qualifyproto.StateDigestMismatch},
		{"closed port", []string{"/app", "--target", closedURL, "--expect-sha", "0000000"}, qualifyproto.StateTargetUnreachable},
		{"no inventory", []string{"/bare", "--target", closedURL, "--expect-sha", "0000000"}, qualifyproto.StateUnsupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out := runQualify(t, root, append(tc.args, "--output=json")...)
			if code != ExitError {
				t.Fatalf("exit = %d, want %d\n%s", code, ExitError, out)
			}
			envelope := decodeEnvelope(t, out)
			if envelope.Status != protocolcli.StatusFailure || envelope.ExitCode != ExitError || envelope.Error == nil {
				t.Errorf("envelope = %+v", envelope)
			}
			verdict := verdictFromEnvelope(t, envelope)
			if verdict.State != tc.state {
				t.Errorf("state = %s, want %s", verdict.State, tc.state)
			}
			if tc.state == qualifyproto.StateUnsupported {
				smoke := verdict.Phases[3]
				if len(smoke.Diagnostics) != 1 || !strings.Contains(smoke.Diagnostics[0].Message, "putnami build --projects /bare") {
					t.Errorf("unsupported remedy = %+v", smoke.Diagnostics)
				}
			}

			// Human mode prints the verdict and exits the same way.
			code, text := runQualify(t, root, tc.args...)
			if code != ExitError || !strings.Contains(text, "verdict: "+string(tc.state)+" ") {
				t.Errorf("human: exit %d\n%s", code, text)
			}
		})
	}
}

func TestCmdQualify_UsageErrorsExitTwo(t *testing.T) {
	spectest.Proves(t, "cli/workload-qualification", "remote-verdict-binds-to-the-deployed-sha", "a-url-target-without-expect-sha-is-a-usage-error")
	spectest.Proves(t, "cli/workload-qualification", "local-verdict-binds-to-the-exact-worktree", "expect-sha-with-a-local-target-is-a-usage-error")
	root := qualifyWorkspace(t)
	sha := qualifyTestSHA[:7]
	cases := map[string][]string{
		"url without expect-sha":       {"/app", "--target", "http://127.0.0.1:1"},
		"no target":                    {"/app"},
		"expect-sha with local target": {"/app", "--target", "local", "--expect-sha", sha},
		"non-http target":              {"/app", "--target", "ftp://host", "--expect-sha", sha},
		"credentials in the target":    {"/app", "--target", "https://user:secret@host", "--expect-sha", sha},
		"short expect-sha":             {"/app", "--target", "http://127.0.0.1:1", "--expect-sha", "abc"},
		"print-contract with a target": {"/app", "--print-contract", "--target", "http://127.0.0.1:1"},
		"no project":                   {"--target", "http://127.0.0.1:1", "--expect-sha", sha},
		"two projects":                 {"/app", "/bare", "--print-contract"},
		"zero ready-timeout":           {"/app", "--print-contract", "--ready-timeout", "0s"},
		"malformed request-timeout":    {"/app", "--print-contract", "--request-timeout", "soon"},
		"query in platform prefix":     {"/app", "--print-contract", "--platform-prefix", "/ops?x=1"},
		"unknown flag":                 {"/app", "--print-contract", "--retries", "3"},
		"unknown project":              {"/missing", "--print-contract"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			if code, out := runQualify(t, root, args...); code != ExitUsage {
				t.Errorf("exit = %d, want %d\n%s", code, ExitUsage, out)
			}
		})
	}

	// A committed inventory that fails its own protocol is refused, not guessed past.
	if err := os.WriteFile(filepath.Join(root, "app", "schema", "http-routes.json"), []byte(`{"protocol":"other"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, out := runQualify(t, root, "/app", "--print-contract"); code != ExitUsage {
		t.Errorf("invalid inventory exit = %d\n%s", code, out)
	}

	if err := cmdQualify(&CommandEnv{Ctx: context.Background(), Cfg: &wsproto.Config{}}); err == nil {
		t.Error("qualify outside a workspace must fail")
	}
}

// A local verdict names the worktree it served. Outside a git worktree there is
// no tree to name, so the command fails (exit 1) before composing anything
// rather than emitting a verdict bound to nothing.
func TestCmdQualify_LocalTargetOutsideAWorktreeFailsBeforeComposing(t *testing.T) {
	root := qualifyWorkspace(t)
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(root))
	for _, args := range [][]string{
		{"/app", "--target", "local", "--verbose"},
		{"/app", "--target", "local", "--output=json"},
	} {
		code, out := runQualify(t, root, args...)
		if code != ExitError {
			t.Errorf("%v: exit = %d, want %d\n%s", args, code, ExitError, out)
		}
		if _, err := os.Stat(filepath.Join(root, ".putnami", "compose")); !os.IsNotExist(err) {
			t.Errorf("%v: a composition was started for a tree that cannot be named", args)
		}
	}
}

// TestParseArgs_PositionalLeafKeepsItsArgument pins the parser rule qualify
// depends on, and that it leaves the commands whose handlers read the first
// bare argument as their subcommand untouched.
func TestParseArgs_PositionalLeafKeepsItsArgument(t *testing.T) {
	t.Parallel()
	parsed := ParseArgs([]string{"qualify", "/app", "--print-contract", "--output=json"}, nil, nil)
	if parsed.Err != nil || parsed.Subcommand != "" || len(parsed.RawJobArgs) == 0 || parsed.RawJobArgs[0] != "/app" {
		t.Fatalf("qualify parsed as sub %q args %v err %v", parsed.Subcommand, parsed.RawJobArgs, parsed.Err)
	}
	if code, rejected := rejectStructuredIfUnsupported("json", "qualify", parsed.Subcommand); rejected {
		t.Errorf("--output=json refused for qualify (exit %d)", code)
	}
	if pin := ParseArgs([]string{"pin", "1.2.3"}, nil, nil); pin.Subcommand != "1.2.3" {
		t.Errorf("pin sub = %q, want the version (its handler reads it there)", pin.Subcommand)
	}
	if help := ParseArgs([]string{"help", "build"}, nil, nil); help.Subcommand != "build" {
		t.Errorf("help sub = %q, want build", help.Subcommand)
	}
	for root, want := range map[string]bool{"qualify": true, "pin": false, "help": false, "version": false, "sessions": false, "missing": false} {
		if got := commandmeta.PositionalLeaf(root); got != want {
			t.Errorf("PositionalLeaf(%q) = %v, want %v", root, got, want)
		}
	}
}
