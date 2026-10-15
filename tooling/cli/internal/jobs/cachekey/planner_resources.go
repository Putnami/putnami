package cachekey

import (
	jobmodel "go.putnami.dev/cli/model/jobs"
)

// FunctionalDegrees returns the job index, in-plan indegree, and dependents
// adjacency of the plan over functional dependencies (DependsOn) alone.
// Dependencies outside the planned set are ignored. Dependent lists follow the
// jobs slice order.
func FunctionalDegrees(jobs []*jobmodel.ScheduledJob) (byKey map[string]*jobmodel.ScheduledJob, indegree map[string]int, dependents map[string][]string) {
	byKey = make(map[string]*jobmodel.ScheduledJob, len(jobs))
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
