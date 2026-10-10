package distributioncli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

type mirrorCall struct {
	method, path, query string
	body                map[string]any
}

// mirrorCLIFixture wires the linked workspace, the native Put credential and a
// transport recorder, so every assertion below is about what the CLI sent.
func mirrorCLIFixture(t *testing.T, calls *[]mirrorCall) (string, map[string]string, clicore.IO) {
	t.Helper()
	root, env, ioctx := distributionWorkflowFixture(t)
	env["PUTNAMI_REGISTRY_PUT_URL"] = "https://registry.example"
	registryToken := workflowJWT(map[string]any{
		"sub": "user-admin", "aud": "distribution", "scope": "put",
		"exp": ioctx.Now().Add(time.Minute).Unix(),
	})
	if err := storeRegistryAccessToken(env, "registry.example", registryAccessToken{
		AccessToken: registryToken, TokenType: "Bearer", ClientID: DefaultRegistryTokenClientID,
		Scope: "put", ExpiresAt: ioctx.Now().Add(time.Minute).Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatal(err)
	}
	ioctx.Client = &http.Client{Transport: workflowRoundTrip(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host != "registry.example" || request.Header.Get("Authorization") != "Bearer "+registryToken ||
			!strings.HasPrefix(request.URL.Path, "/put/_/release-sets/workspaces/ws-consumer/mirrors") {
			t.Fatalf("mirror command bypassed native Put authority: %s %s", request.Method, request.URL)
		}
		current := mirrorCall{method: request.Method, path: request.URL.Path, query: request.URL.RawQuery}
		if request.Body != nil && (request.Method == http.MethodPost || request.Method == http.MethodPut) {
			if err := json.NewDecoder(request.Body).Decode(&current.body); err != nil {
				t.Fatal(err)
			}
		}
		*calls = append(*calls, current)
		if request.Method == http.MethodGet {
			return workflowJSONResponse(http.StatusOK, map[string]any{"targets": []any{
				map[string]any{"id": "npmjs", "ecosystem": "npm", "destination": "https://registry.npmjs.org"},
				map[string]any{
					"id": "github-cli", "ecosystem": "archive", "destination": "github.com/Putnami/cli",
					"package": "putnami/cli",
				},
			}}), nil
		}
		status := http.StatusOK
		if request.Method == http.MethodPost {
			status = http.StatusCreated
		}
		return workflowJSONResponse(status, map[string]any{
			"id": "npmjs", "owner_workspace_id": "ws-consumer", "ecosystem": "npm",
		}), nil
	})}
	return root, env, ioctx
}

func withStdinCredential(t *testing.T, secret string) {
	t.Helper()
	previous := mirrorCredentialInput
	mirrorCredentialInput = strings.NewReader(secret)
	t.Cleanup(func() { mirrorCredentialInput = previous })
}

