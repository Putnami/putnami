package engine

import (
	"context"
	"io"
	"path/filepath"
	"sort"
	"strings"

	"go.putnami.dev/cli/model/extension"
	"go.putnami.dev/cli/model/workspace"
	"go.putnami.dev/tooling/cli/internal/abort"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/watch"
)

// This file is the WATCH ADAPTER over Engine.Run.
//
// Watch is a trigger and REPLAN POLICY, not a second lifecycle: internal/watch
// decides when to re-run and over which projects, and every iteration comes back
// through Engine.Run for the same planning, contract validation,
// missing-extension guards, cache setup, scheduling, session recording and exit
// code as any other run. Before A5b, internal/watch/session_iteration.go called
// jobs.Plan and jobs.NewScheduler itself — the last of the five hand-assembled
// lifecycles ADR 0001 §3 collapses.
//
// The dependency stays engine → watch. Pointing it the other way (watch calling
// Engine.Run directly) closes an import cycle, because the engine enters the loop
// from execute.go. The loop therefore takes an injected watch.RunIteration, which
// this file binds to Engine.Run — the same inversion A5a used for
// commands.LifecycleEnv.RunJob.

// runWatch enters the watch loop and maps its result onto the run's exit code.
func (e *Engine) runWatch(
	ctx context.Context,
	req *Request,
	ws *workspace.Workspace,
	selectedProjects []*workspace.Project,
	planExtensions []*extension.ExtensionDescription,
	sink EventSink,
	serveMode bool,
) SessionResult {
	code := watch.RunWatch(ctx, watch.SessionConfig{
		Workspace:        ws,
		SelectedProjects: selectedProjects,
		CommandParams:    req.CommandParams,
		// The one input the shared change→project mapping needs beyond the paths
		// themselves: a lockfile edit belongs to no project by path, but invalidates
		// every project whose tasks declare it as a workspace-scoped input.
		WorkspaceInputPatterns: workspaceScopedInputPatterns(planExtensions),
		Renderer:               sink,
		ServeMode:              serveMode,
		RunIteration: func(ctx context.Context, projects []*workspace.Project, iterationSink EventSink) watch.IterationResult {
			result, _ := e.Run(ctx, watchIterationRequest(req, planExtensions, projects, serveMode), iterationSink)
			return watchIterationResult(result)
		},
	})

	// Both watch loops exit their select on ctx.Done and return normally,
	// carrying the last iteration's code — which describes that iteration,
	// not why the session ended. Ctrl-C would otherwise report 0, or worse,
	// a stale failure from an iteration the user had already fixed.
	if abort.Source() != "" {
		return SessionResult{ExitCode: ExitSignalReceived}
	}
	return SessionResult{ExitCode: code}
}

