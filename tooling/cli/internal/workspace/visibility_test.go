package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"

	diag "go.putnami.dev/protocol/diagnostic"
	wsproto "go.putnami.dev/protocol/workspace"
)

// visibilityFixture is two scopes, `libs` and `apps`, and one real import that
// crosses from the second into the first. The importing edge is a go.mod
// require, so the provider answer below attributes it as an import.
//
// libsVisibility is written into the imported project's putnami.json, so one
// fixture covers both the refused and the allowed shape. extraIncludes adds
// workspace members a test writes itself.
func visibilityFixture(t *testing.T, mode string, libsVisibility string, extraIncludes ...string) (root string, ws *Workspace) {
	t.Helper()
	root = t.TempDir()
	return root, loadVisibilityFixture(t, root, mode, libsVisibility, extraIncludes...)
}

func loadVisibilityFixture(t *testing.T, root, mode, libsVisibility string, extraIncludes ...string) *Workspace {
	t.Helper()
	options := ""
	if mode != "" {
		options = `,"options":{"workspace":{"visibility":"` + mode + `"}}`
	}
	includes := `"libs/core","apps/api"`
	for _, include := range extraIncludes {
		includes += `,"` + include + `"`
	}
	writeFileAt(t, filepath.Join(root, "putnami.workspace.json"),
		`{"includes":[`+includes+`]`+options+`}`)
	writeFileAt(t, filepath.Join(root, "libs", "putnami.json"), `{"tags":["lib"]}`)
	writeFileAt(t, filepath.Join(root, "apps", "putnami.json"), `{"tags":["app"]}`)

	visibility := ""
	if libsVisibility != "" {
		visibility = `,"visibility":"` + libsVisibility + `"`
	}
	writeFileAt(t, filepath.Join(root, "libs", "core", "putnami.json"),
		`{"name":"acme/core"`+visibility+`}`)
	writeFileAt(t, filepath.Join(root, "libs", "core", "go.mod"), "module acme/core\n\ngo 1.25\n")
	writeFileAt(t, filepath.Join(root, "apps", "api", "putnami.json"), `{"name":"acme/api"}`)
	writeFileAt(t, filepath.Join(root, "apps", "api", "go.mod"),
		"module acme/api\n\ngo 1.25\n\nrequire acme/core v0.0.0\n")

	InvalidateLoadCache(root)
	t.Cleanup(func() { InvalidateLoadCache(root) })
	loaded, err := Load(root)
	if err != nil {
		t.Fatalf("load workspace: %v", err)
	}
	return loaded
}

// visibilityProvider answers the fixture's graph: apps/api imports libs/core
// through its go.mod.
func visibilityProvider(t *testing.T) []ProviderBinding {
	t.Helper()
	provider := &countingProvider{
		name: "@acme/go",
		answer: func(wsproto.ProbeRequest) wsproto.ProbeResult {
			return wsproto.ProbeResult{
				Version:   wsproto.ProbeProtocolVersion,
				Extension: "@acme/go",
				Projects: []wsproto.ProbeProject{
					{Path: "libs/core", SourceName: "acme/core", SourceFile: "libs/core/go.mod"},
					{
						Path:              "apps/api",
						SourceName:        "acme/api",
						SourceFile:        "apps/api/go.mod",
						Dependencies:      []string{"libs/core"},
						DependencySources: map[string]wsproto.DependencySource{"libs/core": wsproto.DependencySourceGoModule},
					},
				},
			}
		},
	}
	return []ProviderBinding{{
		Scope:    scopeFor(t, "@acme/go", []string{"go.mod"}, []string{"**/go.mod"}),
		Provider: provider,
	}}
}

func visibilityFindingsIn(diagnostics []diag.Diagnostic) []diag.Diagnostic {
	var found []diag.Diagnostic
	for _, d := range diagnostics {
		if d.Code == VisibilityDiagnosticCode {
			found = append(found, d)
		}
	}
	return found
}

