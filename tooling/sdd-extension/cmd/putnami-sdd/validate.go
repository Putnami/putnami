package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	protocolcli "go.putnami.dev/protocol/cli"
	diag "go.putnami.dev/protocol/diagnostic"
	featureproto "go.putnami.dev/protocol/features"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/docslinks"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/tooling/sdd/extension/internal/sdd"
	"go.putnami.dev/tooling/sdd/extension/internal/wsview"
)

// This file is the job half of @putnami/sdd: the task bodies the `validate` and
// `validate-workspace` commands run inside the DAG. Each is a thin adapter — build the view the wire entitles it
// to, call ONE engine builder, publish the diagnostics — because what a run
// DECIDES lives in internal/sdd and must be the same answer the interactive
// commands and the MCP tools give.
//
// The two commands exist separately because their SUBJECTS differ, not for
// convenience. features and specs are about a project's own durable documents,
// so they run per project and their cache keys are that project's files.
// architecture is about the workspace graph, so it runs once and its key is the
// workspace's architecture declarations. One command cannot mix the two
// activations, which is what D1 settles.

// runFeaturesValidate is the first step of the project-scoped `validate`
// pipeline: the durable feature manifest and the evidence that backs it.
//
// The task it belongs to is UNCACHEABLE on purpose (D8). This verdict depends
// on evidence source bindings — arbitrary bound files plus their git blob state
// — and a key built from file patterns cannot cover that read set. An
// under-declared key does not merely miss a change; it serves a stale verdict
// while claiming to have checked, which is the one failure a validation job
// must not have.
func runFeaturesValidate(ctx *pctx.Context, emit *jsonl.Emitter, _ []string) (string, map[string]any, error) {
	ws, selection, err := projectValidationView(ctx)
	if err != nil {
		return "", nil, err
	}
	report, verdict := sdd.BuildFeatureValidationResult(ws, selection)
	publishDiagnostics(emit, report.Diagnostics)
	emit.Summary(fmt.Sprintf("features: %d feature(s), %d requirement(s), %d evidence record(s)",
		report.Summary.Features, report.Summary.Requirements, report.Summary.Evidence))
	return finish(report, verdict)
}

// runSpecsValidate is the second step: the specs that detail those features,
// their decision links, and the completeness gaps around them.
//
// Its declared inputs are the project's own putnami.features.json and its
// direct specs/*.json children — exactly the documents this evaluation reads,
// which is what makes it cacheable at all. The narrowing that follows is stated
// rather than hidden: a workspace-wide question ("is this feature id declared
// twice ANYWHERE?") is not this task's to answer, because answering it would
// mean reading every project's manifest under a key that names one project's
// files. `putnami specs validate` unscoped keeps that question.
func runSpecsValidate(ctx *pctx.Context, emit *jsonl.Emitter, _ []string) (string, map[string]any, error) {
	ws, selection, err := projectValidationView(ctx)
	if err != nil {
		return "", nil, err
	}
	report, projection, verdict := sdd.BuildSpecValidationWithCriteria(ws, selection)
	publishDiagnostics(emit, report.Diagnostics)
	if verdict == nil {
		if err := emitCriteriaProjection(ctx, emit, projection); err != nil {
			return "", nil, err
		}
	}
	emit.Summary(fmt.Sprintf("specs: %d spec(s) for %d authored feature(s), %d publishable project(s) assessed",
		report.Summary.Specs, report.Summary.AuthoredFeatures, report.Summary.PublishableProjects))
	return finish(report, verdict)
}

