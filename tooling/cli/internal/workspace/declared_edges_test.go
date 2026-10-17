package workspace

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"

	diag "go.putnami.dev/protocol/diagnostic"
	wsproto "go.putnami.dev/protocol/workspace"
)

// declaredEdgeFixture is one workspace carrying every class of declaration a
// phantom edge can live in, plus the two shapes that must NOT be reported: an
// edge a real import backs, and a contract edge.
func declaredEdgeFixture(t *testing.T) *Workspace {
	t.Helper()
	root := t.TempDir()
	for _, module := range []string{"libs/core", "apps/api", "apps/worker", "services/catalog"} {
		writeFileAt(t, filepath.Join(root, filepath.FromSlash(module), "go.mod"), "module acme/"+module+"\n")
	}
	for _, pkg := range []string{"web/kit", "web/ui"} {
		writeFileAt(t, filepath.Join(root, filepath.FromSlash(pkg), "package.json"), `{"name":"@acme/x"}`)
	}

	core := &Project{ID: "/libs/core", Name: "acme/core", Path: "libs/core"}
	kit := &Project{ID: "/web/kit", Name: "@acme/kit", Path: "web/kit"}
	catalog := &Project{ID: "/services/catalog", Name: "acme/catalog", Path: "services/catalog",
		ContractServiceID: "catalog"}

	// Class 2: a go.mod require of a workspace module no Go file imports.
	api := &Project{ID: "/apps/api", Name: "acme/api", Path: "apps/api",
		Dependencies:      []string{"acme/core"},
		DependencySources: map[string]wsproto.DependencySource{"acme/core": wsproto.DependencySourceDeclared}}
	// The control: the same edge, backed by an import.
	worker := &Project{ID: "/apps/worker", Name: "acme/worker", Path: "apps/worker",
		Dependencies:      []string{"acme/core"},
		DependencySources: map[string]wsproto.DependencySource{"acme/core": wsproto.DependencySourceGoModule}}
	// Class 3: a package.json workspace dependency no source file imports.
	ui := &Project{ID: "/web/ui", Name: "@acme/ui", Path: "web/ui",
		Dependencies:      []string{"@acme/kit"},
		DependencySources: map[string]wsproto.DependencySource{"@acme/kit": wsproto.DependencySourceDeclared}}
	// Class 1: a putnami.json entry nothing imports. The provider DID answer
	// about the edge — it attributed no import — so the attribution is known.
	tool := &Project{ID: "/tools/cli", Name: "acme/tool", Path: "tools/cli",
		Config:            &wsproto.ProjectConfig{Name: "acme/tool", Dependencies: []string{"acme/core"}},
		Dependencies:      []string{"acme/core"},
		DependencySources: map[string]wsproto.DependencySource{"acme/core": wsproto.DependencySourceDeclared}}
	// The same declaration, backed by a file input that reads inside the
	// dependency: the build hashes those bytes, so the edge is real.
	reader := &Project{ID: "/tools/reader", Name: "acme/reader", Path: "tools/reader",
		Config: &wsproto.ProjectConfig{Name: "acme/reader", Dependencies: []string{"acme/core"},
			Options: map[string]map[string]any{"test": {"filePatterns": []any{"../../libs/core/**"}}}},
		Dependencies:      []string{"acme/core"},
		DependencySources: map[string]wsproto.DependencySource{"acme/core": wsproto.DependencySourceDeclared}}
	// No provider said anything about this project's edges at all.
	unattributed := &Project{ID: "/python/app", Name: "acme/py-app", Path: "python/app",
		Config:       &wsproto.ProjectConfig{Name: "acme/py-app", Dependencies: []string{"acme/core"}},
		Dependencies: []string{"acme/core"}}
	// Class 1, contract-backed: the declaration is redundant AND harmful, and
	// the contract edge survives its removal.
	client := &Project{ID: "/clients/catalog-go", Name: "acme/catalog-client", Path: "clients/catalog-go",
		Config:            &wsproto.ProjectConfig{Name: "acme/catalog-client", Dependencies: []string{"acme/catalog"}},
		Dependencies:      []string{"acme/catalog"},
		DependencySources: map[string]wsproto.DependencySource{"acme/catalog": wsproto.DependencySourceDeclared},
		GeneratedClient:   &GeneratedClientBinding{ServiceID: "catalog", ManifestPath: "clients/catalog-go/client.putnami.json"}}
	// Class 4: a committed client manifest no provider answers to.
	orphan := &Project{ID: "/clients/orders-go", Name: "acme/orders-client", Path: "clients/orders-go",
		GeneratedClient: &GeneratedClientBinding{ServiceID: "orders", ManifestPath: "clients/orders-go/client.putnami.json"}}

	return NewWorkspace(root, nil, []*Project{
		core, kit, catalog, api, worker, ui, tool, reader, unattributed, client, orphan})
}

