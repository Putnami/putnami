package cachekey

import (
	jobmodel "go.putnami.dev/cli/model/jobs"
)

// KeylessIdentities names the planned jobs whose cache is off by their own
// contract, so they publish no key, and whose would-be key a job that can use
// the cache folds anyway (CLI ADR 0097): each direct dependency of such a job
// in another project (FoldsKeyless), and each one of those jobs' own
// dependencies that is keyless too.
//
// The key such a job would have is computed as any key is, from its declared
// inputs and the keys of its dependencies, and is handed to its dependents
// alone: it never addresses a lookup, a restore or a store. Without it a task
// that waits for a delegated build, whose tool keeps its own cache, keeps one
// key while that build's sources change, and replays a stored result against
// a rebuilt dependency. A plan where no cacheable job waits for a keyless one
// computes nothing more.
func KeylessIdentities(planned []*jobmodel.ScheduledJob) map[string]bool {
	byKey := make(map[string]*jobmodel.ScheduledJob, len(planned))
	for _, job := range planned {
		if job != nil {
			byKey[job.Key()] = job
		}
	}
	needed := make(map[string]bool)
	var queue []string
	for _, job := range planned {
		if job == nil || !jobmodel.CanUseCache(job) {
			continue
		}
		for _, key := range CacheKeyDependencies(job) {
			if dependency := byKey[key]; dependency != nil && FoldsKeyless(job, dependency) {
				queue = append(queue, key)
			}
		}
	}
	for len(queue) > 0 {
		key := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		dependency := byKey[key]
		if needed[key] || dependency == nil || jobmodel.CanUseCache(dependency) {
			continue
		}
		needed[key] = true
		queue = append(queue, CacheKeyDependencies(dependency)...)
	}
	return needed
}

// FoldsKeyless reports whether job's key folds the identity of dependency when
// dependency's cache is off: always for a keyless job, whose identity is the
// whole chain's, and for a cacheable job only when dependency belongs to
// another project. A job's key already reads its own project's files, and a
// keyless job of the same project that rewrites them, such as a fixer, would
// move its dependent's key between a run and the next.
func FoldsKeyless(job, dependency *jobmodel.ScheduledJob) bool {
	if job == nil || dependency == nil {
		return false
	}
	if !jobmodel.CanUseCache(job) || job.Project == nil || dependency.Project == nil {
		return true
	}
	return job.Project.ID != dependency.Project.ID
}

// keylessView is the map a key of job reads its dependencies' keys from: the
// keys published, and the identity of each keyless dependency job folds.
func keylessView(job *jobmodel.ScheduledJob, keys, identities map[string]string, byKey map[string]*jobmodel.ScheduledJob) map[string]string {
	view := make(map[string]string)
	for _, key := range CacheKeyDependencies(job) {
		if value, ok := keys[key]; ok {
			view[key] = value
		} else if identity, ok := identities[key]; ok && FoldsKeyless(job, byKey[key]) {
			view[key] = identity
		}
	}
	return view
}
