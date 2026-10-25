package compose

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
)

const serveRunRequirement = "each-member-is-an-ordinary-serve-run"

// Each member starts as `putnami serve` for its project alone, with an
// ephemeral port, the display flags of the composition, and its environment
// document on stdin; only the watched target watches. The lease holds both of a
// member's process groups, its serve run's and its serve step's, each with its
// leader's start time.
func TestUp_EachMemberIsAnOrdinaryServeRun(t *testing.T) {
	spectest.Proves(t, "cli/workload-qualification", serveRunRequirement,
		"a-member-starts-as-a-serve-run-of-the-cli-with-its-environment-on-stdin")
	spectest.Proves(t, "cli/workload-qualification", serveRunRequirement,
		"the-lease-records-the-serve-run-and-each-serve-step-with-its-leader-start")
	fixture := newCompositionFixture(t,
		fixtureProject{id: "/provider", name: "provider"},
		fixtureProject{id: "/app", name: "app", runsWith: []string{"provider"}},
	)
	ctx, cancel := context.WithTimeout(context.Background(), hangDetector)
	defer cancel()
	comp, err := up(ctx, Options{
		WorkspaceRoot: fixture.root,
		Target:        fixture.projects["/app"],
		WatchTarget:   true,
		ReadyTimeout:  hangDetector,
		Verbose:       true,
		NoColor:       true,
	}, fixture.deps(nil))
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	defer comp.Close(context.Background())

	want := map[string][]string{
		"/provider": {"serve", "--projects", "/provider", "--port", "0", "--no-watch", "--serve-env", "-", "--output=jsonl", "--verbose", "--no-color"},
		"/app":      {"serve", "--projects", "/app", "--port", "0", "--serve-env", "-", "--output=jsonl", "--verbose", "--no-color"},
	}
	for id, args := range want {
		if got := fixture.argv(t, id); !slices.Equal(got, args) {
			t.Errorf("%s started as %v, want %v", id, got, args)
		}
	}
	// The document reached the serve step: an ephemeral port, and production
	// mode for the dependency only.
	if record := fixture.record(t, "/provider"); record.Port != "0" || record.NodeEnv != "production" {
		t.Errorf("provider env = %+v", record)
	}
	if record := fixture.record(t, "/app"); record.Port != "0" || record.NodeEnv != "" || !slices.Equal(record.ConfigSections, []string{SectionClients}) {
		t.Errorf("target env = %+v", record)
	}

	lease := fixture.lease(t, comp.ID())
	if len(lease.PGIDs) != 4 || len(lease.Groups) != 4 {
		t.Fatalf("lease groups = %v %+v, want the serve run and the serve step of each member", lease.PGIDs, lease.Groups)
	}
	for _, group := range lease.Groups {
		if !slices.Contains(lease.PGIDs, group.PGID) || group.LeaderStart == "" {
			t.Errorf("lease group %+v has no identity among %v", group, lease.PGIDs)
		}
	}
	for _, id := range []string{"/provider", "/app"} {
		for _, group := range fixture.serveGroups(t, id) {
			if !slices.Contains(lease.PGIDs, group) {
				t.Errorf("the serve step group %d of %s is not on the lease %v", group, id, lease.PGIDs)
			}
		}
	}
}

// Close stops the members' serve runs from the last started to the first, so a
// dependant never outlives the provider it calls.
func TestClose_StopsMembersInReverseStartOrder(t *testing.T) {
	spectest.Proves(t, "cli/workload-qualification", serveRunRequirement, "members-stop-in-reverse-start-order")
	fixture := newCompositionFixture(t,
		fixtureProject{id: "/provider", name: "provider"},
		fixtureProject{id: "/middle", name: "middle", runsWith: []string{"provider"}},
		fixtureProject{id: "/app", name: "app", runsWith: []string{"middle"}},
	)
	ctx, cancel := context.WithTimeout(context.Background(), hangDetector)
	defer cancel()
	comp, err := up(ctx, Options{WorkspaceRoot: fixture.root, Target: fixture.projects["/app"], ReadyTimeout: hangDetector}, fixture.deps(nil))
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	if report := comp.Close(context.Background()); report.State != CleanupClean {
		t.Fatalf("cleanup = %+v", report)
	}
	if got := fixture.stopOrder(t); !slices.Equal(got, []string{"/app", "/middle", "/provider"}) {
		t.Errorf("stop order = %v, want /app, /middle, /provider", got)
	}
}

