package engine

import (
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"strings"

	"go.putnami.dev/cli/model/extension"
	"go.putnami.dev/cli/model/workspace"
	protocolcli "go.putnami.dev/protocol/cli"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commandmeta"
	internalextension "go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/output"
)

// buildPlan creates the execution plan from commands, projects, and extensions.
//
// Unexported later, together with selectProjects and printPlan: they were left
// exported only while adapters were still being migrated onto Engine.Run.
// Planning is an engine STAGE, not a service other packages call — an adapter
// that could plan on its own could fork the lifecycle again, which is exactly
// the duplication class ADR 0001 §3 closes.
//
// extensions is the PLANNING scope: the providers whose jobs this plan may
// contain. discovered is what discovery loaded and skipped. The
// missing-extension guards read discovered, not the planning scope, because
// they ask whether a configured extension LOADED. The planning scope is
// narrower on purpose in two places: the alias adapter plans one extension,
// and a release-set publish that changes no member keeps only the providers
// of the verification commands, without their publish jobs. A loaded
// extension that serves none of the selected commands, such as an
// agent-content extension that declares no command, leaves that scope while
// it is installed. Read against the planning scope, the guards reported it
// "not installed" and failed every run that republished an unchanged tree.
// The skip records let the guards name the ROOT CAUSE instead of the blanket
// `putnami install` hint — see reportMissingExtensions. A nil discovered,
// which only a test that plans without discovery passes, makes the planning
// scope stand for what loaded.
func buildPlan(
	req *Request,
	ws *workspace.Workspace,
	selectedProjects []*workspace.Project,
	extensions []*extension.ExtensionDescription,
	discovered *internalextension.DiscoveryResult,
) ([]*jobs.ScheduledJob, int) {
	loaded, skipped := extensions, []extension.SkippedExtension(nil)
	if discovered != nil {
		loaded, skipped = discovered.Extensions, discovered.Skipped
	}
	var disabledJobs, disabledExts []string
	// A lifecycle run plans against every provider (Request.WorkspaceLifecycle
	// note 2): the workspace's disable lists describe its BUILDS, and applying
	// them here would let a workspace that disables a job silently skip restoring
	// its dependencies.
	if req.Config != nil && req.Config.Disable != nil && !req.WorkspaceLifecycle {
		disabledJobs = req.Config.Disable.Jobs
		disabledExts = req.Config.Disable.Extensions
	}

	contractErrors := extension.ValidateContracts(extensions)
	if len(contractErrors) > 0 {
		for _, ce := range contractErrors {
			iox.Fprintf(os.Stderr, "putnami: contract error: %s\n", ce.Error())
		}
		// Reported but never fatal for a lifecycle run: `putnami install` /
		// `putnami upgrade` are how an incompatible extension gets replaced, so
		// blocking them on the mismatch removes the only repair path.
		if !req.Global.ContinueOnErr && !req.WorkspaceLifecycle {
			return nil, ExitUsage
		}
	}

	if req.Global.Debug {
		iox.Fprintf(os.Stderr, "[debug] selected projects: %d, commands: %v\n", len(selectedProjects), req.Commands)
		for _, p := range selectedProjects {
			iox.Fprintf(os.Stderr, "[debug]   project: %s (path: %s, tags: %v)\n", p.Name, p.Path, p.Tags)
		}
		jobMap := extension.BuildJobMap(extensions)
		for _, cmd := range req.Commands {
			defs := jobMap[cmd]
			iox.Fprintf(os.Stderr, "[debug] jobMap[%s]: %d definitions\n", cmd, len(defs))
			for _, d := range defs {
				iox.Fprintf(os.Stderr, "[debug]   from ext: %s, activation: %v, channel: %s\n", d.ExtensionName, d.ActivationFiles, d.Channel)
			}
		}
	}

	planned, err := jobs.Plan(ws, req.Commands, selectedProjects, extensions, req.CommandParams, disabledJobs, disabledExts)
	if err != nil {
		iox.Fprintf(os.Stderr, "putnami: plan: %v\n", err)
		return nil, ExitError
	}
	// An --impacted selection can hold a project for the tasks of a changed
	// extension only; the planner plans the project whole, and the narrowing
	// happens here, on the plan, where the predecessor closure is visible.
	planned = narrowToTaskScopes(ws, planned, req.selectionEvidence.taskScopes)
	if len(planned) == 0 {
		// A lifecycle run reports its own, job-specific no-op ("No jobs matched for
		// deps install.") and tells "no extension provides this job" from "the job
		// matched no project" itself, so it returns before both the generic notice
		// and the missing-extension escalation below — which would advise `putnami
		// install` in the middle of `putnami install`.
		if req.WorkspaceLifecycle {
			return nil, ExitSuccess
		}
		// A broken extension install (e.g. integrity-mismatched remote
		// extensions) leaves the workspace with no jobs to run. Without this
		// guard that surfaces as a success-shaped "No jobs matched" no-op —
		// even under --impacted, which returns success — so CI reads green on a
		// workspace that never actually ran lint/test/build.
		if missing := missingRegistryExtensions(req.Config, loaded); len(missing) > 0 {
			reportMissingExtensions(missing, skipped, "")
			reportUnservedSDDCommands(req.Commands, extensions)
			return nil, ExitError
		}
		reportUnservedSDDCommands(req.Commands, extensions)
		// A release set that changes no member confirms the head from the
		// finalizer. A publish-only session, or a gate that selected nothing,
		// then leaves no job to plan: the empty plan is the whole session, not
		// a selection that matched nothing, so it goes on to execute.
		// Like every empty plan, it stays local under --where remote.
		if req.confirmsReleaseSetHead {
			return nil, ExitSuccess
		}
		if !machineOutputRequested(req) {
			iox.Fprintln(req.stdout(), "  No jobs matched. Nothing to do.")
		}
		if req.Global.Impacted {
			return nil, ExitSuccess
		}
		// Same contract as the empty-selection guard in selection.go: the stdout
		// notice above is the human presentation and machine output suppresses
		// it, so this failing exit used to carry no explanation on any stream.
		// The counts are what separate the two ways to plan nothing —
		// no extension served the command, or none of the selected projects
		// activated a job — which a caller cannot tell from exit 2 alone.
		iox.Fprintf(os.Stderr,
			"putnami: no job matched command(s) %s over %d selected project(s) (%s) from %d loaded extension(s)\n",
			strings.Join(req.Commands, ","), len(selectedProjects),
			summarizeProjectIDs(selectedProjects), len(extensions))
		return nil, ExitUsage
	}

	// Defense-in-depth beyond the zero-jobs guard above: even when some jobs
	// planned, a *selected* command can contribute zero jobs because the
	// configured registry extension that serves it failed to load. That step
	// silently vanishes while the run still reports success — the same
	// green-on-broken-CI failure class, now at per-command granularity. A
	// missing (unloaded) extension exposes no manifest, so we can't
	// map it to the commands it would have served; instead we detect the symptom
	// — a selected command that produced no jobs AND that no loaded extension
	// even declares — and only fail while a required extension is in fact
	// missing. A command a loaded extension declares but that merely matched no
	// selected project is a legitimate no-op and does not trip this.
	if missing := missingRegistryExtensions(req.Config, loaded); len(missing) > 0 {
		if starved := selectedCommandsWithoutJobs(req.Commands, planned, extensions); len(starved) > 0 {
			reportMissingExtensions(missing, skipped,
				"selected command(s) produced no jobs because their extension failed to load: "+strings.Join(starved, ", "))
			reportUnservedSDDCommands(req.Commands, extensions)
			return nil, ExitError
		}
	}

	// The plan is final; the last question is whether the run can ever serve it.
	// A task claiming more units of a named resource than --resource declared
	// exists would sit in the pending queue forever, so it is refused HERE, with
	// the resource and both amounts named, rather than at admission time.
	if err := jobs.ValidateResourceBudgets(planned, req.Global.ResourceBudgets); err != nil {
		iox.Fprintf(os.Stderr, "putnami: %v\n", err)
		return nil, ExitUsage
	}

	// Runtime synchronization left a task toolchain the lock does not satisfy
	// unresolved. A planned job that requires one fails the run here, before
	// any job runs or is keyed, and a run that planned none goes on.
	if err := jobs.RequirePlannedRuntimeToolchains(ws, planned); err != nil {
		iox.Fprintf(os.Stderr, "putnami: synchronize extension runtimes: %v\n", err)
		return nil, ExitError
	}

	return planned, ExitSuccess
}

