package compose

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/dbtestenv"
	"go.putnami.dev/sdk/extension/proctree"
	"go.putnami.dev/tooling/cli/internal/flock"
)

// deadPID returns the pid of a process that has exited and been reaped. The
// process is this test binary running no test, which every platform can start.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	if err := cmd.Run(); err != nil {
		t.Fatalf("run a child that exits: %v", err)
	}
	return cmd.Process.Pid
}

// startGroup starts the fixture workload in mode as the leader of its own
// process group and returns the group id and a channel closed once the leader
// exited and was reaped. Reaping it promptly matters: a zombie still counts as
// a member of its group, which would make a killed group look alive.
func startGroup(t *testing.T, mode string) (int, <-chan struct{}) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	cmd := exec.Command(executable)
	cmd.Env = append(os.Environ(), fixtureModeEnv+"="+mode, fixtureMemberEnv+"=/orphan")
	tree := proctree.New(cmd)
	if err := tree.Start(); err != nil {
		t.Fatalf("start group: %v", err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	t.Cleanup(func() {
		_ = tree.Kill()
		<-done
		_ = tree.Close()
	})
	return tree.ID(), done
}

// writeOrphanLease publishes a lease for a composition whose owner is gone and
// returns its id. Unset fields take orphan defaults.
func writeOrphanLease(t *testing.T, root string, lease Lease) string {
	t.Helper()
	id, err := newCompositionID()
	if err != nil {
		t.Fatal(err)
	}
	lease.Version, lease.ID = LeaseVersion, id
	if lease.PID == 0 {
		lease.PID = deadPID(t)
	}
	if lease.CreatedAt == "" {
		lease.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	}
	dir := filepath.Join(Root(root), id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeLease(dir, lease); err != nil {
		t.Fatal(err)
	}
	return id
}

func waitExited(t *testing.T, pgid int, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(hangDetector):
		t.Fatalf("process group %d is still running", pgid)
	}
}

func TestReapOrphans_SparesLiveOwner_KillsDeadOwnerGroup(t *testing.T) {
	spectest.Proves(t, "cli/workload-qualification", "ungraceful-death-is-reaped-by-the-next-invocation",
		"an-orphan-is-reaped-only-when-its-lock-is-free-and-its-owner-dead")
	root := t.TempDir()
	isolator := &fakeIsolator{}
	deps := reapDeps{
		isolator:  func() (dbtestenv.DatabaseIsolator, bool) { return isolator, true },
		killDelay: 200 * time.Millisecond,
	}

	// An orphan whose member ignores SIGTERM: the reaper must escalate.
	stubbornGroup, stubborn := startGroup(t, fixtureIgnoreTerm)
	orphan := writeOrphanLease(t, root, Lease{
		Target:            "/app",
		PGIDs:             []int{stubbornGroup},
		Groups:            []ProcessGroup{identityOf(t, stubbornGroup)},
		Databases:         []string{"compose_orphan_app_default"},
		ProvisionerDigest: "fakedigest",
		ProxyPorts:        []int{41001},
	})

	// A live owner: its pid is running, so nothing of it may be touched.
	liveGroup, _ := startGroup(t, fixtureNeverReady)
	liveOwner := writeOrphanLease(t, root, Lease{PID: liveGroup, PGIDs: []int{liveGroup}, Groups: []ProcessGroup{identityOf(t, liveGroup)}, Databases: []string{"compose_live_db"}})

	// A dead pid whose lock is still held: another process is reaping or
	// owns it, so it is left alone as well.
	heldGroup, _ := startGroup(t, fixtureNeverReady)
	held := writeOrphanLease(t, root, Lease{PGIDs: []int{heldGroup}, Groups: []ProcessGroup{identityOf(t, heldGroup)}})
	lock, err := flock.Acquire(filepath.Join(Root(root), held, ownerLockName), true, true)
	if err != nil {
		t.Fatalf("hold lock: %v", err)
	}
	defer func() { _ = lock.Release() }()

	// A tree that is not a lease this CLI wrote.
	unreadable := filepath.Join(Root(root), "not-a-lease")
	writeFile(t, filepath.Join(unreadable, leaseFileName), "{")

	reaped := reapOrphans(root, deps)
	if len(reaped) != 1 || reaped[0].ID != orphan {
		t.Fatalf("reaped = %+v, want only %s", reaped, orphan)
	}
	if len(reaped[0].Leftovers) != 0 {
		t.Errorf("leftovers = %v", reaped[0].Leftovers)
	}
	waitExited(t, stubbornGroup, stubborn)
	if processGroupAlive(stubbornGroup) {
		t.Errorf("the orphan's process group survived the reap")
	}
	if !slices.Equal(isolator.dropped, []string{"compose_orphan_app_default"}) {
		t.Errorf("dropped = %v, want exactly the orphan's database", isolator.dropped)
	}
	if _, err := os.Stat(filepath.Join(Root(root), orphan)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the orphan's lease directory survived: %v", err)
	}
	for _, spared := range []string{liveOwner, held} {
		if _, err := os.Stat(filepath.Join(Root(root), spared, leaseFileName)); err != nil {
			t.Errorf("lease %s was touched: %v", spared, err)
		}
	}
	if !processGroupAlive(liveGroup) || !processGroupAlive(heldGroup) {
		t.Errorf("a spared composition's process group was signaled")
	}
	if _, err := os.Stat(unreadable); err != nil {
		t.Errorf("an unreadable tree was removed: %v", err)
	}
	if again := reapOrphans(root, deps); len(again) != 0 {
		t.Errorf("a second reap returned %+v", again)
	}
}

func TestReapOrphans_NamesWhatItCouldNotRelease(t *testing.T) {
	root := t.TempDir()
	writeOrphanLease(t, root, Lease{Databases: []string{"compose_x_app_default"}, ProvisionerDigest: "d"})
	reaped := reapOrphans(root, reapDeps{
		isolator:  func() (dbtestenv.DatabaseIsolator, bool) { return nil, false },
		killDelay: 10 * time.Millisecond,
	})
	if len(reaped) != 1 || len(reaped[0].Leftovers) != 1 || !strings.Contains(reaped[0].Leftovers[0], "compose_x_app_default") {
		t.Fatalf("reaped = %+v, want the undropped database named", reaped)
	}
	if dirs := leaseDirs(t, root); len(dirs) != 0 {
		t.Errorf("a reported reap left its lease: %v", dirs)
	}
}

// TestLease_SurvivesAKilledOwnerAndIsReaped is the SIGKILL conformance test: a
// child process creates a lease, records a running member group in it, and is
// killed without any cleanup. The next reap terminates the group and removes
// the lease.
func TestLease_SurvivesAKilledOwnerAndIsReaped(t *testing.T) {
	spectest.Proves(t, "cli/workload-qualification", "ungraceful-death-is-reaped-by-the-next-invocation",
		"a-killed-composition-leaves-a-lease-the-next-invocation-reaps")
	root := t.TempDir()
	memberGroup, member := startGroup(t, fixtureNeverReady)
	handshake := filepath.Join(t.TempDir(), "lease-id")

	executable, _ := os.Executable()
	holder := exec.Command(executable, "-test.run=^TestLeaseHolderUntilKilled$", "-test.timeout=120s")
	holder.Env = append(os.Environ(),
		"PUTNAMI_COMPOSE_LEASE_ROOT="+root,
		"PUTNAMI_COMPOSE_LEASE_HANDSHAKE="+handshake,
		"PUTNAMI_COMPOSE_LEASE_PGID="+strconv.Itoa(memberGroup),
	)
	if err := holder.Start(); err != nil {
		t.Fatalf("start holder: %v", err)
	}
	defer func() { _ = holder.Process.Kill() }()

	id := waitForFile(t, handshake)
	if err := holder.Process.Kill(); err != nil {
		t.Fatalf("SIGKILL holder: %v", err)
	}
	_ = holder.Wait()

	data, err := os.ReadFile(filepath.Join(Root(root), id, leaseFileName))
	if err != nil {
		t.Fatalf("lease did not survive SIGKILL: %v", err)
	}
	var lease Lease
	if err := json.Unmarshal(data, &lease); err != nil || lease.PID != holder.Process.Pid || !slices.Equal(lease.PGIDs, []int{memberGroup}) {
		t.Fatalf("surviving lease = %s (%v)", data, err)
	}

	reaped := reapOrphans(root, reapDeps{killDelay: time.Second})
	if len(reaped) != 1 || reaped[0].ID != id {
		t.Fatalf("reaped = %+v, want %s", reaped, id)
	}
	waitExited(t, memberGroup, member)
}

// TestLeaseHolderUntilKilled is the child half of the SIGKILL test.
func TestLeaseHolderUntilKilled(t *testing.T) {
	root := os.Getenv("PUTNAMI_COMPOSE_LEASE_ROOT")
	if root == "" {
		t.Skip("child-only helper; driven by TestLease_SurvivesAKilledOwnerAndIsReaped")
	}
	handle, err := createLease(root, "/app")
	if err != nil {
		t.Fatalf("createLease: %v", err)
	}
	pgid, _ := strconv.Atoi(os.Getenv("PUTNAMI_COMPOSE_LEASE_PGID"))
	start, ok := processStartTime(pgid)
	if !ok {
		t.Fatalf("read the start time of group %d", pgid)
	}
	if err := handle.update(func(l *Lease) { l.recordGroup(pgid, start) }); err != nil {
		t.Fatalf("update lease: %v", err)
	}
	if err := os.WriteFile(os.Getenv("PUTNAMI_COMPOSE_LEASE_HANDSHAKE"), []byte(handle.lease.ID), 0o600); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	time.Sleep(time.Minute)
}

func TestLease_PartialCloseKeepsWhatSurvivedForTheNextReap(t *testing.T) {
	root := t.TempDir()
	handle, err := createLease(root, "/app")
	if err != nil {
		t.Fatalf("createLease: %v", err)
	}
	isolator := &fakeIsolator{dropErr: errors.New("exit status 1")}
	_ = isolator.CreateDatabase(context.Background(), "d", "compose_"+handle.lease.ID+"_app_default")
	if err := handle.update(func(l *Lease) {
		l.Databases = []string{"compose_" + handle.lease.ID + "_app_default"}
		l.ProvisionerDigest = "d"
	}); err != nil {
		t.Fatal(err)
	}
	c := &Composition{
		id:        handle.lease.ID,
		root:      root,
		plan:      &Plan{},
		lease:     handle,
		cancel:    func() {},
		pgids:     map[int]trackedGroup{},
		databases: &databaseSet{id: handle.lease.ID, isolator: isolator, digest: "d", isolation: IsolationDatabase, created: []string{"compose_" + handle.lease.ID + "_app_default"}},
	}
	report := c.Close(context.Background())
	if report.State != CleanupPartial || len(report.Leftovers) == 0 {
		t.Fatalf("report = %+v, want partial with the undropped database", report)
	}
	lease, ok := readLease(filepath.Join(Root(root), handle.lease.ID))
	if !ok || !slices.Equal(lease.Databases, []string{"compose_" + handle.lease.ID + "_app_default"}) {
		t.Fatalf("lease after a partial close = %+v (%v), want the database kept on record", lease, ok)
	}
	lock, err := flock.Acquire(filepath.Join(Root(root), handle.lease.ID, ownerLockName), true, true)
	if err != nil {
		t.Fatalf("a partial close kept the owner lock: %v", err)
	}
	_ = lock.Release()
}

// identityOf is the identity a composition records for a running group.
func identityOf(t *testing.T, pgid int) ProcessGroup {
	t.Helper()
	start, ok := processStartTime(pgid)
	if !ok {
		t.Fatalf("read the start time of group %d", pgid)
	}
	return ProcessGroup{PGID: pgid, LeaderStart: start}
}

// TestReapOrphans_SignalsOnlyAGroupWhoseLeaderItConfirms pins the reuse
// guard: a recorded id may name another process's group by the time a reap
// runs, so the reaper signals a group only while its recorded leader leads it.
func TestReapOrphans_SignalsOnlyAGroupWhoseLeaderItConfirms(t *testing.T) {
	root := t.TempDir()
	deps := reapDeps{killDelay: 100 * time.Millisecond}
	stranger, _ := startGroup(t, fixtureNeverReady)

	// A lease without identities: the group runs, nothing confirms it.
	unconfirmed := writeOrphanLease(t, root, Lease{PGIDs: []int{stranger}})
	reaped := reapOrphans(root, deps)
	if len(reaped) != 1 || reaped[0].ID != unconfirmed {
		t.Fatalf("reaped = %+v, want %s", reaped, unconfirmed)
	}
	if !processGroupAlive(stranger) {
		t.Fatal("a group without a recorded identity was signaled")
	}
	if len(reaped[0].Leftovers) != 1 || !strings.Contains(reaped[0].Leftovers[0], "process group "+strconv.Itoa(stranger)+" (not signaled") {
		t.Errorf("leftovers = %v, want the unconfirmed group named", reaped[0].Leftovers)
	}

	// A lease whose leader start differs: the id now names another group.
	replaced := writeOrphanLease(t, root, Lease{PGIDs: []int{stranger}, Groups: []ProcessGroup{{PGID: stranger, LeaderStart: "a process that exited long ago"}}})
	reaped = reapOrphans(root, deps)
	if len(reaped) != 1 || reaped[0].ID != replaced {
		t.Fatalf("reaped = %+v, want %s", reaped, replaced)
	}
	if !processGroupAlive(stranger) {
		t.Fatal("a group whose id was reused by another process was signaled")
	}
	if len(reaped[0].Leftovers) != 0 {
		t.Errorf("leftovers = %v, want none: the recorded group is gone", reaped[0].Leftovers)
	}
}

// TestComposition_ReleasesAGroupOnceItsStepReturned pins that a composition
// never keeps the id of a group that is gone: the lease and the teardown only
// hold groups that may still run.
func TestComposition_ReleasesAGroupOnceItsStepReturned(t *testing.T) {
	root := t.TempDir()
	handle, err := createLease(root, "/app")
	if err != nil {
		t.Fatalf("createLease: %v", err)
	}
	c := &Composition{id: handle.lease.ID, root: root, plan: &Plan{}, lease: handle, cancel: func() {}, pgids: map[int]trackedGroup{}}

	exited, exitedDone := startGroup(t, fixtureNeverReady)
	running, _ := startGroup(t, fixtureNeverReady)
	c.recordProcessGroup("/app", exited)
	c.recordProcessGroup("/app", running)
	if lease := handle.snapshot(); !slices.Equal(lease.PGIDs, []int{exited, running}) || lease.leaderStart(exited) == "" {
		t.Fatalf("lease after two spawns = %+v, want both groups with identities", lease)
	}

	_ = proctree.KillGroup(exited)
	waitExited(t, exited, exitedDone)
	c.releaseProcessGroup(exited)
	c.releaseProcessGroup(running) // its step has not returned: the group runs and stays
	lease := handle.snapshot()
	if !slices.Equal(lease.PGIDs, []int{running}) || len(lease.Groups) != 1 || lease.Groups[0].PGID != running {
		t.Fatalf("lease after the release = %+v, want only the running group", lease)
	}
	if _, kept := c.pgids[exited]; kept {
		t.Error("the composition still tracks a released group")
	}

	// A tracked id now led by another process is not this composition's.
	c.pgids[running] = trackedGroup{member: "/app", leaderStart: "a process that exited long ago"}
	if report := c.Close(context.Background()); report.State != CleanupClean {
		t.Errorf("report = %+v, want clean: the id names another process's group", report)
	}
	if !processGroupAlive(running) {
		t.Error("teardown signaled a group it does not own")
	}
}

// TestLease_PartialCloseKeepsDatabasesItCouldNotConfirmDropped pins that a
// failed confirmation never forgets a database.
func TestLease_PartialCloseKeepsDatabasesItCouldNotConfirmDropped(t *testing.T) {
	root := t.TempDir()
	handle, err := createLease(root, "/app")
	if err != nil {
		t.Fatalf("createLease: %v", err)
	}
	name := "compose_" + handle.lease.ID + "_app_default"
	isolator := &fakeIsolator{listErr: errors.New("docker exec: exit status 125")}
	_ = isolator.CreateDatabase(context.Background(), "d", name)
	if err := handle.update(func(l *Lease) { l.Databases, l.ProvisionerDigest = []string{name}, "d" }); err != nil {
		t.Fatal(err)
	}
	c := &Composition{
		id: handle.lease.ID, root: root, plan: &Plan{}, lease: handle, cancel: func() {}, pgids: map[int]trackedGroup{},
		databases: &databaseSet{id: handle.lease.ID, isolator: isolator, digest: "d", isolation: IsolationDatabase, created: []string{name}},
	}
	if report := c.Close(context.Background()); report.State != CleanupPartial {
		t.Fatalf("report = %+v, want partial: the drop could not be confirmed", report)
	}
	lease, ok := readLease(filepath.Join(Root(root), handle.lease.ID))
	if !ok || !slices.Equal(lease.Databases, []string{name}) {
		t.Fatalf("lease after an unconfirmed drop = %+v (%v), want the database kept on record", lease, ok)
	}
}

func waitForFile(t *testing.T, path string) string {
	t.Helper()
	deadline := time.Now().Add(hangDetector)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
			return string(data)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no handshake at %s", path)
	return ""
}
