package jobs

import (
	"sort"

	"go.putnami.dev/tooling/cli/internal/extension"
)

// serializeWriteResources adds write-serialization edges (ScheduledJob.SerializeAfter)
// between jobs whose declared resource accesses conflict, so two jobs never write
// the same resource concurrently and no reader observes a partial or overwritten
// version of what it consumes.
//
// Tasks declare the resources they touch with `writes` / `reads` in the manifest
// (e.g. a shared .gen tree). This replaces the hard-coded heuristic that ordered
// every non-build generate step after the whole build command: serialization now
// follows declared conflicts, so independent and read-only steps (lint checks,
// jobs that touch unrelated outputs) run as early as their functional
// dependencies allow.
//
// For each resource the writers are ordered into blocks. A reader joins the block
// of the writer it functionally derives from (its nearest writer ancestor), so it
// observes that writer's output. Blocks are then serialized end-to-end: the next
// writer waits for the previous writer AND every reader in its block before
// overwriting. Readers in the same block share no edges, so they still run in
// parallel. Functional dependencies (DependsOn) are never modified, and every
// edge is consistent with a topological order of the functional graph, so the
// combined graph stays acyclic.
func serializeWriteResources(jobs []*ScheduledJob) {
	pos, ok := functionalTopoOrder(jobs)
	if !ok {
		// The functional graph has a cycle; validatePlanDAG reports it. Adding
		// edges on an inconsistent order could only make the diagnosis worse.
		return
	}

	inPlan := make(map[string]bool, len(jobs))
	for _, job := range jobs {
		inPlan[job.Key()] = true
	}
	functionalDeps := make(map[string][]string, len(jobs))
	for _, job := range jobs {
		for _, dep := range job.DependsOn {
			if inPlan[dep] {
				functionalDeps[job.Key()] = append(functionalDeps[job.Key()], dep)
			}
		}
	}

	type group struct {
		writers map[string]bool
		readers []string
	}
	groups := make(map[string]*group)
	ensure := func(ck string) *group {
		g := groups[ck]
		if g == nil {
			g = &group{writers: make(map[string]bool)}
			groups[ck] = g
		}
		return g
	}

	// Group every resource access by its conflict key. A job that both reads and
	// writes a resource is treated purely as a writer (writers are exclusive).
	for _, job := range jobs {
		key := job.Key()
		written := make(map[string]bool)
		for _, ref := range job.JobDef.Writes {
			ck := resourceConflictKey(job.Project.ID, ref)
			if written[ck] {
				continue
			}
			written[ck] = true
			ensure(ck).writers[key] = true
		}
		for _, ref := range job.JobDef.Reads {
			ck := resourceConflictKey(job.Project.ID, ref)
			if written[ck] {
				continue
			}
			g := ensure(ck)
			g.readers = append(g.readers, key)
		}
	}
	if len(groups) == 0 {
		return
	}

	edges := make(map[string]map[string]bool)
	addEdge := func(pred, succ string) {
		if pred == "" || pred == succ {
			return
		}
		if edges[succ] == nil {
			edges[succ] = make(map[string]bool)
		}
		edges[succ][pred] = true
	}

	groupKeys := make([]string, 0, len(groups))
	for ck := range groups {
		groupKeys = append(groupKeys, ck)
	}
	sort.Strings(groupKeys)

	for _, ck := range groupKeys {
		g := groups[ck]
		if len(g.writers) == 0 {
			// Nobody writes this resource in the plan, so its readers observe a
			// static tree and never conflict — they all run in parallel.
			continue
		}

		writerKeys := make([]string, 0, len(g.writers))
		for w := range g.writers {
			writerKeys = append(writerKeys, w)
		}
		sort.Slice(writerKeys, func(i, j int) bool {
			if pos[writerKeys[i]] != pos[writerKeys[j]] {
				return pos[writerKeys[i]] < pos[writerKeys[j]]
			}
			return writerKeys[i] < writerKeys[j]
		})

		// Assign each reader to the writer block it derives from: the writer with
		// the highest topological position among its functional ancestors. Readers
		// with no writer ancestor read the pre-existing tree (epoch zero).
		blockReaders := make(map[string][]string)
		var epochZeroReaders []string
		for _, r := range g.readers {
			if w := nearestAncestorWriter(r, g.writers, functionalDeps, pos); w != "" {
				blockReaders[w] = append(blockReaders[w], r)
			} else {
				epochZeroReaders = append(epochZeroReaders, r)
			}
		}

		// Serialize the blocks. The first writer waits for the epoch-zero readers
		// it would overwrite; every later writer waits for the entire previous
		// block (its writer and that writer's readers).
		for i, w := range writerKeys {
			if i == 0 {
				for _, r := range epochZeroReaders {
					addEdge(r, w)
				}
				continue
			}
			prev := writerKeys[i-1]
			addEdge(prev, w)
			for _, r := range blockReaders[prev] {
				addEdge(r, w)
			}
		}
	}

	for _, job := range jobs {
		preds := edges[job.Key()]
		if len(preds) == 0 {
			continue
		}
		// Drop edges already implied by a functional dependency so SerializeAfter
		// only records ordering that DependsOn does not.
		functional := make(map[string]bool, len(job.DependsOn))
		for _, d := range job.DependsOn {
			functional[d] = true
		}
		extra := make([]string, 0, len(preds))
		for p := range preds {
			if functional[p] {
				continue
			}
			extra = append(extra, p)
		}
		if len(extra) == 0 {
			continue
		}
		job.SerializeAfter = dedupeSorted(append(job.SerializeAfter, extra...))
	}
}

