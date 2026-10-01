package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/sdk/extension/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/registrycred"

	"go.putnami.dev/protocol/features/spectest"
)

// stubGoCredential replaces the seam and records the hosts it was asked about.
func stubGoCredential(t *testing.T, outcome registrycred.Outcome) *[]string {
	t.Helper()
	original := ensureGoRegistryCredential
	t.Cleanup(func() { ensureGoRegistryCredential = original })
	var hosts []string
	ensureGoRegistryCredential = func(_ context.Context, _ string, host string) registrycred.Outcome {
		hosts = append(hosts, host)
		return outcome
	}
	return &hosts
}

// declaredOriginHost is the vanity module server the test workspaces declare. It
// is not go.putnami.dev on purpose: the origin is DECLARED by a workspace, never
// built into the tooling.
const declaredOriginHost = "modules.example.test"

// declaringWorkspace returns a workspace root that declares a Go module origin,
// the same way a real one does.
func declaringWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	body := `{"registries":{"go":{"origin":"https://` + declaredOriginHost + `"}}}`
	if err := os.WriteFile(filepath.Join(root, "putnami.workspace.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func captureLogs(t *testing.T, fn func(*jsonl.Emitter)) []map[string]any {
	t.Helper()
	path := filepath.Join(t.TempDir(), "events.jsonl")
	out, err := os.Create(path) //nolint:gosec // test fixture path
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = out
	func() {
		defer func() {
			os.Stdout = original
			_ = out.Close()
		}()
		fn(jsonl.New())
	}()
	data, err := os.ReadFile(path) //nolint:gosec // test fixture path
	if err != nil {
		t.Fatal(err)
	}
	var events []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("decode %q: %v", line, err)
		}
		events = append(events, event)
	}
	return events
}

// TestCommandHandlersWrapEveryModuleFetchingJob pins the choke point: the
// dispatch table. A new job that runs `go` must be listed, or it will fetch from
// the module origin with no credential.
func TestCommandHandlersWrapEveryModuleFetchingJob(t *testing.T) {
	handlers := commandHandlers()
	for name := range moduleFetchingJobs {
		if _, ok := handlers[name]; !ok {
			t.Errorf("moduleFetchingJobs names %q, which is not a dispatched job", name)
		}
	}
	for _, name := range []string{"build", "test", "lint", "workspace-sync"} {
		if _, ok := handlers[name]; !ok {
			t.Errorf("dispatch table lost %q", name)
		}
	}
	// Local-only jobs are deliberately not wrapped.
	for _, name := range []string{"config-extract", "config-merge"} {
		if moduleFetchingJobs[name] {
			t.Errorf("%q reaches no registry and must not mint a credential", name)
		}
	}
}

func TestWithRegistryCredential_RefreshesBeforeAFetchingJobAndNeverBlocksIt(t *testing.T) {
	root := declaringWorkspace(t)
	hosts := stubGoCredential(t, registrycred.Outcome{
		Kind: registrycred.KindNotSignedIn, Level: "warn",
		Message: "not signed in for " + declaredOriginHost + "; " + registrycred.SignInHint,
	})
	t.Setenv("NETRC", "")
	t.Setenv("GO_REGISTRY_URL", "")

	ran := false
	job := withRegistryCredential("build", func(*pctx.Context, *jsonl.Emitter, []string) (string, map[string]any, error) {
		if len(*hosts) != 1 {
			t.Errorf("the credential was not refreshed before the job ran: %v", *hosts)
		}
		ran = true
		return "OK", nil, nil
	})

	ctx := &pctx.Context{WorkspaceRoot: root}
	var status string
	events := captureLogs(t, func(emit *jsonl.Emitter) {
		status, _, _ = job(ctx, emit, nil)
	})

	if !ran || status != "OK" {
		t.Fatalf("job ran=%v status=%q; a failed refresh must never block the job", ran, status)
	}
	if len(*hosts) != 1 || (*hosts)[0] != declaredOriginHost {
		t.Fatalf("hosts = %v, want [%s]", *hosts, declaredOriginHost)
	}
	var warned bool
	for _, e := range events {
		if e["level"] == "warn" && strings.Contains(e["message"].(string), registrycred.SignInHint) {
			warned = true
		}
	}
	if !warned {
		t.Errorf("no sign-in warning emitted: %#v", events)
	}
}

