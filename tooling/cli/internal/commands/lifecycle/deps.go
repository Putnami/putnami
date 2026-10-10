package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"go.putnami.dev/cli/model/extension"
	protocolcli "go.putnami.dev/protocol/cli"
	extensionproto "go.putnami.dev/protocol/extension"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commands/shared"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/runcredential"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// WorkspaceJobOutcome reports how a workspace-level lifecycle job run ended.
type WorkspaceJobOutcome int

const (
	// WorkspaceJobOK: the job ran and every scheduled task succeeded.
	WorkspaceJobOK WorkspaceJobOutcome = iota
	// WorkspaceJobMissing: no discovered extension provides the job at all.
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
	// AvailableJobs lists the providable job names, set only for
	// WorkspaceJobMissing so the caller can print an actionable error.
	AvailableJobs string
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
	// channel is the release channel `putnami init` resolves on when it is not
	// latest; every other command leaves it empty. The workspace installers
	// receive it as a job option (installParams), and the project create of
	// init resolves its template and its Go framework version on it.
	channel string
}

// installParams is the job options of the workspace fetch and installers: the
// channel of an init that resolves on one, as the option `putnami upgrade`
// hands the deps-upgrade job, else none.
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
//     store, which receives the job credential (jobs.WithDependencyFetch);
//  2. BeforeRepositoryCode, which hands the run credential to the remote cache
//     provider;
//  3. the workspace-fetch of every path extension, which is repository code
//     and receives nothing (jobs.WithPathExtensionFetch).
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
	if err := depsFetch(jobs.WithDependencyFetch(ctx), wsRoot, cfg, filterTag, excludeTag, e); err != nil {
		return err
	}
	if e.BeforeRepositoryCode != nil {
		if err := e.BeforeRepositoryCode(ctx); err != nil {
			return err
		}
	}
	return depsFetch(jobs.WithPathExtensionFetch(ctx), wsRoot, cfg, filterTag, excludeTag, e)
}

// DepsInstall runs the "workspace-install" task from all extensions that
// provide it, effectively installing project dependencies.
// filterTag/excludeTag scope which extensions run (by matching project tags).
//
// A hosted run (runcredential.Hosted) first runs the store extensions' fetch,
// env.BeforeRepositoryCode, then the path extensions' fetch, each to
// completion (LifecycleEnv.fetchBeforeRepositoryCode): the fetch downloads the
// dependencies, and the installers then run offline.
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
		iox.Fprintf(os.Stderr, "  No extension provides the %q job.\n", "workspace-install")
		iox.Fprintf(os.Stderr, "  Available jobs: %s\n", result.AvailableJobs)
		return fmt.Errorf("no job found for deps install")
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

// DepsAdd adds one or more dependencies to a project's manifest via the owning
// ecosystem's tooling, then reconciles the dependency closure so the standing
// "run `putnami deps …` instead of a package manager" rule has a real path for
// adding, not just installing. Only Go modules are wired today (`go get` +
// `go mod tidy`); a TypeScript target reports an actionable message rather than
// silently doing nothing. env runs the workspace installers when the host has
// no go command yet. When those installers fail but still leave a go, the
// module is edited with it and the command exits non-zero naming the command
// that finishes the install, as `projects create` does.
func DepsAdd(ctx context.Context, wsRoot string, modules []string, projectSelector string, env LifecycleEnv) error {
	installErr, err := runGoDeps(ctx, wsRoot, "add", modules, projectSelector, depsCommand("add", modules, projectSelector), env)
	return goDepsResult("add", installErr, err)
}

// DepsRemove drops one or more dependencies from a Go module (`go get mod@none`)
// and tidies the closure. See DepsAdd for the ecosystem/selection rules and
// the failed-install report.
func DepsRemove(ctx context.Context, wsRoot string, modules []string, projectSelector string, env LifecycleEnv) error {
	installErr, err := runGoDeps(ctx, wsRoot, "remove", modules, projectSelector, depsCommand("remove", modules, projectSelector), env)
	return goDepsResult("remove", installErr, err)
}

// depsCommand is the deps command the user ran: action on modules, which prune
// takes from the workspace rather than its arguments, with the project
// selector the user gave.
func depsCommand(action string, modules []string, projectSelector string) string {
	command := "putnami deps " + action
	if action != "prune" {
		command += " " + strings.Join(modules, " ")
	}
	if projectSelector != "" {
		command += " --projects " + projectSelector
	}
	return command
}

