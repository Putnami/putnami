package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"

	"go.putnami.dev/cli/model/extension"
	"go.putnami.dev/cli/model/workspace"
	ciproto "go.putnami.dev/protocol/ci"
	protocolcli "go.putnami.dev/protocol/cli"
	extensionproto "go.putnami.dev/protocol/extension"
	protocoljob "go.putnami.dev/protocol/job"
	runner "go.putnami.dev/protocol/runner"
	"go.putnami.dev/tooling/cli/internal/changeplan"
	internalextension "go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/git"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/lockfile"
	"go.putnami.dev/tooling/cli/internal/runnerprovider"
	"go.putnami.dev/tooling/cli/internal/workspace_state"
)

// PortableExecution binds an engine run to a bound execution request it
// received from a runner provider. The EXECUTING engine records the remote
// placement and never resolves placement itself. For a frozen version 1
// request it honors the frozen selection and refuses to schedule a plan that
// differs from the expected one. A commit-addressed version 2 request is
// engine-planned instead: the run resolves the requested selection on its
// checkout and stamps versions from its Git history, like a local run. Only
// the bound-request adapter sets it.
type PortableExecution struct {
	// Request is the snapshot-addressed version 1 request the run executes
	// frozen. It is unused when Commit is set.
	Request runner.ExecutionRequest
	// Commit is the commit-addressed version 2 request the run plans itself,
	// or nil for a version 1 request.
	Commit *runner.CommitRequest
}

// frozen reports whether the run executes a frozen version 1 request: the
// stages that plan, stamp versions and record the tree then read the request
// instead of the root the run sits in, which is a snapshot with no Git
// history. It is false without a bound request and for a version 2 one.
func (p *PortableExecution) frozen() bool {
	return p != nil && p.Commit == nil
}

// invocation is the invocation block of the bound request, nil without one.
func (p *PortableExecution) invocation() *runner.InvocationBlock {
	switch {
	case p == nil:
		return nil
	case p.Commit != nil:
		return &p.Commit.Invocation
	}
	return &p.Request.Invocation
}

// bindSelection projects a version 2 request's requested selection onto the
// run's selection flags, which the ordinary selection stage then resolves on
// the checkout: impacted against source.base, the projects selectors, or
// every project. The request is the run's only selection authority, so every
// other selection flag is cleared. A version 1 request, or none, leaves the
// flags as they are: a frozen selection binds at the selection stage
// (selectFrozenProjects).
func (p *PortableExecution) bindSelection(global *GlobalFlags) {
	if p == nil || p.Commit == nil {
		return
	}
	global.Projects, global.All, global.Impacted, global.Baseline, global.AutoSelected = "", false, false, "", false
	global.FilterTag, global.ExcludeTag, global.Exclude, global.NoCacheProjects = "", "", "", ""
	switch p.Commit.Selection.Mode {
	case runner.SelectionModeImpacted:
		global.Impacted, global.Baseline = true, p.Commit.Source.Base
	case runner.SelectionModeProjects:
		global.Projects = strings.Join(p.Commit.Selection.Projects, ",")
	default:
		global.All = true
	}
}

// selectionEvidence is what the selection stage learned beyond the resolved
// selection itself: the mode the caller asked for, the changed paths an
// impacted run measured, and the notices it printed. It is engine-internal
// plumbing between selection and the portable projection.
type selectionEvidence struct {
	requestedMode string
	changedPaths  []string
	diagnostics   []string
	// openingEvents are the session records execute hands the scheduler to
	// record after its opening event: the --impacted selection's complete
	// trace (workspace.ImpactedSelection.TraceRecord), nil for any other
	// selection.
	openingEvents []jobs.SessionRecord
	// closingNotes are printed after the run: the --impacted change's size by
	// category and its mixed-intent warning
	// (workspace.ImpactedSelection.ChangeShapeEvidence), "" for any other
	// selection.
	closingNotes string
	// taskScopes maps a selected project ID to the sorted IDs of the impacted
	// in-workspace extension projects whose tasks it runs
	// (workspace.ImpactedSelection.TaskScopes). buildPlan keeps those tasks
	// and their predecessors on such a project and drops the rest; a project
	// absent from the map runs every task. Nil for any other selection, and
	// for a task-scoped selection the executing side of a portable request
	// receives frozen.
	taskScopes map[string][]string
}