func findingFor(t *testing.T, findings []DeclaredEdgeFinding, project string) DeclaredEdgeFinding {
	t.Helper()
	for _, finding := range findings {
		if finding.ProjectName == project {
			return finding
		}
	}
	t.Fatalf("no finding for %s in %+v", project, findings)
	return DeclaredEdgeFinding{}
}

func TestEveryDeclarationWithoutAnImportIsReported(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "declared-edge-check",
		"every-declaration-without-an-import-is-reported")
	findings := DeclaredEdgeFindings(declaredEdgeFixture(t))
	if len(findings) != 5 {
		t.Fatalf("findings = %+v, want one per class plus the contract-backed declaration", findings)
	}

	config := findingFor(t, findings, "acme/tool")
	if config.Class != DeclaredEdgeProjectConfig || config.File != "tools/cli/putnami.json" {
		t.Errorf("putnami.json class = %+v", config)
	}
	goMod := findingFor(t, findings, "acme/api")
	if goMod.Class != DeclaredEdgeManifest || goMod.Target != "/libs/core" {
		t.Errorf("Go manifest class = %+v", goMod)
	}
	packageJSON := findingFor(t, findings, "@acme/ui")
	if packageJSON.Class != DeclaredEdgeManifest || packageJSON.Target != "/web/kit" {
		t.Errorf("TypeScript manifest class = %+v", packageJSON)
	}
	orphan := findingFor(t, findings, "acme/orders-client")
	if orphan.Class != DeclaredEdgeOrphanClient || orphan.File != "clients/orders-go/client.putnami.json" {
		t.Errorf("orphan manifest class = %+v", orphan)
	}
	if orphan.Target != "" {
		t.Errorf("orphan target = %q, want none: no project answers to its service", orphan.Target)
	}
}

func TestAnImportBackedEdgeIsNotReportedAndAContractEdgeSurvivesTheDeclaration(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "declared-edge-check",
		"an-import-backed-edge-is-not-reported")
	ws := declaredEdgeFixture(t)
	findings := DeclaredEdgeFindings(ws)
	for _, finding := range findings {
		if finding.ProjectName == "acme/worker" {
			t.Fatalf("an edge a go.mod require and a real import back was reported: %+v", finding)
		}
		if finding.ProjectName == "acme/reader" {
			t.Fatalf("an edge a declared file input backs was reported: %+v", finding)
		}
		if finding.ProjectName == "acme/py-app" {
			t.Fatalf("an edge no provider attributed at all was reported as declared: %+v", finding)
		}
	}

	// The generated client's declaration IS reported: it re-creates the full
	// dependency edge the contract edge exists to avoid. What must hold is that
	// the contract edge outlives the declaration and keeps ordering the client.
	contractBacked := findingFor(t, findings, "acme/catalog-client")
	if !contractBacked.ContractBacked {
		t.Errorf("finding = %+v, want the contract edge named as the surviving relation", contractBacked)
	}
	if got := ws.Graph.ContractProviderOf("/clients/catalog-go"); got != "/services/catalog" {
		t.Errorf("contract provider = %q, want the provider the manifest names", got)
	}
	// The client whose provider is gone is the orphan, not this one.
	if ws.Graph.ContractProviderOf("/clients/orders-go") != "" {
		t.Errorf("the orphan client resolved a provider")
	}
}

func TestSynchronizeReportsADeclaredEdgeAndEnforceRefusesIt(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "declared-edge-check",
		"enforce-refuses-the-graph-and-report-is-the-default")
	report, err := ResolveDeclaredEdgesMode(nil)
	if err != nil || report != DeclaredEdgesModeReport {
		t.Fatalf("default mode = %q, %v; want report", report, err)
	}
	if _, err := ResolveDeclaredEdgesMode(&wsproto.Config{
		Options: map[string]map[string]any{"workspace": {"declaredEdges": "nope"}},
	}); err == nil {
		t.Fatal("an unknown mode resolved silently; it must be an error")
	}

	ws := declaredEdgeFixture(t)
	warnings := DeclaredEdgeDiagnostics(ws, DeclaredEdgesModeReport)
	if len(warnings) != 6 {
		t.Fatalf("report produced %d diagnostics, want one per finding plus the attribution notice", len(warnings))
	}
	for _, warning := range warnings {
		if warning.Severity != diag.Warning || warning.Code != DeclaredEdgeDiagnosticCode {
			t.Fatalf("diagnostic = %+v, want a warning under the declared-edge code", warning)
		}
	}
	if muted := DeclaredEdgeDiagnostics(ws, DeclaredEdgesModeOff); len(muted) != 0 {
		t.Fatalf("off produced %d diagnostics, want none", len(muted))
	}

	refused := DeclaredEdgeDiagnostics(ws, DeclaredEdgesModeEnforce)
	for _, finding := range refused {
		if strings.Contains(finding.Message, "attribution unavailable") {
			continue
		}
		if finding.Severity != diag.Error {
			t.Fatalf("diagnostic = %+v, want an error under enforce", finding)
		}
	}
	refusal := declaredEdgesRefusal(refused)
	if refusal.Kind != wsproto.ProbeFailureDeclaredEdge {
		t.Fatalf("kind = %q, want the typed declared-edge refusal", refusal.Kind)
	}
	var failure *wsproto.ProbeFailure
	if !asProbeFailure(refusal, &failure) {
		t.Fatalf("refusal does not travel as a probe failure")
	}
}