// dedupePlanByExtension keeps the FIRST scheduled job of each extension and
// drops the rest, which is what makes a workspace-level lifecycle job run once
// per provider instead of once per project (Request.WorkspaceLifecycle note 1).
// jobs.Plan is deterministic, so "first" is a stable choice.
//
// It allocates a new slice rather than filtering in place: the pre-A5a
// composition reused the planned slice's backing array (planned[:0]), which is
// safe only while nothing else holds the plan — and the engine hands the plan
// back on SessionResult.Plan.
func dedupePlanByExtension(planned []*jobs.ScheduledJob) []*jobs.ScheduledJob {
	seen := make(map[string]bool, len(planned))
	deduped := make([]*jobs.ScheduledJob, 0, len(planned))
	for _, j := range planned {
		if j == nil {
			continue
		}
		// A job with no extension cannot be attributed to a provider, so it is
		// KEPT rather than deduped away: dropping planned work silently is the
		// failure mode this function must never have.
		if j.Extension == nil {
			deduped = append(deduped, j)
			continue
		}
		if seen[j.Extension.Name] {
			continue
		}
		seen[j.Extension.Name] = true
		deduped = append(deduped, j)
	}
	return deduped
}

// reportMissingExtensions writes the missing-extension guard's message, naming
// each missing extension's ROOT CAUSE when discovery recorded one.
//
// The distinction matters since the require-v3 flip:
// before it, a configured extension went missing because it was not installed,
// and `putnami install` was the remedy. Now the common cause is an extension
// that IS installed and simply loads below the current contract — for which
// installing again changes nothing, and the actual remedy (`putnami extensions
// update` / re-package) is inside the skip record's Reason. That reason was
// printed only under --debug (engine/workspace.go), so the user saw the one
// hint that could not help them. A missing extension WITHOUT a skip record is
// still genuinely absent, so the install hint is printed for exactly those.
//
// detail, when non-empty, is the guard-specific line printed before the causes.
func reportMissingExtensions(missing []string, skipped []extension.SkippedExtension, detail string) {
	reasons := make([]error, len(missing))
	explained := 0
	for i, name := range missing {
		if reason, ok := skipReasonFor(name, skipped); ok {
			reasons[i] = reason
			explained++
		}
	}

	// The headline states which of the two failures this is, because they have
	// opposite remedies. "not installed" over an extension the loader REJECTED
	// would send the user to `putnami install`, which reinstalls the same
	// unloadable bytes.
	headline := "required extensions are not installed"
	if explained == len(missing) {
		headline = "required extensions did not load"
	}
	iox.Fprintf(os.Stderr, "putnami: %s: %s\n", headline, strings.Join(missing, ", "))
	if detail != "" {
		iox.Fprintf(os.Stderr, "  %s\n", detail)
	}
	for i, name := range missing {
		if reasons[i] != nil {
			iox.Fprintf(os.Stderr, "  %s was found but not loaded: %v\n", name, reasons[i])
		}
	}
	if explained < len(missing) {
		iox.Fprintln(os.Stderr, "  run `putnami install` and resolve any integrity/lock errors above before retrying")
	}
}