// emitCriteriaProjection writes the executable-criteria projection this
// project's specs derive (decision 9, fixed: the extension judges what a
// run is expected to prove; core collects observations and sanctions). The
// artifact is the task's declared output, so a cache hit restores the same
// bytes an uncached run writes; a project whose specs state no requirement
// writes nothing, which the declaration marks optional.
func emitCriteriaProjection(ctx *pctx.Context, emit *jsonl.Emitter, projection *featureproto.SpecCriteriaProjection) error {
	if projection == nil || len(projection.Groups) == 0 {
		return nil
	}
	if ctx.OutputPath == "" {
		return protocolcli.Classify(
			fmt.Errorf("the job context names no output path for the criteria projection"),
			protocolcli.ErrInvalidConfig,
		)
	}
	encoded, err := featureproto.MarshalSpecCriteriaProjection(projection)
	if err != nil {
		return protocolcli.Classify(fmt.Errorf("encode criteria projection: %w", err), protocolcli.ErrInvalidConfig)
	}
	if err := os.MkdirAll(ctx.OutputPath, 0o755); err != nil {
		return protocolcli.Classify(fmt.Errorf("prepare criteria projection output: %w", err), protocolcli.ErrInvalidConfig)
	}
	destination := filepath.Join(ctx.OutputPath, featureproto.SpecCriteriaProjectionFilename)
	if err := os.WriteFile(destination, encoded, 0o644); err != nil {
		return protocolcli.Classify(fmt.Errorf("write criteria projection: %w", err), protocolcli.ErrInvalidConfig)
	}
	// The event names the ABSOLUTE destination, the same spelling the Go
	// extension's coverage artifact uses: the CLI normalizes an absolute path
	// under the workspace to its workspace-relative display form, while a bare
	// filename would be resolved lexically against this task's project cwd and
	// point at a file that does not exist there.
	emit.Artifact(featureproto.SpecCriteriaProjectionArtifactID, "Executable criteria projection", "report", destination)
	return nil
}

// runArchitectureValidate is the whole of the workspace-scoped
// `validate-workspace` command: ARC/DARC declarations, and the exact
// cross-domain project edges the resolved workspace graph contains.
//
// It runs WITHOUT a baseline (D9). The comparison is a git read, and this
// task's declared inputs are committed file patterns only (ARC declarations,
// capability evidence, and the workspace policy file), so folding a commit into
// the verdict would make a cached answer depend on state the key does not name.
// Dropping it makes the check stricter rather than laxer — nothing is excused
// as known debt — and shrink-only ratcheting stays with the interactive
// command.
func runArchitectureValidate(ctx *pctx.Context, emit *jsonl.Emitter, _ []string) (string, map[string]any, error) {
	workspaceOptions, err := wsview.WorkspaceOptionsFromContext(ctx)
	if err != nil {
		return "", nil, protocolcli.Classify(
			fmt.Errorf("decode workspace architecture verification policy: %w", err),
			protocolcli.ErrInvalidConfig,
		)
	}
	mode, source, err := featureproto.ResolveWorkspaceVerificationMode(
		featureproto.VerificationDomainArchitecture,
		"putnami.workspace.json",
		workspaceOptions,
	)
	if err != nil {
		return "", nil, protocolcli.Classify(
			fmt.Errorf("resolve workspace architecture verification policy: %w", err),
			protocolcli.ErrInvalidConfig,
		)
	}
	taskReport := sdd.ArchitectureTaskReport{
		Mode:                mode,
		ModeSource:          source,
		AutomaticEvaluation: mode != featureproto.VerificationModeOff,
	}
	if mode == featureproto.VerificationModeOff {
		emit.Summary(fmt.Sprintf("architecture: automatic evaluation skipped (mode=%s source=%s)", mode, source))
		return finish(taskReport, nil)
	}

	ws, err := workspaceValidationView(ctx)
	if err != nil {
		return finish(taskReport, err)
	}
	report, verdict := sdd.BuildArchitectureWorktreeValidationResult(ws)
	taskReport.ArchitectureValidationReport = &report
	publishDiagnostics(emit, report.Diagnostics)
	// Findings are the ratchet's verdict on the graph rather than a complaint
	// about one document, so they carry an edge and no path. They are published
	// with an empty file position instead of an invented one: guessing which of
	// the two manifests is "the" file would send half the readers to the wrong
	// one.
	for _, finding := range report.Findings {
		severity := finding.Severity
		if mode == featureproto.VerificationModeReport {
			severity = diag.Warning
		}
		emit.DiagnosticWithCode(string(severity), finding.Message, "", 0, 0, finding.Code)
	}
	emit.Summary(fmt.Sprintf("architecture: %d domain(s), %d declared edge(s), %d observed edge(s), %d finding(s), mode=%s source=%s",
		report.Summary.Domains, report.Summary.DeclaredEdges, report.Summary.ObservedEdges, report.Summary.Findings,
		mode, source))
	if mode == featureproto.VerificationModeReport && report.StructurallyCoherent() {
		verdict = nil
	}
	return finish(taskReport, verdict)
}