// goDepsResult is the error of a deps command that edited Go modules. err is
// the failure of the edit itself. installErr is the failure of the workspace
// installers that installed the go the edit ran with: the edit still ran, and
// the command fails naming the command that finishes the install, so a failed
// install never ends in success.
func goDepsResult(action string, installErr, err error) error {
	if installErr == nil {
		return err
	}
	return protocolcli.WithNext(
		errors.Join(err, fmt.Errorf("install workspace dependencies during deps %s: %w", action, installErr)),
		"putnami deps install")
}

// runGoDeps resolves the target Go module and the go command (goDepsCommand),
// then edits the module's requirements (editGoModule). installErr is the
// failure of the workspace installers that installed the go; err is the
// failure of the edit. The caller reports both (goDepsResult).
func runGoDeps(ctx context.Context, wsRoot, action string, modules []string, projectSelector, rerun string, lifecycleEnv LifecycleEnv) (installErr, err error) {
	if len(modules) == 0 {
		return nil, fmt.Errorf("deps %s requires at least one module (e.g. `putnami deps %s golang.org/x/text@latest`)", action, action)
	}

	ws, err := workspace.Load(wsRoot)
	if err != nil {
		return nil, fmt.Errorf("load workspace: %w", err)
	}
	proj, err := resolveGoDepsProject(wsRoot, ws, projectSelector)
	if err != nil {
		return nil, err
	}
	goCmd, err := goDepsCommand(ctx, wsRoot, proj.Path, action, rerun, lifecycleEnv)
	if err != nil {
		return nil, err
	}
	return goCmd.installErr, editGoModule(ctx, wsRoot, proj, goCmd, action, modules)
}

// goDepsCommand returns the go command a deps edit runs: the one a task of the
// workspace's Go extension runs, the release the workspace lock pins, installed
// first on a host that has none (ensureGoCommand). A caller resolves it before
// its first edit, so a failure edits no go.mod or putnami.json. When the version
// probe of a go timed out, that go is there and slow: the error names rerun, the
// deps command the user ran, as the next step. Any other failure to find a go
// names putnami install.
func goDepsCommand(ctx context.Context, wsRoot, projectPath, action, rerun string, env LifecycleEnv) (goCommand, error) {
	goCmd, err := ensureGoCommand(ctx, wsRoot, projectPath, "", goToolchainExtension(wsRoot), env)
	if err != nil {
		next := "putnami install"
		if errors.Is(err, jobs.ErrToolchainProbeTimeout) {
			next = rerun
		}
		return goCommand{}, protocolcli.WithNext(fmt.Errorf("deps %s: %w", action, err), next)
	}
	return goCmd, nil
}

// editGoModule edits the requirements of proj's Go module with goCmd and runs
// `go mod tidy`, both in the module directory with GOWORK pointed at the
// workspace file so `replace` directives resolve. add runs `go get` with the
// module specs as given; remove runs `go get <module>@none`; prune runs `go mod
// edit -droprequire=<module>` (goDepsEdit says why).
func editGoModule(ctx context.Context, wsRoot string, proj *workspace.Project, goCmd goCommand, action string, modules []string) error {
	moduleDir := filepath.Join(wsRoot, proj.Path)
	env := shared.GoCommandEnvFrom(goCmd.env, moduleDir)

	verb := map[string]string{"add": "Adding", "remove": "Removing", "prune": "Removing"}[action]
	iox.Fprintf(os.Stdout, "  %s %s in %s...\n", verb, strings.Join(modules, " "), proj.Name)

	edit, name := goDepsEdit(action, modules)
	if err := runGoInModule(ctx, goCmd.path, moduleDir, env, edit); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	// Reconcile the module graph so the closure and go.sum stay consistent.
	if err := runGoInModule(ctx, goCmd.path, moduleDir, env, []string{"mod", "tidy"}); err != nil {
		return fmt.Errorf("go mod tidy: %w", err)
	}
	iox.Fprintf(os.Stdout, "  ✓ updated %s\n", filepath.Join(proj.Path, "go.mod"))
	return nil
}

// goDepsEdit returns the go arguments that edit the module's requirements for
// action, and the command's name for an error.
//
// prune drops each requirement from go.mod without resolving it. `go get
// <module>@none` loads the module graph, which resolves every required version
// through the module proxy even when go.work uses that module. A requirement
// that only go.work satisfies, such as `require example.com/lib v0.0.0` of a
// workspace module no proxy serves, then fails the very prune that removes it.
// The requirements prune drops are workspace modules no import backs, so the
// `go mod tidy` that follows has nothing of theirs to resolve.
func goDepsEdit(action string, modules []string) (args []string, name string) {
	switch action {
	case "prune":
		args = []string{"mod", "edit"}
		for _, m := range modules {
			args = append(args, "-droprequire="+strings.SplitN(m, "@", 2)[0])
		}
		return args, "go mod edit"
	case "remove":
		// `go get <mod>@none` drops a requirement; strip any version the caller gave.
		args = []string{"get"}
		for _, m := range modules {
			args = append(args, strings.SplitN(m, "@", 2)[0]+"@none")
		}
		return args, "go get"
	default:
		return append([]string{"get"}, modules...), "go get"
	}
}