// validateCommitPublication is the executing side's publication check of a
// version 2 request on the plan this engine made, since no expected plan came
// with it. A plan with a publication task, by command name or by declared
// registry or cloud effects, is held to both version 1 checks over the plan
// without the nodes a publication-v1 provider adds: the protocol's, which
// classifies by command name and which a version 1 request passes when it is
// parsed, and the executing engine's (jobs.ValidatePortablePublication). It
// needs invocation.publication, and every publication task waits for every
// task of the barrier commands. A plan with no publication task passes, the
// block or not: the caller authorized publication before any plan existed,
// and a commit whose selection publishes nothing is a run that publishes
// nothing.
func validateCommitPublication(request runner.CommitRequest, comparable, planned []*jobs.ScheduledJob) error {
	if !slices.ContainsFunc(planned, func(job *jobs.ScheduledJob) bool {
		return jobs.HasExternalEffects(job) || slices.Contains(runner.PublicationCommands, job.CommandName())
	}) {
		return nil
	}
	plan, err := expectedPlan(comparable)
	if err != nil {
		return err
	}
	if err := runner.ValidatePublication(request.Invocation, plan, runner.IsPublicationTask); err != nil {
		return err
	}
	return jobs.ValidatePortablePublication(runner.ExecutionRequest{Invocation: request.Invocation, Plan: plan}, planned)
}

// remoteDeadline is the finite deadline a submission carries. The provider
// enforces it; the client's own observation is bounded by its context only.
const remoteDeadline = 4 * time.Hour