// runSpecsRatchetValidate is the second step of the workspace-scoped
// `validate-workspace` command: the rollout ratchet of the executable-spec
// gate. It derives the CURRENT enforce floor from the committed
// manifests and options and compares it against the committed
// specs.baseline.json files with the protocol's shrink-only rule.
//
// Like `architecture-validate` it runs over the CURRENT WORKTREE ONLY and
// reads no git history: both sides of the comparison are committed files the
// task's input patterns name, so a cached verdict never depends on state the
// key does not cover. The reviewed policy change the epic requires is an edit
// to the committed baseline in the same diff — which changes an input and
// re-keys the task.
func runSpecsRatchetValidate(ctx *pctx.Context, emit *jsonl.Emitter, _ []string) (string, map[string]any, error) {
	ws, err := workspaceValidationView(ctx)
	if err != nil {
		return "", nil, err
	}
	report, verdict := sdd.BuildSpecsRatchetResult(ws)
	publishDiagnostics(emit, report.Diagnostics)
	enforced := 0
	if report.Current != nil {
		enforced = len(report.Current.Projects)
	}
	emit.Summary(fmt.Sprintf("specs ratchet: %d enforced project(s), baseline %d, %d regression(s), %d lost requirement(s)",
		enforced, report.Summary.BaselineProjects, report.Summary.RegressedProjects, report.Summary.LostRequirements))
	return finish(report, verdict)
}

// runDecisionsValidate is the third step of the workspace-scoped
// `validate-workspace` command: every committed decisions.json — the root one
// and each project's — and the files each of their checks names.
//
// The task it belongs to is UNCACHEABLE on purpose, and for the same reason
// `features-validate` is (D8). A check's read set is whatever its own `files`
// globs name, and those globs are authored per repository, so no static
// task-input pattern can cover them. An under-declared key does not merely miss
// a change; it serves a stale verdict while claiming to have checked, which is
// the one failure a validation job must not have.
//
// There is no verification-mode knob. `architecture` has one because a
// workspace adopts a graph gradually; a decision a repository has written down
// and dated is either held or re-decided, and a "report" mode for it would be
// the silent re-decision the registry exists to prevent.
func runDecisionsValidate(ctx *pctx.Context, emit *jsonl.Emitter, _ []string) (string, map[string]any, error) {
	ws, err := workspaceValidationView(ctx)
	if err != nil {
		return "", nil, err
	}
	report, verdict := sdd.BuildDecisionsResult(ws)
	publishDiagnostics(emit, report.Diagnostics)
	// One diagnostic per violating file, anchored on that file rather than on
	// the registry: the reader has to open the file that broke the decision,
	// and the message already names the decision that was broken.
	for _, finding := range report.Findings {
		emit.DiagnosticWithCode(string(diag.Error), finding.Message, finding.Path, 0, 0,
			featureproto.ErrorCodeDecisionViolated)
	}
	emit.Summary(fmt.Sprintf("decisions: %d registr(ies), %d settled (%d enforced, %d review-only), %d file(s) checked, %d violation(s)",
		len(report.Registries), report.Summary.Decisions, report.Summary.Enforced, report.Summary.ReviewOnly,
		report.Summary.FilesChecked, report.Summary.Violations))
	return finish(report, verdict)
}

// runRecipesValidate is the fourth step of the workspace-scoped
// `validate-workspace` command: every committed `<lang>/samples/recipes.json`.
// A recipe that names a sample the worktree does not contain, or a
// second recipe for one intention, fails naming the index entry.
//
// Like decisions-validate it is UNCACHEABLE: the verdict depends on whether
// each named sample directory exists, and no file-pattern key expresses that.
func runRecipesValidate(ctx *pctx.Context, emit *jsonl.Emitter, _ []string) (string, map[string]any, error) {
	ws, err := workspaceValidationView(ctx)
	if err != nil {
		return "", nil, err
	}
	report, verdict := sdd.BuildRecipesResult(ws)
	publishDiagnostics(emit, report.Diagnostics)
	emit.Summary(fmt.Sprintf("recipes: %d recipe(s) in %d index(es), %d diagnostic(s)",
		report.Recipes, len(report.Indexes), len(report.Diagnostics)))
	return finish(report, verdict)
}

