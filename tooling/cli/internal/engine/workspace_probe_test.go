package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	extproto "go.putnami.dev/protocol/extension"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

func probeTestWorkspace(t *testing.T) *workspace.Workspace {
	t.Helper()
	root := t.TempDir()
	write := func(rel, content string) {
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("putnami.workspace.json", `{"includes":["apps"]}`)
	// Core no longer reads package.json (slice C4b), so the scope namePattern is
	// what gives a convention-named project its resolved name.
	write("apps/putnami.json", `{"includes":["web"],"publishConfig":{"npm":{"namePattern":"@acme/{name}"}}}`)
	write("apps/web/package.json", `{"version":"0.0.0"}`)

	workspace.InvalidateLoadCache(root)
	t.Cleanup(func() { workspace.InvalidateLoadCache(root) })
	ws, err := workspace.Load(root)
	if err != nil {
		t.Fatalf("load workspace: %v", err)
	}
	return ws
}

// An extension with no workspace adapter is never probed. That is what keeps
// the cost of this phase proportional to the number of extensions that actually
// own project metadata — today, one.
func TestWorkspaceProviderBindings_OnlyAdapterDeclaringExtensions(t *testing.T) {
	t.Parallel()
	ws := probeTestWorkspace(t)
	discovered := &extension.DiscoveryResult{Extensions: []*extension.ExtensionDescription{
		{Name: "@putnami/go"},
		{Name: "@putnami/typescript", Workspace: &extproto.WorkspaceAdapter{
			Markers: []string{"package.json"}, Inputs: []string{"package.json"},
		}},
		{Name: "@putnami/broken", Workspace: &extproto.WorkspaceAdapter{Markers: []string{"x"}}},
		nil,
	}}

	bindings := workspaceProviderBindings(context.Background(), ws, discovered, nil)
	if len(bindings) != 1 {
		t.Fatalf("bindings = %d, want only the adapter-declaring, well-formed extension: %+v", len(bindings), bindings)
	}
	if bindings[0].Scope.Extension != "@putnami/typescript" {
		t.Fatalf("bound %q", bindings[0].Scope.Extension)
	}
}

// Runtime preparation must be LAZY: a workspace whose snapshot validates never
// starts a provider, so it must never compile one either. The binding therefore
// carries a resolver rather than a resolved path.
func TestWorkspaceProviderBindings_DeferRuntimePreparation(t *testing.T) {
	t.Parallel()
	ws := probeTestWorkspace(t)
	discovered := &extension.DiscoveryResult{Extensions: []*extension.ExtensionDescription{
		{Name: "@putnami/typescript", Workspace: &extproto.WorkspaceAdapter{
			Markers: []string{"package.json"}, Inputs: []string{"package.json"},
		}},
	}}

	bindings := workspaceProviderBindings(context.Background(), ws, discovered, nil)
	provider, ok := bindings[0].Provider.(*workspace.ExecProbeProvider)
	if !ok {
		t.Fatalf("provider = %T, want the exec-backed one", bindings[0].Provider)
	}
	if provider.Executable != "" {
		t.Errorf("binding carries a resolved executable %q; preparation must not have run yet", provider.Executable)
	}
	if provider.Resolve == nil {
		t.Error("binding carries no resolver; the probe would be permanently unavailable")
	}
	if provider.Dir != ws.Root {
		t.Errorf("provider Dir = %q, want the workspace root %q", provider.Dir, ws.Root)
	}
}

// The failure policy is the asymmetry: a graph-dependent command cannot run on
// a half-known workspace, and the commands that REPAIR the workspace must
// survive — or one broken extension makes a workspace unrecoverable.
func TestReportProbeFailure_RecoveryPathsSurvive(t *testing.T) {
	t.Parallel()
	failure := wsproto.NewProbeFailure(wsproto.ProbeFailureUnavailable, "@putnami/typescript", "runtime missing")

	cases := []struct {
		name string
		req  Request
		want int
	}{
		{"build fails", Request{Commands: []string{"build"}}, ExitError},
		{"test fails", Request{Commands: []string{"test"}}, ExitError},
		{"install survives", Request{Commands: []string{"install"}}, ExitSuccess},
		{"projects sync survives", Request{Commands: []string{"projects sync"}}, ExitSuccess},
		{"workspace lifecycle survives", Request{
			Commands: []string{"workspace-install"}, WorkspaceLifecycle: true,
		}, ExitSuccess},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := c.req
			req.Global.Quiet = true
			if got := reportProbeFailure(&req, failure); got != c.want {
				t.Fatalf("reportProbeFailure = %d, want %d", got, c.want)
			}
		})
	}
}

func TestReportProbeFailure_UnclassifiedErrorStillFails(t *testing.T) {
	t.Parallel()
	req := Request{Commands: []string{"build"}}
	req.Global.Quiet = true
	if got := reportProbeFailure(&req, os.ErrClosed); got != ExitError {
		t.Fatalf("reportProbeFailure = %d, want ExitError for an unclassified error", got)
	}
}