// portableSeam is the ONE seam between the final plan and execution where a
// run either leaves this engine for a provider or, on the executing side,
// proves its re-planned graph equals the expected one. It runs after the
// production preflight so a blocked run is blocked before any transfer, and
// it returns (result, true) exactly when the run must not execute here.
//
// The executing side compares the expected plan with the graph without the
// nodes a publication-v1 provider adds (jobs.ReleaseSetRun.WithoutPublicationNodes),
// so the submitter plans the same graph whether or not the provider echoes
// the capability. The publication and admission checks read the whole graph.
// A version 2 request carries no expected plan and captured no input, and no
// CLI submitter ran rejectUnsupportedRemoteShape on it: the protocol refuses
// its unportable commands (runner.UnportableCommands), and its executing side
// refuses a task outside the checkout, then checks publication
// (validateCommitPublication). Its publication rides invocation.publication,
// as on the version 1 executing side, so declared effects are not refused.
//
// Between the unsupported-shape refusal and the request projection sits the
// input admission (ADR 0037): on the plan's declared inputs it binds every
// required git-ignored file into the request, reports the ignored files a
// task with no declaration keys on without binding them, or refuses an
// env-keyed task before anything is captured. The executing side re-derives
// the same decision on the materialized tree and refuses a divergence exactly
// as it refuses a divergent plan.
func (e *Engine) portableSeam(ctx context.Context, req *Request, ws *workspace.Workspace, discovered *internalextension.DiscoveryResult, planned []*jobs.ScheduledJob, releaseSetRun *jobs.ReleaseSetRun) (SessionResult, bool) {
	if req.Portable != nil && !req.Portable.frozen() {
		for _, job := range planned {
			if err := outsideWorkspace(req.WorkspaceRoot, job); err != nil {
				iox.Fprintf(os.Stderr, "putnami: portable execution refused: %v\n", err)
				return SessionResult{ExitCode: ExitError, Plan: planned}, true
			}
		}
		if err := validateCommitPublication(*req.Portable.Commit, releaseSetRun.WithoutPublicationNodes(planned), planned); err != nil {
			iox.Fprintf(os.Stderr, "putnami: portable execution refused: %v\n", err)
			return SessionResult{ExitCode: ExitError, Plan: planned}, true
		}
		return SessionResult{}, false
	}
	if req.Portable != nil {
		if err := validateExpectedPlan(req.Portable.Request.Plan, releaseSetRun.WithoutPublicationNodes(planned)); err != nil {
			iox.Fprintf(os.Stderr, "putnami: portable execution refused: %v\n", err)
			return SessionResult{ExitCode: ExitError, Plan: planned}, true
		}
		if err := jobs.ValidatePortablePublication(req.Portable.Request, planned); err != nil {
			iox.Fprintf(os.Stderr, "putnami: portable execution refused: %v\n", err)
			return SessionResult{ExitCode: ExitError, Plan: planned}, true
		}
		if err := runnerprovider.VerifyAdmission(req.WorkspaceRoot, req.Portable.Request.Source.Bound, jobs.PortableInputs(ws, planned, req.CommandParams)); err != nil {
			iox.Fprintf(os.Stderr, "putnami: portable execution refused: %v\n", err)
			return SessionResult{ExitCode: ExitError, Plan: planned}, true
		}
		return SessionResult{}, false
	}
	// An empty plan — the legitimate empty impacted selection above all — stays
	// the explicit local no-op it always was; nothing is submitted for it.
	if req.runnerProvider == nil || req.previewsOnly() || len(planned) == 0 {
		return SessionResult{}, false
	}
	if err := rejectUnsupportedRemoteShape(req, planned); err != nil {
		iox.Fprintf(os.Stderr, "putnami: --where remote: %v\n", err)
		return SessionResult{ExitCode: ExitUsage, Plan: planned}, true
	}
	admission, err := runnerprovider.AdmitInputs(ctx, req.WorkspaceRoot, jobs.PortableInputs(ws, planned, req.CommandParams))
	if err != nil {
		iox.Fprintf(os.Stderr, "putnami: --where remote: %v\n", err)
		if errors.Is(err, runnerprovider.ErrInadmissible) {
			return SessionResult{ExitCode: ExitUsage, Plan: planned}, true
		}
		return SessionResult{ExitCode: ExitError, Plan: planned}, true
	}
	// What the admission did NOT bind is said out loud before submission.
	admission.ReportUnbound(os.Stderr)
	request, err := portableRequest(req, ws, discovered, releaseSetRun.WithoutPublicationNodes(planned), admission.Bound)
	if err != nil {
		iox.Fprintf(os.Stderr, "putnami: --where remote: %v\n", err)
		return SessionResult{ExitCode: ExitError, Plan: planned}, true
	}
	launch, err := runnerprovider.LaunchSpecFor(ctx, req.WorkspaceRoot, discovered.Extensions, req.runnerProvider)
	if err != nil {
		iox.Fprintf(os.Stderr, "putnami: --where remote: %v\n", err)
		return SessionResult{ExitCode: ExitError, Plan: planned}, true
	}
	outcome, err := runnerprovider.Execute(ctx, runnerprovider.Offload{
		WorkspaceRoot: req.WorkspaceRoot, Workspace: jobs.RunMarkerWorkspaceID(ws),
		ProviderName: req.runnerProvider.ExtensionName, Launch: launch, Request: request,
		Retention: workspace_state.RetentionFromWorkspace(req.Config),
		Stdout:    req.stdout(), Stderr: os.Stderr,
	})
	if err != nil {
		iox.Fprintf(os.Stderr, "putnami: remote execution: %v\n", err)
		// An interrupted observation ends as an interrupted run does locally:
		// the attempt was canceled within bounded time and the process says
		// "someone stopped this" rather than filing it as a build failure.
		if errors.Is(err, protocolcli.ErrSignal) {
			return SessionResult{ExitCode: ExitSignalReceived, Plan: planned}, true
		}
		if errors.Is(err, runnerprovider.ErrUnsupportedRequest) {
			return SessionResult{ExitCode: ExitUsage, Plan: planned}, true
		}
		return SessionResult{ExitCode: ExitError, Plan: planned}, true
	}
	return SessionResult{ExitCode: outcome.ExitCode, Plan: planned}, true
}