// The advice promises only the repair `putnami deps prune` makes: it rewrites
// a putnami.json entry, and for a language manifest it removes what it can
// edit and names the edit to make where it cannot.
func TestDeclaredEdgeAdvicePromisesOnlyWhatPruneRepairs(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "declared-edge-check",
		"the-advice-promises-only-the-repair-prune-makes")
	findings := DeclaredEdgeFindings(declaredEdgeFixture(t))
	const pruneRemovesIt = "run `putnami deps prune` to remove it"
	const pruneNamesTheEdit = "names the edit to make where it cannot"

	if message := DeclaredEdgeMessage(findingFor(t, findings, "acme/tool")); !strings.Contains(message, pruneRemovesIt) {
		t.Errorf("putnami.json advice = %q, want %q", message, pruneRemovesIt)
	}
	for _, project := range []string{"acme/api", "@acme/ui"} {
		message := DeclaredEdgeMessage(findingFor(t, findings, project))
		if strings.Contains(message, pruneRemovesIt) || !strings.Contains(message, pruneNamesTheEdit) {
			t.Errorf("%s manifest advice = %q, want %q and no promise that prune removes it", project, message, pruneNamesTheEdit)
		}
	}
	contractBacked := findingFor(t, findings, "acme/catalog-client")
	contractBacked.Class = DeclaredEdgeManifest
	if message := DeclaredEdgeMessage(contractBacked); !strings.Contains(message, "survives its removal") ||
		strings.Contains(message, pruneRemovesIt) || !strings.Contains(message, pruneNamesTheEdit) {
		t.Errorf("contract-backed manifest advice = %q, want the surviving contract edge and %q", message, pruneNamesTheEdit)
	}
	if refusal := declaredEdgesRefusal(nil); !strings.Contains(refusal.Message, "names the edit to make for the rest") {
		t.Errorf("enforce refusal = %q, want it to say prune names the edits it cannot make", refusal.Message)
	}
}

// asProbeFailure states that the refusal travels on the channel every
// graph-dependent command already fails on.
func asProbeFailure(err error, target **wsproto.ProbeFailure) bool {
	return errors.As(err, target)
}

// TestUnknownAttributionIsReportedOnceAndNeverAsAViolation: a project whose
// provider attributes nothing is outside the check, and saying so is what keeps
// a workspace from reading silence as a clean bill of health.
func TestUnknownAttributionIsReportedOnceAndNeverAsAViolation(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "declared-edge-check",
		"a-declared-input-backs-an-edge-and-unattributed-projects-are-skipped")
	ws := declaredEdgeFixture(t)

	unattributed := UnattributedProjects(ws)
	if len(unattributed) != 1 || unattributed[0] != "acme/py-app" {
		t.Fatalf("unattributed = %v, want the one project no provider answered about", unattributed)
	}

	notices := 0
	for _, d := range DeclaredEdgeDiagnostics(ws, DeclaredEdgesModeEnforce) {
		if !strings.Contains(d.Message, "attribution unavailable") {
			continue
		}
		notices++
		if d.Severity != diag.Warning {
			t.Errorf("severity = %q, want a warning even under enforce: a check that could not run is not a violation",
				d.Severity)
		}
	}
	if notices != 1 {
		t.Fatalf("attribution notices = %d, want exactly one", notices)
	}
}