// nearestAncestorWriter walks the functional dependency graph upward from start
// and returns the writer (a key in writers) with the highest topological
// position reachable — the most recent producer whose output start consumes.
// It returns "" when start has no writer ancestor.
func nearestAncestorWriter(start string, writers map[string]bool, deps map[string][]string, pos map[string]int) string {
	best := ""
	bestPos := -1
	seen := make(map[string]bool)
	stack := append([]string(nil), deps[start]...)
	for len(stack) > 0 {
		k := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[k] {
			continue
		}
		seen[k] = true
		if writers[k] && pos[k] > bestPos {
			best = k
			bestPos = pos[k]
		}
		stack = append(stack, deps[k]...)
	}
	return best
}

// resourceConflictKey maps a resource reference to the key two accesses must
// share to conflict. Project-scoped resources are confined to their project;
// workspace-scoped resources collide across the whole workspace.
func resourceConflictKey(projectID string, ref extension.ResourceRef) string {
	if ref.EffectiveScope() == extension.ResourceScopeWorkspace {
		return "ws::" + ref.ID
	}
	return "proj::" + projectID + "::" + ref.ID
}

// functionalDegrees returns the job index, in-plan indegree, and dependents
// adjacency of the plan over functional dependencies (DependsOn) alone.
// Dependencies outside the planned set are ignored. Dependent lists follow the
// jobs slice order.
func functionalDegrees(jobs []*ScheduledJob) (byKey map[string]*ScheduledJob, indegree map[string]int, dependents map[string][]string) {
	byKey = make(map[string]*ScheduledJob, len(jobs))
	for _, job := range jobs {
		byKey[job.Key()] = job
	}

	indegree = make(map[string]int, len(jobs))
	dependents = make(map[string][]string)
	for _, job := range jobs {
		n := 0
		for _, dep := range job.DependsOn {
			if _, ok := byKey[dep]; ok {
				n++
				dependents[dep] = append(dependents[dep], job.Key())
			}
		}
		indegree[job.Key()] = n
	}
	return byKey, indegree, dependents
}

// functionalTopoOrder returns a deterministic topological ordering of the plan
// over functional dependencies alone, as a map of job key → position. The ready
// queue is drained in sorted-key order so the result does not depend on slice
// order or map iteration. ok is false when the functional graph has a cycle.
func functionalTopoOrder(jobs []*ScheduledJob) (map[string]int, bool) {
	_, indegree, dependents := functionalDegrees(jobs)

	var ready []string
	for _, job := range jobs {
		if indegree[job.Key()] == 0 {
			ready = append(ready, job.Key())
		}
	}
	sort.Strings(ready)

	pos := make(map[string]int, len(jobs))
	idx := 0
	for len(ready) > 0 {
		key := ready[0]
		ready = ready[1:]
		pos[key] = idx
		idx++

		var unlocked []string
		for _, dep := range dependents[key] {
			indegree[dep]--
			if indegree[dep] == 0 {
				unlocked = append(unlocked, dep)
			}
		}
		if len(unlocked) > 0 {
			ready = append(ready, unlocked...)
			sort.Strings(ready)
		}
	}

	if idx != len(jobs) {
		return nil, false
	}
	return pos, true
}
