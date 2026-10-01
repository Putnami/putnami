package jobs

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	distribution "go.putnami.dev/protocol/distribution"
	extproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/sdk/extension/releaseset"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// CacheEntryPresent reports whether the local store contains the cache entry a
// job would actually consume. Declared-capture entries and legacy entries use
// different, format-qualified addresses; keeping this projection here prevents
// plan emitters from treating a legacy blob as a hit for a declared task.
func CacheEntryPresent(cache *store.CacheManager, job *ScheduledJob, hash string) (bool, error) {
	if cache == nil {
		return false, fmt.Errorf("cache manager is nil")
	}
	if hash == "" {
		return false, nil
	}
	if usesDeclaredCapture(job) {
		entry, err := cache.LookupTaskEntry(hash)
		return err == nil && entry != nil, err
	}
	entry, err := cache.Lookup(hash)
	return err == nil && entry != nil, err
}

// CacheKeysRecomputableAtRevision reports whether every cacheable task can be
// keyed from a checked-out revision and this CLI binary alone. A ChangePlan
// must not carry a key that depends on an environment value or an ambient
// toolchain identity that its digest deliberately does not publish.
func CacheKeysRecomputableAtRevision(ws *workspace.Workspace, planned []*ScheduledJob, noCache bool) bool {
	if noCache {
		return false
	}
	for _, job := range planned {
		if job == nil || job.JobDef == nil || !isCacheEnabled(job, CacheBypass{All: noCache}) {
			continue
		}
		if cacheKeyUsesAmbientInputs(ws, job) {
			return false
		}
	}
	return true
}

func cacheKeyUsesAmbientInputs(ws *workspace.Workspace, job *ScheduledJob) bool {
	if job == nil || job.JobDef == nil {
		return false
	}
	if policy := job.JobDef.TaskCachePolicy; policy != nil && policy.Key != nil &&
		(len(policy.Key.Env) > 0 || len(policy.Key.Runtime) > 0) {
		return true
	}
	return len(projectEnvInputs(ws, job)) > 0
}

// PrecomputeKeys computes the cache key for every cacheable job in the plan in
// a single topological pass, before any job executes.
//
// Cache keys are input-derived and deterministic, and the upstream hash mixed
// into a key is the upstream task's cache key (not its output bytes), so the
// entire key graph is computable in dependency order ahead of execution. The
// returned map is keyed by job key and holds an entry for each job that has
// caching enabled. It is the input to a single batch cache negotiation: the
// whole build's key set is known up front, so hit/miss can be resolved in one
// round trip instead of lazily, key by key, during execution.
//
// The CacheManager's memoized file-hash cache is reused, so files hashed here
// are not re-read when the same keys are recomputed during execution.
func PrecomputeKeys(
	ws *workspace.Workspace,
	planned []*ScheduledJob,
	commandParams map[string]any,
	versions RunVersions,
	cache *store.CacheManager,
	bypass CacheBypass,
	stats ...*CacheStats,
) (map[string]string, error) {
	var cacheStats *CacheStats
	if len(stats) > 0 {
		cacheStats = stats[0]
	}
	order, err := topoSortJobs(planned)
	if err != nil {
		return nil, err
	}

	keys := make(map[string]string, len(planned))
	for _, job := range order {
		// Skip jobs that won't be cached: they contribute no key to the batch
		// and no upstream hash to their dependents — matching execution, where
		// only cacheable results populate the hash map.
		if !isCacheEnabled(job, bypass) {
			continue
		}

		started := time.Now()
		hash, err := computeJobCacheHash(ws, job, commandParams, versions, cache, keys)
		cacheStats.recordLocalKeys(started, time.Now())
		if err != nil {
			return nil, fmt.Errorf("precompute cache key for %s: %w", job.Key(), err)
		}
		keys[job.Key()] = hash
	}

	return keys, nil
}

