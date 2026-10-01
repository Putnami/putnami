package sdd

import (
	"errors"
	"fmt"

	archproto "go.putnami.dev/protocol/architecture"
	diag "go.putnami.dev/protocol/diagnostic"
	internalgit "go.putnami.dev/tooling/sdd/extension/internal/gitread"
	workspace "go.putnami.dev/tooling/sdd/extension/internal/wsview"
)

// EvaluateWorkspace performs one contained discovery, validates the declared
// repository, extracts current project edges, loads the prior frozen baseline,
// and publishes a snapshot only when structural inputs are valid.
func EvaluateWorkspace(ws *workspace.Workspace, options EvaluationOptions) Result {
	if ws == nil {
		return Result{Diagnostics: []diag.Diagnostic{diag.Errorf(ErrorCodeReadFailure, "workspace", "workspace is required")}}
	}
	discovery := Discover(ws.Root)
	diagnostics := append([]diag.Diagnostic(nil), discovery.Diagnostics...)
	diagnostics = append(diagnostics, archproto.ValidateRepository(discovery.Sources)...)
	debtDiagnostics := archproto.ValidateDebtFiles(discovery.Baseline, discovery.Waivers)
	appendPathDiagnostics(&diagnostics, archproto.WaiverFilename, debtDiagnostics)
	// The membership check and the fail-closed refusal are EXCLUSIVE, and the
	// order matters. Over a partial project view, "this manifest names a project
	// the workspace does not contain" is unanswerable: a real member that the
	// view simply does not list is indistinguishable from one that does not
	// exist, so running the check would report a false violation ON TOP of the
	// refusal and point the reader at the wrong file. One refusal, naming the
	// partial view, is the whole verdict such a run is entitled to.
	if ws.HasWarningCode(workspace.WarningCodeProviderViewUnavailable) {
		diagnostics = append(diagnostics, diag.Errorf(
			ErrorCodeIncompleteProjectGraph,
			"workspace",
			"project dependency enforcement requires a complete project view; this run received a partial one, "+
				"so a project that is absent cannot be told from one that was merely not selected "+
				"(run `putnami projects sync` when a provider probe is the cause)",
		))
	} else {
		diagnostics = append(diagnostics, validateWorkspaceProjects(ws, discovery.Sources)...)
	}

	// Framework evidence is read here, beside the declarations, for the same
	// reason the project graph is: both are derived facts this run compares with
	// the manifests, and neither can authorize anything. A manifest that does not
	// parse produces a diagnostic rather than an absence, because treating a
	// broken producer artifact as "implements nothing" would turn a failed build
	// into a clean architecture verdict.
	evidence, evidenceDiagnostics := DetectFrameworkEvidence(ws, discovery.Sources)
	diagnostics = append(diagnostics, evidenceDiagnostics...)

	var (
		previous            *archproto.Baseline
		ratchetOptions      archproto.RatchetOptions
		baselineStatus      BaselineStatus
		baselineDiagnostics []diag.Diagnostic
	)
	// WorktreeOnly does not merely discard the comparison, it never performs it:
	// the point is that no git command runs, so nothing outside the declared
	// inputs can move the verdict. BaselineStatus.Compared stays false, which is
	// how a reader tells "the baseline held" from "the baseline was not
	// consulted" — reporting the second as the first is how a ratchet stops
	// being one.
	if !options.WorktreeOnly {
		previous, ratchetOptions, baselineStatus, baselineDiagnostics =
			loadPreviousBaseline(ws.Root, discovery.Baseline, options)
	}
	diagnostics = append(diagnostics, baselineDiagnostics...)
	ratchetOptions.PreviousBaseline = previous
	ratchetOptions.Today = options.Today
	sortDiagnostics(diagnostics)
	result := Result{Diagnostics: diagnostics, Baseline: baselineStatus}
	if diag.HasErrors(diagnostics) {
		return result
	}
	graph := archproto.BuildGraph(discovery.Sources)
	observed := archproto.Observations{Edges: DetectProjectDependencies(ws, discovery.Sources)}
	observed.Evidence = evidence
	result.Snapshot = archproto.BuildSnapshot(graph, observed, discovery.Baseline, discovery.Waivers, ratchetOptions)
	return result
}