// rejectUnsupportedRemoteShape refuses, before any transfer, the invocations
// this release does not carry: long-running or interactive modes, source
// rewriting, and jobs with side effects outside the workspace. Rejection is a
// precise diagnostic, never a silent modification of the request. Its command
// refusals cover runner.UnportableCommands, which a version 2 request meets in
// the protocol instead.
func rejectUnsupportedRemoteShape(req *Request, planned []*jobs.ScheduledJob) error {
	if req.Global.Watch {
		return fmt.Errorf("--watch is not portable; remote execution covers finite commands only")
	}
	if req.ExecutesUnderDryRun {
		return fmt.Errorf("an executing dry-run is not portable")
	}
	for _, command := range req.Commands {
		switch command {
		case "serve", "run", "compose":
			return fmt.Errorf("%q streams a live workload and is not portable", command)
		case "qualify":
			return fmt.Errorf("%q reaches a live workload from this machine and is not portable", command)
		case "format":
			return fmt.Errorf("%q rewrites source and is not portable; remote edits would never return to this worktree", command)
		}
	}
	for _, job := range planned {
		if job == nil || job.JobDef == nil {
			continue
		}
		if job.JobDef.Traits.SideEffects != extensionproto.SideEffectsNone {
			return fmt.Errorf("task %s declares %s side effects; publication and deployment never ride a verification request", job.Key(), job.JobDef.Traits.SideEffects)
		}
		if jobs.HasExternalEffects(job) {
			return fmt.Errorf("task %s declares registry or cloud effects; publication and deployment never ride a verification request", job.Key())
		}
		if err := outsideWorkspace(req.WorkspaceRoot, job); err != nil {
			return err
		}
	}
	return nil
}

// outsideWorkspace refuses a task whose absolute working directory lies
// outside the workspace root: a portable run reaches nothing beyond its tree.
func outsideWorkspace(root string, job *jobs.ScheduledJob) error {
	if job == nil || job.JobDef == nil {
		return nil
	}
	if cwd := job.JobDef.Cwd; filepath.IsAbs(cwd) && !strings.HasPrefix(filepath.Clean(cwd)+string(filepath.Separator), filepath.Clean(root)+string(filepath.Separator)) {
		return fmt.Errorf("task %s runs outside the workspace (%s) and is not portable", job.Key(), cwd)
	}
	return nil
}

