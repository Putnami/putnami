package cachekey

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	jobmodel "go.putnami.dev/cli/model/jobs"
	distribution "go.putnami.dev/protocol/distribution"
	extproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/sdk/extension/releaseset"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// CacheEntryPresent reports whether the local store contains the cache entry a
// job would actually consume: the task-owned entry of a job that declares its
// outputs. The scheduler reuses nothing else, so a job without a declaration
// is never reported present, and a format-1 entry at the raw key never reads
// as a hit.
//
// A source-bound task's entry is never reported present here. Its store key
// also binds the project's current source binding (SourceBoundEntryKey), which
// this projection does not compute, so the honest answer is absent: a reader
// then plans an execution, never a hit the lookup could not serve. A reader
// that knows the binding asks CacheEntryPresentAt.
func CacheEntryPresent(cache *store.CacheManager, job *jobmodel.ScheduledJob, hash string) (bool, error) {
	return CacheEntryPresentAt(cache, job, hash, nil)
}

// EntryKeyResolver returns the store key a job's entry is looked up at for its
// cache key: the key itself for a task that is not source-bound, and the key
// bound to its project's current source binding for one that is
// (SourceBoundEntryKey). The second result is false when there is none: the
// task then has no entry to read.
type EntryKeyResolver func(job *jobmodel.ScheduledJob, key string) (string, bool)

// CacheEntryPresentAt is CacheEntryPresent at the store key entryKey resolves,
// which is how a source-bound task's entry is found: at the binding the job
// would look it up at when it starts. A nil entryKey reports a source-bound
// entry absent, as CacheEntryPresent does.
func CacheEntryPresentAt(cache *store.CacheManager, job *jobmodel.ScheduledJob, hash string, entryKey EntryKeyResolver) (bool, error) {
	if cache == nil {
		return false, fmt.Errorf("cache manager is nil")
	}
	if hash == "" || !jobmodel.UsesDeclaredCapture(job) {
		return false, nil
	}
	address := hash
	if TaskIsSourceBound(job) {
		if entryKey == nil {
			return false, nil
		}
		bound, ok := entryKey(job, hash)
		if !ok || bound == "" {
			return false, nil
		}
		address = bound
	}
	entry, err := cache.LookupTaskEntry(address)
	return err == nil && entry != nil, err
}

// CacheKeysRecomputableAtRevision reports whether every cacheable task can be
// keyed from a checked-out revision and this CLI binary alone. An ImpactPlan,
// and the ChangePlan a consumer projects from it, must not carry a key that
// depends on an environment value or an ambient toolchain identity that the
// ChangePlan digest deliberately does not publish.
func CacheKeysRecomputableAtRevision(ws *workspace.Workspace, planned []*jobmodel.ScheduledJob, noCache bool) bool {
	if noCache {
		return false
	}
	for _, job := range planned {
		if job == nil || job.JobDef == nil || !IsCacheEnabled(job, CacheBypass{All: noCache}) {
			continue
		}
		if cacheKeyUsesAmbientInputs(ws, job) {
			return false
		}
	}
	return true
}

func cacheKeyUsesAmbientInputs(ws *workspace.Workspace, job *jobmodel.ScheduledJob) bool {
	if job == nil || job.JobDef == nil {
		return false
	}
	if policy := job.JobDef.TaskCachePolicy; policy != nil && policy.Key != nil &&
		(len(policy.Key.Env) > 0 || len(policy.Key.Runtime) > 0) {
		return true
	}
	return len(ProjectEnvInputs(ws, job)) > 0
}

// PrecomputeOption configures one PrecomputeKeys call.
type PrecomputeOption func(*precomputeConfig)

type precomputeConfig struct {
	stats    *CacheStats
	entryKey EntryKeyResolver
	// revisionOnly reads no local entry (RevisionOnly).
	revisionOnly bool
}

// WithCacheStats records the call's key computations in stats.
func WithCacheStats(stats *CacheStats) PrecomputeOption {
	return func(config *precomputeConfig) { config.stats = stats }
}

// AtEntryKeys makes PrecomputeKeys read a source-bound producer's entry at the
// store key entryKey resolves for it, the key its lookup reads when it starts
// (SourceBoundEntryKey at its project's current binding), and fold the digest
// that entry recorded into its consumers' keys, as a hit there would publish
// it. A nil entryKey reads none.
func AtEntryKeys(entryKey EntryKeyResolver) PrecomputeOption {
	return func(config *precomputeConfig) { config.entryKey = entryKey }
}

