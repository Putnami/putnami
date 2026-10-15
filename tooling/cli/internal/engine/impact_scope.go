package engine

import (
	"path"
	"path/filepath"
	"strings"

	modelext "go.putnami.dev/cli/model/extension"
	modeljobs "go.putnami.dev/cli/model/jobs"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// neverNarrowedCommands are the commands whose jobs a task scope never narrows
// by name (ADR 0068). `validate` and `validate-workspace` check what the
// session verified, including the spec gate's criteria projection on every
// project that runs `test`. A scope names the tasks a change's declared inputs
// reach, and no declaration names the tasks these two follow, so their names
// stand in for that declaration. A kept `validate~specs` keeps in turn the
// `test` command whose reports it judges (narrowToTaskScopes).
//
// Every other command is kept or dropped by its declarations alone: see
// narrowToTaskScopes.
var neverNarrowedCommands = map[string]bool{
	"validate": true, "validate-workspace": true,
}

// extensionScopeSeparator joins an extension project id to a task scope inside
// one scope entry (workspace.TaskIndex owns the spelling).
const extensionScopeSeparator = "#"

// narrowToTaskScopes applies an --impacted selection's task scopes to a plan
// (workspace.ImpactedSelection.TaskScopes, ADR 0044).
//
// A job is kept when any of these holds:
//
//   - its project is not task-scoped: it runs every task, as before;
//   - the scope names its task — `<command>~<step>`, or the command alone for a
//     job with no pipeline step;
//   - its command's traits declare side effects, or its well-known verb
//     has a default one that a declaration never removes: the
//     release-set coordinator decides which members a session releases, and a
//     scope narrows what the diff VERIFIES, never what the session RELEASES;
//   - its command is `validate` or `validate-workspace`
//     (neverNarrowedCommands);
//   - it is a `test` job, and a kept job of the same project projects the
//     spec criteria judged against that command's verification reports: the
//     spec gate reads the reports a full plan gives it, and an unchanged test
//     is a cache hit;
//   - a kept job depends on it or is serialized after it, transitively: the
//     kept plan stays a closed graph, the rule serve_withhold.go applies to
//     the jobs it hands a supervisor. A scoped `test` keeps the `build` it
//     depends on whichever extension owns it, and a dependency's `^generate`
//     step keeps running.
//
// Every other job of a task-scoped project is dropped: no action of the project
// that the diff reaches runs that task. The order of the plan is preserved, and
// a new slice is returned for the reason dedupePlanByExtension states.
func narrowToTaskScopes(ws *workspace.Workspace, planned []*jobs.ScheduledJob, scopes map[string][]string) []*jobs.ScheduledJob {
	if len(scopes) == 0 {
		return planned
	}
	byKey := make(map[string]*jobs.ScheduledJob, len(planned))
	for _, job := range planned {
		if job != nil && job.Project != nil && job.JobDef != nil {
			byKey[job.Key()] = job
		}
	}
	kept := make(map[string]bool, len(planned))
	var frontier []*jobs.ScheduledJob
	judged := make(map[string]bool)
	for _, job := range planned {
		if job == nil || job.Project == nil || job.JobDef == nil {
			continue
		}
		scope, scoped := scopes[job.Project.ID]
		// This rule also counts the well-known verb's default side effect when
		// a declaration drops it, so `publish` and `deploy` keep their jobs
		// whatever their manifest declares. Every other reader of a command's
		// side effects follows the declared traits as resolved.
		command := modeljobs.JobCommandName(job)
		sideEffects := job.JobDef.Traits.SideEffects != extensionproto.SideEffectsNone ||
			extensionproto.EffectiveTraits(command, nil).SideEffects != extensionproto.SideEffectsNone
		if !scoped || sideEffects || neverNarrowedCommands[command] || jobInScope(ws, job, scope) {
			kept[job.Key()] = true
			frontier = append(frontier, job)
			if scoped && jobs.ProducesSpecCriteriaProjection(job) {
				judged[job.Project.ID] = true
			}
		}
	}
	// A kept criteria projection is judged against the verification reports of
	// its project's `test` command, so that command stays whole: the steps that
	// write the reports, and the finalizers that tear down what they ran on.
	for _, job := range planned {
		if job != nil && job.Project != nil && job.JobDef != nil && judged[job.Project.ID] &&
			!kept[job.Key()] && modeljobs.JobCommandName(job) == "test" {
			kept[job.Key()] = true
			frontier = append(frontier, job)
		}
	}
	// Predecessor closure: whatever a kept job needs is kept, however many
	// hops away and whichever extension owns it.
	for len(frontier) > 0 {
		job := frontier[0]
		frontier = frontier[1:]
		for _, key := range job.SchedulingPredecessors() {
			if kept[key] {
				continue
			}
			if predecessor, ok := byKey[key]; ok {
				kept[key] = true
				frontier = append(frontier, predecessor)
			}
		}
	}
	narrowed := make([]*jobs.ScheduledJob, 0, len(kept))
	for _, job := range planned {
		if job == nil || job.Project == nil || job.JobDef == nil || kept[job.Key()] {
			narrowed = append(narrowed, job)
		}
	}
	return narrowed
}

// jobInScope reports whether a project's scope names this job.
//
// A scope entry is `[<extension-project-id>#]<command>~<step>`. The task half is
// matched against the job's own command and step, never against its display
// name, which namespacing rewrites when two extensions serve one command on one
// project. The qualifier, when present, additionally requires the job to be
// that extension's: two extensions declare the same command and step routinely,
// and an extension-consumer scope names only the tasks the rebuilt tool runs.
//
// An entry that is a bare project id is the whole of an extension's tasks —
// the scope a selection computed without a task index produces (ADR 0042,
// ImpactTrace.Scopes). Matching it is what keeps such a selection narrowing
// what it always narrowed.
func jobInScope(ws *workspace.Workspace, job *jobs.ScheduledJob, scope []string) bool {
	task := jobTaskScope(job)
	for _, entry := range scope {
		extensionID, taskEntry, qualified := strings.Cut(entry, extensionScopeSeparator)
		if !qualified {
			if strings.HasPrefix(entry, "/") {
				if extensionProjectIs(ws, job, entry) {
					return true
				}
				continue
			}
			if entry == task {
				return true
			}
			continue
		}
		if taskEntry == task && extensionProjectIs(ws, job, extensionID) {
			return true
		}
	}
	return false
}

// jobTaskScope is a planned job's task scope: `<command>~<step>`, or the
// command alone for a job that is not an expanded pipeline step.
func jobTaskScope(job *jobs.ScheduledJob) string {
	command := job.CommandName()
	step := job.StepID()
	if step == "" {
		return command
	}
	return command + modelext.StepSeparator + step
}

// extensionProjectIs reports whether a job's extension is the named extension
// project. An in-workspace extension is discovered from its project directory,
// so its description's workspace-relative path is the project's path
// (internal/extension discovery), and a pinned build names the project whose
// manifest it replaces; names are not compared, because a registry-installed
// extension can share a name with a project.
func extensionProjectIs(ws *workspace.Workspace, job *jobs.ScheduledJob, projectID string) bool {
	if job.Extension == nil {
		return false
	}
	source := job.Extension.RelPath
	if source == "" {
		source = job.Extension.PinnedOver
	}
	if source == "" {
		return false
	}
	p := ws.ProjectByID(projectID)
	if p == nil {
		return false
	}
	return path.Clean(filepath.ToSlash(p.Path)) == path.Clean(filepath.ToSlash(source))
}