// portableRequest projects the effective run onto the typed request: the
// semantic invocation with typed parameters, the frozen selection, the
// expected plan, the pinned environment and the admitted bound inputs. The
// source identity is bound by the provider client from the snapshot it
// captures, which carries exactly the bound set named here.
func portableRequest(req *Request, ws *workspace.Workspace, discovered *internalextension.DiscoveryResult, planned []*jobs.ScheduledJob, bound []string) (runner.ExecutionRequest, error) {
	params := make(map[string]runner.ParamValue, len(req.CommandParams))
	for name, value := range req.CommandParams {
		param, err := runner.NewParam(value)
		if err != nil {
			return runner.ExecutionRequest{}, fmt.Errorf("parameter %q: %w", name, err)
		}
		params[name] = param
	}
	plan, err := expectedPlan(planned)
	if err != nil {
		return runner.ExecutionRequest{}, err
	}
	if req.selection == nil {
		return runner.ExecutionRequest{}, fmt.Errorf("selection was not resolved")
	}
	// The frozen baseline is the immutable commit the ref named when selection
	// ran, never the ref: a moving ref cannot change admitted work later.
	baseline, diagnostics := "", append([]string{}, req.selectionEvidence.diagnostics...)
	if req.selection.Mode == protocoljob.SelectionModeImpacted && req.selection.Baseline != "" {
		sha, err := git.ResolveCommit(req.WorkspaceRoot, req.selection.Baseline)
		if err != nil {
			return runner.ExecutionRequest{}, fmt.Errorf("freeze baseline %q: %w", req.selection.Baseline, err)
		}
		baseline = sha
		diagnostics = append(diagnostics, fmt.Sprintf("baseline %s resolved to commit %s", req.selection.Baseline, sha))
	}
	key := make([]byte, 16)
	if _, err := rand.Read(key); err != nil {
		return runner.ExecutionRequest{}, err
	}
	global := req.Global
	budgets := map[string]int{}
	for name, units := range global.ResourceBudgets {
		budgets[name] = units
	}
	return runner.ExecutionRequest{
		Version:  runner.ExecutionRequestVersion,
		Protocol: runner.ProtocolBlock{Version: runner.ProviderProtocolVersion, Capabilities: []string{}},
		Source:   runner.SourceBlock{Versions: lineVersions(req.runVersions(ws)), Bound: bound},
		Invocation: runner.InvocationBlock{
			Commands: append([]string{}, req.Commands...), Params: params, Cwd: workspaceCwd(req.WorkspaceRoot),
			Providers: runner.CanonicalProviders(global.Providers),
			Flags: runner.ExecutionFlags{
				NoCache: global.NoCache, NoCacheExplicit: global.NoCacheExplicit, RetryFailed: global.RetryFailed,
				ContinueOnError: global.ContinueOnErr, ImpactedStrict: global.ImpactedStrict,
				Verbose: global.Verbose, Debug: global.Debug, Quiet: global.Quiet,
				Retry: global.Retry, MaxParallel: global.MaxParallel, MaxParallelMode: global.MaxParallelMode,
				CPUBudgetPolicy: global.CPUBudgetPolicy, CacheTrust: global.CacheTrust, Output: global.Output,
				Profile: global.EnvProfile, ResourceBudgets: budgets,
			},
		},
		Selection: runner.SelectionBlock{
			RequestedMode: req.selectionEvidence.requestedMode, Mode: req.selection.Mode, Scoped: req.selection.Scoped,
			Projects: runner.SortStrings(req.selection.ProjectIDs), Baseline: baseline, BaselineSource: req.selection.BaselineSource,
			ChangedPaths: runner.SortStrings(req.selectionEvidence.changedPaths), Diagnostics: diagnostics,
			NoCacheProjects: runner.SortStrings(slices.Collect(maps.Keys(req.noCacheProjects))),
			TaskScopes:      req.selectionEvidence.taskScopes,
		},
		Plan:        plan,
		Environment: pinnedEnvironment(req.WorkspaceRoot, discovered.Extensions),
		Control: runner.ControlBlock{
			Caller: runner.CallerCLI, IdempotencyKey: hex.EncodeToString(key),
			Deadline: time.Now().UTC().Add(remoteDeadline).Truncate(time.Second).Format(time.RFC3339),
		},
	}, nil
}

// expectedPlan projects the final DAG through the ChangePlan's projection so
// the two documents can never describe one plan two ways, and adds the one
// plan property the ChangePlan omits: whether a task is cacheable at all.
// Cache PRESENCE is deliberately absent; it is an observation, not identity.
func expectedPlan(planned []*jobs.ScheduledJob) (runner.PlanBlock, error) {
	tasks, err := changeplan.ProjectTasks(planned)
	if err != nil {
		return runner.PlanBlock{}, err
	}
	cacheable := make(map[string]bool, len(planned))
	for _, job := range planned {
		if job != nil && job.JobDef != nil {
			cacheable[job.Key()] = job.JobDef.Cache
		}
	}
	plan := runner.PlanBlock{Tasks: make([]runner.PlannedTask, 0, len(tasks))}
	for _, task := range tasks {
		plan.Tasks = append(plan.Tasks, runner.PlannedTask{
			Identity: task.Identity, DependsOn: append([]string{}, task.DependsOn...), SerializeAfter: append([]string{}, task.SerializeAfter...),
			ContractDigest: task.ContractDigest, DeadlineMs: task.DeadlineMS, Cacheable: cacheable[task.Identity.Key],
			Resources: runner.TaskResources{Heavy: task.ResourceClass.Heavy, CPUWeight: task.ResourceClass.CPUWeight,
				Reads: planResources(task.ResourceClass.Reads), Writes: planResources(task.ResourceClass.Writes)},
		})
	}
	return plan, nil
}