// A refused graph states its own repair; the generic re-probe advice would
// send the user to commands that edit no import.
func TestReportProbeFailure_RefusedGraphNamesItsOwnRepair(t *testing.T) {
	failure := wsproto.NewProbeFailure(wsproto.ProbeFailureVisibility, "", "1 import(s) cross a scope boundary")
	failure.Diagnostics = []diag.Diagnostic{diag.Errorf("workspace.visibility_violation", "/apps/api", "imports /libs/core")}
	req := Request{Commands: []string{"build"}}
	var code int
	stderr := captureStderr(t, func() { code = reportProbeFailure(&req, failure) })
	if code != ExitError {
		t.Fatalf("reportProbeFailure = %d, want ExitError", code)
	}
	if !strings.Contains(stderr, "[error] /apps/api: imports /libs/core") {
		t.Fatalf("stderr = %q, want each finding printed", stderr)
	}
	if strings.Contains(stderr, "to repair the workspace") {
		t.Fatalf("stderr = %q, want no generic re-probe advice", stderr)
	}
}

// A workspace with no adapter-declaring extension must not pay for this phase
// at all, and must not fail because of it.
func TestSynchronizeWorkspaceProbe_NoAdaptersIsANoOp(t *testing.T) {
	t.Parallel()
	ws := probeTestWorkspace(t)
	req := &Request{WorkspaceRoot: ws.Root, Commands: []string{"build"}}
	discovered := &extension.DiscoveryResult{Extensions: []*extension.ExtensionDescription{{Name: "@putnami/go"}}}

	if code := synchronizeWorkspaceProbe(context.Background(), req, ws, discovered); code != ExitSuccess {
		t.Fatalf("synchronizeWorkspaceProbe = %d, want ExitSuccess", code)
	}
	if _, err := os.Stat(workspace.SnapshotPath(ws.Root)); !os.IsNotExist(err) {
		t.Errorf("a run with no adapters touched the workspace index (stat err = %v)", err)
	}
}

// The reason a provider is asked is advisory, but a preview must announce
// itself: it is the run whose view will not be persisted.
func TestProbeReasonFor(t *testing.T) {
	t.Parallel()
	load := &Request{}
	if got := probeReasonFor(load); got != wsproto.ProbeReasonLoad {
		t.Errorf("ordinary run reason = %q, want %q", got, wsproto.ProbeReasonLoad)
	}
	plan := &Request{}
	plan.Global.Plan = true
	if got := probeReasonFor(plan); got != wsproto.ProbeReasonPlan {
		t.Errorf("--plan reason = %q, want %q", got, wsproto.ProbeReasonPlan)
	}
	dryRun := &Request{}
	dryRun.Global.DryRun = true
	if got := probeReasonFor(dryRun); got != wsproto.ProbeReasonPlan {
		t.Errorf("--dry-run reason = %q, want %q", got, wsproto.ProbeReasonPlan)
	}
	// An extension alias that forwards --dry-run as a job param really runs.
	alias := &Request{ExecutesUnderDryRun: true}
	alias.Global.DryRun = true
	if got := probeReasonFor(alias); got != wsproto.ProbeReasonLoad {
		t.Errorf("alias dry-run reason = %q, want %q", got, wsproto.ProbeReasonLoad)
	}
}

// C3b's old/new equivalence guard deleted itself with its subject in slice C4b:
// with core's parsers gone there is nothing left for a provider to disagree
// WITH. What this phase still reports is the divergence `projects sync` exists
// to align — a manifest on disk spelling the project differently from the name
// the workspace resolved for it.
func TestNameDivergences_ReportsAnUnalignedManifest(t *testing.T) {
	t.Parallel()
	ws := probeTestWorkspace(t)

	// The probe found a package.json that declares no name, so the scope
	// namePattern is authoritative and the manifest on disk is out of alignment:
	// exactly the state the TypeScript extension's sync task repairs.
	ws.AdoptProbeView(map[string]wsproto.MergedProject{"apps/web": {Path: "apps/web"}})
	findings := workspace.NameDivergences(ws)
	if len(findings) != 1 || !strings.Contains(findings[0], "@acme/web") {
		t.Fatalf("findings = %v, want one naming the resolved name", findings)
	}
	if !strings.Contains(findings[0], `"web"`) {
		t.Errorf("finding = %q, want it to name the source identity too", findings[0])
	}

	// Once the manifest declares it, the identity outranks the pattern and there
	// is nothing left to align.
	ws.AdoptProbeView(map[string]wsproto.MergedProject{"apps/web": {Path: "apps/web", SourceName: "@acme/web"}})
	if findings := workspace.NameDivergences(ws); len(findings) != 0 {
		t.Fatalf("an aligned manifest reported a divergence: %v", findings)
	}
}