// sddExtensionCommands are the four command groups that moved out of the CLI
// into an extension.
//
// They were built in for two years, so `putnami features` is in muscle memory,
// in shell history and in checked-in scripts. Without this list, a workspace
// that has not declared the extension answers "No jobs matched. Nothing to do."
// — which is true, and useless: it describes the plan instead of the reason the
// plan is empty.
//
// The list is a literal rather than a read of the extension's manifest on
// purpose. The whole case it serves is the one where that manifest is NOT in
// the workspace, so there is nothing to read; the manifest's own copy is pinned
// by tooling/sdd-extension's contract test.
var sddExtensionCommands = []string{"architecture", "contracts", "features", "specs"}

// sddExtensionHintRemedy is the one-line answer, stated once so the two callers
// below cannot drift into two different instructions. It names no extension and
// no path: the engine does not know which extension a workspace installs for
// these commands, and a path of one repository means nothing in another. The
// published reference it cites names the extension.
const sddExtensionHintRemedy = "moved from the core CLI to an extension that is not loaded; " +
	commandmeta.CommandsThatLeftTheCoreURL + " names that extension: " +
	"declare it under \"extensions\" in putnami.workspace.json, then run `putnami install`"

// reportUnservedSDDCommands writes the courtesy hint for every selected command
// that moved into an extension and that no loaded extension serves.
//
// It is a HINT, never a verdict: it changes no exit code and prints nothing
// when the extension is loaded. The "no loaded extension declares it" condition
// is what keeps it from firing on a workspace that has the extension and simply
// selected no matching project — telling a user to install what they already
// have is worse than saying nothing.
func reportUnservedSDDCommands(commands []string, extensions []*extension.ExtensionDescription) {
	jobMap := extension.BuildJobMap(extensions)
	seen := make(map[string]bool, len(commands))
	unserved := make([]string, 0, len(commands))
	for _, cmd := range commands {
		if seen[cmd] || len(jobMap[cmd]) > 0 {
			continue
		}
		if !slices.Contains(sddExtensionCommands, cmd) {
			continue
		}
		seen[cmd] = true
		unserved = append(unserved, cmd)
	}
	if len(unserved) == 0 {
		return
	}
	sort.Strings(unserved)
	iox.Fprintf(os.Stderr, "putnami: %s %s\n", strings.Join(unserved, ", "), sddExtensionHintRemedy)
}