// validateExpectedPlan is the executing side's admission check: the graph
// re-planned from the materialized snapshot must equal the expected plan in
// every identity, edge, contract digest and resource declaration. A
// difference means the snapshot, the engine or an extension diverged from
// what was submitted, and nothing is scheduled.
func validateExpectedPlan(expected runner.PlanBlock, planned []*jobs.ScheduledJob) error {
	actual, err := expectedPlan(planned)
	if err != nil {
		return err
	}
	expectedByKey := make(map[string]runner.PlannedTask, len(expected.Tasks))
	for _, task := range expected.Tasks {
		expectedByKey[task.Identity.Key] = task
	}
	var differences []string
	for _, task := range actual.Tasks {
		want, ok := expectedByKey[task.Identity.Key]
		if !ok {
			differences = append(differences, "unexpected task "+task.Identity.Key)
			continue
		}
		delete(expectedByKey, task.Identity.Key)
		if diff := describePlanDifference(want, task); diff != "" {
			differences = append(differences, task.Identity.Key+": "+diff)
		}
	}
	for key := range expectedByKey {
		differences = append(differences, "missing task "+key)
	}
	if len(differences) == 0 {
		return nil
	}
	sort.Strings(differences)
	return fmt.Errorf("the re-planned graph differs from the expected plan (%d difference(s)):\n  %s", len(differences), strings.Join(differences, "\n  "))
}

func describePlanDifference(want, got runner.PlannedTask) string {
	switch {
	case want.Identity != got.Identity:
		return fmt.Sprintf("identity %+v, expected %+v", got.Identity, want.Identity)
	case strings.Join(want.DependsOn, ",") != strings.Join(got.DependsOn, ","):
		return fmt.Sprintf("dependsOn %v, expected %v", got.DependsOn, want.DependsOn)
	case strings.Join(want.SerializeAfter, ",") != strings.Join(got.SerializeAfter, ","):
		return fmt.Sprintf("serializeAfter %v, expected %v", got.SerializeAfter, want.SerializeAfter)
	case want.ContractDigest != got.ContractDigest:
		return "task contract digest differs"
	case want.DeadlineMs != got.DeadlineMs:
		return fmt.Sprintf("deadline %d ms, expected %d ms", got.DeadlineMs, want.DeadlineMs)
	case want.Cacheable != got.Cacheable:
		return fmt.Sprintf("cacheable %t, expected %t", got.Cacheable, want.Cacheable)
	case want.Resources.Heavy != got.Resources.Heavy || want.Resources.CPUWeight != got.Resources.CPUWeight ||
		fmt.Sprint(want.Resources.Reads) != fmt.Sprint(got.Resources.Reads) || fmt.Sprint(want.Resources.Writes) != fmt.Sprint(got.Resources.Writes):
		return "declared resources differ"
	}
	return ""
}

// pinnedEnvironment reads the engine pin and toolchains from the lock, the
// extension set from this run's discovery, and the platform from the process.
func pinnedEnvironment(wsRoot string, extensions []*extension.ExtensionDescription) runner.EnvironmentBlock {
	environment := runner.EnvironmentBlock{
		CLI:        runner.PinnedCLI{Source: runner.CLISourceUnpinned},
		Extensions: []runner.PinnedComponent{},
		Toolchains: []runner.PinnedComponent{},
		Platform:   runner.Platform{OS: runtime.GOOS, Arch: runtime.GOARCH},
	}
	if lock, err := lockfile.ReadLockFile(wsRoot); err == nil && lock != nil {
		if lock.CLI != nil {
			if lock.CLI.Source == lockfile.SourceWorkspace {
				environment.CLI = runner.PinnedCLI{Source: runner.CLISourceWorkspace}
			} else if lock.CLI.Version != "" {
				environment.CLI = runner.PinnedCLI{Source: runner.CLISourcePublished, Version: lock.CLI.Version}
			}
		}
		for name, entry := range lock.Toolchains {
			environment.Toolchains = append(environment.Toolchains, runner.PinnedComponent{Name: name, Version: entry.Version})
		}
	}
	seen := make(map[string]bool, len(extensions))
	for _, ext := range extensions {
		if ext == nil || seen[ext.Name] {
			continue
		}
		seen[ext.Name] = true
		environment.Extensions = append(environment.Extensions, runner.PinnedComponent{Name: ext.Name, Version: ext.Version})
	}
	for _, components := range [][]runner.PinnedComponent{environment.Extensions, environment.Toolchains} {
		sort.Slice(components, func(i, j int) bool { return components[i].Name < components[j].Name })
	}
	return environment
}