// A serve run that fails before its serve step starts — a usage error, or a
// finite step of the serve pipeline — fails the composition as
// compose.prepare_failed for that member, at once, with the last lines of the
// run's output as detail and never in the message.
func TestUp_AServeRunThatFailsBeforeItsServeStepIsPrepareFailed(t *testing.T) {
	spectest.Proves(t, "cli/workload-qualification", serveRunRequirement,
		"a-failed-preparation-is-prepare-failed-with-the-output-tail")
	for _, tc := range []struct {
		cli, message, detail string
	}{
		{cli: fakeCLIUsage, message: "the serve run exited before the serve step started (exit code 2)", detail: "unknown flag --fixture-refused"},
		{cli: fakeCLIFailPrepare, message: "the serve run's step /app:serve~generate failed before the serve step started (failed, exit code 1)", detail: "generate failed: fixture boom"},
	} {
		t.Run(tc.cli, func(t *testing.T) {
			fixture := newCompositionFixture(t,
				fixtureProject{id: "/provider", name: "provider"},
				fixtureProject{id: "/app", name: "app", runsWith: []string{"provider"}, cli: tc.cli},
			)
			ctx, cancel := context.WithTimeout(context.Background(), hangDetector)
			defer cancel()
			_, err := up(ctx, Options{
				WorkspaceRoot: fixture.root,
				Target:        fixture.projects["/app"],
				// Far longer than the hang detector: only the failure can end the
				// wait in time.
				ReadyTimeout: 10 * hangDetector,
			}, fixture.deps(nil))
			composeErr := composeError(t, err)
			if composeErr.Code != CodePrepareFailed || composeErr.Member != "/app" || composeErr.Phase != PhasePrepare {
				t.Fatalf("error = %v, want compose.prepare_failed for /app in prepare", err)
			}
			if composeErr.Message != tc.message {
				t.Errorf("message = %q, want %q", composeErr.Message, tc.message)
			}
			if !strings.Contains(strings.Join(composeErr.Detail, "\n"), tc.detail) {
				t.Errorf("detail = %q, want the run's output %q", composeErr.Detail, tc.detail)
			}
			if composeErr.Cleanup == nil || composeErr.Cleanup.State != CleanupClean {
				t.Errorf("cleanup = %+v, want a clean teardown", composeErr.Cleanup)
			}
			if dirs := leaseDirs(t, fixture.root); len(dirs) != 0 {
				t.Errorf("a failed start left leases behind: %v", dirs)
			}
		})
	}
}

// A line of the serve run's stdout that is not a session stream record is
// output: it joins the failure detail, and the member is judged by its records.
func TestUp_AStrayLineOfTheServeRunJoinsItsOutput(t *testing.T) {
	fixture := newCompositionFixture(t, fixtureProject{id: "/app", name: "app", mode: fixtureExitEarly, cli: fakeCLIStrayLine})
	ctx, cancel := context.WithTimeout(context.Background(), hangDetector)
	defer cancel()
	_, err := up(ctx, Options{WorkspaceRoot: fixture.root, Target: fixture.projects["/app"], ReadyTimeout: 10 * hangDetector}, fixture.deps(nil))
	composeErr := composeError(t, err)
	if composeErr.Code != CodeMemberExited || composeErr.Message != "the serve step exited before its ready event (failed, exit code 3)" {
		t.Fatalf("error = %v, want the serve step's exit", err)
	}
	if !slices.Contains(composeErr.Detail, "a line that is not a record") {
		t.Errorf("detail = %q, want the stray line", composeErr.Detail)
	}
}

// The ready timeout bounds the serve step, from its process event: a serve run
// whose finite steps outlast it still starts a member that is ready in time.
func TestUp_TheReadyTimeoutCountsFromTheServeStepStart(t *testing.T) {
	spectest.Proves(t, "cli/workload-qualification", serveRunRequirement,
		"the-ready-timeout-counts-from-the-serve-step-start")
	fixture := newCompositionFixture(t, fixtureProject{id: "/app", name: "app", cli: fakeCLISlowPrepare})
	ctx, cancel := context.WithTimeout(context.Background(), hangDetector)
	defer cancel()
	readyTimeout := fakeCLISlowPrepareDelay * 2 / 3
	comp, err := up(ctx, Options{WorkspaceRoot: fixture.root, Target: fixture.projects["/app"], ReadyTimeout: readyTimeout}, fixture.deps(nil))
	if err != nil {
		t.Fatalf("Up with a %s preparation and a %s ready timeout: %v", fakeCLISlowPrepareDelay, readyTimeout, err)
	}
	defer comp.Close(context.Background())
	members := comp.Status().Members
	if len(members) != 1 || members[0].ReadyMs >= fakeCLISlowPrepareDelay.Milliseconds() {
		t.Errorf("members = %+v, want readyMs counted from the serve step's start", members)
	}
}

