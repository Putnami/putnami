package jobs

import "slices"

// WithoutPublicationNodes is planned as the legacy attachment plans the same
// run: without the open node and the upload nodes attachPublication adds, and
// with each edge to them rewritten. An edge to an upload node names the
// publication job that node uploads for; an edge to the open node goes. The
// rewritten edges are deduplicated and sorted, as the planner sorts them.
//
// Those nodes run engine code only, never repository code, so a bound
// request's expected plan, which its submitter computes without knowing
// whether the provider echoes publication-v1, names none of them. The engine
// compares the expected plan with this projection, and one submitted plan
// validates both paths. Every other check, and the scheduler, reads planned
// itself: the projection copies each job whose edges it rewrites and never
// changes planned.
//
// A run that does not publish through publication-v1 returns planned.
func (run *ReleaseSetRun) WithoutPublicationNodes(planned []*ScheduledJob) []*ScheduledJob {
	if !run.Publication() || run.uploads == nil {
		return planned
	}
	derived := make(map[string]string, len(run.uploads)+1)
	derived[publicationOpenKey] = ""
	for upload, publish := range run.uploads {
		derived[upload] = publish
	}
	projected := make([]*ScheduledJob, 0, len(planned))
	for _, job := range planned {
		if job == nil {
			projected = append(projected, job)
			continue
		}
		if _, attached := derived[job.Key()]; attached {
			continue
		}
		dependsOn, rewroteDependsOn := withoutDerivedEdges(job.DependsOn, derived)
		serializeAfter, rewroteSerializeAfter := withoutDerivedEdges(job.SerializeAfter, derived)
		if !rewroteDependsOn && !rewroteSerializeAfter {
			projected = append(projected, job)
			continue
		}
		copied := *job
		copied.DependsOn, copied.SerializeAfter = dependsOn, serializeAfter
		projected = append(projected, &copied)
	}
	return projected
}

// withoutDerivedEdges rewrites the edges that name a node of derived to the
// key derived maps it to, drops those it maps to "", and reports whether any
// edge named one. It returns edges unchanged when none does.
func withoutDerivedEdges(edges []string, derived map[string]string) ([]string, bool) {
	if !slices.ContainsFunc(edges, func(key string) bool { _, ok := derived[key]; return ok }) {
		return edges, false
	}
	rewritten := make([]string, 0, len(edges))
	for _, key := range edges {
		target, ok := derived[key]
		switch {
		case !ok:
			rewritten = append(rewritten, key)
		case target != "":
			rewritten = append(rewritten, target)
		}
	}
	return dedupeSorted(rewritten), true
}