func lineVersions(versions jobs.RunVersions) []runner.LineVersion {
	out := make([]runner.LineVersion, 0, len(versions))
	for line, version := range versions {
		if version == nil {
			continue
		}
		out = append(out, runner.LineVersion{Line: line, Base: version.Base, Full: version.Full, SHA: version.SHA, Branch: version.Branch,
			Suffix: version.Suffix, Tag: version.Tag, Tagged: version.Tagged, Dirty: version.IsDirty})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Line < out[j].Line })
	return out
}

// portableRunVersions is the executing side's version table: exactly the
// lines the submitter stamped, so the snapshot needs no Git history.
func portableRunVersions(versions []runner.LineVersion) jobs.RunVersions {
	out := make(jobs.RunVersions, len(versions))
	for _, version := range versions {
		out[version.Line] = &jobs.JobContextVersion{Base: version.Base, Full: version.Full, SHA: version.SHA, Branch: version.Branch,
			Tag: version.Tag, Suffix: version.Suffix, Tagged: version.Tagged, IsDirty: version.Dirty, Line: version.Line}
	}
	return out
}

func planResources(resources []ciproto.ChangePlanResource) []runner.TaskResource {
	out := make([]runner.TaskResource, 0, len(resources))
	for _, resource := range resources {
		out = append(out, runner.TaskResource{ID: resource.ID, Scope: resource.Scope})
	}
	return out
}

func workspaceCwd(wsRoot string) string {
	cwd, err := os.Getwd()
	if err != nil {
		return "."
	}
	rel, err := filepath.Rel(wsRoot, cwd)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return "."
	}
	return filepath.ToSlash(rel)
}

// selectFrozenProjects is the executing side's selection stage: it plans
// exactly the ids the submitter froze, keeps the recorded mode and baseline
// the submitter resolved, and refuses a snapshot that cannot resolve them.
// Impact analysis never reruns here, and no marker can widen the set.
func selectFrozenProjects(req *Request, ws *workspace.Workspace) ([]*workspace.Project, int) {
	frozen := req.Portable.Request.Selection
	selected := workspace.FilterProjects(ws, workspace.FilterOptions{
		Projects: strings.Join(frozen.Projects, ","), DirectTarget: true, ScopeIndex: ws.ScopeIndex,
	})
	resolved := make([]string, 0, len(selected))
	for _, project := range selected {
		resolved = append(resolved, project.ID)
	}
	if strings.Join(runner.SortStrings(resolved), ",") != strings.Join(frozen.Projects, ",") {
		iox.Fprintf(os.Stderr, "putnami: portable execution refused: the snapshot resolves projects %v, the frozen selection names %v\n", resolved, frozen.Projects)
		return nil, ExitError
	}
	req.Global.Projects = strings.Join(frozen.Projects, ",")
	req.Global.Impacted, req.Global.All = false, false
	req.noCacheProjects = nil
	if len(frozen.NoCacheProjects) > 0 && !req.Global.NoCache {
		req.noCacheProjects = make(map[string]bool, len(frozen.NoCacheProjects))
		for _, id := range frozen.NoCacheProjects {
			req.noCacheProjects[id] = true
		}
	}
	req.selection = &protocoljob.Selection{Mode: frozen.Mode, Scoped: frozen.Scoped, Baseline: frozen.Baseline,
		BaselineSource: frozen.BaselineSource, ProjectIDs: append([]string{}, frozen.Projects...)}
	// The task scopes are frozen with the projects: the submitter's plan kept
	// a task-scoped project's jobs of the changed extensions only, and this
	// side must plan the same graph to prove it equal.
	req.selectionEvidence.taskScopes = nil
	if len(frozen.TaskScopes) > 0 {
		req.selectionEvidence.taskScopes = make(map[string][]string, len(frozen.TaskScopes))
		for id, extensions := range frozen.TaskScopes {
			req.selectionEvidence.taskScopes[id] = append([]string{}, extensions...)
		}
	}
	return selected, ExitSuccess
}
