package cachekey

import (
	"path/filepath"
	"sort"

	jobmodel "go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// KeyInputs reads the file inputs of a plan's cache keys under one
// invocation's command parameters.
type KeyInputs struct {
	Workspace     *workspace.Workspace
	CommandParams map[string]any
}

// regions returns the region of every file a job's cache key reads: its
// project and workspace patterns, its closure patterns in every member of the
// closure, its generate assets, and the same inputs of the producing action the
// key of an invocation-scoped resource's consumer folds. It reads the
// declarations ComputeJobCacheHashWith keys, through the same helpers, the way
// portableKeyFiles does.
func (k KeyInputs) regions(job *jobmodel.ScheduledJob) []store.InputRegion {
	if k.Workspace == nil || job == nil || job.JobDef == nil || job.Project == nil {
		return nil
	}
	files, workspaceFiles := KeyFilePatterns(k.Workspace, job, k.CommandParams)
	regions := []store.InputRegion{store.FileInputRegion(filepath.Join(k.Workspace.Root, job.Project.Path), files)}
	if len(workspaceFiles) > 0 {
		regions = append(regions, store.FileInputRegion(k.Workspace.Root, workspaceFiles))
	}
	if patterns := closureKeyPatterns(job); len(patterns) > 0 {
		for _, root := range ProjectClosureRoots(k.Workspace, job.Project) {
			regions = append(regions, store.FileInputRegion(root, patterns))
		}
	}
	if assets := GenerateAssetFiles(k.Workspace, job); len(assets) > 0 {
		regions = append(regions, store.PathsRegion(assets))
	}
	if job.InvocationProducer != nil {
		regions = append(regions, k.regions(job.InvocationProducer)...)
	}
	return regions
}

// writes returns where a planned job may write a file another job's key reads:
// its declared outputs, resolved before it runs, and for a source rewriter
// every input its own key reads, since it rewrites the sources it reads.
func (k KeyInputs) writes(job *jobmodel.ScheduledJob) []store.InputRegion {
	var regions []store.InputRegion
	if paths := DeclaredOutputPaths(WorkspaceRoots{Root: k.Workspace.Root}, job, &jobmodel.JobResult{}); len(paths) > 0 {
		regions = append(regions, store.PathsRegion(paths))
	}
	if RewritesSourceTree(job) {
		regions = append(regions, k.regions(job)...)
	}
	return regions
}

// DeclaredOutputPaths returns the absolute path of every durable output job
// declares. A literal path is joined to its root. A path a port reports is
// read from the result's data; an output whose port reported nothing usable —
// in a task's result, or in the empty result of a task that has not run yet —
// contributes its whole root, because the task may write anywhere beneath it.
// An output under a root this CLI cannot resolve contributes the workspace
// root. Invocation-scoped outputs live in the invocation's private tree, which
// no key reads.
func DeclaredOutputPaths(roots WorkspaceRoots, job *jobmodel.ScheduledJob, result *jobmodel.JobResult) []string {
	declaration := jobmodel.TaskDeclarationOf(job)
	if declaration == nil {
		return nil
	}
	paths := make([]string, 0, len(declaration.Outputs))
	for _, output := range declaration.Outputs {
		if output.IsInvocationScoped() {
			continue
		}
		base, ok := roots.OutputRoot(job, output.EffectiveRoot())
		if !ok {
			base = roots.Root
		}
		rel, resolved, err := declaredOutputPath(output, result.Data)
		if err != nil || !resolved {
			paths = append(paths, base)
			continue
		}
		paths = append(paths, filepath.Join(base, filepath.FromSlash(rel)))
	}
	sort.Strings(paths)
	return paths
}

// RestoredOutputPaths returns the absolute path of every output a task-owned
// entry records, present or recorded empty: every path its restore rewrites or
// clears.
func RestoredOutputPaths(roots WorkspaceRoots, job *jobmodel.ScheduledJob, entry *store.TaskEntry) []string {
	if entry == nil {
		return nil
	}
	paths := make([]string, 0, len(entry.Outputs))
	for _, output := range entry.Outputs {
		base, ok := roots.OutputRoot(job, output.Root)
		if !ok {
			base = roots.Root
		}
		paths = append(paths, filepath.Join(base, filepath.FromSlash(output.Path)))
	}
	sort.Strings(paths)
	return paths
}

// KeyMovement is one plan's answer to "may the key this job computes before
// any task runs differ from the key it computes when it starts?".
type KeyMovement struct {
	// Writers maps a job key to the other planned jobs that may write a file
	// its key reads, sorted.
	Writers map[string][]string
	// Moving holds every job that has such a writer, and every job whose key
	// folds the key of a moving job (CacheKeyDependencies).
	Moving map[string]bool
}

// PlanKeyMovement decides KeyMovement for planned, from the declarations alone.
// A writer counts whatever its place in the schedule, so a job is reported
// moving whenever it cannot be shown not to be.
func PlanKeyMovement(inputs KeyInputs, planned []*jobmodel.ScheduledJob) KeyMovement {
	movement := KeyMovement{Writers: make(map[string][]string), Moving: make(map[string]bool)}
	if inputs.Workspace == nil {
		return movement
	}
	keyed := func(job *jobmodel.ScheduledJob) bool {
		return job != nil && job.JobDef != nil && job.Project != nil
	}
	var index store.RegionIndex
	for _, job := range planned {
		if !keyed(job) {
			continue
		}
		for _, region := range inputs.writes(job) {
			index.Add(job.Key(), region)
		}
	}

	dependents := make(map[string][]string, len(planned))
	var queue []string
	for _, job := range planned {
		if !keyed(job) {
			continue
		}
		key := job.Key()
		for _, dependency := range CacheKeyDependencies(job) {
			dependents[dependency] = append(dependents[dependency], key)
		}
		for _, writer := range index.Overlapping(inputs.regions(job)...) {
			if writer != key {
				movement.Writers[key] = append(movement.Writers[key], writer)
			}
		}
		if len(movement.Writers[key]) > 0 {
			movement.Moving[key] = true
			queue = append(queue, key)
		}
	}
	for len(queue) > 0 {
		key := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		for _, dependent := range dependents[key] {
			if !movement.Moving[dependent] {
				movement.Moving[dependent] = true
				queue = append(queue, dependent)
			}
		}
	}
	return movement
}

// JobsByKey indexes planned by job key.
func JobsByKey(planned []*jobmodel.ScheduledJob) map[string]*jobmodel.ScheduledJob {
	byKey := make(map[string]*jobmodel.ScheduledJob, len(planned))
	for _, job := range planned {
		if job != nil {
			byKey[job.Key()] = job
		}
	}
	return byKey
}

// schedulingAncestors returns the key of every planned job that finishes before
// job starts: its scheduling predecessors, functional and ordering-only, and
// theirs.
func schedulingAncestors(job *jobmodel.ScheduledJob, byKey map[string]*jobmodel.ScheduledJob) map[string]bool {
	ancestors := make(map[string]bool)
	pending := append([]string(nil), job.SchedulingPredecessors()...)
	for len(pending) > 0 {
		key := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if ancestors[key] {
			continue
		}
		ancestors[key] = true
		if predecessor := byKey[key]; predecessor != nil {
			pending = append(pending, predecessor.SchedulingPredecessors()...)
		}
	}
	return ancestors
}

// SettlesBefore reports whether every planned job that can make consumer's key
// differ from its precomputed one finishes before producer starts: each writer
// of a file the key reads, unless it only runs after consumer, and each planned
// job whose key the consumer's key folds, apart from producer itself. When it
// holds, the key consumer computes as producer starts is the key it computes
// when it starts itself.
func SettlesBefore(producer, consumer *jobmodel.ScheduledJob, movement KeyMovement, byKey map[string]*jobmodel.ScheduledJob) bool {
	before := schedulingAncestors(producer, byKey)
	for _, dependency := range CacheKeyDependencies(consumer) {
		if _, planned := byKey[dependency]; !planned || dependency == producer.Key() {
			continue
		}
		if !before[dependency] {
			return false
		}
	}
	for _, writer := range movement.Writers[consumer.Key()] {
		if before[writer] {
			continue
		}
		if job := byKey[writer]; job != nil && schedulingAncestors(job, byKey)[consumer.Key()] {
			continue
		}
		return false
	}
	return true
}
