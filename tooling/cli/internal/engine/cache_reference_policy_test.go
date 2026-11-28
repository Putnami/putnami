package engine

import (
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/jobs/scheduler"
	"go.putnami.dev/tooling/cli/internal/store"
)

// TestARunRecordsByReferenceOnlyWhereEveryReaderResolves decides, for each
// kind of run, whether its session records a reused task by reference to its
// cache entry: a run whose readers all resolve references does, whichever
// reporters it selects; one that executes a bound portable request, runs
// nested in one, verifies the cache in another store or reuses nothing writes
// every record, and tells the runs it spawns when its sessions travel.
func TestARunRecordsByReferenceOnlyWhereEveryReaderResolves(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "cached-task-by-reference", "a-session-whose-readers-cannot-resolve-writes-the-records")
	t.Setenv(scheduler.RecordSessionsWholeEnv, "")
	cache := store.NewCacheManager(store.NewLocalStore(t.TempDir()))
	for name, tc := range map[string]struct {
		req   *Request
		cache *store.CacheManager
		want  bool
	}{
		"a local run":           {&Request{}, cache, true},
		"a bound portable run":  {&Request{Portable: &PortableExecution{}}, cache, false},
		"a cache verification":  {&Request{CacheVerification: &CacheVerificationRequest{}}, cache, false},
		"no cache (--no-cache)": {&Request{}, nil, false},
	} {
		if got := recordsReusedTasksByReference(tc.req, tc.cache); got != tc.want {
			t.Errorf("%s: records by reference = %v, want %v", name, got, tc.want)
		}
	}
	if !recordsSessionsWhole(&Request{Portable: &PortableExecution{}}) || recordsSessionsWhole(&Request{}) {
		t.Fatal("only a bound portable run tells the runs it spawns that its sessions travel")
	}

	t.Setenv(scheduler.RecordSessionsWholeEnv, "1")
	if recordsReusedTasksByReference(&Request{}, cache) || !recordsSessionsWhole(&Request{}) {
		t.Fatal("a run nested in a bound portable run records by reference, or does not tell the runs it spawns")
	}
}