// RevisionOnly makes PrecomputeKeys read no store: a consumer's key folds its
// producer's key on the edge of an artifact input, never a digest a local
// entry recorded, so every key is recomputable from the revision alone, on any
// machine. The ChangePlan computes its keys this way.
func RevisionOnly() PrecomputeOption {
	return func(config *precomputeConfig) { config.revisionOnly = true }
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
//
// A consumer's key folds the artifact digest its producer's local entry
// recorded (publishRecordedArtifacts), so it depends on what this machine's
// store holds. A caller whose keys are recorded or compared across machines
// passes RevisionOnly instead.
//
// Without AtEntryKeys, PrecomputeKeys reads no source-bound producer's entry:
// its store key binds its project's current source binding, which this
// function does not compute, so such a producer contributes its key. A caller
// whose precomputed keys must equal the keys execution computes passes the
// resolver of those store keys.
func PrecomputeKeys(
	ws *workspace.Workspace,
	planned []*jobmodel.ScheduledJob,
	commandParams map[string]any,
	versions RunVersions,
	cache *store.CacheManager,
	bypass CacheBypass,
	options ...PrecomputeOption,
) (map[string]string, error) {
	var config precomputeConfig
	for _, option := range options {
		option(&config)
	}
	recorded, entryKey, cacheStats := !config.revisionOnly, config.entryKey, config.stats
	order, err := topoSortJobs(planned)
	if err != nil {
		return nil, err
	}
	PrimePlanInputs(ws, planned, cache)

	keys := make(map[string]string, len(planned))
	// identities holds the identity of each keyless job a cacheable key reads
	// (KeylessIdentities); each key reads its dependencies through a
	// KeylessResolver, as execution does.
	identities := make(map[string]string)
	// artifacts holds the artifact digests a producer's local entry recorded
	// (publishRecordedArtifacts), published as a hit publishes them, apart from
	// the job keys this function returns.
	artifacts := make(map[string]string)
	keyless := KeylessIdentities(planned)
	resolver := KeylessResolver{
		ByKey: JobsByKey(planned),
		Published: func(key string) (string, bool) {
			if value, ok := keys[key]; ok {
				return value, true
			}
			value, ok := artifacts[key]
			return value, ok
		},
		Identity: func(key string) (string, bool) {
			value, ok := identities[key]
			return value, ok
		},
	}
	for _, job := range order {
		// Skip jobs that won't be cached: they contribute no key to the batch
		// and no upstream hash to their dependents — matching execution, where
		// only cacheable results populate the hash map. A keyless job a
		// cacheable key folds contributes its identity to its dependents
		// alone; one whose identity cannot be computed contributes nothing,
		// as at execution.
		if !IsCacheEnabled(job, bypass) {
			if keyless[job.Key()] {
				if identity, err := ComputeJobCacheHash(ws, job, commandParams, versions, cache, resolver.View(job)); err == nil {
					identities[job.Key()] = identity
				}
			}
			continue
		}

		started := time.Now()
		hash, err := ComputeJobCacheHash(ws, job, commandParams, versions, cache, resolver.View(job))
		cacheStats.RecordLocalKeys(started, time.Now())
		if err != nil {
			return nil, fmt.Errorf("precompute cache key for %s: %w", job.Key(), err)
		}
		keys[job.Key()] = hash
		if recorded {
			publishRecordedArtifacts(job, hash, cache, entryKey, artifacts)
		}
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
// package step. Its command is the one extensions declare for that route
// (extension.MemberPackageCommand), never a command name the core knows. The
// ecosystem profile owner supplies validation and registry semantics. A
// project publishing to two ecosystems has two package jobs and two
// fingerprints, which is what makes one project able to yield several
// independently-selected members.
func SelectionFingerprints(
	ws *workspace.Workspace,
	planned []*jobmodel.ScheduledJob,
	commandParams map[string]any,
	cache *store.CacheManager,
	profiles *extproto.ProfileRegistry,
	extensions []*extension.ExtensionDescription,
	members map[string]*workspace.Project,
) (map[string]string, error) {
	if profiles == nil {
		return nil, fmt.Errorf("selection fingerprints require the workspace ecosystem profiles")
	}
	order, err := topoSortJobs(planned)
	if err != nil {
		return nil, err
	}
	PrimePlanInputs(ws, planned, cache)
	keys := make(map[string]string, len(planned))
	for _, job := range order {
		if !IsCacheEnabled(job, CacheBypass{}) {
			continue
		}
		// The version stamp is deliberately absent: a selection key that moved
		// with it would republish every member on every publication.
		hash, err := ComputeJobCacheHashWith(ws, job, commandParams, nil, cache, keys, true)
		if err != nil {
			return nil, fmt.Errorf("compute selection key for %s: %w", job.Key(), err)
		}
		keys[job.Key()] = hash
	}

	fingerprints := make(map[string]string, len(members))
	for _, key := range slices.Sorted(maps.Keys(members)) {
		project := members[key]
		ecosystem, coordinate, _ := strings.Cut(key, "\x00")
		profile, _, known := profiles.Profile(ecosystem)
		if !known {
			return nil, fmt.Errorf("release-set member %s of project %q names ecosystem %q, which no installed extension declares",
				PrintableMemberKey(key), projectID(project), ecosystem)
		}
		metadata, found, err := releaseset.ProjectReleaseMetadata(project.Metadata)
		if err != nil {
			return nil, fmt.Errorf("release-set member %s of project %q has invalid metadata: %w", PrintableMemberKey(key), projectID(project), err)
		}
		publisher, declared := metadata.PackagePublisherFor(distribution.Ecosystem(ecosystem), coordinate)
		owner, published := metadata.PublisherFor(distribution.Ecosystem(ecosystem), coordinate)
		if !found || !declared || !published {
			return nil, fmt.Errorf("release-set member %s of project %q has no declaring extension", PrintableMemberKey(key), projectID(project))
		}
		packageStep, routed := metadata.PackageStepFor(distribution.Ecosystem(ecosystem), coordinate)
		if !routed {
			return nil, fmt.Errorf("release-set member %s of project %q has no declared package step", PrintableMemberKey(key), projectID(project))
		}
		command := extension.MemberPackageCommand(extensions, owner, publisher, profile.Publish, packageStep)
		if command == "" {
			return nil, fmt.Errorf("release-set member %s of project %q has no package command: %q declares no single command that %q's %s runs first with the injectPublishChannels trait and step %q",
				PrintableMemberKey(key), projectID(project), publisher, owner, profile.Publish, packageStep)
		}
		var packageJob *jobmodel.ScheduledJob
		for _, job := range order {
			if job.CommandName() != command || job.Project == nil || job.Extension == nil {
				continue
			}
			if job.Project.ID != projectID(project) || job.Extension.Name != publisher || job.StepID() != packageStep {
				continue
			}
			if packageJob != nil {
				return nil, fmt.Errorf("release-set member %s has more than one %q package step %q in project %q",
					PrintableMemberKey(key), publisher, packageStep, projectID(project))
			}
			packageJob = job
		}
		if packageJob == nil {
			return nil, fmt.Errorf("release-set member %s has no %q package step %q in project %q; its selection fingerprint cannot be computed",
				PrintableMemberKey(key), publisher, packageStep, projectID(project))
		}
		packageKey := keys[packageJob.Key()]
		if packageKey == "" {
			// A package step may have a deterministic task key while remaining
			// intentionally ineligible for cache restore. Compute that key from
			// the already-keyed dependency graph instead of treating cache
			// eligibility as a prerequisite for release-set selection.
			packageKey, err = ComputeJobCacheHashWith(ws, packageJob, commandParams, nil, cache, keys, true)
			if err != nil {
				return nil, fmt.Errorf("compute selection key for %s member %s: %w", packageJob.Key(), PrintableMemberKey(key), err)
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

func PrintableMemberKey(key string) string { return strings.ReplaceAll(key, "\x00", "/") }

// topoSortJobs returns the planned jobs in dependency order: every job appears
// after all of its in-plan dependencies. Dependencies that fall outside the
// planned set are ignored (they never produce a cache key, so they cannot
// contribute an upstream hash). Iteration follows the planned slice order so
// the result is deterministic. An error is returned if the dependency graph
// contains a cycle.
func topoSortJobs(planned []*jobmodel.ScheduledJob) ([]*jobmodel.ScheduledJob, error) {
	byKey, indegree, dependents := FunctionalDegrees(planned)

	queue := make([]*jobmodel.ScheduledJob, 0, len(planned))
	for _, job := range planned {
		if indegree[job.Key()] == 0 {
			queue = append(queue, job)
		}
	}

	order := make([]*jobmodel.ScheduledJob, 0, len(planned))
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