// skipReasonFor finds the discovery skip record for a configured extension
// name. A skip is matched on either field it can carry the name in: Ref is the
// reference discovery was resolving (the config key), Name the manifest's own
// name — a manifest that fails to load may yield only one of the two.
func skipReasonFor(name string, skipped []extension.SkippedExtension) (error, bool) {
	for _, s := range skipped {
		if s.Reason == nil {
			continue
		}
		if s.Ref == name || s.Name == name {
			return s.Reason, true
		}
	}
	return nil, false
}

// missingRegistryExtensions returns the workspace-declared registry extensions
// (named like "@scope/name") that are neither loaded nor explicitly disabled.
// extensions must be what discovery loaded, never a planning scope: an
// extension the plan leaves out on purpose is not missing (see buildPlan).
// A non-empty result means the declared toolchain is not installed, so a run
// that planned zero jobs is a broken install rather than a legitimate no-op.
func missingRegistryExtensions(cfg *wsproto.Config, extensions []*extension.ExtensionDescription) []string {
	if cfg == nil {
		return nil
	}
	loaded := make(map[string]bool, len(extensions))
	for _, ext := range extensions {
		if ext != nil {
			loaded[ext.Name] = true
		}
	}
	disabled := make(map[string]bool)
	if cfg.Disable != nil {
		for _, name := range cfg.Disable.Extensions {
			disabled[name] = true
		}
	}
	var missing []string
	for name := range cfg.Extensions.List {
		// Only registry refs are resolvable by name here; local path refs
		// (./foo, /foo) load under their manifest name, so skip them.
		if !strings.HasPrefix(name, "@") {
			continue
		}
		if loaded[name] || disabled[name] {
			continue
		}
		missing = append(missing, name)
	}
	sort.Strings(missing)
	return missing
}

// selectedCommandsWithoutJobs returns the selected commands that contributed no
// jobs to the plan AND that no loaded extension even declares. Such a command
// has lost its provider — nothing loaded serves it and the plan scheduled
// nothing for it — which, paired with a missing required extension, is the
// observable symptom of a configured+locked extension that failed to load.
// A command a loaded extension declares but that merely matched no
// selected project is a legitimate no-op and is excluded; that exclusion is what
// keeps this guard from failing healthy partial plans.
func selectedCommandsWithoutJobs(commands []string, planned []*jobs.ScheduledJob, extensions []*extension.ExtensionDescription) []string {
	plannedCommands := make(map[string]bool, len(planned))
	for _, job := range planned {
		if job == nil {
			continue
		}
		plannedCommands[job.CommandName()] = true
	}
	jobMap := extension.BuildJobMap(extensions)
	var starved []string
	seen := make(map[string]bool, len(commands))
	for _, cmd := range commands {
		if plannedCommands[cmd] || len(jobMap[cmd]) > 0 || seen[cmd] {
			continue
		}
		seen[cmd] = true
		starved = append(starved, cmd)
	}
	sort.Strings(starved)
	return starved
}

