package engine

import (
	"path"
	"path/filepath"
	"strings"

	modelext "go.putnami.dev/cli/model/extension"
	modeljobs "go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// neverNarrowedCommands are never narrowed by a task scope, for two different
// reasons.
//
// `publish` and `deploy`: a release session's members are decided by the
// release-set coordinator, not by the diff, and a member that is also a
// task-scoped verification project must still publish. The scope narrows what
// the diff VERIFIES, never what the session RELEASES.
//
// `validate` and `validate-workspace`: their steps read the verification
// reports the session recorded, not files. `validate~features` declares no
// input and depends on no task, so no changed file ever names it and the scope
// drops it — and `validate~specs`, which depends on it, with it. That retires
// the spec gate for exactly the projects this selection newly makes
// task-scoped, silently: `test~test` re-runs green and no criteria projection
// is written for the finalizer to join.
//
// Both entries are a command-level answer to the same general gap: a task that
// declares no file input has said nothing about what it reads, and the index
// reads that silence as "reads nothing". Fixing it there — silence means the
// task runs whenever its project runs anything — is the durable repair; it also
// drags a finalizer's successors into scopes that never asked for them, so it
// needs its own change.
var neverNarrowedCommands = map[string]bool{
	"publish": true, "deploy": true, "validate": true, "validate-workspace": true,
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
//   - its command is never narrowed (neverNarrowedCommands): a release
//     command, or a `validate` whose steps read session state and not files;
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
	for _, job := range planned {
		if job == nil || job.Project == nil || job.JobDef == nil {
			continue
		}
		scope, scoped := scopes[job.Project.ID]
		if !scoped || neverNarrowedCommands[modeljobs.JobCommandName(job)] || jobInScope(ws, job, scope) {
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