// runGoInModule runs the go command at goPath with args in dir with the given
// environment, streaming output to the user's terminal. What the go command
// runs is up to the module the repository declares (a toolchain directive
// selects another go), so this process records repository code before it
// starts it (runcredential.MarkRepositoryCodeStarted).
func runGoInModule(ctx context.Context, goPath, dir string, env, args []string) error {
	runcredential.MarkRepositoryCodeStarted("go " + strings.Join(args, " "))
	cmd := exec.CommandContext(ctx, goPath, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// goModExists reports whether dir is a Go module (has a go.mod file).
func goModExists(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, "go.mod"))
	return err == nil && !info.IsDir()
}

// resolveGoDepsProject selects the single Go module a deps add/remove targets. A
// --projects selector wins (and must name a Go module); otherwise the sole Go
// module in the workspace is used. No selector with several Go modules, or a
// non-Go target, is an actionable error rather than a silent guess.
func resolveGoDepsProject(wsRoot string, ws *workspace.Workspace, selector string) (*workspace.Project, error) {
	selector = strings.TrimSpace(selector)
	switch selector {
	case "", "*", ".", "[impacted]":
		// No explicit single-project selection — fall through to auto-detect.
	default:
		if strings.Contains(selector, ",") {
			return nil, fmt.Errorf("deps add/remove targets a single project; got --projects %q", selector)
		}
		var p *workspace.Project
		for _, cand := range ws.Projects {
			if cand.Name == selector {
				p = cand
				break
			}
		}
		if p == nil {
			return nil, fmt.Errorf("project %q not found in the workspace", selector)
		}
		if !goModExists(filepath.Join(wsRoot, p.Path)) {
			return nil, fmt.Errorf("project %q is not a Go module (no go.mod); for TypeScript, edit package.json and run `putnami deps install`", p.Name)
		}
		return p, nil
	}

	var goProjects []*workspace.Project
	for _, p := range ws.Projects {
		if goModExists(filepath.Join(wsRoot, p.Path)) {
			goProjects = append(goProjects, p)
		}
	}
	switch len(goProjects) {
	case 0:
		return nil, fmt.Errorf("no Go module found in the workspace; `deps add`/`deps remove` currently support Go modules only")
	case 1:
		return goProjects[0], nil
	default:
		names := make([]string, len(goProjects))
		for i, p := range goProjects {
			names[i] = p.Name
		}
		return nil, fmt.Errorf("multiple Go modules in the workspace; select one with --projects <name> (%s)", strings.Join(names, ", "))
	}
}

// runWorkspaceJob materializes the workspace's lock-pinned artifacts and then
// hands the run to the injected engine-backed runner.
//
// Before slice A5a this function WAS a private lifecycle: its own
// workspace load, its own extension discovery, its own jobs.Plan, its own
// dedup-by-extension filter, its own SchedulerConfig and its own
// jobs.NewScheduler — the third of the five hand-assembled copies ADR 0001 §3
// collapses. All of it now lives behind LifecycleEnv.RunJob, over Engine.Run.
func runWorkspaceJob(ctx context.Context, env LifecycleEnv, req WorkspaceJobRequest) (WorkspaceJobResult, error) {
	// Structured commands (deps install, upgrade) skip App.Run's pre-discovery
	// EnsureArtifacts, so on a fresh zero-init worktree the lock-pinned
	// workspace-install/deps-upgrade providers would not yet be materialized and
	// the run's extension discovery would find none. Materialize them FIRST; it
	// is deduped per-process and a no-op on the warm path, and stays best-effort
	// (WorkspaceJobMissing remains the authoritative "no provider" signal).
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

// FormatJobNames renders a job map's names as a stable, comma-separated list for
// the "no extension provides that job" message. It is exported for the lifecycle
// adapter in internal/cli, which owns the discovery that produces the map.
func FormatJobNames(jobMap map[string][]*extension.JobDefinition) string {
	names := make([]string, 0, len(jobMap))
	for name := range jobMap {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}
