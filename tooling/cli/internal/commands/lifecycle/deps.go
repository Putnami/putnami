package lifecycle

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	protocolcli "go.putnami.dev/protocol/cli"
	extensionproto "go.putnami.dev/protocol/extension"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/cmderr"
	"go.putnami.dev/tooling/cli/internal/commands/shared"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/jobs/scheduler"
	"go.putnami.dev/tooling/cli/internal/runcredential"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// WorkspaceJobOutcome reports how a workspace-level lifecycle job run ended.
type WorkspaceJobOutcome int

const (
	// WorkspaceJobOK: the job ran and every scheduled task succeeded.
	WorkspaceJobOK WorkspaceJobOutcome = iota
	// WorkspaceJobMissing: no discovered extension provides the job at all, and
	// every extension the workspace needs loaded.
	WorkspaceJobMissing
	// WorkspaceJobNoMatches: the job exists but matched no project, so nothing
	// ran. It is a legitimate no-op, not a failure.
	WorkspaceJobNoMatches
	// WorkspaceJobFailed: the run executed and failed, or ended before it could.
	WorkspaceJobFailed
)

// WorkspaceJobRequest describes one workspace-level lifecycle job run.
//
// Params carries the job parameters verbatim. A param value's Go TYPE is part of
// every cache key (store.hashParams) and of every run-marker key
// (workspace_state.LastBuildParamsHash), so nothing on the way to the scheduler
// may rebuild, reorder, or re-type this map.
type WorkspaceJobRequest struct {
	WorkspaceRoot string
	Config        *wsproto.Config
	// Job is the workspace-level job name ("workspace-install", "deps-upgrade").
	Job    string
	Params map[string]any
	// Projects selects the projects whose extensions run the job, as a
	// --projects value: comma-separated project IDs for the project jobs of
	// create and deps (project_jobs.go). Empty selects every project.
	Projects string
	// FilterTag/ExcludeTag scope which projects (and therefore which providers)
	// take part, e.g. --tag go on a Go-only CI runner.
	FilterTag  string
	ExcludeTag string
	// ContinueOnError lets the run finish the remaining providers after one
	// fails. `upgrade` sets it from --continue-on-error so a channel one
	// ecosystem does not serve stops the others instead of half-upgrading the
	// workspace.
	ContinueOnError bool
	// NoCache records that the user typed --no-cache on the lifecycle command.
	// The job runs uncached either way; this only decides whether extensions
	// receive `no-cache`/`cache: false`. runWorkspaceJob fills it from
	// LifecycleEnv.NoCache.
	NoCache bool
	// Out receives the run's human-readable progress.
	Out io.Writer
	// Display carries terminal presentation preferences from the outer lifecycle
	// command. It never affects planning, cache keys, or execution.
	Display LifecycleDisplay
	// OnAction receives one deterministic action after a provider's complete
	// workspace-level pipeline succeeds. It is nil for callers that do not need
	// to compose a higher-level install summary.
	OnAction func(LifecycleAction)
}

// LifecycleDisplay controls only human lifecycle rendering. Structured output
// is deliberately not forwarded to the nested workspace-job renderer: the
// outer command owns stdout's machine contract, while the nested run may write
// human diagnostics only to its explicitly supplied stream.
type LifecycleDisplay struct {
	Verbose     bool
	Debug       bool
	Quiet       bool
	NoColor     bool
	Interactive bool
}

// LifecycleAction is one completed, user-visible mutation or reconciliation
// step. Description is already suitable for concise human rendering; Kind and
// Name keep tests and future renderers from having to parse it.
type LifecycleAction struct {
	Kind        string
	Name        string
	Description string
}

// WorkspaceJobResult is what a workspace-level lifecycle job run produced.
type WorkspaceJobResult struct {
	Outcome WorkspaceJobOutcome
}

// WorkspaceJobRunner runs one workspace-level lifecycle job through the CLI
// engine (Engine.Run), which owns planning, the workspace-once dedup, execution
// and rendering.
//
// It is INJECTED rather than called directly to keep the dependency pointing
// one way: internal/cli — which already owns the terminal and extension-alias
// adapters, and is the only package that knows how a run is rendered — supplies
// the binding (cli.RunWorkspaceJob) and is the only production supplier. A nil
// runner fails LOUDLY (see runWorkspaceJob): silently skipping the install is
// the one outcome that must never happen.
//
// Until slice C8 the reason was mechanical: internal/engine imported
// this package for the production doctor gate, so `commands` could not import
// `engine` at all without a cycle. That edge is inverted now
// (engine.PreflightGate), and the ratchet in internal/cli keeps it inverted, so
// the direction here is a decision rather than a consequence.
type WorkspaceJobRunner func(context.Context, WorkspaceJobRequest) (WorkspaceJobResult, error)