// printPlan renders the execution plan onto the run's human-notice stream
// (Request.stdout). It takes the writer rather than reaching for os.Stdout so
// the MCP adapter's --plan preview cannot emit a table into its JSON-RPC frame
// stream.
//
// selected is the project set selection resolved, which the plan may exceed:
// a job that declares a `^step` need pulls that step in for every unselected
// dependency (planner_deps.go). The footer separates the two, because one
// number that covers both changes with the command set for the same diff and
// reads as "--impacted selected 106 projects" when it selected 72.
func printPlan(w io.Writer, planned []*jobs.ScheduledJob, selected []*workspace.Project) {
	// Group by project
	type projectGroup struct {
		name string
		jobs []*jobs.ScheduledJob
	}
	groups := make(map[string]*projectGroup)
	var order []string
	for _, j := range planned {
		name := j.Project.Name
		g, ok := groups[name]
		if !ok {
			g = &projectGroup{name: name}
			groups[name] = g
			order = append(order, name)
		}
		g.jobs = append(g.jobs, j)
	}

	iox.Fprintln(w)
	for _, name := range order {
		g := groups[name]
		iox.Fprintf(w, "  %s\n", name)
		for _, j := range g.jobs {
			// MISS means "cacheable, and this plan has no entry for it yet".
			// A job that cannot consult the cache at all — no task declaration,
			// cache disabled on the task or on the step — is NO-CACHE, so a cold
			// cache and a structurally uncacheable task stop looking identical.
			// CanUseCache already covers JobDef.Cache.
			cache := "MISS"
			if !jobs.CanUseCache(j) {
				cache = "NO-CACHE"
			}
			deps := ""
			if len(j.DependsOn) > 0 {
				deps = fmt.Sprintf("  [deps: %s]", strings.Join(j.DependsOn, ", "))
			}
			if len(j.SerializeAfter) > 0 {
				deps += fmt.Sprintf("  [after: %s]", strings.Join(j.SerializeAfter, ", "))
			}
			// The plan-level shared node this job belongs to, when it has one:
			// the nodes carrying the same tag are content-identical work several
			// commands scheduled, and exactly one of them will spawn a
			// subprocess.
			if shared := j.SharedExecution(); shared != "" {
				deps += fmt.Sprintf("  [%s]", shared)
			}
			iox.Fprintf(w, "    %-30s  %s%s\n", displayJobName(j), cache, deps)
		}
		iox.Fprintln(w)
	}

	m := jobs.ComputePlanMetrics(planned)
	// The project count covers every project that owns a planned job. When the
	// plan pulled dependencies in, say how many of them are the selection and
	// how many are those builds; when it pulled none, print the footer this
	// table has always printed. The selected side counts only projects that
	// planned at least one job, so it never exceeds what the plan holds.
	planning := make(map[string]bool, len(planned))
	for _, j := range planned {
		if j.Project != nil {
			planning[j.Project.ID] = true
		}
	}
	selectedWithJobs := 0
	for _, p := range selected {
		if p != nil && planning[p.ID] {
			selectedWithJobs++
		}
	}
	projects := fmt.Sprintf("%d projects", m.Projects)
	if dependencyBuilds := m.Projects - selectedWithJobs; dependencyBuilds > 0 {
		projects = fmt.Sprintf("%s (%d selected · %d dependency builds)", projects, selectedWithJobs, dependencyBuilds)
	}
	iox.Fprintf(w, "  %d jobs  ·  %d edges  ·  %s\n", m.Jobs, m.Edges, projects)
	if cmds := m.CommandsSorted(); len(cmds) > 0 {
		parts := make([]string, len(cmds))
		for i, c := range cmds {
			parts[i] = fmt.Sprintf("%s %d", c, m.ByCommand[c])
		}
		iox.Fprintf(w, "  by command: %s\n", strings.Join(parts, ", "))
	}
	iox.Fprintln(w)
}

// renderPlan selects the preview surface once for both --plan and plan-only
// --dry-run. Explicit JSON modes receive a typed plan verdict; every human mode
// retains the existing table byte-for-byte. A preview never constructs a
// renderer or session because no task executed and no session artifact exists.
func renderPlan(req *Request, planned []*jobs.ScheduledJob, selected []*workspace.Project) int {
	written, err := output.WritePreviewPlan(
		req.stdout(), req.Global.Output, strings.Join(req.Commands, ","), planned,
	)
	if err != nil {
		iox.Fprintf(os.Stderr, "putnami: render plan: %v\n", err)
		return ExitError
	}
	if !written {
		printPlan(req.stdout(), planned, selected)
	}
	return ExitSuccess
}

// summarizeProjectIDsLimit bounds the empty-plan diagnostic's project list. The
// line goes to a CI log, and a workspace-wide selection would otherwise print
// hundreds of ids to say one thing; the count precedes the list, so the elision
// loses nothing a reader needs to act.
const summarizeProjectIDsLimit = 10

func summarizeProjectIDs(projects []*workspace.Project) string {
	ids := make([]string, 0, min(len(projects), summarizeProjectIDsLimit))
	for _, project := range projects {
		if project == nil {
			continue
		}
		if len(ids) == summarizeProjectIDsLimit {
			return strings.Join(ids, ", ") + ", …"
		}
		ids = append(ids, project.ID)
	}
	if len(ids) == 0 {
		return "none"
	}
	return strings.Join(ids, ", ")
}

