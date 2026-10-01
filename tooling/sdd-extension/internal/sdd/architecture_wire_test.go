package sdd

import (
	"go.putnami.dev/protocol/features/spectest"

	"path/filepath"
	"testing"

	archproto "go.putnami.dev/protocol/architecture"
	wsproto "go.putnami.dev/protocol/workspace"
	pctx "go.putnami.dev/sdk/extension/context"
	workspace "go.putnami.dev/tooling/sdd/extension/internal/wsview"
)

// This file is the verdict-parity harness for `architecture validate` across
// the process boundary: the SAME repository, evaluated once through a
// loader-built workspace (what the CLI core does) and once through a view built
// from the job-context wire (what the extension task does), must reach the SAME
// verdict.
//
// It exists because the answer was measured, not assumed. Before
// `workspaceProjects[].dependencies`, the wire-built evaluation of a workspace
// with one undeclared cross-domain dependency reported:
//
//	valid:true  observedEdges:0  findings:[]
//
// while the loader-built one reported:
//
//	valid:false observedEdges:1
//	findings:[architecture.undeclared_project_dependency ...]
//
// A job wired into the canonical CI gate on those terms would have been green
// precisely because it could not see the violation it exists to catch.

// architectureFixtureRoot writes the two-domain repository both parity tests
// evaluate: a producer domain, a consumer domain, and NO declared binding
// between them, so an actual project edge is an undeclared dependency.
func architectureFixtureRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeArchitectureTestFile(t, filepath.Join(root, "producer", archproto.ManifestFilename), architectureProducerManifest())
	writeArchitectureTestFile(t, filepath.Join(root, "consumer", archproto.ManifestFilename), architectureConsumerManifest(false))
	return root
}

func architectureWireContext(root string, refs []pctx.ProjectRef, selection *pctx.Selection) *pctx.Context {
	selected := make([]pctx.ProjectRef, 0, len(refs))
	for _, ref := range refs {
		ref.FullPath = filepath.Join(root, ref.Path)
		selected = append(selected, ref)
	}
	return &pctx.Context{
		WorkspaceRoot:     root,
		Project:           pctx.Project{Name: "workspace", Path: ".", FullPath: root},
		SelectedProjects:  selected,
		WorkspaceProjects: selected,
		Selection:         selection,
	}
}

// TestArchitectureVerdictMatchesTheLoaderAcrossTheWire is the parity assertion.
// Same repository, same edge, two views: the verdict must not depend on which
// side of the process boundary evaluated it.
func TestArchitectureVerdictMatchesTheLoaderAcrossTheWire(t *testing.T) {
	root := architectureFixtureRoot(t)

	loaded := workspace.NewWorkspace(root, &wsproto.Config{Name: "parity"}, []*workspace.Project{
		{ID: "/producer", Name: "producer", Path: "producer"},
		{ID: "/consumer", Name: "consumer", Path: "consumer", Dependencies: []string{"producer"}},
	})
	loadedReport, loadedErr := BuildArchitectureValidationResult(loaded, "")
	if loadedErr == nil || loadedReport.Valid {
		t.Fatalf("the loader-built evaluation missed the undeclared dependency: %+v", loadedReport)
	}

	// The wire view. Edges arrive already resolved to ids, which is the whole
	// difference: the extension never translates a declared name itself.
	wired, _ := workspace.FromContext(architectureWireContext(root, []pctx.ProjectRef{
		{ID: "/consumer", Name: "consumer", Path: "consumer", Dependencies: []string{"/producer"}},
		{ID: "/producer", Name: "producer", Path: "producer"},
	}, &pctx.Selection{Mode: pctx.SelectionModeAll, ProjectIDs: []string{"/consumer", "/producer"}}))
	wiredReport, wiredErr := BuildArchitectureValidationResult(wired, "")

	if wiredErr == nil {
		t.Fatal("the wire-built evaluation passed; it cannot see the undeclared dependency")
	}
	if wiredReport.Valid != loadedReport.Valid {
		t.Fatalf("valid = %t across the wire, %t through the loader", wiredReport.Valid, loadedReport.Valid)
	}
	if wiredReport.Summary.ObservedEdges != loadedReport.Summary.ObservedEdges {
		t.Fatalf("observed edges = %d across the wire, %d through the loader",
			wiredReport.Summary.ObservedEdges, loadedReport.Summary.ObservedEdges)
	}
	if len(wiredReport.Findings) != len(loadedReport.Findings) {
		t.Fatalf("findings = %d across the wire, %d through the loader",
			len(wiredReport.Findings), len(loadedReport.Findings))
	}
	for i := range wiredReport.Findings {
		if wiredReport.Findings[i].ID != loadedReport.Findings[i].ID {
			t.Fatalf("finding[%d] = %q across the wire, %q through the loader",
				i, wiredReport.Findings[i].ID, loadedReport.Findings[i].ID)
		}
	}
	if wiredReport.Findings[0].Code != archproto.ErrorCodeUndeclaredProjectDependency {
		t.Fatalf("finding code = %q, want %q", wiredReport.Findings[0].Code, archproto.ErrorCodeUndeclaredProjectDependency)
	}
}