func TestMirrorCLIUsesNativePutAndTheLinkedOwnerWorkspace(t *testing.T) {
	var calls []mirrorCall
	root, env, ioctx := mirrorCLIFixture(t, &calls)

	withStdinCredential(t, "npm-owner-token\n")
	if err := Distribution(map[string]any{
		"ecosystem": "npm", "id": "npmjs", "destination": "https://registry.npmjs.org",
		"alias":     []any{"registry.npmjs.org"},
		"workspace": map[string]any{"workspace_id": "ambient-cannot-replace-link"},
	}, []string{"mirrors", "add"}, root, env, ioctx); err != nil {
		t.Fatalf("mirrors add: %v", err)
	}
	withStdinCredential(t, "oci-owner-password")
	if err := Distribution(map[string]any{
		"ecosystem": "oci", "id": "ghcr", "destination": "ghcr.io/putnami", "username": "publisher",
	}, []string{"mirrors", "add"}, root, env, ioctx); err != nil {
		t.Fatalf("mirrors add oci: %v", err)
	}
	for _, command := range []struct {
		args   []string
		params map[string]any
	}{
		{[]string{"mirrors", "list"}, map[string]any{"include-revoked": true}},
		{[]string{"mirrors", "list"}, nil},
		{[]string{"mirrors", "remove", "npmjs"}, nil},
	} {
		if err := Distribution(command.params, command.args, root, env, ioctx); err != nil {
			t.Fatalf("%v: %v", command.args, err)
		}
	}
	withStdinCredential(t, "rotated-token")
	if err := Distribution(nil, []string{"mirrors", "rotate-credential", "npmjs"}, root, env, ioctx); err != nil {
		t.Fatalf("mirrors rotate-credential: %v", err)
	}

	if len(calls) != 6 {
		t.Fatalf("mirror lifecycle made %d requests: %#v", len(calls), calls)
	}
	// The token arrives once, trimmed of its trailing newline, and the OCI
	// target sends username/password instead.
	if calls[0].body["token"] != "npm-owner-token" || calls[0].body["username"] != nil ||
		calls[0].body["destination"] != "https://registry.npmjs.org" {
		t.Fatalf("npm onboarding body = %#v", calls[0].body)
	}
	aliases, _ := calls[0].body["aliases"].([]any)
	if len(aliases) != 1 || aliases[0] != "registry.npmjs.org" {
		t.Fatalf("aliases = %#v", calls[0].body["aliases"])
	}
	if calls[1].body["username"] != "publisher" || calls[1].body["password"] != "oci-owner-password" ||
		calls[1].body["token"] != nil {
		t.Fatalf("oci onboarding body = %#v", calls[1].body)
	}
	if calls[2].query != "include_revoked=true" || calls[3].query != "" {
		t.Fatalf("list queries = %q / %q", calls[2].query, calls[3].query)
	}
	if calls[4].method != http.MethodDelete || calls[4].path != "/put/_/release-sets/workspaces/ws-consumer/mirrors/npmjs" {
		t.Fatalf("remove = %#v", calls[4])
	}
	if calls[5].method != http.MethodPut ||
		calls[5].path != "/put/_/release-sets/workspaces/ws-consumer/mirrors/npmjs/credential" ||
		calls[5].body["token"] != "rotated-token" || len(calls[5].body) != 1 {
		t.Fatalf("rotate = %#v", calls[5])
	}
}