// SelectionFingerprints computes, for every release-set member, the value the
// member records so a later publication can decide whether to republish it
// (D13).
//
// The value is the execution key of the member's PACKAGE task minus the
// embedded version — the packaging recipe's identity rather than the tree's.
// That is why it is computed here and not from a tree hash: a change to the
// packager, to the toolchain, to a task contract, or to any upstream task moves
// this key exactly as a source edit does, and none of those move a tree hash.
//
// The cacheable graph is keyed in one topological pass with its own map, then
// each declared package step is keyed even when it is deliberately not cache
// restorable (for example because it also updates a shared metadata index).
// Cache eligibility and deterministic identity are separate questions. Every
// upstream hash mixed into a member's key is itself a selection key: a member
// whose dependency's recipe changed sees it, and neither map contaminates the
// execution-key map.
//
// The package job of a member is the one named by PackagePublisherFor. It
// defaults to the extension metadata block that declared the member, while an
// explicit route lets a publisher consume another installed extension's real
// package step. The ecosystem profile owner supplies validation and registry
// semantics. A project publishing to two ecosystems has two package jobs and
// two fingerprints, which is what makes one project able to yield several
// independently-selected members.
func SelectionFingerprints(
	ws *workspace.Workspace,
	planned []*ScheduledJob,
	commandParams map[string]any,
	cache *store.CacheManager,
	profiles *extproto.ProfileRegistry,
	members map[string]*workspace.Project,
) (map[string]string, error) {
	if profiles == nil {
		return nil, fmt.Errorf("selection fingerprints require the workspace ecosystem profiles")
	}
	order, err := topoSortJobs(planned)
	if err != nil {
		return nil, err
	}
	keys := make(map[string]string, len(planned))
	for _, job := range order {
		if !isCacheEnabled(job, CacheBypass{}) {
			continue
		}
		// The version stamp is deliberately absent: a selection key that moved
		// with it would republish every member on every publication.
		hash, err := computeJobCacheHashWith(ws, job, commandParams, nil, cache, keys, true)
		if err != nil {
			return nil, fmt.Errorf("compute selection key for %s: %w", job.Key(), err)
		}
		keys[job.Key()] = hash
	}

	fingerprints := make(map[string]string, len(members))
	for _, key := range slices.Sorted(maps.Keys(members)) {
		project := members[key]
		ecosystem, coordinate, _ := strings.Cut(key, "\x00")
		_, _, known := profiles.Profile(ecosystem)
		if !known {
			return nil, fmt.Errorf("release-set member %s of project %q names ecosystem %q, which no installed extension declares",
				printableMemberKey(key), projectID(project), ecosystem)
		}
		metadata, found, err := releaseset.ProjectReleaseMetadata(project.Metadata)
		if err != nil {
			return nil, fmt.Errorf("release-set member %s of project %q has invalid metadata: %w", printableMemberKey(key), projectID(project), err)
		}
		publisher, declared := metadata.PackagePublisherFor(distribution.Ecosystem(ecosystem), coordinate)
		if !found || !declared {
			return nil, fmt.Errorf("release-set member %s of project %q has no declaring extension", printableMemberKey(key), projectID(project))
		}
		packageStep, routed := metadata.PackageStepFor(distribution.Ecosystem(ecosystem), coordinate)
		if !routed {
			return nil, fmt.Errorf("release-set member %s of project %q has no declared package step", printableMemberKey(key), projectID(project))
		}
		var packageJob *ScheduledJob
		for _, job := range order {
			if job.CommandName() != "package" || job.Project == nil || job.Extension == nil {
				continue
			}
			if job.Project.ID != projectID(project) || job.Extension.Name != publisher || job.StepID() != packageStep {
				continue
			}
			if packageJob != nil {
				return nil, fmt.Errorf("release-set member %s has more than one %q package step %q in project %q",
					printableMemberKey(key), publisher, packageStep, projectID(project))
			}
			packageJob = job
		}
		if packageJob == nil {
			return nil, fmt.Errorf("release-set member %s has no %q package step %q in project %q; its selection fingerprint cannot be computed",
				printableMemberKey(key), publisher, packageStep, projectID(project))
		}
		packageKey := keys[packageJob.Key()]
		if packageKey == "" {
			// A package step may have a deterministic task key while remaining
			// intentionally ineligible for cache restore. Compute that key from
			// the already-keyed dependency graph instead of treating cache
			// eligibility as a prerequisite for release-set selection.
			packageKey, err = computeJobCacheHashWith(ws, packageJob, commandParams, nil, cache, keys, true)
			if err != nil {
				return nil, fmt.Errorf("compute selection key for %s member %s: %w", packageJob.Key(), printableMemberKey(key), err)
			}
			keys[packageJob.Key()] = packageKey
		}
		fingerprints[key] = releaseset.SelectionFingerprint(packageKey)
	}
	return fingerprints, nil
}

func projectID(project *workspace.Project) string {
	if project == nil {
		return ""
	}
	return project.ID
}

func printableMemberKey(key string) string { return strings.ReplaceAll(key, "\x00", "/") }

// topoSortJobs returns the planned jobs in dependency order: every job appears
// after all of its in-plan dependencies. Dependencies that fall outside the
// planned set are ignored (they never produce a cache key, so they cannot
// contribute an upstream hash). Iteration follows the planned slice order so
// the result is deterministic. An error is returned if the dependency graph
// contains a cycle.
func topoSortJobs(planned []*ScheduledJob) ([]*ScheduledJob, error) {
	byKey, indegree, dependents := functionalDegrees(planned)

	queue := make([]*ScheduledJob, 0, len(planned))
	for _, job := range planned {
		if indegree[job.Key()] == 0 {
			queue = append(queue, job)
		}
	}

	order := make([]*ScheduledJob, 0, len(planned))
	for len(queue) > 0 {
		job := queue[0]
		queue = queue[1:]
		order = append(order, job)

		for _, depKey := range dependents[job.Key()] {
			indegree[depKey]--
			if indegree[depKey] == 0 {
				queue = append(queue, byKey[depKey])
			}
		}
	}

	if len(order) != len(planned) {
		return nil, fmt.Errorf("dependency cycle detected: %d of %d jobs could not be ordered", len(planned)-len(order), len(planned))
	}

	return order, nil
}
