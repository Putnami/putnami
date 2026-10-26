package planner

import (
	"sort"

	jobmodel "go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs/cachekey"
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
// observes that writer's output. Blocks are then serialized end-to-end: a
// writer waits for every earlier writer AND every reader of an earlier block
// before overwriting, through a direct edge to each, so removing any planned
// job later never leaves two of the jobs that remain unordered. Readers in the
// same block share no edges, so they still run in parallel. Functional
// dependencies (DependsOn) are never modified.
//
// The edges of one resource are not all consistent with one topological order:
// a reader joins the block of its writer ancestor, not of its position, so a
// later writer can wait for a reader that comes after it in that order. Each
// resource is ordered on its own, so two resources can order two jobs opposite
// ways and close a cycle. ValidatePlanDAG rejects such a plan.
//
// leaders maps each shared-execution follower to its leader. A follower writes
// nothing to the tree: it captures what its leader wrote. It joins its leader's
// place in the write order, and the readers of that block wait for it.
func serializeWriteResources(jobs []*jobmodel.ScheduledJob, leaders map[string]string) {
	pos, ok := functionalTopoOrder(jobs, leaders)
	if !ok {
		// The functional graph has a cycle; ValidatePlanDAG reports it. Adding
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
		// A follower observes what its leader wrote, so a reader that derives
		// from the follower derives from the leader's write.
		if leader := leaders[job.Key()]; inPlan[leader] {
			functionalDeps[job.Key()] = append(functionalDeps[job.Key()], leader)
		}
	}

	type group struct {
		writers   map[string]bool
		followers map[string][]string
		readers   []string
	}
	groups := make(map[string]*group)
	ensure := func(ck string) *group {
		g := groups[ck]
		if g == nil {
			g = &group{writers: make(map[string]bool), followers: make(map[string][]string)}
			groups[ck] = g
		}
		return g
	}

	// Group every resource access by its conflict key. A job that both reads and
	// writes a resource is treated purely as a writer (writers are exclusive).
	// A follower writes nothing to the tree: it adopts its leader's result, and
	// it takes its leader's place in the write order instead of a place of its
	// own. Its leader is the same task over the same project, so it writes the
	// same resources.
	for _, job := range jobs {
		key := job.Key()
		written := make(map[string]bool)
		for _, ref := range job.JobDef.Writes {
			ck := ResourceConflictKey(job.Project.ID, ref)
			if written[ck] {
				continue
			}
			written[ck] = true
			if leader, ok := leaders[key]; ok {
				ensure(ck).followers[leader] = append(ensure(ck).followers[leader], key)
				continue
			}
			ensure(ck).writers[key] = true
		}
		for _, ref := range job.JobDef.Reads {
			ck := ResourceConflictKey(job.Project.ID, ref)
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

		// Serialize the blocks. Each writer waits directly for every access
		// before it: the epoch-zero readers, every earlier writer and the readers
		// of every earlier block. The edges the previous writer already implies
		// are kept on purpose. A pass that removes planned jobs after this one,
		// such as a release-set scope that drops the package steps it does not
		// publish, strips the edges to the jobs it removes, and the writers it
		// keeps must stay ordered without the writer in between. A writer's
		// followers take its place and wait for the same accesses.
		preceding := append([]string(nil), epochZeroReaders...)
		for _, w := range writerKeys {
			unit := append([]string{w}, g.followers[w]...)
			for _, member := range unit {
				for _, p := range preceding {
					addEdge(p, member)
				}
			}
			waitForFollowers(g.followers[w], blockReaders[w], functionalDeps, addEdge)
			preceding = append(preceding, unit...)
			preceding = append(preceding, blockReaders[w]...)
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
		job.SerializeAfter = jobmodel.DedupeSorted(append(job.SerializeAfter, extra...))
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

// waitForFollowers orders every reader of a block after the followers of the
// block's writer, so a follower captures the tree as its leader left it, before
// a reader that also writes into it runs. A reader the follower derives from
// keeps no such edge: it would close a cycle.
func waitForFollowers(followers, readers []string, deps map[string][]string, addEdge func(pred, succ string)) {
	for _, follower := range followers {
		for _, r := range readers {
			if !functionallyReaches(follower, r, deps) {
				addEdge(follower, r)
			}
		}
	}
}

// functionallyReaches reports whether target is start or one of its functional
// ancestors.
func functionallyReaches(start, target string, deps map[string][]string) bool {
	seen := make(map[string]bool)
	stack := []string{start}
	for len(stack) > 0 {
		k := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if k == target {
			return true
		}
		if seen[k] {
			continue
		}
		seen[k] = true
		stack = append(stack, deps[k]...)
	}
	return false
}

// ResourceConflictKey maps a resource reference to the key two accesses must
// share to conflict. Project-scoped resources are confined to their project;
// workspace-scoped resources collide across the whole workspace.
func ResourceConflictKey(projectID string, ref extension.ResourceRef) string {
	if ref.EffectiveScope() == extension.ResourceScopeWorkspace {
		return "ws::" + ref.ID
	}
	return "proj::" + projectID + "::" + ref.ID
}

// functionalTopoOrder returns a deterministic topological ordering of the plan
// over functional dependencies alone, as a map of job key → position. The ready
// queue is drained in sorted-key order so the result does not depend on slice
// order or map iteration. ok is false when the functional graph has a cycle.
func functionalTopoOrder(jobs []*jobmodel.ScheduledJob, leaders map[string]string) (map[string]int, bool) {
	byKey, indegree, dependents := cachekey.FunctionalDegrees(jobs)
	for follower, leader := range leaders {
		if byKey[follower] == nil || byKey[leader] == nil {
			continue
		}
		indegree[follower]++
		dependents[leader] = append(dependents[leader], follower)
	}

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