// LifecycleEnv is what a lifecycle command needs from its caller: where human
// progress goes, and how a workspace-level job runs.
//
// Out exists so the first-use bootstrap can send an implicit install's chatter
// to stderr under --output=json|jsonl. Before slice A5a that was done by
// reassigning the process-global os.Stdout under a mutex for the duration of the
// install (withStdoutRoutedTo) — the only global stdout replacement in the tree.
type LifecycleEnv struct {
	// Out receives human progress output. Nil means os.Stdout.
	Out io.Writer
	// RunJob runs one workspace-level job through the CLI engine.
	RunJob WorkspaceJobRunner
	// CLIVersion is the running binary's exact version. It lets install stamp an
	// existing pin's machine-output protocol only when the pin names this binary;
	// an empty value leaves the declaration untouched.
	CLIVersion string
	// Display controls human presentation without changing lifecycle behavior.
	Display LifecycleDisplay
	// OnAction composes completed work into a caller-owned final summary.
	OnAction func(LifecycleAction)
	// NoCache records that the user typed --no-cache on THIS lifecycle command,
	// and runWorkspaceJob forwards it to every job the command runs. The
	// first-use bootstrap leaves it false: a --no-cache typed on the build that
	// triggered the implicit install is about that build, and forwarding it
	// would leave the worktree without its remote-cache configuration.
	NoCache bool
	// BeforeRepositoryCode runs on a hosted run (runcredential.Hosted) once
	// the workspace-fetch of every extension installed from the artifact store
	// has run, before the first repository code of the command: a path
	// extension's workspace-fetch, an extension onInstall hook or a workspace
	// installer. The first-use bootstrap starts the hosted run's remote cache
	// provider there, because no process that starts after repository code
	// receives the run credential. Nil does nothing. An error fails the command
	// before any repository code runs.
	BeforeRepositoryCode func(context.Context) error
	// fetched records that Install already ran the hosted fetch steps and
	// BeforeRepositoryCode, so DepsInstall runs none of them again.
	fetched bool
	// channel is the release channel `putnami init` or `putnami projects
	// create` resolves on when it is not latest; every other command leaves it
	// empty. The workspace installers and the project-setup job receive it as a
	// job option (installParams), and the project create resolves its template
	// on it.
	channel string
	// pinToolchains is the lock step create takes when a toolchain the
	// project-setup job requires does not resolve (setUpProject). Nil is
	// fillImplicitToolchainPins.
	pinToolchains func(ctx context.Context, wsRoot string) (bool, error)
}

// installParams is the job options of the workspace fetch and installers: the
// channel of an init or a create that resolves on one, as the option
// `putnami upgrade` hands the deps-upgrade job, else none.
func (e LifecycleEnv) installParams() map[string]any {
	if e.channel == "" {
		return nil
	}
	return map[string]any{initChannelJobOption: e.channel}
}

// out returns the caller's human stream, defaulting to os.Stdout.
func (e LifecycleEnv) out() io.Writer {
	if e.Out != nil {
		return e.Out
	}
	return os.Stdout
}

// fetchBeforeRepositoryCode is the first step of a hosted run's workspace
// installation (runcredential.Hosted), in the scope of the installers that
// follow. It runs, each to completion:
//
//  1. the workspace-fetch of every extension installed from the artifact
//     store, which receives the job credential (scheduler.WithDependencyFetch);
//  2. BeforeRepositoryCode, which hands the run credential to the remote cache
//     provider;
//  3. the workspace-fetch of every path extension, which is repository code
//     and receives nothing (scheduler.WithPathExtensionFetch).
//
// A hosted run hands its credential to no process that starts after
// repository code, so the path extensions' fetch runs last. It does nothing
// without a run credential, or when Install already ran it.
func (e LifecycleEnv) fetchBeforeRepositoryCode(ctx context.Context, wsRoot string, cfg *wsproto.Config, filterTag, excludeTag string) error {
	if !runcredential.Hosted() || e.fetched {
		return nil
	}
	// Only the jobs of this run receive the job credential of a hosted run;
	// every one of them must.
	if err := depsFetch(scheduler.WithDependencyFetch(ctx), wsRoot, cfg, filterTag, excludeTag, e); err != nil {
		return err
	}
	if e.BeforeRepositoryCode != nil {
		if err := e.BeforeRepositoryCode(ctx); err != nil {
			return err
		}
	}
	return depsFetch(scheduler.WithPathExtensionFetch(ctx), wsRoot, cfg, filterTag, excludeTag, e)
}