// An archive target copies the platform files of one package to the assets of
// a GitHub Release, so the package is part of the target and GitHub takes a
// token only.
func TestMirrorCLIOnboardsAnArchiveTargetForOnePackage(t *testing.T) {
	var calls []mirrorCall
	root, env, ioctx := mirrorCLIFixture(t, &calls)
	archive := func(extra map[string]any) map[string]any {
		params := map[string]any{"ecosystem": "archive", "id": "github-cli", "destination": "github.com/Putnami/cli"}
		for key, value := range extra {
			params[key] = value
		}
		return params
	}

	for name, refused := range map[string]struct {
		params map[string]any
		want   string
	}{
		"no package":       {archive(nil), "--package"},
		"padded package":   {archive(map[string]any{"package": " putnami/cli"}), "--package"},
		"basic credential": {archive(map[string]any{"package": "putnami/cli", "username": "bot"}), "--username"},
		"npm package": {map[string]any{
			"ecosystem": "npm", "id": "npmjs", "destination": "https://registry.npmjs.org", "package": "putnami/cli",
		}, "--package"},
		// The registry name is not an ecosystem; the refusal names the right one.
		"put ecosystem": {map[string]any{
			"ecosystem": "put", "id": "github-cli", "destination": "github.com/Putnami/cli", "package": "putnami/cli",
		}, "--ecosystem archive"},
	} {
		withStdinCredential(t, "github-token")
		err := Distribution(refused.params, []string{"mirrors", "add"}, root, env, ioctx)
		if err == nil || clicore.ExitCode(err) != clicore.ExitUsage || !strings.Contains(err.Error(), refused.want) {
			t.Fatalf("%s was accepted: %v", name, err)
		}
	}
	if len(calls) != 0 {
		t.Fatalf("a refused archive target reached the network: %#v", calls)
	}

	withStdinCredential(t, "github-token\n")
	if err := Distribution(archive(map[string]any{"package": "putnami/cli"}), []string{"mirrors", "add"}, root, env, ioctx); err != nil {
		t.Fatalf("mirrors add archive: %v", err)
	}
	if len(calls) != 1 || calls[0].body["ecosystem"] != "archive" || calls[0].body["package"] != "putnami/cli" ||
		calls[0].body["destination"] != "github.com/Putnami/cli" || calls[0].body["token"] != "github-token" ||
		calls[0].body["username"] != nil {
		t.Fatalf("archive onboarding body = %#v", calls)
	}

	var lines []string
	ioctx.Stdout = func(line string) { lines = append(lines, line) }
	if err := Distribution(nil, []string{"mirrors", "list"}, root, env, ioctx); err != nil {
		t.Fatalf("mirrors list: %v", err)
	}
	listed := strings.Join(lines, "\n")
	if !strings.Contains(listed, "github-cli") || !strings.Contains(listed, "package=putnami/cli") ||
		strings.Count(listed, "package=") != 1 {
		t.Fatalf("list does not name the archive target's package once:\n%s", listed)
	}
	// The npm and OCI lines keep the columns they had before the third adapter.
	if !slices.Contains(lines, fmt.Sprintf("%-4s %-24s %-40s %s", "npm", "npmjs", "https://registry.npmjs.org", "live")) {
		t.Fatalf("the npm line moved:\n%s", listed)
	}
}

// A credential must never be passable as a flag: it would land in argv and
// shell history. The CLI reads stdin, and an empty stdin is a usage error, not
// an empty credential.
func TestMirrorCLIReadsTheCredentialFromStdinOnly(t *testing.T) {
	var calls []mirrorCall
	root, env, ioctx := mirrorCLIFixture(t, &calls)
	base := map[string]any{"ecosystem": "npm", "id": "npmjs", "destination": "https://registry.npmjs.org"}

	for _, empty := range []string{"", "\n", "   \n"} {
		withStdinCredential(t, empty)
		err := Distribution(base, []string{"mirrors", "add"}, root, env, ioctx)
		if err == nil || clicore.ExitCode(err) != clicore.ExitUsage || !strings.Contains(err.Error(), "standard input") {
			t.Fatalf("empty stdin %q was accepted: %v", empty, err)
		}
	}
	withStdinCredential(t, strings.Repeat("x", (16<<10)+1))
	if err := Distribution(base, []string{"mirrors", "add"}, root, env, ioctx); err == nil ||
		!strings.Contains(err.Error(), "too large") {
		t.Fatalf("an oversized stdin credential was accepted: %v", err)
	}
	if len(calls) != 0 {
		t.Fatalf("a credential-less command reached the network: %#v", calls)
	}
	// A --token flag is not a recognized parameter, so it cannot smuggle a
	// credential past the stdin requirement.
	withStdinCredential(t, "")
	flagged := map[string]any{
		"ecosystem": "npm", "id": "npmjs", "destination": "https://registry.npmjs.org", "token": "from-argv",
	}
	if err := Distribution(flagged, []string{"mirrors", "add"}, root, env, ioctx); err == nil ||
		!strings.Contains(err.Error(), "standard input") {
		t.Fatalf("--token substituted for stdin: %v", err)
	}
}