// runCodeownersSync is the fifth step of the workspace-scoped
// `validate-workspace` command. It renders .github/CODEOWNERS from the
// owners each putnami.json declares and rewrites the committed file when it
// differs, so the file follows the declarations without a command of its own.
// A stale or missing committed file is written on CI too, where the rule that
// fails a run whose gate changed the tree reports it.
//
// Like decisions-validate it is UNCACHEABLE: it reads the putnami.json of every
// project and of every directory above one, and it rewrites a file that is also
// its input, so a replayed success over a stale file would skip the rewrite.
func runCodeownersSync(ctx *pctx.Context, emit *jsonl.Emitter, _ []string) (string, map[string]any, error) {
	workspaceOptions, err := wsview.WorkspaceOptionsFromContext(ctx)
	if err != nil {
		return "", nil, protocolcli.Classify(
			fmt.Errorf("decode workspace owners: %w", err),
			protocolcli.ErrInvalidConfig,
		)
	}
	ws, err := workspaceValidationView(ctx)
	if err != nil {
		return "", nil, err
	}
	report, verdict := sdd.BuildCodeownersResult(ws, workspaceOptions)
	publishDiagnostics(emit, report.Diagnostics)
	switch {
	case verdict != nil:
		emit.Summary(fmt.Sprintf("codeowners: %d finding(s), %s not written", len(report.Diagnostics), sdd.CodeownersPath))
	case !report.Adopted:
		emit.Summary("codeowners: no owners declared, " + sdd.CodeownersPath + " left alone")
	case report.Written:
		emit.Summary(fmt.Sprintf("codeowners: %d rule(s), %s rewritten", len(report.Rules), sdd.CodeownersPath))
	default:
		emit.Summary(fmt.Sprintf("codeowners: %d rule(s), %s up to date", len(report.Rules), sdd.CodeownersPath))
	}
	return finish(report, verdict)
}

// runDocsLinksValidate is the sixth step of the workspace-scoped
// `validate-workspace` command. It checks the relative links of every
// document of the workspace, whatever the selection, with the rule each
// language extension's `lint-docs` task applies inside its project.
//
// Like recipes-validate it is UNCACHEABLE: a link may name any file of the
// workspace, and a file's existence is not something a file-pattern key can
// express.
func runDocsLinksValidate(ctx *pctx.Context, emit *jsonl.Emitter, _ []string) (string, map[string]any, error) {
	workspaceOptions, err := wsview.WorkspaceOptionsFromContext(ctx)
	if err != nil {
		return "", nil, protocolcli.Classify(
			fmt.Errorf("decode workspace lint options: %w", err),
			protocolcli.ErrInvalidConfig,
		)
	}
	ws, err := workspaceValidationView(ctx)
	if err != nil {
		return "", nil, err
	}
	report, findings, verdict := sdd.BuildDocsLinksResult(ws, workspaceOptions)
	docslinks.Emit(emit, ws.Root, findings)
	emit.Summary(fmt.Sprintf("docs-links: %d document(s), %d broken link(s)",
		report.Documents, len(report.Findings)))
	return finish(report, verdict)
}

