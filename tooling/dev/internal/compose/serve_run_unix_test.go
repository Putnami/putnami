//go:build unix

package compose

import (
	"context"
	"slices"
	"syscall"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
)

// A serve run that answers no signal is killed once the teardown's deadline
// passed, and the teardown waits for it no longer than that: the run's serve
// step, in a group of its own, outlives it and keeps its output open, which
// the wait for the killed run would otherwise outlast the deadline for. The
// report names the run, and the lease keeps what survived for the next
// invocation's reap.
func TestClose_WaitsForAKilledServeRunNoLongerThanItsDeadline(t *testing.T) {
	spectest.Proves(t, "tooling/workload-qualification", teardownRequirement,
		"the-teardown-waits-no-longer-than-its-deadline")
	fixture := newCompositionFixture(t, fixtureProject{id: "/app", name: "app", mode: fixtureOutliveParent, cli: fakeCLIIgnoreStop})
	ctx, cancel := context.WithTimeout(context.Background(), hangDetector)
	defer cancel()
	comp, err := up(ctx, Options{Workspace: fixture.ws, Executable: fixture.executable, Target: fixture.projects["/app"], ReadyTimeout: hangDetector}, fixture.deps(nil))
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	groups := fixture.serveGroups(t, "/app")
	// The serve step outlives the killed run; once it is gone too, the run's
	// output closes and the member's end is recorded before the fixture goes.
	t.Cleanup(func() {
		for _, group := range groups {
			_ = syscall.Kill(-group, syscall.SIGKILL)
		}
		select {
		case <-comp.runtimes[0].exited:
		case <-time.After(hangDetector):
			t.Errorf("the serve run's output stayed open after its serve step was killed")
		}
	})

	deadline := time.Second
	teardown, stop := context.WithTimeout(context.Background(), deadline)
	defer stop()
	started := time.Now()
	report := comp.Close(teardown)
	if elapsed := time.Since(started); elapsed >= serveRunWaitDelay {
		t.Errorf("Close took %s with a %s deadline, want it bounded by the deadline", elapsed, deadline)
	}
	if want := "serve run of /app did not stop before the teardown deadline"; report.State != CleanupPartial || !slices.Contains(report.Leftovers, want) {
		t.Errorf("cleanup = %+v, want a partial one naming %q", report, want)
	}
	if lease := fixture.lease(t, comp.ID()); !slices.Contains(lease.PGIDs, groups[0]) {
		t.Errorf("lease groups = %v, want the surviving serve step %d kept for the reap", lease.PGIDs, groups[0])
	}
}