// A watched target's serve run replaces its serve step on a change. The
// composition follows it: it releases the group of the step that ended,
// records the new one, and forwards to the port the new step announced.
func TestUp_AWatchedTargetFollowsItsServeStepAcrossARestart(t *testing.T) {
	spectest.Proves(t, "cli/workload-qualification", serveRunRequirement,
		"a-watched-target-follows-its-serve-step-across-restarts")
	fixture := newCompositionFixture(t, fixtureProject{id: "/app", name: "app", cli: fakeCLIRestart})
	ctx, cancel := context.WithTimeout(context.Background(), hangDetector)
	defer cancel()
	comp, err := up(ctx, Options{WorkspaceRoot: fixture.root, Target: fixture.projects["/app"], WatchTarget: true, ReadyTimeout: hangDetector}, fixture.deps(nil))
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	defer comp.Close(context.Background())

	endpoint, _ := comp.Endpoint("/app")
	var lease Lease
	var groups []int
	for {
		lease, groups = fixture.lease(t, comp.ID()), fixture.serveGroups(t, "/app")
		if len(groups) == 2 && len(lease.PGIDs) == 2 && slices.Contains(lease.PGIDs, groups[1]) &&
			!slices.Contains(lease.PGIDs, groups[0]) && comp.Status().Members[0].BackendPort != 0 {
			break
		}
		if ctx.Err() != nil {
			t.Fatalf("the composition did not follow the restart: serve groups %v, lease %v, status %+v", groups, lease.PGIDs, comp.Status())
		}
		time.Sleep(50 * time.Millisecond)
	}
	if code, body := get(t, endpoint+"/whoami"); code != http.StatusOK || !strings.HasPrefix(body, "/app host=") {
		t.Errorf("after the restart the proxy answered %d %q, want the new serve step", code, body)
	}
}

// A composition killed without a teardown leaves both groups of each member on
// its lease, its serve run's and its serve step's; the next invocation's reap
// confirms and stops both.
func TestReapOrphans_StopsTheServeRunAndTheServeStepOfAKilledComposition(t *testing.T) {
	spectest.Proves(t, "cli/workload-qualification", serveRunRequirement,
		"a-killed-composition-leaves-both-groups-of-each-member-to-the-reap")
	root := t.TempDir()
	handshake := filepath.Join(t.TempDir(), "composition-id")
	executable, _ := os.Executable()
	holder := exec.Command(executable, "-test.run=^TestCompositionHolderUntilKilled$", "-test.timeout=120s")
	holder.Env = append(os.Environ(), "PUTNAMI_COMPOSE_HOLDER_ROOT="+root, "PUTNAMI_COMPOSE_HOLDER_HANDSHAKE="+handshake)
	if err := holder.Start(); err != nil {
		t.Fatalf("start holder: %v", err)
	}
	defer func() { _ = holder.Process.Kill() }()

	id := waitForFile(t, handshake)
	if err := holder.Process.Kill(); err != nil {
		t.Fatalf("SIGKILL holder: %v", err)
	}
	_ = holder.Wait()

	lease, ok := readLease(filepath.Join(Root(root), id))
	if !ok || len(lease.PGIDs) != 2 || len(lease.Groups) != 2 {
		t.Fatalf("lease of the killed composition = %+v (%v), want the serve run and the serve step", lease, ok)
	}
	for _, pgid := range lease.PGIDs {
		if !processGroupAlive(pgid) {
			t.Fatalf("group %d of the killed composition is already gone", pgid)
		}
	}
	reaped := reapOrphans(root, reapDeps{killDelay: time.Second})
	if len(reaped) != 1 || reaped[0].ID != id || len(reaped[0].Leftovers) != 0 {
		t.Fatalf("reaped = %+v, want %s released", reaped, id)
	}
	deadline := time.Now().Add(hangDetector)
	for _, pgid := range lease.PGIDs {
		for processGroupAlive(pgid) {
			if time.Now().After(deadline) {
				t.Fatalf("group %d survived the reap", pgid)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
}

// TestCompositionHolderUntilKilled is the child half of the reap test: it
// starts a one-member composition and waits to be killed.
func TestCompositionHolderUntilKilled(t *testing.T) {
	root := os.Getenv("PUTNAMI_COMPOSE_HOLDER_ROOT")
	if root == "" {
		t.Skip("child-only helper; driven by TestReapOrphans_StopsTheServeRunAndTheServeStepOfAKilledComposition")
	}
	fixture := newCompositionFixtureAt(t, root, fixtureProject{id: "/app", name: "app"})
	comp, err := up(t.Context(), Options{WorkspaceRoot: root, Target: fixture.projects["/app"], ReadyTimeout: hangDetector}, fixture.deps(nil))
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	if err := os.WriteFile(os.Getenv("PUTNAMI_COMPOSE_HOLDER_HANDSHAKE"), []byte(comp.ID()), 0o600); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	time.Sleep(time.Minute)
}