func TestSynchronizeRefusesACrossScopeImportByDefault(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "import-visibility-check", "enforce-is-the-default-and-a-declared-report-changes-no-exit-code")
	_, ws := visibilityFixture(t, "", "")
	_, err := Synchronize(SyncRequest{
		Workspace: ws, Providers: visibilityProvider(t), Reason: wsproto.ProbeReasonLoad,
	})
	failure := &wsproto.ProbeFailure{}
	if !errors.As(err, &failure) || failure.Kind != wsproto.ProbeFailureVisibility {
		t.Fatalf("err = %v, want the visibility refusal: a workspace that declares no mode enforces", err)
	}
	if len(visibilityFindingsIn(failure.Diagnostics)) != 1 {
		t.Fatalf("diagnostics = %v, want the violated edge attached", failure.Diagnostics)
	}
	for _, want := range []string{`"enforce", the default`, `visibility "report" to see them as warnings`} {
		if !strings.Contains(failure.Message, want) {
			t.Fatalf("message = %q, want it to contain %q", failure.Message, want)
		}
	}
}

func TestSynchronizeAcceptsACrossScopeImportOfAGeneratedClient(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "import-visibility-check", "the-check-runs-on-the-synchronized-graph")
	root := t.TempDir()
	// libs/core is a generated client of the service libs/svc commits.
	writeContractFile(t, root, "libs/core/client.putnami.json", clientManifestBody("acme.svc", testContractDigest))
	writeContractFile(t, root, "libs/svc/schema/openapi.json", contractBody("acme.svc"))
	writeFileAt(t, filepath.Join(root, "libs", "svc", "putnami.json"), `{"name":"acme/svc"}`)
	ws := loadVisibilityFixture(t, root, "", "", "libs/svc")

	outcome, err := Synchronize(SyncRequest{
		Workspace: ws, Providers: visibilityProvider(t), Reason: wsproto.ProbeReasonLoad,
	})
	if err != nil {
		t.Fatalf("Synchronize: %v, want the import of a generated client allowed under the default", err)
	}
	if findings := visibilityFindingsIn(outcome.Diagnostics); len(findings) != 0 {
		t.Fatalf("findings = %v, want none", findings)
	}
}

func TestSynchronizeReportsACrossScopeImportAsAWarningWhenReportIsDeclared(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "import-visibility-check", "enforce-is-the-default-and-a-declared-report-changes-no-exit-code")
	_, ws := visibilityFixture(t, string(VisibilityModeReport), "")
	outcome, err := Synchronize(SyncRequest{
		Workspace: ws, Providers: visibilityProvider(t), Reason: wsproto.ProbeReasonLoad,
	})
	if err != nil {
		t.Fatalf("Synchronize: %v", err)
	}
	findings := visibilityFindingsIn(outcome.Diagnostics)
	if len(findings) != 1 {
		t.Fatalf("findings = %v, want exactly one", outcome.Diagnostics)
	}
	if findings[0].Severity != diag.Warning {
		t.Fatalf("severity = %q, want a warning: a declared report mode does not refuse", findings[0].Severity)
	}
	if !strings.Contains(findings[0].Message, "/libs/core") || !strings.Contains(findings[0].Message, "go-module") {
		t.Fatalf("message does not name the edge and its source: %q", findings[0].Message)
	}
}

func TestSynchronizeAcceptsACrossScopeImportOfAPublicProject(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "import-visibility-check", "the-check-runs-on-the-synchronized-graph")
	_, ws := visibilityFixture(t, "", string(wsproto.VisibilityPublic))
	outcome, err := Synchronize(SyncRequest{
		Workspace: ws, Providers: visibilityProvider(t), Reason: wsproto.ProbeReasonLoad,
	})
	if err != nil {
		t.Fatalf("Synchronize: %v", err)
	}
	if findings := visibilityFindingsIn(outcome.Diagnostics); len(findings) != 0 {
		t.Fatalf("findings = %v, want none", findings)
	}
}

