package cachekey

import (
	"path/filepath"
	"sort"

	jobmodel "go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// KeyInputs reads the file inputs of a plan's cache keys under one
// invocation's command parameters, with the resolver the keys expand their
// input selectors through.
type KeyInputs struct {
	Workspace     *workspace.Workspace
	CommandParams map[string]any
	Resolver      store.InputResolver
}

// reads returns the selection of every file a job's cache key reads: its
// project and workspace patterns, its closure patterns in every member of the
// closure, its generate assets, and the same inputs of the producing action the
// key of an invocation-scoped resource's consumer folds. It reads the
// declarations ComputeJobCacheHashWith keys, through the same helpers, the way
// portableKeyFiles does.
func (k KeyInputs) reads(job *jobmodel.ScheduledJob, scanner *selectionScanner) []fileSelection {
	if k.Workspace == nil || job == nil || job.JobDef == nil || job.Project == nil {
		return nil
	}
	files, workspaceFiles := KeyFilePatterns(k.Workspace, job, k.CommandParams)
	expander := JobInputExpander(k.Workspace, job, k.CommandParams, k.Resolver)
	reads := []fileSelection{scanner.Select(filepath.Join(k.Workspace.Root, job.Project.Path), files, expander)}
	if len(workspaceFiles) > 0 {
		reads = append(reads, scanner.Select(k.Workspace.Root, workspaceFiles, nil))
	}
	if patterns := closureKeyPatterns(job); len(patterns) > 0 {
		for _, root := range ProjectClosureRoots(k.Workspace, job.Project) {
			reads = append(reads, scanner.Select(root, patterns, nil))
		}
	}
	if assets := GenerateAssetFiles(k.Workspace, job); len(assets) > 0 {
		reads = append(reads, regionSelection(store.PathsRegion(assets)))
	}
	if job.InvocationProducer != nil {
		reads = append(reads, k.reads(job.InvocationProducer, scanner)...)
	}
	return reads
}

// plannedWrites is where a planned job may write a file another job's key
// reads: its declared outputs, resolved before it runs, and for a source
// rewriter the region of every input its own key reads, since it rewrites the
// sources it reads.
type plannedWrites struct {
	outputs   []plannedWrite
	rewritten []store.InputRegion
}

func (k KeyInputs) writes(job *jobmodel.ScheduledJob, scanner *selectionScanner) plannedWrites {
	writes := plannedWrites{outputs: declaredOutputWrites(WorkspaceRoots{Root: k.Workspace.Root}, job, &jobmodel.JobResult{})}
	for i, output := range writes.outputs {
		writes.outputs[i] = output.withRegion()
	}
	if RewritesSourceTree(job) {
		for _, read := range k.reads(job, scanner) {
			writes.rewritten = append(writes.rewritten, read.Region())
		}
	}
	return writes
}

// reachedBy reports whether writes can change a file one of reads selects.
func reachedBy(reads []fileSelection, writes plannedWrites) bool {
	for _, read := range reads {
		for _, output := range writes.outputs {
			if read.Reaches(output) {
				return true
			}
		}
		for _, region := range writes.rewritten {
			if read.Region().Overlaps(region) {
				return true
			}
		}
	}
	return false
}

// widenedFor returns reads with every selection one of whose input selectors
// computed its expansion from a basis one of writes can reach bounded by its
// region alone: its narrower bound holds only while every path that basis
// selects, through a content or a listing set, stays as it is.
func widenedFor(reads []fileSelection, writes []plannedWrites) []fileSelection {
	widened := make([]fileSelection, len(reads))
	for i, read := range reads {
		widened[i] = read
		if basisReached(read, writes) {
			widened[i] = read.Widened()
		}
	}
	return widened
}

func basisReached(read fileSelection, writes []plannedWrites) bool {
	for _, write := range writes {
		for _, output := range write.outputs {
			if read.ReachesBasis(output) {
				return true
			}
		}
		for _, region := range write.rewritten {
			if read.ReachesBasisIn(region) {
				return true
			}
		}
	}
	return false
}

// DeclaredOutputPaths returns the absolute path of every durable output job
// declares. A literal path is joined to its root. A path a port reports is
// read from the result's data; an output whose port reported nothing usable —
// in a task's result, or in the empty result of a task that has not run yet —
// contributes its within bound, or its whole root when it declares none,
// because the task may write anywhere beneath it.
// An output under a root this CLI cannot resolve contributes the workspace
// root. Invocation-scoped outputs live in the invocation's private tree, which
// no key reads.
func DeclaredOutputPaths(roots WorkspaceRoots, job *jobmodel.ScheduledJob, result *jobmodel.JobResult) []string {
	writes := declaredOutputWrites(roots, job, result)
	if writes == nil {
		return nil
	}
	paths := make([]string, 0, len(writes))
	for _, write := range writes {
		paths = append(paths, write.Path)
	}
	return paths
}

// declaredOutputWrites returns DeclaredOutputPaths as planned writes, sorted by
// path: a file output writes its path, and any other output may write anywhere
// at or under its path. A pathFrom output whose port reported no path may write
// anywhere under its within bound, or under its root when it declares none; one
// whose port reported a path it cannot use may write anywhere under its root.
func declaredOutputWrites(roots WorkspaceRoots, job *jobmodel.ScheduledJob, result *jobmodel.JobResult) []plannedWrite {
	declaration := jobmodel.TaskDeclarationOf(job)
	if declaration == nil {
		return nil
	}
	writes := make([]plannedWrite, 0, len(declaration.Outputs))
	for _, output := range declaration.Outputs {
		if output.IsInvocationScoped() {
			continue
		}
		base, ok := roots.OutputRoot(job, output.EffectiveRoot())
		if !ok {
			base = roots.Root
		}
		rel, resolved, err := declaredOutputPath(output, result.Data)
		_, reported := declaredPortPathValue(result.Data, output.PathFrom)
		switch bound := output.WithinBound(); {
		case err == nil && resolved:
			writes = append(writes, plannedWrite{
				Path: filepath.Join(base, filepath.FromSlash(rel)),
				Tree: output.Kind != extension.OutputKindFile,
			})
		case bound != "" && !reported:
			writes = append(writes, plannedWrite{Path: filepath.Join(base, filepath.FromSlash(bound)), Tree: true})
		default:
			writes = append(writes, plannedWrite{Path: base, Tree: true})
		}
	}
	sort.Slice(writes, func(i, j int) bool { return writes[i].Path < writes[j].Path })
	return writes
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
	// its key reads, or change which files it reads, sorted.
	Writers map[string][]string
	// Moving holds every job that has such a writer, and every job whose key
	// folds the key of a moving job (CacheKeyDependencies).
	Moving map[string]bool
}

// PlanKeyMovement decides KeyMovement for planned, from the declarations and
// the expansions of their input selectors in the tree as it stands. A writer counts whatever
// its place in the schedule, and when one of its writes can reach a file the
// key selects (fileSelection), so a job is reported moving whenever it
// cannot be shown not to be.
func PlanKeyMovement(inputs KeyInputs, planned []*jobmodel.ScheduledJob) KeyMovement {
	movement := KeyMovement{Writers: make(map[string][]string), Moving: make(map[string]bool)}
	if inputs.Workspace == nil {
		return movement
	}
	keyed := func(job *jobmodel.ScheduledJob) bool {
		return job != nil && job.JobDef != nil && job.Project != nil
	}
	var scanner selectionScanner
	var index store.RegionIndex
	writes := make(map[string]plannedWrites, len(planned))
	for _, job := range planned {
		if !keyed(job) {
			continue
		}
		key := job.Key()
		jobWrites := inputs.writes(job, &scanner)
		writes[key] = jobWrites
		for _, output := range jobWrites.outputs {
			index.Add(key, output.Region())
		}
		for _, region := range jobWrites.rewritten {
			index.Add(key, region)
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
		reads := inputs.reads(job, &scanner)
		regions := make([]store.InputRegion, 0, len(reads))
		for _, read := range reads {
			regions = append(regions, read.Region())
		}
		var candidates []string
		var candidateWrites []plannedWrites
		for _, writer := range index.Overlapping(regions...) {
			if writer != key {
				candidates = append(candidates, writer)
				candidateWrites = append(candidateWrites, writes[writer])
			}
		}
		reads = widenedFor(reads, candidateWrites)
		for i, writer := range candidates {
			if reachedBy(reads, candidateWrites[i]) {
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