func TestMirrorCLIValidatesTheTargetBeforeCredentials(t *testing.T) {
	for _, params := range []map[string]any{
		nil,
		{"ecosystem": "npm", "id": "npmjs"},
		{"ecosystem": "npm", "destination": "https://registry.npmjs.org"},
		{"id": "npmjs", "destination": "https://registry.npmjs.org"},
		{"ecosystem": "go", "id": "proxy", "destination": "https://proxy.golang.org"},
		{"ecosystem": "npm", "id": " npmjs ", "destination": "https://registry.npmjs.org"},
	} {
		err := Distribution(params, []string{"mirrors", "add"}, "", nil, clicore.IO{})
		if err == nil || clicore.ExitCode(err) != clicore.ExitUsage || !strings.Contains(err.Error(), "--ecosystem") {
			t.Fatalf("incomplete command %#v reached credential resolution: %v", params, err)
		}
	}
	complete := map[string]any{"ecosystem": "npm", "id": "npmjs", "destination": "https://registry.npmjs.org"}
	for _, alias := range []any{" spaced", "", map[string]any{"to": "x"}} {
		params := map[string]any{}
		for key, value := range complete {
			params[key] = value
		}
		params["alias"] = []any{alias}
		if err := Distribution(params, []string{"mirrors", "add"}, "", nil, clicore.IO{}); err == nil ||
			!strings.Contains(err.Error(), "--alias") {
			t.Fatalf("alias %#v was accepted: %v", alias, err)
		}
	}
}

func TestMirrorCLIRefusesRetiredAuthorityFlagsAndIncompletePositionals(t *testing.T) {
	for _, operation := range []string{"add", "list", "remove", "rotate-credential"} {
		params := map[string]any{
			"ecosystem": "npm", "id": "npmjs", "destination": "https://registry.npmjs.org", "namespace": "retired",
		}
		err := Distribution(params, []string{"mirrors", operation, "npmjs"}, "", nil, clicore.IO{})
		if err == nil || clicore.ExitCode(err) != clicore.ExitUsage || !strings.Contains(err.Error(), "--namespace") {
			t.Fatalf("mirrors %s --namespace did not fail before credentials: %v", operation, err)
		}
	}
	// --package names an archive target's package on add only; elsewhere it is
	// still the retired grant authority.
	for _, command := range [][]string{
		{"mirrors", "list"}, {"mirrors", "remove", "npmjs"}, {"mirrors", "rotate-credential", "npmjs"},
		{"grants", "list"}, {"grants", "create"},
	} {
		err := Distribution(map[string]any{"package": "putnami/cli"}, command, "", nil, clicore.IO{})
		if err == nil || clicore.ExitCode(err) != clicore.ExitUsage || !strings.Contains(err.Error(), "--package") {
			t.Fatalf("%v --package did not fail before credentials: %v", command, err)
		}
	}
	for _, operation := range []string{"remove", "rotate-credential"} {
		err := Distribution(nil, []string{"mirrors", operation}, "", nil, clicore.IO{})
		if err == nil || clicore.ExitCode(err) != clicore.ExitUsage || !strings.Contains(err.Error(), "<id>") {
			t.Fatalf("mirrors %s without an id: %v", operation, err)
		}
	}
	if err := Distribution(nil, []string{"mirrors", "teleport"}, "", nil, clicore.IO{}); err == nil ||
		!strings.Contains(err.Error(), "mirrors <add|list|remove|rotate-credential>") {
		t.Fatalf("unknown mirrors subcommand: %v", err)
	}
}

func TestDistributionHelpDocumentsTheMirrorCommands(t *testing.T) {
	var lines []string
	ioctx := clicore.IO{Stdout: func(line string) { lines = append(lines, line) }}
	if err := Distribution(nil, []string{"help"}, "", nil, ioctx); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(lines, "\n")
	for _, command := range []string{
		"cloud packages mirrors add", "cloud packages mirrors list",
		"cloud packages mirrors remove", "cloud packages mirrors rotate-credential",
	} {
		if !strings.Contains(joined, command) {
			t.Errorf("help does not document %q:\n%s", command, joined)
		}
	}
	if !strings.Contains(joined, "stdin") {
		t.Errorf("help does not say where the credential comes from:\n%s", joined)
	}
}