// machineOutputRequested includes structured execution as well as previews.
// Empty selections use typed records; human notices would corrupt the stream.
func machineOutputRequested(req *Request) bool {
	return protocolcli.OutputMode(req.Global.Output).IsStructured()
}

func displayJobName(job *jobs.ScheduledJob) string {
	name := job.DisplayName()
	if job.PlanName() != name && job.Extension != nil && job.Extension.Name != "" {
		return name + " (" + job.Extension.Name + ")"
	}
	return name
}

// PlanSnapshot, the v1 plan.json projection, was deleted here
// together with the v1 session writer it fed. A recorded plan is the versioned
// cli.SessionPlanFile (machine.SessionPlanFile) now, and the readers keep a v1
// fallback for snapshots already on disk.

// dropUndryableSideEffectJobs is the safety half of `publish --dry-run`:
// when the TERMINAL adapter opts a run into execution with a
// synthesized dry-run param (applyPublishDryRun), only tasks that DECLARE a
// dry-run flag can be trusted to interpret it — a registry- or
// cloud-side-effecting task without the declaration would drop the unknown
// param and publish for real. Those jobs are excluded from the plan, loudly,
// together with any job that functionally depends on an excluded one (a
// dependent would otherwise wait on a dependency that never runs).
//
// Side-effect-free jobs (build, package) keep running: the publishers'
// dry-run output is computed from real local artifacts, and the cache makes
// them cheap.
//
// Two deliberate exclusions from the condition:
//   - PlanExtension != nil (the alias adapter) is exempt — the alias gates the
//     param on the command's own group⊕subcommand flag surface, which the
//     planned JobDef does not carry, so this filter would misread a correctly
//     gated alias job as undeclared.
//   - Global.DryRun is never read — every "does this run execute?" decision
//     stays previewsOnly's (request_seams_test pins that); this decides which
//     jobs are SAFE inside a run that is already executing, from the
//     synthesized param itself.
func dropUndryableSideEffectJobs(req *Request, planned []*jobs.ScheduledJob) []*jobs.ScheduledJob {
	if !req.ExecutesUnderDryRun || req.PlanExtension != nil {
		return planned
	}
	if dryRun, ok := req.CommandParams["dry-run"].(bool); !ok || !dryRun {
		return planned
	}

	excluded := make(map[string]string, 4) // job key → reason
	for _, job := range planned {
		if job == nil || job.JobDef == nil || job.JobDef.Traits.SideEffects == "" {
			continue
		}
		if jobDeclaresDryRun(job) {
			continue
		}
		excluded[job.Key()] = job.JobDef.Traits.SideEffects + " side effects, no declared dry-run flag"
	}
	if len(excluded) == 0 {
		return planned
	}

	// Cascade over functional dependencies until stable: a kept job must never
	// wait on an excluded one.
	for changed := true; changed; {
		changed = false
		for _, job := range planned {
			key := job.Key()
			if _, gone := excluded[key]; gone {
				continue
			}
			for _, dep := range job.DependsOn {
				if _, gone := excluded[dep]; gone {
					excluded[key] = "depends on an excluded job"
					changed = true
					break
				}
			}
		}
	}

	kept := make([]*jobs.ScheduledJob, 0, len(planned)-len(excluded))
	dropped := make([]string, 0, len(excluded))
	for _, job := range planned {
		if reason, gone := excluded[job.Key()]; gone {
			dropped = append(dropped, job.Key()+" ("+reason+")")
			continue
		}
		kept = append(kept, job)
	}
	if !req.Global.Quiet {
		iox.Fprintf(os.Stderr, "putnami: --dry-run: excluded %d job(s) that cannot run safely without a declared dry-run flag:\n  %s\n",
			len(dropped), strings.Join(dropped, "\n  "))
	}
	return kept
}

// jobDeclaresDryRun reports whether a job can consume the synthesized dry-run
// parameter. Pipeline steps intentionally do not carry CLI flags; they inherit
// their flag surface from the command that expanded them.
func jobDeclaresDryRun(job *jobs.ScheduledJob) bool {
	if _, declared := job.JobDef.Flags["dry-run"]; declared {
		return true
	}
	if job.Extension == nil {
		return false
	}
	command := job.Extension.Jobs[job.CommandName()]
	if command == nil {
		return false
	}
	_, declared := command.Flags["dry-run"]
	return declared
}
