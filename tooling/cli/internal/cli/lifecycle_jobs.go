package cli

import (
	"context"
	"io"
	"os"
	"sort"

	extensionproto "go.putnami.dev/protocol/extension"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commands/doctor"
	"go.putnami.dev/tooling/cli/internal/commands/lifecycle"
	"go.putnami.dev/tooling/cli/internal/engine"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/output"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// lifecycleEnv is what every lifecycle entry point hands to internal/commands:
// the process stdout for human progress, and the engine-backed job runner. It is
// spelled once so a new lifecycle command cannot forget the runner and silently
// inherit a no-op install (lifecycle.runWorkspaceJob turns a nil runner into a
// loud error rather than a skipped install, but not forgetting is better).
//
// The first-use bootstrap does NOT use this: it picks its own writer, because
// under --output=json|jsonl an implicit install's chatter must go to stderr.
func lifecycleEnv(env *CommandEnv) lifecycle.LifecycleEnv {
	out := io.Writer(os.Stdout)
	if output.StructuredOutput(env.OutputFormat) {
		out = os.Stderr
	}
	noColor := env.Global.Color != nil && !*env.Global.Color
	display := lifecycle.LifecycleDisplay{
		Verbose:     env.Global.Verbose,
		Debug:       env.Global.Debug,
		Quiet:       env.Global.Quiet,
		NoColor:     noColor,
		Interactive: !output.StructuredOutput(env.OutputFormat) && output.ShouldUseLiveRenderer(out) && !noColor,
	}
	return lifecycle.LifecycleEnv{
		Out:        out,
		RunJob:     RunWorkspaceJob,
		CLIVersion: Version,
		Display:    display,
		OnAction: func(action lifecycle.LifecycleAction) {
			if !display.Quiet {
				_, _ = io.WriteString(out, "  ✓ "+action.Description+"\n")
			}
		},
		NoCache: env.Global.NoCacheExplicit,
	}
}

// RunWorkspaceJob is the LIFECYCLE ADAPTER over engine.Run (slice A5a).
// It backs `putnami install`'s workspace-install pass and `putnami upgrade`'s
// deps-upgrade pass — and, through them, `deps install`, `projects sync`,
// `workspace init` and the first-use bootstrap.
//
// Before A5a those ran on a private composition in internal/commands/deps.go: a
// second workspace load, a second (non-Detailed) extension discovery, a direct
// jobs.Plan, a hand-rolled dedup-by-extension filter, a fixed SchedulerConfig
// and a direct jobs.NewScheduler. That was the third of the five hand-assembled
// lifecycles ADR 0001 §3 collapses; all of it is gone.
//
// It lives in internal/cli, beside the terminal and extension-alias adapters,
// because internal/engine imports internal/commands for the production doctor
// gate — so `commands` cannot import `engine`. lifecycle.LifecycleEnv.RunJob is
// the seam it is bound to.
//
// Four seams are deliberately NOT the terminal path's:
//
//   - The telemetry observer stays nil (ADR 0001 §4). `putnami install` reported
//     no telemetry before this slice and must not start: routing a surface
//     through the engine must never synthesize a user session. The
//     Request literal below therefore never names the field, which is also what
//     keeps internal/engine's TestOnlyTheTerminalAdapterSuppliesAnObserver at
//     exactly one production injection site.
//   - Hooks stay nil. `putnami upgrade` runs the deps-upgrade command hooks
//     itself, at its own point in the sequence, and `putnami install` never ran
//     lifecycle hooks; firing them here would double-run one and invent the other.
//   - Request.Stdout is io.Discard. The engine's generic no-op notices ("No jobs
//     matched. Nothing to do.") would duplicate — and contradict — the
//     job-specific messages DepsInstall/upgradeDeps print for the same outcome,
//     which this adapter reports back to them instead. No other engine notice is
//     reachable here: --plan is never set, project selection is explicit so the
//     auto-selection note never fires, and the production preflight is inert
//     without an EnvProfile.
//   - Request.WorkspaceLifecycle is set, which is what keeps a lifecycle run from
//     inheriting the build lifecycle wholesale: one job per provider, no session
//     file, no successful-run marker, no opportunistic GC, and no gate that the
//     install itself exists to repair. See its doc on engine.Request.
func RunWorkspaceJob(ctx context.Context, req lifecycle.WorkspaceJobRequest) (lifecycle.WorkspaceJobResult, error) {
	out := req.Out
	if out == nil {
		out = os.Stdout
	}
	indexBefore := workspaceIndexIdentity(req.WorkspaceRoot)

	global := engine.GlobalFlags{
		// Lifecycle jobs are deliberately uncached: `workspace-install` reconciles
		// a mutable tree (node_modules, go.sum, extension-local state) that no
		// action-cache key describes. NoCache also keeps the remote cache, the
		// remote run markers and the store GC out of the run entirely.
		NoCache: true,
		// Extensions see `cache: false` only when the user typed --no-cache on
		// the lifecycle command; otherwise they keep their own `cache` default.
		NoCacheExplicit: req.NoCache,
		// "*" rather than All: it selects every project exactly like the pre-A5a
		// `ws.Projects`, and it keeps the engine's DirectTarget true so the
		// workspace's disable.tags cannot narrow an install. It also leaves
		// isBareProjectSelection false, so auto-selection — and the speculative
		// run-marker lookup inside it — never runs.
		Projects:   "*",
		FilterTag:  req.FilterTag,
		ExcludeTag: req.ExcludeTag,
		// Off by default: a lifecycle job that fails in one ecosystem must not
		// let the next one rewrite the workspace's dependency metadata against a
		// selector that ecosystem could not resolve.
		ContinueOnErr: req.ContinueOnError,
	}

	result, runErr := engine.New().Run(ctx, engine.Request{
		WorkspaceRoot: req.WorkspaceRoot,
		Config:        req.Config,
		Commands:      []string{req.Job},
		Global:        global,
		// Forwarded verbatim: a param value's Go type is part of every cache key
		// (store.hashParams), so `putnami upgrade`'s deps-upgrade params keep
		// producing byte-identical keys to the ones they produced before A5a.
		CommandParams:      req.Params,
		WorkspaceLifecycle: true,
		// A lifecycle run executes extension jobs like any other, so it gates
		// like any other. See terminalRequest for why this is injected.
		Preflight: doctor.DoctorPreflight,
		Stdout:    io.Discard,
	}, output.NewLifecycleRenderer(output.Config{
		Verbose: req.Display.Verbose,
		Debug:   req.Display.Debug,
		Quiet:   req.Display.Quiet,
		NoColor: req.Display.NoColor,
		Out:     out,
		Err:     out,
	}, req.Display.Interactive))
	// Index publication completes during graph preparation and can precede a
	// later installer failure. Keep that physically completed action in the
	// partial summary even when runErr reports the subsequent failure.
	reportWorkspaceIndexAction(req, indexBefore, workspaceIndexIdentity(req.WorkspaceRoot))
	if runErr != nil {
		return lifecycle.WorkspaceJobResult{Outcome: lifecycle.WorkspaceJobFailed}, runErr
	}

	// No scheduler ran. Under WorkspaceLifecycle an empty selection or an empty
	// plan ends the run as a success-shaped no-op, so this is where "no extension
	// provides the job" is told apart from "the job matched no project" — the two
	// outcomes the callers report differently.
	if result.Session == nil {
		if result.ExitCode != engine.ExitSuccess {
			return lifecycle.WorkspaceJobResult{Outcome: lifecycle.WorkspaceJobFailed}, nil
		}
		if available, provided := workspaceJobProviders(req.WorkspaceRoot, req.Config, req.Job); !provided {
			return lifecycle.WorkspaceJobResult{
				Outcome:       lifecycle.WorkspaceJobMissing,
				AvailableJobs: available,
			}, nil
		}
		return lifecycle.WorkspaceJobResult{Outcome: lifecycle.WorkspaceJobNoMatches}, nil
	}

	reportWorkspaceJobActions(req, result.Plan, result.Results)
	if result.ExitCode != engine.ExitSuccess {
		return lifecycle.WorkspaceJobResult{Outcome: lifecycle.WorkspaceJobFailed}, nil
	}
	return lifecycle.WorkspaceJobResult{Outcome: lifecycle.WorkspaceJobOK}, nil
}

type lifecycleIndexIdentity struct {
	info  os.FileInfo
	valid bool
}

func workspaceIndexIdentity(wsRoot string) lifecycleIndexIdentity {
	info, err := os.Stat(workspace.SnapshotPath(wsRoot))
	if err == nil {
		return lifecycleIndexIdentity{info: info, valid: true}
	}
	if os.IsNotExist(err) {
		return lifecycleIndexIdentity{valid: true}
	}
	return lifecycleIndexIdentity{}
}

func reportWorkspaceIndexAction(req lifecycle.WorkspaceJobRequest, before, after lifecycleIndexIdentity) {
	if req.OnAction == nil || !before.valid || !after.valid || after.info == nil {
		return
	}
	description := "Workspace index created"
	if before.info != nil {
		if os.SameFile(before.info, after.info) {
			return
		}
		description = "Workspace index refreshed"
	}
	req.OnAction(lifecycle.LifecycleAction{Kind: "workspace-index", Name: "workspace-index.json", Description: description})
}

// reportWorkspaceJobActions reports one action per provider only after every
// planned step for that provider succeeds. The engine result does not claim a
// package-manager diff, so the wording truthfully describes reconciliation.
func reportWorkspaceJobActions(req lifecycle.WorkspaceJobRequest, plan []*jobs.ScheduledJob, results map[string]*jobs.JobResult) {
	if req.OnAction == nil {
		return
	}
	providers := make(map[string]bool)
	for _, job := range plan {
		name := "workspace"
		if job.Extension != nil && job.Extension.Name != "" {
			name = job.Extension.Name
		} else if job.Project != nil && job.Project.Name != "" {
			name = job.Project.Name
		}
		if _, seen := providers[name]; !seen {
			providers[name] = true
		}
		result := results[job.Key()]
		if result == nil || result.Status != string(jobs.TaskStatusSuccess) {
			providers[name] = false
		}
	}
	names := make([]string, 0, len(providers))
	for name, ok := range providers {
		if ok {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		description := "Workspace setup completed (" + name + ")"
		switch req.Job {
		case "deps-upgrade":
			description = "Workspace upgrade completed (" + name + ")"
		case extensionproto.WorkspaceFetchCommand:
			description = "Workspace dependencies fetched (" + name + ")"
		}
		req.OnAction(lifecycle.LifecycleAction{Kind: req.Job, Name: name, Description: description})
	}
}

// workspaceJobProviders reports whether any discovered extension provides
// jobName, plus the providable job names for the caller's error message.
//
// It re-discovers because the engine owns the run's discovery and does not hand
// the extension set back. That second pass only ever runs when the run planned
// NOTHING — an error or no-op path — so a healthy install still discovers once.
func workspaceJobProviders(wsRoot string, cfg *wsproto.Config, jobName string) (available string, provided bool) {
	ws, err := workspace.Load(wsRoot)
	if err != nil {
		return "", false
	}
	projectPaths := make([]string, len(ws.Projects))
	for i, p := range ws.Projects {
		projectPaths[i] = p.Path
	}
	extensions, err := extension.DiscoverExtensions(wsRoot, cfg, projectPaths)
	if err != nil {
		return "", false
	}
	jobMap := extension.BuildJobMap(extensions)
	return lifecycle.FormatJobNames(jobMap), len(jobMap[jobName]) > 0
}