// watchIterationRequest builds the Request for ONE replan iteration.
//
// It is the whole watch→engine mapping, in one place, so "what a watch iteration
// is allowed to do" is checkable by reading a single function. Routing a surface
// through the engine grants it the entire lifecycle unless the adapter says
// otherwise (A4 and A5a both learned this), so every field below is either
// forwarded deliberately or withheld deliberately:
//
//   - Observer is NEVER set (ADR 0001 §4). Watch reports session:start exactly
//     once, from the outer terminal run's initial plan, and nothing per
//     iteration. A per-iteration observer would turn one `putnami serve` into a
//     session report per file save — a consent incident arriving as a refactor
//     side effect.
//   - Hooks stay nil. The outer run's before/after hooks bracket the whole watch
//     SESSION, exactly as they did before A5b; firing them per iteration would
//     run a workspace's `before` hook on every keystroke-triggered rebuild.
//   - ValidateCommandFlags stays nil. The outer run already validated the command
//     line once; re-running it would reprint the undeclared-flag deprecation
//     warnings on every iteration.
//   - VersionSnapshot is FORWARDED, not recaptured. Iteration N's tree already
//     contains iteration N-1's generated output, so a fresh capture would fold
//     putnami's own writes into a false "-<dirtyhash>" version stamp —
//     and that stamp is part of every cache key.
//   - PlanExtensions pins the iteration to the extension set the FIRST plan used.
//     It is what keeps `putnami <group> serve --watch` from widening to every
//     discovered extension on entering the loop, and it preserves the pre-A5b
//     behavior of replanning against a frozen extension set.
//   - Stdout is discarded. The loop owns its own reporting (the "[watch] …"
//     chrome); the engine's generic notices — "No jobs matched. Nothing to do."
//     above all — would print on every save. Diagnostics stay on stderr, which is
//     not routed here for any adapter.
//   - Preflight is FORWARDED. Every iteration executes jobs, so every iteration
//     is gated exactly as the outer run is; withholding it would make an
//     ungated iteration out of a gated session — and, because a nil gate fails
//     closed, would break `--profile production --watch` loudly instead.
//   - RunMarkerParams is not set, and cannot matter: the two stages that read it
//     (auto-selection's speculative marker lookup and recordSuccessfulBuild) are
//     both unreachable for an iteration, per the selection flags below.
func watchIterationRequest(
	req *Request,
	planExtensions []*extension.ExtensionDescription,
	projects []*workspace.Project,
	serveMode bool,
) Request {
	return Request{
		WorkspaceRoot: req.WorkspaceRoot,
		Config:        req.Config,
		Commands:      req.Commands,
		Global:        watchIterationFlags(req.Global, projects, serveMode),
		// Forwarded verbatim: a param value's Go type is part of every cache key
		// (store.hashParams), so an iteration's keys stay identical to the ones the
		// same command produced outside watch.
		CommandParams:  req.CommandParams,
		PlanExtensions: planExtensions,
		// An extension alias forwards --dry-run to the extension as a job param and
		// still executes; dropping this would silently turn every iteration of
		// `putnami <group> serve --dry-run --watch` into a plan preview.
		ExecutesUnderDryRun: req.ExecutesUnderDryRun,
		watchIteration:      true,
		VersionSnapshot:     req.VersionSnapshot,
		Preflight:           req.Preflight,
		Stdout:              io.Discard,
	}
}

// watchIterationFlags derives one iteration's run-shaping flags from the outer
// run's. Five changes, each preserving what watch did before A5b:
//
//  1. Watch is cleared — the loop IS the replan policy, so an iteration must not
//     re-enter it.
//  2. The selection becomes the explicit project-ID list the loop computed. All,
//     Impacted and AutoSelected are cleared with it, which is also what keeps a
//     watch iteration from writing a successful-run marker: shouldRecordSuccessfulBuild
//     requires All or AutoSelected. A marker written per iteration would key
//     (branch, commands, params) → HEAD after rebuilding ONE project, and the next
//     `--impacted` run would skip everything up to that commit.
//  3. The tag/exclude filters are cleared. The loop classifies against the whole
//     workspace and hands over the result, exactly as it did before A5b;
//     re-applying the outer filters here would narrow that set. (Whether watch
//     SHOULD honor --filter-tag is a behavior question, not a refactor one.)
//  4. CacheTrust drops to "none", which disables the REMOTE cache while leaving
//     the local one alone. Watch never negotiated a remote cache, and doing it per
//     file save would put a network round trip — and a degraded-cache notice — in
//     the inner dev loop.
//  5. EnvProfile is cleared. The production preflight is an ENTRY gate the outer
//     run already passed; re-running a whole doctor scan on every save would put a
//     workspace-wide scan in the loop. EnvProfile is read by exactly one engine
//     stage (runProductionPreflight) and never reaches a job's environment.
//
// Non-serve iterations additionally force NoCache, reproducing the pre-A5b rule
// that only serve mode kept the cache (so a restart can skip generate steps)
// while test/lint/build watch runs always run fresh. The forced value leaves
// NoCacheExplicit alone, so an extension sees `cache: false` only when the user
// typed --no-cache.
func watchIterationFlags(outer GlobalFlags, projects []*workspace.Project, serveMode bool) GlobalFlags {
	g := outer
	g.Watch = false

	ids := make([]string, len(projects))
	for i, p := range projects {
		ids[i] = p.ID
	}
	g.Projects = strings.Join(ids, ",")
	g.All = false
	g.Impacted = false
	g.AutoSelected = false
	g.FilterTag = ""
	g.ExcludeTag = ""
	g.Exclude = ""

	g.CacheTrust = string(store.CacheTrustNone)
	g.EnvProfile = ""

	if !serveMode {
		g.NoCache = true
	}
	return g
}