// zeroEdgeFixture is the workspace of a provider that answered and found no
// edge, beside a project no provider answers about:
//
//   - apps/b declares /libs/a in putnami.json and reads a file inside it
//     through a declared test input;
//   - apps/c declares /libs/a in putnami.json and reads nothing of it;
//   - docs/site declares only an entry that names no workspace project.
//
// The provider answers apps/b and apps/c with an empty dependencySources map:
// their go.mod requires no workspace module.
func zeroEdgeFixture(t *testing.T) (*Workspace, *countingProvider, []ProviderBinding) {
	t.Helper()
	root := t.TempDir()
	writeFileAt(t, filepath.Join(root, "putnami.workspace.json"),
		`{"includes":["libs/a","apps/b","apps/c","docs/site"]}`)
	writeFileAt(t, filepath.Join(root, "libs", "a", "go.mod"), "module example.com/a\n\ngo 1.25\n")
	writeFileAt(t, filepath.Join(root, "libs", "a", "putnami.json"), `{"name":"libs/a"}`)
	writeFileAt(t, filepath.Join(root, "apps", "b", "go.mod"), "module example.com/b\n\ngo 1.25\n")
	writeFileAt(t, filepath.Join(root, "apps", "b", "putnami.json"),
		`{"name":"apps/b","dependencies":["/libs/a"],`+
			`"options":{"test":{"filePatterns":["../../libs/a/testdata/*.json"]}}}`)
	writeFileAt(t, filepath.Join(root, "apps", "c", "go.mod"), "module example.com/c\n\ngo 1.25\n")
	writeFileAt(t, filepath.Join(root, "apps", "c", "putnami.json"),
		`{"name":"apps/c","dependencies":["/libs/a"]}`)
	writeFileAt(t, filepath.Join(root, "docs", "site", "putnami.json"),
		`{"name":"docs/site","dependencies":["example.com/x/log"]}`)

	InvalidateLoadCache(root)
	t.Cleanup(func() { InvalidateLoadCache(root) })
	ws, err := Load(root)
	if err != nil {
		t.Fatalf("load workspace: %v", err)
	}
	provider := &countingProvider{
		name: "@acme/go",
		answer: func(wsproto.ProbeRequest) wsproto.ProbeResult {
			none := func() map[string]wsproto.DependencySource { return map[string]wsproto.DependencySource{} }
			return wsproto.ProbeResult{
				Version:   wsproto.ProbeProtocolVersion,
				Extension: "@acme/go",
				Projects: []wsproto.ProbeProject{
					{Path: "libs/a", SourceName: "example.com/a", SourceFile: "libs/a/go.mod", DependencySources: none()},
					{Path: "apps/b", SourceName: "example.com/b", SourceFile: "apps/b/go.mod", DependencySources: none()},
					{Path: "apps/c", SourceName: "example.com/c", SourceFile: "apps/c/go.mod", DependencySources: none()},
				},
			}
		},
	}
	bindings := []ProviderBinding{{
		Scope:    scopeFor(t, "@acme/go", []string{"go.mod"}, []string{"**/go.mod"}),
		Provider: provider,
	}}
	return ws, provider, bindings
}

func declaredEdgeDiagnosticsIn(diagnostics []diag.Diagnostic) []diag.Diagnostic {
	var found []diag.Diagnostic
	for _, d := range diagnostics {
		if d.Code == DeclaredEdgeDiagnosticCode {
			found = append(found, d)
		}
	}
	return found
}

// A provider that answered and found no edge leaves every putnami.json entry
// `declared`, so the check judges it: a declared input backs apps/b's entry,
// nothing backs apps/c's. An entry naming no workspace project raises no
// attribution notice. The answer survives the recorded snapshot.
func TestAnAnswerWithNoEdgesLeavesEveryEntryDeclared(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "declared-edge-check",
		"an-answer-with-no-edges-leaves-every-entry-declared")
	ws, provider, bindings := zeroEdgeFixture(t)

	for _, run := range []string{"probed", "recorded"} {
		outcome, err := Synchronize(SyncRequest{Workspace: ws, Providers: bindings, Reason: wsproto.ProbeReasonLoad})
		if err != nil {
			t.Fatalf("%s: Synchronize: %v", run, err)
		}
		if run == "recorded" && (!outcome.Fresh || provider.calls != 1) {
			t.Fatalf("recorded: fresh=%v calls=%d, want the snapshot replayed", outcome.Fresh, provider.calls)
		}
		found := declaredEdgeDiagnosticsIn(outcome.Diagnostics)
		if len(found) != 1 || found[0].Field != "apps/c/putnami.json" {
			t.Fatalf("%s: diagnostics = %+v, want only apps/c's unbacked putnami.json entry", run, found)
		}
	}
}

// An entry that names no workspace project is one the check never judges, so
// it cannot make a project unattributed.
func TestAnEntryNamingNoWorkspaceProjectRaisesNoAttributionNotice(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "declared-edge-check",
		"an-entry-naming-no-workspace-project-raises-no-attribution-notice")
	ws := declaredEdgeFixture(t)
	external := &Project{ID: "/docs/site", Name: "docs/site", Path: "docs/site",
		Config:       &wsproto.ProjectConfig{Name: "docs/site", Dependencies: []string{"example.com/x/log"}},
		Dependencies: []string{"example.com/x/log"}}
	ws = NewWorkspace(ws.Root, nil, append(ws.Projects, external))
	if unattributed := UnattributedProjects(ws); len(unattributed) != 1 || unattributed[0] != "acme/py-app" {
		t.Fatalf("unattributed = %v, want only the project whose entry names a workspace project", unattributed)
	}
}