func validateWorkspaceProjects(ws *workspace.Workspace, sources []archproto.ManifestSource) []diag.Diagnostic {
	var diagnostics []diag.Diagnostic
	for _, source := range sources {
		if source.Manifest == nil {
			continue
		}
		for index, project := range source.Manifest.Projects {
			if ws.ProjectByID(project) == nil {
				diagnostics = append(diagnostics, diag.Errorf(
					archproto.ErrorCodeUnknownProject,
					fmt.Sprintf("%s#projects[%d]", source.Path, index),
					"project %q is not present in the resolved Putnami workspace", project))
			}
		}
	}
	return diagnostics
}

func loadPreviousBaseline(root string, current *archproto.Baseline, options EvaluationOptions) (*archproto.Baseline, archproto.RatchetOptions, BaselineStatus, []diag.Diagnostic) {
	status := BaselineStatus{Requested: options.BaselineRef}
	if current == nil {
		return nil, archproto.RatchetOptions{}, status, nil
	}
	ref, err := internalgit.ResolveBaseline(root, options.BaselineRef)
	if err != nil {
		return nil, archproto.RatchetOptions{}, status, []diag.Diagnostic{
			diag.Errorf(ErrorCodeBaselineUnavailable, archproto.BaselineFilename, "resolve architecture comparison baseline: %v", err),
		}
	}
	commit, err := internalgit.ResolveCommit(root, ref)
	if err != nil {
		return nil, archproto.RatchetOptions{}, status, []diag.Diagnostic{
			diag.Errorf(ErrorCodeBaselineUnavailable, archproto.BaselineFilename, "resolve architecture comparison commit: %v", err),
		}
	}
	status.Requested = ref
	status.Commit = "git:" + commit
	status.Compared = true
	entry, exists, err := internalgit.TreeEntryAt(root, commit, archproto.BaselineFilename)
	if err != nil {
		return nil, archproto.RatchetOptions{}, status, []diag.Diagnostic{
			diag.Errorf(ErrorCodeBaselineUnavailable, archproto.BaselineFilename, "read prior architecture baseline: %v", err),
		}
	}
	if !exists {
		return nil, archproto.RatchetOptions{PreviousBaselineKnown: false}, status, nil
	}
	status.PriorFile = true
	if entry.Type != "blob" || (entry.Mode != "100644" && entry.Mode != "100755") {
		return nil, archproto.RatchetOptions{}, status, []diag.Diagnostic{
			diag.Errorf(ErrorCodeBaselineUnavailable, archproto.BaselineFilename, "prior architecture baseline is not a regular file"),
		}
	}
	data, err := internalgit.ReadTreeBlob(root, entry.ObjectID, MaximumManifestBytes)
	if errors.Is(err, internalgit.ErrTreeObjectTooLarge) {
		err = fmt.Errorf("file exceeds %d bytes", MaximumManifestBytes)
	}
	if err != nil {
		return nil, archproto.RatchetOptions{}, status, []diag.Diagnostic{
			diag.Errorf(ErrorCodeBaselineUnavailable, archproto.BaselineFilename, "read prior architecture baseline: %v", err),
		}
	}
	previous, diagnostics := archproto.ParseAndValidateBaseline(data)
	var sourced []diag.Diagnostic
	appendPathDiagnostics(&sourced, "git:"+commit+":"+archproto.BaselineFilename, diagnostics)
	if previous == nil && len(sourced) == 0 {
		sourced = append(sourced, diag.Errorf(ErrorCodeBaselineUnavailable, archproto.BaselineFilename, "prior architecture baseline is unavailable"))
	}
	return previous, archproto.RatchetOptions{PreviousBaselineKnown: true}, status, sourced
}
