package jobs

import (
	"maps"
	"reflect"
	"testing"

	"go.putnami.dev/tooling/cli/internal/store"
)

// A cleanup collapsed "construct a Scheduler, maybe SetRemoteCache,
// maybe SetSessionEventHandler, then Run" into the single exported RunPlan, and
// unexported the three steps. These tests pin what that collapse must preserve.

// TestRunRequest_ForwardsCommandParamsWithTheirGoTypes is the cache-key
// invariant. A param value's Go TYPE is part of every cache key
// (store.hashParams, and workspace_state.LastBuildParamsHash for the run
// markers), so the params map must reach the scheduler as the caller built it:
// the same map, not a copy, not a re-derivation, not a round trip through a
// typed shape that would turn 3 into "3" and miss every warm entry in the
// store.
func TestRunRequest_ForwardsCommandParamsWithTheirGoTypes(t *testing.T) {
	t.Parallel()
	ws := testWorkspace(t.TempDir())
	// One value per Go type buildCommandParams can produce: --flag=value is a
	// string, --no-flag is a bool false, a bare --flag is a bool true. An int is
	// included because extension-declared numeric params reach the same map.
	params := map[string]any{
		"target":  "linux/amd64",
		"retry":   3,
		"dry-run": true,
		"cache":   false,
	}

	// Snapshot values and types BEFORE the call. Comparing the scheduler's map
	// against the caller's would be vacuous once identity holds (they are the
	// same map), and would not notice a conversion applied in place.
	wantValues := maps.Clone(params)
	wantTypes := make(map[string]reflect.Type, len(params))
	for key, value := range params {
		wantTypes[key] = reflect.TypeOf(value)
	}

	s := RunRequest{Workspace: ws, CommandParams: params, Config: SchedulerConfig{MaxParallel: 1}, Renderer: &mockRenderer{}}.scheduler()

	if !reflect.DeepEqual(s.commandParams, wantValues) {
		t.Fatalf("scheduler command params = %#v, want %#v", s.commandParams, wantValues)
	}
	for key, wantType := range wantTypes {
		got, present := s.commandParams[key]
		if !present {
			t.Errorf("param %q did not reach the scheduler", key)
			continue
		}
		if gotType := reflect.TypeOf(got); gotType != wantType {
			t.Errorf("param %q arrived as %v, want %v — a param's Go type is part of every cache key "+
				"(store.hashParams), so a conversion here invalidates every warm entry", key, gotType, wantType)
		}
	}
	// Identity, not just equality: a copy would silently decouple the two, and
	// the next thing to grow between them is a conversion.
	params["added-after"] = "x"
	if _, propagated := s.commandParams["added-after"]; !propagated {
		t.Error("the scheduler holds a COPY of the command params; it must hold the caller's map, " +
			"so nothing can convert values on the way in")
	}
}

// TestRunRequest_WiresTheRemoteCacheAndSessionSeams proves the two optional
// seams are still wired, and still wired conditionally. Before A6a each adapter
// decided for itself whether to call SetRemoteCache and SetSessionEventHandler,
// and the ones that forgot ran without a session file or a remote cache and
// looked fine. Now the decision is data on the request.
func TestRunRequest_WiresTheRemoteCacheAndSessionSeams(t *testing.T) {
	t.Parallel()
	ws := testWorkspace(t.TempDir())
	cache := store.NewCacheManager(store.NewLocalStore(t.TempDir()))
	remote := &RemoteCache{}
	handler := func(SessionRecord) {}

	wired := RunRequest{Workspace: ws, Cache: cache, Remote: remote, SessionEvents: handler, Renderer: &mockRenderer{}}.scheduler()
	if wired.remote != remote {
		t.Error("RunRequest.Remote did not reach the scheduler: the run would silently be local-only")
	}
	if wired.onSessionEvent == nil {
		t.Error("RunRequest.SessionEvents did not reach the scheduler: the session file and profiler would record nothing")
	}

	// Nil seams stay nil rather than becoming no-op wrappers: the scheduler's hot
	// path tests onSessionEvent for nil, and a non-nil wrapper would allocate a
	// session record per job end on runs that record nothing.
	bare := RunRequest{Workspace: ws, Cache: cache, Renderer: &mockRenderer{}}.scheduler()
	if bare.remote != nil {
		t.Error("a request with no Remote produced a scheduler with one")
	}
	if bare.onSessionEvent != nil {
		t.Error("a request with no SessionEvents produced a non-nil handler")
	}

	// A remote cache without a local one is refused, not half-enabled: remote
	// entries are materialized through the local store, so a nil Cache with a
	// non-nil Remote would be a run that downloads and cannot write.
	noLocal := RunRequest{Workspace: ws, Remote: remote, Renderer: &mockRenderer{}}.scheduler()
	if noLocal.remote != nil {
		t.Error("remote cache enabled with no local cache manager")
	}
}