func TestSynchronizeRefusesACrossScopeImportUnderEnforce(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "import-visibility-check", "enforce-refuses-the-graph-and-keeps-the-repair-commands-reachable")
	root, ws := visibilityFixture(t, string(VisibilityModeEnforce), "")
	_, err := Synchronize(SyncRequest{
		Workspace: ws, Providers: visibilityProvider(t), Reason: wsproto.ProbeReasonLoad,
		Policy: SnapshotWritePolicy{},
	})
	failure := &wsproto.ProbeFailure{}
	if !errors.As(err, &failure) {
		t.Fatalf("err = %v, want a typed probe failure", err)
	}
	if failure.Kind != wsproto.ProbeFailureVisibility {
		t.Fatalf("kind = %q, want %q", failure.Kind, wsproto.ProbeFailureVisibility)
	}
	if len(visibilityFindingsIn(failure.Diagnostics)) != 1 {
		t.Fatalf("diagnostics = %v, want the violated edge attached", failure.Diagnostics)
	}
	if !strings.Contains(failure.Message, `"enforce", declared in putnami.workspace.json`) {
		t.Fatalf("message = %q, want it to name the declared switch", failure.Message)
	}
	if !RefusesAUsableGraph(failure) {
		t.Fatal("RefusesAUsableGraph = false, want the recorded answer usable by a reader")
	}
	// A refused run keeps the index it just wrote: the tree did not move, and
	// re-probing every later command would punish the workspace twice.
	if _, statErr := os.Stat(filepath.Join(root, ".putnami", WorkspaceIndexFilename)); statErr != nil {
		t.Fatalf("workspace index was not persisted before the refusal: %v", statErr)
	}
	// A recovery command survives the refusal; a graph-dependent one does not.
	if guardErr := RequireProbe("install", failure); guardErr != nil {
		t.Fatalf("RequireProbe(install) = %v, want the repair path to stay reachable", guardErr)
	}
	if guardErr := RequireProbe("build", failure); guardErr == nil {
		t.Fatal("RequireProbe(build) = nil, want the graph-dependent command refused")
	}
}

func TestSynchronizeSkipsTheCheckWhenItIsOff(t *testing.T) {
	_, ws := visibilityFixture(t, string(VisibilityModeOff), "")
	outcome, err := Synchronize(SyncRequest{
		Workspace: ws, Providers: visibilityProvider(t), Reason: wsproto.ProbeReasonLoad,
	})
	if err != nil {
		t.Fatalf("Synchronize: %v", err)
	}
	if findings := visibilityFindingsIn(outcome.Diagnostics); len(findings) != 0 {
		t.Fatalf("findings = %v, want none when the check is off", findings)
	}
}

func TestSynchronizeIgnoresADeclaredEdgeWithNoImport(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "import-visibility-check", "the-check-runs-on-the-synchronized-graph")
	root := t.TempDir()
	writeFileAt(t, filepath.Join(root, "putnami.workspace.json"), `{"includes":["libs/core","apps/api"]}`)
	writeFileAt(t, filepath.Join(root, "libs", "putnami.json"), `{"tags":["lib"]}`)
	writeFileAt(t, filepath.Join(root, "apps", "putnami.json"), `{"tags":["app"]}`)
	writeFileAt(t, filepath.Join(root, "libs", "core", "putnami.json"), `{"name":"acme/core"}`)
	writeFileAt(t, filepath.Join(root, "apps", "api", "putnami.json"),
		`{"name":"acme/api","dependencies":["acme/core"]}`)

	InvalidateLoadCache(root)
	t.Cleanup(func() { InvalidateLoadCache(root) })
	ws, err := Load(root)
	if err != nil {
		t.Fatalf("load workspace: %v", err)
	}
	outcome, err := Synchronize(SyncRequest{Workspace: ws, Reason: wsproto.ProbeReasonLoad})
	if err != nil {
		t.Fatalf("Synchronize: %v", err)
	}
	if findings := visibilityFindingsIn(outcome.Diagnostics); len(findings) != 0 {
		t.Fatalf("findings = %v, want none: a declared edge is not an import", findings)
	}
}

func TestResolveVisibilityMode(t *testing.T) {
	mode, err := ResolveVisibilityMode(nil)
	if err != nil || mode != VisibilityModeEnforce {
		t.Fatalf("ResolveVisibilityMode(nil) = (%q, %v), want (%q, nil)", mode, err, VisibilityModeEnforce)
	}
	mode, err = ResolveVisibilityMode(&wsproto.Config{})
	if err != nil || mode != VisibilityModeEnforce {
		t.Fatalf("ResolveVisibilityMode(no switch) = (%q, %v), want (%q, nil)", mode, err, VisibilityModeEnforce)
	}

	for _, declared := range ValidVisibilityModes {
		mode, err = ResolveVisibilityMode(workspaceVisibilityConfig(string(declared)))
		if err != nil || mode != declared {
			t.Fatalf("ResolveVisibilityMode(%q) = (%q, %v)", declared, mode, err)
		}
	}

	if _, err = ResolveVisibilityMode(workspaceVisibilityConfig("warn")); err == nil {
		t.Fatal("an unknown mode was accepted; a typo must not decide whether a violation fails a run")
	}
	if _, err = ResolveVisibilityMode(workspaceVisibilityConfig(true)); err == nil {
		t.Fatal("a non-string mode was accepted")
	}
}

