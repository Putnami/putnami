// Which cache keys a task's writes can move.
//
// A cache key reads file bytes through the CacheManager's memo. A run computes a
// job's key before any task runs — PrecomputeKeys, for the remote negotiation
// and the invocation pruning — and again when the job starts, in
// lookupRestoreOrClaim. Only the second one addresses a lookup, a restore, a
// capture and an upload, and between the two, earlier tasks write their
// declared outputs. Two rules keep it the key of the bytes the job reads:
//
//   - every write drops the memoized digests it can move before the writing
//     task's result is published, so before any task that reads the paths can
//     start: finalizeExecutedJob for an execution, restoreDeclaredCacheHit for
//     a restore;
//   - a decision taken on a precomputed key is trusted only for a job whose
//     key no planned write can move (planKeyMovement): the negotiation records
//     no other job's key, and a pruning that rests on a movable consumer key
//     is confirmed from the key the consumer computes once every write that
//     can move it happened (invocationRuntime.isPruned).
package jobs

import (
	"path/filepath"
	"sort"
	"sync"

	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// keyInputs reads the file inputs of a plan's cache keys under one
// invocation's command parameters.
type keyInputs struct {
	ws            *workspace.Workspace
	commandParams map[string]any
}

// regions returns the region of every file a job's cache key reads: its
// project and workspace patterns, its closure patterns in every member of the
// closure, its generate assets, and the same inputs of the producing action the
// key of an invocation-scoped resource's consumer folds. It reads the
// declarations computeJobCacheHashWith keys, through the same helpers, the way
// portableKeyFiles does.
func (k keyInputs) regions(job *ScheduledJob) []store.InputRegion {
	if k.ws == nil || job == nil || job.JobDef == nil || job.Project == nil {
		return nil
	}
	files, workspaceFiles := keyFilePatterns(k.ws, job, k.commandParams)
	regions := []store.InputRegion{store.FileInputRegion(filepath.Join(k.ws.Root, job.Project.Path), files)}
	if len(workspaceFiles) > 0 {
		regions = append(regions, store.FileInputRegion(k.ws.Root, workspaceFiles))
	}
	if patterns := closureKeyPatterns(job); len(patterns) > 0 {
		for _, root := range projectClosureRoots(k.ws, job.Project) {
			regions = append(regions, store.FileInputRegion(root, patterns))
		}
	}
	if assets := generateAssetFiles(k.ws, job); len(assets) > 0 {
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
func (k keyInputs) writes(job *ScheduledJob) []store.InputRegion {
	var regions []store.InputRegion
	if paths := declaredOutputPaths(workspaceRoots{root: k.ws.Root}, job, &JobResult{}); len(paths) > 0 {
		regions = append(regions, store.PathsRegion(paths))
	}
	if rewritesSourceTree(job) {
		regions = append(regions, k.regions(job)...)
	}
	return regions
}

// declaredOutputPaths returns the absolute path of every durable output job
// declares. A literal path is joined to its root. A path a port reports is
// read from the result's data; an output whose port reported nothing usable —
// in a task's result, or in the empty result of a task that has not run yet —
// contributes its whole root, because the task may write anywhere beneath it.
// An output under a root this CLI cannot resolve contributes the workspace
// root. Invocation-scoped outputs live in the invocation's private tree, which
// no key reads.
func declaredOutputPaths(roots workspaceRoots, job *ScheduledJob, result *JobResult) []string {
	declaration := taskDeclarationOf(job)
	if declaration == nil {
		return nil
	}
	paths := make([]string, 0, len(declaration.Outputs))
	for _, output := range declaration.Outputs {
		if output.IsInvocationScoped() {
			continue
		}
		base, ok := roots.outputRoot(job, output.EffectiveRoot())
		if !ok {
			base = roots.root
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

// restoredOutputPaths returns the absolute path of every output a task-owned
// entry records, present or recorded empty: every path its restore rewrites or
// clears.
func restoredOutputPaths(roots workspaceRoots, job *ScheduledJob, entry *store.TaskEntry) []string {
	if entry == nil {
		return nil
	}
	paths := make([]string, 0, len(entry.Outputs))
	for _, output := range entry.Outputs {
		base, ok := roots.outputRoot(job, output.Root)
		if !ok {
			base = roots.root
		}
		paths = append(paths, filepath.Join(base, filepath.FromSlash(output.Path)))
	}
	sort.Strings(paths)
	return paths
}

// keyMovement is one plan's answer to "may the key this job computes before
// any task runs differ from the key it computes when it starts?".
type keyMovement struct {
	// writers maps a job key to the other planned jobs that may write a file
	// its key reads, sorted.
	writers map[string][]string
	// moving holds every job that has such a writer, and every job whose key
	// folds the key of a moving job (cacheKeyDependencies).
	moving map[string]bool
}

// planKeyMovement decides keyMovement for planned, from the declarations alone.
// A writer counts whatever its place in the schedule, so a job is reported
// moving whenever it cannot be shown not to be.
func planKeyMovement(inputs keyInputs, planned []*ScheduledJob) keyMovement {
	movement := keyMovement{writers: make(map[string][]string), moving: make(map[string]bool)}
	if inputs.ws == nil {
		return movement
	}
	keyed := func(job *ScheduledJob) bool {
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
		for _, dependency := range cacheKeyDependencies(job) {
			dependents[dependency] = append(dependents[dependency], key)
		}
		for _, writer := range index.Overlapping(inputs.regions(job)...) {
			if writer != key {
				movement.writers[key] = append(movement.writers[key], writer)
			}
		}
		if len(movement.writers[key]) > 0 {
			movement.moving[key] = true
			queue = append(queue, key)
		}
	}
	for len(queue) > 0 {
		key := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		for _, dependent := range dependents[key] {
			if !movement.moving[dependent] {
				movement.moving[dependent] = true
				queue = append(queue, dependent)
			}
		}
	}
	return movement
}

// cachedAtCurrentKey reports whether the local store holds job's entry at the
// key it computes now, from the dependency keys this run published and the
// tree as it stands: the key lookupRestoreOrClaim computes when the job starts,
// once nothing that can move it is left to run.
func (s *Scheduler) cachedAtCurrentKey(job *ScheduledJob, mu *sync.Mutex, hashes map[string]string) bool {
	if s.cache == nil {
		return false
	}
	mu.Lock()
	hashCopy := make(map[string]string, len(job.DependsOn))
	for _, depKey := range cacheKeyDependencies(job) {
		if value, ok := hashes[depKey]; ok {
			hashCopy[depKey] = value
		}
	}
	mu.Unlock()
	hash, err := computeJobCacheHash(s.ws, job, s.commandParams, s.cfg.VersionInfo, s.cache, hashCopy)
	return err == nil && cachedEntryPresent(s.cache, job, hash)
}

// jobsByKey indexes planned by job key.
func jobsByKey(planned []*ScheduledJob) map[string]*ScheduledJob {
	byKey := make(map[string]*ScheduledJob, len(planned))
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
func schedulingAncestors(job *ScheduledJob, byKey map[string]*ScheduledJob) map[string]bool {
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

// settlesBefore reports whether every planned job that can make consumer's key
// differ from its precomputed one finishes before producer starts: each writer
// of a file the key reads, unless it only runs after consumer, and each planned
// job whose key the consumer's key folds, apart from producer itself. When it
// holds, the key consumer computes as producer starts is the key it computes
// when it starts itself.
func settlesBefore(producer, consumer *ScheduledJob, movement keyMovement, byKey map[string]*ScheduledJob) bool {
	before := schedulingAncestors(producer, byKey)
	for _, dependency := range cacheKeyDependencies(consumer) {
		if _, planned := byKey[dependency]; !planned || dependency == producer.Key() {
			continue
		}
		if !before[dependency] {
			return false
		}
	}
	for _, writer := range movement.writers[consumer.Key()] {
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