func TestWithRegistryCredential_LeavesLocalJobsAlone(t *testing.T) {
	root := declaringWorkspace(t)
	hosts := stubGoCredential(t, registrycred.Outcome{Kind: registrycred.KindMaterialized})
	t.Setenv("NETRC", "")

	var inner cli.JobFunc = func(*pctx.Context, *jsonl.Emitter, []string) (string, map[string]any, error) {
		return "OK", nil, nil
	}
	job := withRegistryCredential("config-extract", inner)
	if _, _, err := job(&pctx.Context{WorkspaceRoot: root}, jsonl.New(), nil); err != nil {
		t.Fatal(err)
	}
	if len(*hosts) != 0 {
		t.Errorf("a local job asked the cloud about %v, want no call", *hosts)
	}
}

func TestRefreshModuleOriginCredential_SkipsWithoutADeclaredOrigin(t *testing.T) {
	hosts := stubGoCredential(t, registrycred.Outcome{Kind: registrycred.KindMaterialized})
	t.Setenv("NETRC", "")
	t.Setenv("GO_REGISTRY_URL", "")

	// A workspace that declares no vanity module server has no private modules.
	refreshModuleOriginCredential(&pctx.Context{WorkspaceRoot: t.TempDir()}, jsonl.New())
	if len(*hosts) != 0 {
		t.Errorf("undeclared origin asked the cloud about %v, want no call", *hosts)
	}
}

// TestRefreshModuleOriginCredential_RefreshesACallerPinnedNetrc covers a
// caller that sets NETRC: the cloud writes the entry into the file NETRC names,
// so the refresh still runs, or that file's entry expires and the origin
// answers 401.
func TestRefreshModuleOriginCredential_RefreshesACallerPinnedNetrc(t *testing.T) {
	root := declaringWorkspace(t)
	hosts := stubGoCredential(t, registrycred.Outcome{Kind: registrycred.KindMaterialized})
	t.Setenv("NETRC", filepath.Join(t.TempDir(), "caller.netrc"))

	refreshModuleOriginCredential(&pctx.Context{WorkspaceRoot: root}, jsonl.New())
	if len(*hosts) != 1 {
		t.Errorf("a caller-pinned NETRC asked the cloud about %v, want one call", *hosts)
	}
}

// TestRefreshModuleOriginCredential_SkipsOffline pins the hosted-run half: with
// the offline signal no job asks for a credential or writes one into the home,
// even for a workspace that declares an origin and pins no NETRC.
func TestRefreshModuleOriginCredential_SkipsOffline(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "hosted-run-dependencies",
		"an-offline-job-refreshes-no-credential")
	root := declaringWorkspace(t)
	hosts := stubGoCredential(t, registrycred.Outcome{Kind: registrycred.KindMaterialized})
	t.Setenv("NETRC", "")
	t.Setenv("GO_REGISTRY_URL", "")
	t.Setenv("PUTNAMI_OFFLINE_DEPENDENCIES", "1")

	refreshModuleOriginCredential(&pctx.Context{WorkspaceRoot: root}, jsonl.New())
	if len(*hosts) != 0 {
		t.Errorf("an offline job asked the cloud about %v, want no call", *hosts)
	}

	// The same workspace without the signal does ask: the skip is the signal's.
	t.Setenv("PUTNAMI_OFFLINE_DEPENDENCIES", "")
	refreshModuleOriginCredential(&pctx.Context{WorkspaceRoot: root}, jsonl.New())
	if len(*hosts) != 1 || (*hosts)[0] != declaredOriginHost {
		t.Fatalf("hosts = %v, want [%s] without the offline signal", *hosts, declaredOriginHost)
	}
}

func TestRefreshModuleOriginCredential_PrefersTheEnvironmentDeclaration(t *testing.T) {
	root := declaringWorkspace(t)
	hosts := stubGoCredential(t, registrycred.Outcome{Kind: registrycred.KindMaterialized})
	t.Setenv("NETRC", "")
	t.Setenv("GO_REGISTRY_URL", "https://go.putnami.dev")

	refreshModuleOriginCredential(&pctx.Context{WorkspaceRoot: root}, jsonl.New())
	if len(*hosts) != 1 || (*hosts)[0] != "go.putnami.dev" {
		t.Fatalf("hosts = %v, want [go.putnami.dev] — GO_REGISTRY_URL wins", *hosts)
	}
}