// projectValidationView builds the membership and the projection a PROJECT-
// SCOPED validation task runs over.
//
// The two are deliberately different sizes, and that is the engines' own
// two-tier contract rather than a convenience here (internal/features/scope.go).
// MEMBERSHIP is the whole workspace: durable feature manifests are the only
// artifacts that mint an identity, and a feature relation resolves against
// every one of them, so a view that saw one project reported a real
// cross-project relation as dangling — measured on go/templates/go-library,
// whose parent feature is authored in tooling/scaffold. PROJECTION is the job's
// OWN project: everything this task OWNS and reports is its project's.
//
// The projection is never the run's selection. The run selection says which
// projects the invocation acts on; this task acts on exactly one of them, and
// scoping it to the rest would make its verdict depend on how many siblings
// happened to be selected beside it — the same key, two answers.
func projectValidationView(ctx *pctx.Context) (*wsview.Workspace, sdd.Selection, error) {
	ws, _ := wsview.FromContext(ctx)
	if ws == nil || len(ws.Projects) == 0 {
		return nil, sdd.Selection{}, protocolcli.Classify(
			fmt.Errorf("the job context names no project to validate"),
			protocolcli.ErrInvalidConfig,
		)
	}
	own := wsview.ProjectIDFromPath(ctx.Project.Path)
	if ws.ProjectByID(own) == nil {
		return nil, sdd.Selection{}, protocolcli.Classify(
			fmt.Errorf("the job runs for project %q, which the workspace membership on the wire does not contain", own),
			protocolcli.ErrInvalidConfig,
		)
	}
	selection := sdd.Selection{
		Mode:       pctx.SelectionModeProjects,
		Scoped:     true,
		ProjectIDs: []string{own},
	}
	if ctx.Selection != nil {
		// The baseline is evidence about the RUN, reported verbatim so a reader
		// can join this verdict to the invocation that scheduled it. It never
		// changes what was evaluated.
		selection.Baseline = ctx.Selection.Baseline
		selection.BaselineSource = ctx.Selection.BaselineSource
	}
	return ws, selection, nil
}

// workspaceValidationView builds the membership a WORKSPACE-SCOPED validation
// task runs over, and refuses when the wire did not publish one.
//
// The refusal is the point. `architecture validate` asks two workspace-wide
// questions — is every project a manifest names a real member, and does any
// project depend across a domain boundary without a declared binding — and
// neither is answerable from a subset. Given only the selection, a narrowed run
// reports a real member as absent and never walks an edge into an unselected
// project: a false violation and a missed one. wsview attaches the
// incomplete-view warning in that case and the engine fails closed on it; this
// only turns the missing membership into a message that names the cause.
func workspaceValidationView(ctx *pctx.Context) (*wsview.Workspace, error) {
	ws, _ := wsview.FromContext(ctx)
	if ws == nil {
		return nil, protocolcli.Classify(
			fmt.Errorf("the job context carries no workspace"),
			protocolcli.ErrInvalidConfig,
		)
	}
	if len(ctx.WorkspaceProjects) == 0 {
		return nil, protocolcli.Classify(
			fmt.Errorf("the job context carries no workspace membership (`workspaceProjects`), "+
				"so no workspace-wide architecture verdict is provable; this orchestrator predates the member"),
			protocolcli.ErrInvalidConfig,
		)
	}
	return ws, nil
}

// publishDiagnostics forwards the engine's findings onto the JSONL wire so the
// CLI renders them the way it renders every other task's, and so a reader sees
// WHICH document failed rather than only that something did.
//
// The diagnostic Field is either "<path>" or "<path>#<member>", the convention
// discovery and the protocol validators already share, so it lands in the file
// position unchanged. There is no line number to report: these documents are
// validated as parsed JSON, not as text.
func publishDiagnostics(emit *jsonl.Emitter, diagnostics []diag.Diagnostic) {
	for _, finding := range diagnostics {
		emit.DiagnosticWithCode(string(finding.Severity), finding.Message, finding.Field, 0, 0, finding.Code)
	}
}

// finish turns an engine report into the JSONL result payload.
//
// The report travels whole: the engine already decided what a run may claim,
// and re-projecting it here would be a second opinion about the same verdict.
// A failing verdict returns the error as well, which is what selects the
// process exit code through the shared taxonomy.
func finish(report any, verdict error) (string, map[string]any, error) {
	data, err := reportData(report)
	if err != nil {
		return "", nil, err
	}
	if verdict != nil {
		return "", data, verdict
	}
	return "OK", data, nil
}

func reportData(report any) (map[string]any, error) {
	encoded, err := json.Marshal(report)
	if err != nil {
		return nil, protocolcli.Classify(fmt.Errorf("encode validation report: %w", err), protocolcli.ErrInvalidConfig)
	}
	var data map[string]any
	if err := json.Unmarshal(encoded, &data); err != nil {
		return nil, protocolcli.Classify(fmt.Errorf("decode validation report: %w", err), protocolcli.ErrInvalidConfig)
	}
	return data, nil
}
