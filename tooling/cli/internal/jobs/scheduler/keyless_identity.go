package scheduler

import (
	"sync"

	"go.putnami.dev/tooling/cli/internal/jobs/cachekey"
)

// dependencyKeys copies, under mu, the keys of job's cache-key dependencies
// (cachekey.CacheKeyDependencies) that this run published into hashes, then
// adds the identity of each one whose cache is off by contract and whose key
// job folds (cachekey.KeylessIdentities, cachekey.FoldsKeyless). That identity is
// computed when a dependent first asks for it, whether the keyless job ran,
// was pruned or has not finished, as PrecomputeKeys computes it, and is never
// published as the job's own key: it addresses no lookup, restore or store. An
// identity that cannot be computed is left out, as PrecomputeKeys leaves it.
func (s *Scheduler) dependencyKeys(job *ScheduledJob, mu *sync.Mutex, hashes map[string]string) map[string]string {
	dependencies := cachekey.CacheKeyDependencies(job)
	keys := make(map[string]string, len(dependencies))
	var keyless []string
	mu.Lock()
	for _, depKey := range dependencies {
		if value, ok := hashes[depKey]; ok {
			keys[depKey] = value
		} else if s.keyless[depKey] && cachekey.FoldsKeyless(job, s.byKey[depKey]) {
			keyless = append(keyless, depKey)
		}
	}
	mu.Unlock()
	for _, depKey := range keyless {
		if identity, ok := s.keylessIdentity(depKey, mu, hashes); ok {
			keys[depKey] = identity
		}
	}
	return keys
}

// keylessIdentity is the identity of the keyless job at key, computed once
// per run from its own dependencies' keys.
func (s *Scheduler) keylessIdentity(key string, mu *sync.Mutex, hashes map[string]string) (string, bool) {
	s.keylessMu.Lock()
	identity, ok := s.keylessMemo[key]
	s.keylessMu.Unlock()
	if ok {
		return identity, identity != ""
	}
	job := s.byKey[key]
	if job == nil || s.cache == nil {
		return "", false
	}
	identity, err := cachekey.ComputeJobCacheHash(s.ws, job, s.commandParams, s.cfg.VersionInfo, s.cache, s.dependencyKeys(job, mu, hashes))
	if err != nil {
		identity = ""
	}
	s.keylessMu.Lock()
	s.keylessMemo[key] = identity
	s.keylessMu.Unlock()
	return identity, identity != ""
}