// DepsInstall runs the "workspace-install" task from all extensions that
// provide it, effectively installing project dependencies.
// filterTag/excludeTag scope which extensions run (by matching project tags).
//
// A hosted run (runcredential.Hosted) first runs the store extensions' fetch,
// env.BeforeRepositoryCode, then the path extensions' fetch, each to
// completion (LifecycleEnv.fetchBeforeRepositoryCode): the fetch downloads the
// dependencies, and the installers then run offline.
//
// A workspace where no extension provides "workspace-install", and every
// extension it needs loaded, has nothing to install: DepsInstall succeeds and
// prints one line saying so, unless ctx marks an implicit install
// (shared.WithImplicitInstall). When an extension did not load, the runner
// fails the run instead, since that extension may have been the provider.
func DepsInstall(ctx context.Context, wsRoot string, cfg *wsproto.Config, filterTag, excludeTag string, env LifecycleEnv) error {
	if err := env.fetchBeforeRepositoryCode(ctx, wsRoot, cfg, filterTag, excludeTag); err != nil {
		return err
	}
	result, err := runWorkspaceJob(ctx, env, WorkspaceJobRequest{
		WorkspaceRoot: wsRoot,
		Config:        cfg,
		Job:           "workspace-install",
		Params:        env.installParams(),
		FilterTag:     filterTag,
		ExcludeTag:    excludeTag,
		Display:       env.Display,
		OnAction:      env.OnAction,
	})
	if err != nil {
		return err
	}
	switch result.Outcome {
	case WorkspaceJobMissing:
		// No loaded extension installs anything, so there is nothing to do. An
		// implicit install stays silent; an explicit command says so once.
		if !shared.IsImplicitInstall(ctx) {
			iox.Fprintln(env.out(), "  No extension provides workspace-install, so there is nothing to install.")
		}
		return nil
	case WorkspaceJobNoMatches:
		iox.Fprintln(env.out(), "  No jobs matched for deps install.")
		return nil
	case WorkspaceJobFailed:
		return fmt.Errorf("deps install failed")
	}
	return nil
}

// depsFetch runs extensionproto.WorkspaceFetchCommand from every extension
// that provides it and that the step ctx marks plans, with the scope of the
// install that follows. A workspace whose extensions provide no fetch, or
// whose fetch matches no project, has nothing to download, and the install
// runs as it would. A failed fetch fails the install before any installer
// runs.
func depsFetch(ctx context.Context, wsRoot string, cfg *wsproto.Config, filterTag, excludeTag string, env LifecycleEnv) error {
	result, err := runWorkspaceJob(ctx, env, WorkspaceJobRequest{
		WorkspaceRoot: wsRoot,
		Config:        cfg,
		Job:           extensionproto.WorkspaceFetchCommand,
		Params:        env.installParams(),
		FilterTag:     filterTag,
		ExcludeTag:    excludeTag,
		Display:       env.Display,
		OnAction:      env.OnAction,
	})
	if err != nil {
		return err
	}
	if result.Outcome == WorkspaceJobFailed {
		return fmt.Errorf("deps fetch failed")
	}
	return nil
}

// DepsAdd adds one or more dependencies to a project's manifest through the
// deps-edit job of the project's extension, which edits the manifest of its
// language and reconciles the dependency closure, so the standing "run
// `putnami deps …` instead of a package manager" rule has a real path for
// adding, not just installing. A project whose extensions declare no
// deps-edit job is refused, naming them (depsEditTarget). A spec at the
// version none is refused before any job runs (refuseDropOnAdd).
func DepsAdd(ctx context.Context, wsRoot string, modules []string, projectSelector string, env LifecycleEnv) error {
	if err := refuseDropOnAdd(modules, projectSelector); err != nil {
		return err
	}
	return editDeps(ctx, wsRoot, "add", modules, projectSelector, env)
}

// refuseDropOnAdd fails `deps add` with a usage error for a spec whose version
// is none, which a package manager reads as "drop this module". The error
// suggests the `deps remove` command that does it.
func refuseDropOnAdd(modules []string, projectSelector string) error {
	var dropped []string
	for _, spec := range modules {
		if path, version, ok := splitSpecVersion(spec); ok && version == "none" {
			dropped = append(dropped, path)
		}
	}
	if len(dropped) == 0 {
		return nil
	}
	next := "putnami deps remove " + strings.Join(dropped, " ")
	if projectSelector != "" {
		next += " --projects " + projectSelector
	}
	return protocolcli.WithNext(
		protocolcli.Usagef("deps add does not drop a module: %s names the version none; deps remove drops it", strings.Join(dropped, ", ")),
		next)
}

// splitSpecVersion splits a dependency spec at its last "@" that is not its
// first character, so a scoped name such as @scope/pkg has no version and
// @scope/pkg@none has the version none.
func splitSpecVersion(spec string) (path, version string, ok bool) {
	i := strings.LastIndex(spec, "@")
	if i <= 0 {
		return spec, "", false
	}
	return spec[:i], spec[i+1:], true
}