// TestArchitectureFailsClosedOnASelectionOnlyView pins the other half of the
// measurement, and it is the half that decides whether this task may be wired
// into the gate at all.
//
// Handed the SELECTION instead of the membership, a narrowed run once reported
// `architecture.unknown_project` for /producer — a real member of the workspace
// that the run simply had not selected. That is a false violation, and its twin
// is the false pass: an edge into an unselected project is never walked. Both
// are unprovable from a subset, so the view says the graph is incomplete and
// the evaluation refuses rather than publishing either answer.
func TestArchitectureFailsClosedOnASelectionOnlyView(t *testing.T) {
	root := architectureFixtureRoot(t)
	narrowed, _ := workspace.FromContext(&pctx.Context{
		WorkspaceRoot: root,
		Project:       pctx.Project{Name: "workspace", Path: ".", FullPath: root},
		SelectedProjects: []pctx.ProjectRef{
			{ID: "/consumer", Name: "consumer", Path: "consumer", FullPath: filepath.Join(root, "consumer")},
		},
		Selection: &pctx.Selection{Mode: pctx.SelectionModeImpacted, Scoped: true, ProjectIDs: []string{"/consumer"}},
	})
	report, err := BuildArchitectureValidationResult(narrowed, "")
	if err == nil || report.Valid {
		t.Fatalf("a subset view produced a workspace verdict: %+v", report)
	}
	codes := map[string]bool{}
	for _, finding := range report.Diagnostics {
		codes[finding.Code] = true
	}
	if !codes[ErrorCodeIncompleteProjectGraph] {
		t.Fatalf("diagnostics = %+v, want the incomplete-project-graph refusal", report.Diagnostics)
	}
	// And the refusal must REPLACE the false violation rather than accompany it:
	// naming a real member as unknown is the misleading half.
	if codes[archproto.ErrorCodeUnknownProject] {
		t.Fatalf("a real workspace member was reported as unknown: %+v", report.Diagnostics)
	}
}

// TestWorktreeValidationReadsNoGitHistory pins D9: the cached task's verdict is
// a function of the three file patterns it declares, and of nothing else.
//
// The fixture is the cheapest possible proof. A frozen baseline file is present
// — which is exactly the condition that makes the ordinary evaluation resolve a
// comparison ref — and the workspace is not a git repository at all, so any git
// read fails and would surface as architecture.baseline_unavailable. The
// worktree evaluation must produce no such diagnostic, and must report
// Compared:false rather than claiming a comparison it did not make.
func TestWorktreeValidationReadsNoGitHistory(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "declared-inputs-cover-the-read-set", "worktree-validation-reads-no-git-history")
	spectest.Proves(t, "architecture/executable-contracts", "contained-read-only-tooling", "worktree-validation-reads-no-git-history")
	root := architectureFixtureRoot(t)
	writeArchitectureTestFile(t, filepath.Join(root, archproto.BaselineFilename),
		`{"protocolVersion":1,"findings":[]}`)
	ws := workspace.NewWorkspace(root, &wsproto.Config{Name: "worktree-only"}, []*workspace.Project{
		{ID: "/producer", Name: "producer", Path: "producer"},
		{ID: "/consumer", Name: "consumer", Path: "consumer", Dependencies: []string{"producer"}},
	})

	// The ordinary evaluation reaches for git and cannot find a repository.
	ordinary, _ := BuildArchitectureValidationResult(ws, "")
	if !hasDiagnosticCode(ordinary.Diagnostics, ErrorCodeBaselineUnavailable) {
		t.Fatalf("the fixture does not exercise the git path: %+v", ordinary.Diagnostics)
	}

	worktree, err := BuildArchitectureWorktreeValidationResult(ws)
	if hasDiagnosticCode(worktree.Diagnostics, ErrorCodeBaselineUnavailable) {
		t.Fatalf("the worktree evaluation read git history: %+v", worktree.Diagnostics)
	}
	if worktree.Baseline.Compared || worktree.Baseline.PriorFile || worktree.Baseline.Requested != "" {
		t.Fatalf("baseline status = %+v, want an explicit not-consulted", worktree.Baseline)
	}
	// It still reaches the verdict: dropping the comparison makes the check
	// stricter, never laxer, because no finding can be excused as known debt.
	if err == nil || worktree.Valid {
		t.Fatalf("the worktree evaluation passed over an undeclared dependency: %+v", worktree)
	}
	if worktree.Ratchet == nil || worktree.Ratchet.New != 1 || worktree.Ratchet.KnownDebt != 0 {
		t.Fatalf("ratchet = %+v, want the finding reported as new", worktree.Ratchet)
	}
}

// TestArchitectureContextReadsNoGitHistory gives the MCP-facing builder the
// same no-repository fixture as the cached task, with a coherent declaration so
// the only thing that can make it fail is an accidental baseline lookup.
func TestArchitectureContextReadsNoGitHistory(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "architecture-agent-context", "architecture-context-reads-worktree-only")
	ws := architectureBuildersWorkspace(t, true)
	writeArchitectureTestFile(t, filepath.Join(ws.Root, archproto.BaselineFilename),
		`{"protocolVersion":1,"findings":[]}`)

	ordinary, _ := BuildArchitectureInspectionResult(ws, "consumer", "")
	if !hasDiagnosticCode(ordinary.Diagnostics, ErrorCodeBaselineUnavailable) {
		t.Fatalf("the fixture does not exercise the interactive git path: %+v", ordinary.Diagnostics)
	}

	context, err := BuildArchitectureWorktreeInspectionResult(ws, "consumer")
	if err != nil {
		t.Fatalf("worktree architecture context: %v (%+v)", err, context)
	}
	if hasDiagnosticCode(context.Diagnostics, ErrorCodeBaselineUnavailable) {
		t.Fatalf("the agent context read git history: %+v", context.Diagnostics)
	}
	if context.Domain == nil || context.Domain.ID != "consumer" || context.Baseline.Compared || context.Baseline.Requested != "" {
		t.Fatalf("worktree context = %+v", context)
	}
}