// watchIterationResult maps one engine run onto the loop's verdict.
//
// The one translation: an iteration that scheduled NOTHING and ended on the
// engine's generic usage code is a no-op, not a failure. The loop classifies
// changed files against the whole workspace, so it routinely selects a project
// that declares no `test` job — before A5b it planned that case itself and
// reported "[watch] done" with exit 0, while the engine's verdict for an empty
// plan is ExitUsage. A plan ERROR (ExitError), a missing required extension
// (ExitError) and a failed execution all keep their code.
func watchIterationResult(result SessionResult) watch.IterationResult {
	code := result.ExitCode
	if result.Session == nil && code == ExitUsage {
		code = ExitSuccess
	}
	return watch.IterationResult{
		ExitCode: code,
		Aborted:  result.Session != nil && result.Session.Aborted,
	}
}

// workspaceScopedInputPatterns collects the file patterns declared by task inputs
// scoped `from: "workspace"` — lockfiles, root compiler/linter config — UNION the
// metadata inputs and markers declared by each extension's workspace adapter.
// They are the widening the deleted watch classifier owned: such a file belongs to
// no project by path, yet changing it invalidates every project that may run the
// task. Sorted and deduplicated so the same extension set always yields the same
// patterns.
//
// The adapter half extends this seam. It closes a
// gap the task-input half cannot: a provider's metadata inputs are what decide
// the workspace snapshot's validity, so a change to one MUST reach a watch
// iteration — the iteration is where the snapshot is re-validated and the one
// owning provider is re-probed. A metadata input that no watch iteration ever
// noticed would leave the workspace resolved from a stale index until the next
// command outside the loop.
//
// The patterns are workspace-ROOT relative here, exactly like the task-declared
// ones, because that is what ChangeImpactOptions means by a workspace-scoped
// input: a file that belongs to no project by path. Adapter patterns containing
// a slash stay candidate-relative and are omitted: a nested project's own file
// already maps to that project through the path-prefix rule. Treating a
// classification witness such as `**/*.go` as workspace-root-scoped would turn
// every Go source edit into a full-workspace rebuild merely to trigger the
// snapshot revalidation that the owning project's ordinary watch event already
// reaches.
func workspaceScopedInputPatterns(extensions []*extension.ExtensionDescription) []string {
	seen := make(map[string]bool)
	var patterns []string
	add := func(pattern string) {
		if pattern == "" || seen[pattern] {
			return
		}
		seen[pattern] = true
		patterns = append(patterns, pattern)
	}
	for _, ext := range extensions {
		if ext == nil {
			continue
		}
		for _, task := range ext.Tasks {
			for _, input := range task.Inputs {
				if input.From != "workspace" {
					continue
				}
				for _, pattern := range input.Files {
					add(pattern)
				}
			}
		}
		if ext.Workspace != nil {
			for _, pattern := range ext.Workspace.Inputs {
				if strings.Contains(filepath.ToSlash(pattern), "/") {
					continue
				}
				add(pattern)
			}
			for _, pattern := range ext.Workspace.Markers {
				if strings.Contains(filepath.ToSlash(pattern), "/") {
					continue
				}
				add(pattern)
			}
		}
	}
	sort.Strings(patterns)
	return patterns
}
