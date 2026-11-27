package scheduler

import (
	"testing"

	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs/cachekey"
	"go.putnami.dev/tooling/cli/internal/jobs/planner"
	"go.putnami.dev/tooling/cli/internal/store"
)

// A plan none of whose relations may prune under the command params
// precomputes no key: the lookups could not change the outcome, and the
// remote negotiation computes its keys itself (tooling/cli ADR 0113 §8). A
// relation that may prune still has its keys precomputed.
func TestPruneComputesNoKeyWhenNoRelationMayPrune(t *testing.T) {
	cases := []struct {
		name       string
		pruneIf    string
		infraDown  string // "" leaves the param out
		precompute bool
	}{
		{"no predicate", "", "", true},
		{"a predicate that holds", "!params.infra-down", "false", true},
		{"a predicate that is false", "!params.infra-down", "true", false},
		{"a predicate on a missing param (invalid)", "!params.infra-down", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fixture := newInvocationFixture(t, provisionBody, consumeBody)
			fixture.finalizer.Step.Finalizes.PruneIf = c.pruneIf
			planner.ResolveInvocationRelations(fixture.planned)
			rt := newInvocationRuntime(fixture.ws, fixture.planned)
			if rt == nil || len(rt.relations) != 1 {
				t.Fatalf("the fixture plans %v; want one finalizes relation", rt)
			}
			params := extension.ParamMap{}
			if c.infraDown != "" {
				params["infra-down"] = c.infraDown == "true"
			}
			cache := store.NewCacheManager(store.NewLocalStore(t.TempDir()))
			stats := &cachekey.CacheStats{}
			keys := rt.prune(fixture.ws, fixture.planned, params, nil, cache, cachekey.CacheBypass{}, stats)
			if got := keys != nil; got != c.precompute {
				t.Fatalf("precomputed keys = %v, want %v", got, c.precompute)
			}
		})
	}
}
