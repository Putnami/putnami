package engine

import (
	"path"
	"path/filepath"
	"slices"
	"strings"

	modelext "go.putnami.dev/cli/model/extension"
	jobmodel "go.putnami.dev/cli/model/jobs"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/tooling/cli/internal/jobs/scheduler"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

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
//   - its resolved traits declare side effects: the release-set coordinator
//     decides which members a session releases, and a scope narrows what the
//     diff VERIFIES, never what the session RELEASES;
//   - its command's resolved traits declare the criteria kind (ADR 0072): such
//     a command checks what the session verified, including the spec gate's
//     criteria projection. A scope names the tasks a change's declared inputs
//     reach, and no declaration names the session results these commands
//     follow, so the kind stands in for that declaration;
//   - a kept job depends on it, transitively: the kept plan stays a closed
//     graph, the rule serve_withhold.go applies to the jobs it hands a
//     supervisor. A scoped `test` keeps the `build` it depends on whichever
//     extension owns it, and a dependency's `^generate` step keeps running.
//
// A kept criteria job keeps what it depends on and nothing else. The spec gate
// reads the report of a verification job the plan dropped from that task's
// cache entry, and a check no entry answers warns and never blocks (ADR 0026).
//
// A write-serialization edge (ScheduledJob.SerializeAfter) orders two jobs and
// keeps neither. A kept job's edge to a dropped job becomes edges to the kept
// jobs that the dropped job waits for, found by walking scheduling
// predecessors through dropped jobs only. An edge that duplicates a functional
// dependency of the kept job is left out. For any two kept jobs, the narrowed
// plan orders them as the full plan does, and it names no job it does not hold.
//
// Every other job of a task-scoped project is dropped: no action of the project
// that the diff reaches runs that task. The order of the plan is preserved, and
// a new slice is returned for the reason dedupePlanByExtension states. A job
// whose edges change is a copy, so the planned jobs keep the edges the planner
// gave them.
func narrowToTaskScopes(ws *workspace.Workspace, planned []*scheduler.ScheduledJob, scopes map[string][]string) []*scheduler.ScheduledJob {
	if len(scopes) == 0 {
		return planned
	}
	byKey := make(map[string]*scheduler.ScheduledJob, len(planned))
	for _, job := range planned {
		if job != nil && job.Project != nil && job.JobDef != nil {
			byKey[job.Key()] = job
		}
	}
	kept := make(map[string]bool, len(planned))
	var frontier []*scheduler.ScheduledJob
	for _, job := range planned {
		if job == nil || job.Project == nil || job.JobDef == nil {
			continue
		}
		scope, scoped := scopes[job.Project.ID]
		// The side effects are the job's resolved traits: the ones its manifest
		// declares, or its verb's default traits when it declares none. The core
		// adds none of its own.
		sideEffects := job.JobDef.Traits.SideEffects != extensionproto.SideEffectsNone
		criteria := job.JobDef.Traits.Kind == extensionproto.CommandKindCriteria
		if !scoped || sideEffects || criteria || jobInScope(ws, job, scope) {
			kept[job.Key()] = true
			frontier = append(frontier, job)
		}
	}
	// Functional closure: whatever a kept job depends on is kept, however many
	// hops away and whichever extension owns it.
	for len(frontier) > 0 {
		job := frontier[0]
		frontier = frontier[1:]
		for _, key := range job.DependsOn {
			if kept[key] {
				continue
			}
			if predecessor, ok := byKey[key]; ok {
				kept[key] = true
				frontier = append(frontier, predecessor)
			}
		}
	}
	// Ordering closure: a dropped job stands for the kept jobs it waits for,
	// however many dropped jobs lie between. Each dropped job is walked once.
	waitsFor := make(map[string][]string)
	var keptPredecessors func(key string) []string
	keptPredecessors = func(key string) []string {
		if found, ok := waitsFor[key]; ok {
			return found
		}
		// The planner refuses a cyclic plan; the mark still ends a walk on one.
		waitsFor[key] = nil
		var found []string
		if job := byKey[key]; job != nil {
			for _, predecessor := range job.SchedulingPredecessors() {
				if kept[predecessor] {
					found = append(found, predecessor)
				} else {
					found = append(found, keptPredecessors(predecessor)...)
				}
			}
		}
		found = jobmodel.DedupeSorted(found)
		waitsFor[key] = found
		return found
	}
	dropped := func(key string) bool { return !kept[key] }
	narrowed := make([]*scheduler.ScheduledJob, 0, len(kept))
	for _, job := range planned {
		if job == nil || job.Project == nil || job.JobDef == nil {
			narrowed = append(narrowed, job)
			continue
		}
		if !kept[job.Key()] {
			continue
		}
		if slices.ContainsFunc(job.SerializeAfter, dropped) {
			var after []string
			for _, key := range job.SerializeAfter {
				if kept[key] {
					after = append(after, key)
					continue
				}
				for _, predecessor := range keptPredecessors(key) {
					if !slices.Contains(job.DependsOn, predecessor) {
						after = append(after, predecessor)
					}
				}
			}
			copied := *job
			copied.SerializeAfter = jobmodel.DedupeSorted(after)
			job = &copied
		}
		narrowed = append(narrowed, job)
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
func jobInScope(ws *workspace.Workspace, job *scheduler.ScheduledJob, scope []string) bool {
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
func jobTaskScope(job *scheduler.ScheduledJob) string {
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
func extensionProjectIs(ws *workspace.Workspace, job *scheduler.ScheduledJob, projectID string) bool {
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
