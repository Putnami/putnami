package cachekey

import (
	"testing"

	jobmodel "go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// TestNewCacheBypass_ResolvesTheDependentClosure pins the closure rule on its
// own, including the two edges that must NOT extend it.
func TestNewCacheBypass_ResolvesTheDependentClosure(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "scoped-cache-bypass",
		"only-the-named-projects-and-their-dependents-lose-the-cache")

	job := func(projectID, name string) *jobmodel.ScheduledJob {
		return &jobmodel.ScheduledJob{
			Project: &workspace.Project{ID: projectID, Name: projectID, Path: projectID[1:]},
			JobDef:  &extension.JobDefinition{Name: name, Cache: true},
		}
	}
	seed := job("/lib", "build~compile")
	dependent := job("/app", "build~compile")
	dependent.DependsOn = []string{seed.Key()}
	transitive := job("/site", "build~compile")
	transitive.DependsOn = []string{dependent.Key()}
	// SerializeAfter orders execution and never contributes to a cache key, so
	// a job that merely waits for a bypassed one keeps its cache.
	serialized := job("/tool", "build~compile")
	serialized.SerializeAfter = []string{seed.Key()}
	unrelated := job("/docs", "build~compile")
	planned := []*jobmodel.ScheduledJob{seed, dependent, transitive, serialized, unrelated}

	bypass := NewCacheBypass(planned, false, map[string]bool{"/lib": true})
	for _, excluded := range []*jobmodel.ScheduledJob{seed, dependent, transitive} {
		if !bypass.Excludes(excluded) {
			t.Errorf("%s keeps the cache; it is the named project or downstream of it", excluded.Key())
		}
	}
	for _, kept := range []*jobmodel.ScheduledJob{serialized, unrelated} {
		if bypass.Excludes(kept) {
			t.Errorf("%s lost the cache; only the named projects and their cache-key dependents may", kept.Key())
		}
	}

	// An empty selection bypasses nothing, and --no-cache subsumes the flag.
	if NewCacheBypass(planned, false, nil).Excludes(seed) {
		t.Error("an empty --no-cache-projects bypassed a job")
	}
	if !NewCacheBypass(planned, true, nil).Excludes(unrelated) {
		t.Error("--no-cache did not bypass every job")
	}
	// A project the plan does not contain names nothing, and must not make the
	// bypass silently cover something else.
	if NewCacheBypass(planned, false, map[string]bool{"/absent": true}).Excludes(seed) {
		t.Error("a project outside the plan bypassed an unrelated job")
	}
}