// workspaceVisibilityConfig is one workspace document declaring one switch
// value, whatever its JSON type.
func workspaceVisibilityConfig(value any) *wsproto.Config {
	options := map[string]map[string]any{"workspace": {"visibility": value}}
	return &wsproto.Config{Options: options}
}

func TestSynchronizeNamesAnUnreadableModeInTheRefusal(t *testing.T) {
	_, ws := visibilityFixture(t, "warn", "")
	_, err := Synchronize(SyncRequest{
		Workspace: ws, Providers: visibilityProvider(t), Reason: wsproto.ProbeReasonLoad,
	})
	failure := &wsproto.ProbeFailure{}
	if !errors.As(err, &failure) || failure.Kind != wsproto.ProbeFailureVisibility {
		t.Fatalf("err = %v, want the refusal of the default mode the unreadable switch fell back to", err)
	}
	if findings := visibilityFindingsIn(failure.Diagnostics); len(findings) != 1 {
		t.Fatalf("findings = %v, want the one violation it refused", findings)
	}
	switchWarnings := diagnosticsWithCode(failure.Diagnostics, VisibilityModeInvalidCode)
	if len(switchWarnings) != 1 || switchWarnings[0].Severity != diag.Warning ||
		!strings.Contains(switchWarnings[0].Message, `"warn"`) {
		t.Fatalf("diagnostics = %v, want one warning naming the unreadable switch", failure.Diagnostics)
	}
	if failure.Diagnostics[0].Code != VisibilityModeInvalidCode {
		t.Fatalf("diagnostics = %v, want the switch warning ahead of the findings", failure.Diagnostics)
	}
	if !strings.HasPrefix(failure.Message, "1 import(s)") || !strings.Contains(failure.Message, "the default") {
		t.Fatalf("message = %q, want one import counted under the default mode", failure.Message)
	}
}

func diagnosticsWithCode(diagnostics []diag.Diagnostic, code string) []diag.Diagnostic {
	var found []diag.Diagnostic
	for _, d := range diagnostics {
		if d.Code == code {
			found = append(found, d)
		}
	}
	return found
}

func TestSynchronizeReportsAnUnreadableModeWhenNothingIsViolated(t *testing.T) {
	_, ws := visibilityFixture(t, "warn", string(wsproto.VisibilityPublic))
	outcome, err := Synchronize(SyncRequest{
		Workspace: ws, Providers: visibilityProvider(t), Reason: wsproto.ProbeReasonLoad,
	})
	if err != nil {
		t.Fatalf("Synchronize: %v", err)
	}
	if findings := visibilityFindingsIn(outcome.Diagnostics); len(findings) != 0 {
		t.Fatalf("findings = %v, want none", findings)
	}
	if warnings := diagnosticsWithCode(outcome.Diagnostics, VisibilityModeInvalidCode); len(warnings) != 1 {
		t.Fatalf("diagnostics = %v, want the unreadable switch reported once", outcome.Diagnostics)
	}
}

func TestReadableRefusalServesARefusedGraphOnly(t *testing.T) {
	_, ws := visibilityFixture(t, "", "")
	_, err := Synchronize(SyncRequest{
		Workspace: ws, Providers: visibilityProvider(t), Reason: wsproto.ProbeReasonLoad,
	})
	outcome, readErr := ReadableRefusal(err)
	if readErr != nil || outcome == nil || len(visibilityFindingsIn(outcome.Diagnostics)) != 1 {
		t.Fatalf("ReadableRefusal(visibility) = (%v, %v), want the findings and no error", outcome, readErr)
	}
	unavailable := wsproto.NewProbeFailure(wsproto.ProbeFailureUnavailable, "@acme/go", "runtime missing")
	if outcome, readErr := ReadableRefusal(unavailable); readErr == nil || outcome != nil {
		t.Fatalf("ReadableRefusal(unavailable) = (%v, %v), want the error", outcome, readErr)
	}
}