// DepsRemove drops one or more dependencies from a project's manifest through
// the deps-edit job of the project's extension. See DepsAdd.
func DepsRemove(ctx context.Context, wsRoot string, modules []string, projectSelector string, env LifecycleEnv) error {
	return editDeps(ctx, wsRoot, "remove", modules, projectSelector, env)
}

// editDeps selects the project and its editor (depsEditTarget), then runs the
// editor's deps-edit job with action on modules.
func editDeps(ctx context.Context, wsRoot, action string, modules []string, projectSelector string, env LifecycleEnv) error {
	if len(modules) == 0 {
		return cmderr.Usagef("deps %s requires at least one dependency: putnami deps %s <dependency>... [--projects <name>]", action, action)
	}
	ws, err := workspace.Load(wsRoot)
	if err != nil {
		return fmt.Errorf("load workspace: %w", err)
	}
	editors, err := projectJobProviders(ctx, wsRoot, ws, depsEditJob)
	if err != nil {
		return err
	}
	project, err := depsEditTarget(ws, editors, action, projectSelector)
	if err != nil {
		return err
	}
	return runDepsEdit(ctx, wsRoot, editors[project.ID], action, []depsEdit{{project: project, modules: modules}}, env)
}

// depsEditTarget selects the single project a deps add or remove edits. A
// --projects selector names it, by name or ID, and a project whose extensions
// declare no deps-edit job (editors) is refused. Without a selector, the sole
// project with an editor is used, and several are an error that lists them.
func depsEditTarget(ws *workspace.Workspace, editors map[string]string, action, selector string) (*workspace.Project, error) {
	selector = strings.TrimSpace(selector)
	switch selector {
	case "", "*", ".", "[impacted]":
		// No single project named: the sole one with an editor.
	default:
		if strings.Contains(selector, ",") {
			return nil, fmt.Errorf("deps %s edits a single project; got --projects %q", action, selector)
		}
		for _, project := range ws.Projects {
			if project.Name != selector && project.ID != selector {
				continue
			}
			if editors[project.ID] == "" {
				return nil, fmt.Errorf("deps %s cannot edit %s: no extension of the project declares the %s job, and %s; "+
					"edit the project's manifest by hand, then run `putnami deps install`",
					action, project.Name, depsEditJob, projectExtensionsText(project))
			}
			return project, nil
		}
		return nil, fmt.Errorf("project %q not found in the workspace", selector)
	}

	var editable []*workspace.Project
	for _, project := range ws.Projects {
		if editors[project.ID] != "" {
			editable = append(editable, project)
		}
	}
	switch len(editable) {
	case 0:
		return nil, fmt.Errorf("deps %s: no project of the workspace has an extension that declares the %s job; "+
			"edit the project's manifest by hand, then run `putnami deps install`", action, depsEditJob)
	case 1:
		return editable[0], nil
	default:
		names := make([]string, len(editable))
		for i, project := range editable {
			names[i] = project.Name
		}
		return nil, fmt.Errorf("deps %s: several projects have an extension that declares the %s job; select one with --projects <name> (%s)",
			action, depsEditJob, strings.Join(names, ", "))
	}
}

// runWorkspaceJob materializes the workspace's lock-pinned artifacts and then
// hands the run to the injected engine-backed runner.
//
// Before slice A5a this function WAS a private lifecycle: its own
// workspace load, its own extension discovery, its own planner call, its own
// dedup-by-extension filter, its own SchedulerConfig and its own
// scheduler — the third of the five hand-assembled copies ADR 0001 §3
// collapses. All of it now lives behind LifecycleEnv.RunJob, over Engine.Run.
func runWorkspaceJob(ctx context.Context, env LifecycleEnv, req WorkspaceJobRequest) (WorkspaceJobResult, error) {
	// Structured commands (deps install, upgrade) skip App.Run's pre-discovery
	// EnsureArtifacts, so on a fresh zero-init worktree the lock-pinned
	// workspace-install/deps-upgrade providers would not yet be materialized and
	// the run's extension discovery would find none. Materialize them FIRST; it
	// is deduped per-process and a no-op on the warm path, and stays best-effort:
	// an extension it could not download does not load, and the runner then
	// fails the run naming it, rather than report WorkspaceJobMissing, which
	// means that no declared extension provides the job.
	_ = EnsureArtifacts(ctx, req.WorkspaceRoot, req.Config)

	if env.RunJob == nil {
		// Fail loudly rather than report a silent no-op: a lifecycle command whose
		// runner was never wired must not look like a workspace that had nothing
		// to install (see WorkspaceJobRunner).
		return WorkspaceJobResult{Outcome: WorkspaceJobFailed},
			fmt.Errorf("no workspace job runner wired for %q", req.Job)
	}
	if req.Out == nil {
		req.Out = env.out()
	}
	if env.NoCache {
		req.NoCache = true
	}
	return env.RunJob(ctx, req)
}
