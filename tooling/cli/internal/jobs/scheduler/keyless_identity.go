package scheduler

import (
	"sync"

	"go.putnami.dev/tooling/cli/internal/jobs/cachekey"
)

// dependencyKeys answers the map job's key reads its dependencies' keys from:
// the keys this run published into hashes, read under mu, and for each
// keyless dependency what it stands for (cachekey.KeylessResolver). A keyless
// identity is the one memoized when its job started (memoizeKeylessIdentity),
// else, for a job pruned or not started yet, computed when a dependent first
// asks, as PrecomputeKeys computes it before anything runs.
func (s *Scheduler) dependencyKeys(job *ScheduledJob, mu *sync.Mutex, hashes map[string]string) map[string]string {
	return s.keylessResolver(mu, hashes).View(job)
}

func (s *Scheduler) keylessResolver(mu *sync.Mutex, hashes map[string]string) cachekey.KeylessResolver {
	return cachekey.KeylessResolver{
		ByKey: s.byKey,
		Published: func(key string) (string, bool) {
			mu.Lock()
			defer mu.Unlock()
			value, ok := hashes[key]
			return value, ok
		},
		Identity: func(key string) (string, bool) {
			if !s.keyless[key] {
				return "", false
			}
			return s.keylessIdentity(key, mu, hashes)
		},
	}
}

// memoizeKeylessIdentity computes, as a keyless job a cacheable key reads
// starts, its identity from the bytes it reads before it writes any output,
// the state PrecomputeKeys keyed.
func (s *Scheduler) memoizeKeylessIdentity(job *ScheduledJob, mu *sync.Mutex, hashes map[string]string) {
	if s.keyless[job.Key()] {
		s.keylessIdentity(job.Key(), mu, hashes)
	}
}

// keylessIdentity is the identity of the keyless job at key, computed once
// per run.
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
	if memo, ok := s.keylessMemo[key]; ok {
		identity = memo
	} else {
		s.keylessMemo[key] = identity
	}
	s.keylessMu.Unlock()
	return identity, identity != ""
}
