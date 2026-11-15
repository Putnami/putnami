package cachekey

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"

	jobmodel "go.putnami.dev/cli/model/jobs"
)

// A job whose cache is off by its own contract publishes no key. A cacheable
// job that waits for one still folds what that job stands for (CLI ADR 0097),
// or a test that waits for a build a monorepo tool runs keeps one key while
// the build's sources change, and replays a stored result against a rebuilt
// dependency:
//
//   - a keyless dependency in another project contributes its identity: the
//     key it would have, computed from its declared inputs and from its own
//     dependencies, every keyless one of them folded the same way;
//   - a keyless dependency in the job's own project contributes the digest of
//     what reaches it from other projects (KeylessResolver.through): the keys
//     its dependencies publish and the identities of its keyless dependencies
//     in other projects, through its keyless dependencies of the same
//     project. Its own project's files are left out: the job's key reads them
//     already, and a keyless fixer that rewrites them would move the key
//     between a run and the next.
//
// An identity never addresses a lookup, a restore or a store. A plan where no
// cacheable job waits for a keyless one computes none.

// KeylessResolver answers what a job's key reads of its dependencies: the
// keys they publish, and for each keyless one, what it stands for.
type KeylessResolver struct {
	// ByKey indexes the plan by job key.
	ByKey map[string]*jobmodel.ScheduledJob
	// Published answers the key a dependency published, when it did.
	Published func(key string) (string, bool)
	// Identity answers the identity of the keyless job at key, when it can
	// be computed.
	Identity func(key string) (string, bool)
}

// View is the map job's key reads its dependencies' keys from
// (ComputeJobCacheHash): each published key of CacheKeyDependencies, then for
// each keyless dependency what it stands for. A keyless job folds the
// identity of each keyless dependency, whatever its project, so its own
// identity covers the whole chain.
func (r KeylessResolver) View(job *jobmodel.ScheduledJob) map[string]string {
	view := make(map[string]string)
	for _, key := range CacheKeyDependencies(job) {
		if value, ok := r.Published(key); ok {
			view[key] = value
			continue
		}
		dependency := r.ByKey[key]
		if dependency == nil || jobmodel.CanUseCache(dependency) {
			continue
		}
		if !jobmodel.CanUseCache(job) || !sameProject(job, dependency) {
			if identity, ok := r.Identity(key); ok {
				view[key] = identity
			}
			continue
		}
		if digest := r.through(job, dependency, map[string]bool{}); digest != "" {
			view[key] = digest
		}
	}
	return view
}

// through digests what reaches the keyless dependency of job's own project
// from other projects: each key its dependencies publish, and each keyless
// dependency's identity in another project, or what reaches a keyless one of
// the same project in turn. Empty when nothing does.
func (r KeylessResolver) through(job, dependency *jobmodel.ScheduledJob, seen map[string]bool) string {
	var parts []string
	r.walkThrough(job, dependency, seen, &parts)
	if len(parts) == 0 {
		return ""
	}
	slices.Sort(parts)
	sum := sha256.Sum256([]byte("keyless-through-v1\n" + strings.Join(parts, "\n")))
	return hex.EncodeToString(sum[:])
}

func (r KeylessResolver) walkThrough(job, dependency *jobmodel.ScheduledJob, seen map[string]bool, parts *[]string) {
	if seen[dependency.Key()] {
		return
	}
	seen[dependency.Key()] = true
	for _, key := range CacheKeyDependencies(dependency) {
		if value, ok := r.Published(key); ok {
			*parts = append(*parts, key+"="+value)
			continue
		}
		upstream := r.ByKey[key]
		if upstream == nil || jobmodel.CanUseCache(upstream) {
			continue
		}
		if sameProject(job, upstream) {
			r.walkThrough(job, upstream, seen, parts)
			continue
		}
		if identity, ok := r.Identity(key); ok {
			*parts = append(*parts, key+"="+identity)
		}
	}
}

// KeylessIdentities names the keyless jobs whose identity some cacheable
// job's key reads through a KeylessResolver: each keyless dependency of such
// a job in another project, each one in another project reached through its
// keyless dependencies of its own project, and every keyless dependency of
// those, whose identities theirs fold.
func KeylessIdentities(planned []*jobmodel.ScheduledJob) map[string]bool {
	byKey := JobsByKey(planned)
	needed := make(map[string]bool)
	var mark func(key string)
	mark = func(key string) {
		job := byKey[key]
		if needed[key] || job == nil || jobmodel.CanUseCache(job) {
			return
		}
		needed[key] = true
		for _, dependency := range CacheKeyDependencies(job) {
			mark(dependency)
		}
	}
	for _, job := range planned {
		if job == nil || !jobmodel.CanUseCache(job) {
			continue
		}
		seen := make(map[string]bool)
		var visit func(from *jobmodel.ScheduledJob)
		visit = func(from *jobmodel.ScheduledJob) {
			for _, key := range CacheKeyDependencies(from) {
				dependency := byKey[key]
				if dependency == nil || jobmodel.CanUseCache(dependency) || seen[key] {
					continue
				}
				seen[key] = true
				if sameProject(job, dependency) {
					visit(dependency)
					continue
				}
				mark(key)
			}
		}
		visit(job)
	}
	return needed
}

// sameProject reports whether two jobs belong to one project.
func sameProject(a, b *jobmodel.ScheduledJob) bool {
	return a.Project != nil && b.Project != nil && a.Project.ID == b.Project.ID
}
