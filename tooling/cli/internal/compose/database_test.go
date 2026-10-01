package compose

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	pdb "go.putnami.dev/protocol/database"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

func databasePlan() *Plan {
	api := &Member{Project: &workspace.Project{ID: "/apps/api"}, Databases: []DatabaseBinding{{Datasource: "default", Schema: "public"}}}
	worker := &Member{Project: &workspace.Project{ID: "/apps/worker"}, Databases: []DatabaseBinding{{Datasource: "default", Schema: "jobs"}, {Datasource: "audit", Schema: "public"}}}
	web := &Member{Project: &workspace.Project{ID: "/apps/web"}}
	return &Plan{Target: web, Members: []*Member{api, worker, web}}
}

func TestProvisionDatabases_ProvidedServerIsSharedAndReportedAsIsolationNone(t *testing.T) {
	spectest.Proves(t, "cli/workload-qualification", "ephemeral-databases-per-composition",
		"a-provided-server-is-shared-and-reported-as-isolation-none")
	lease, err := createLease(t.TempDir(), "/apps/web")
	if err != nil {
		t.Fatalf("createLease: %v", err)
	}
	defer func() { _ = lease.release() }()
	plan := databasePlan()

	set, err := provisionDatabases(context.Background(), lease.lease.ID, plan, lease, sharedServer{})
	if err != nil {
		t.Fatalf("provisionDatabases: %v", err)
	}
	if set.isolation != IsolationNone || len(set.created) != 0 {
		t.Errorf("isolation %q, created %v; want none and nothing created", set.isolation, set.created)
	}
	for _, member := range plan.Members {
		for _, binding := range member.Databases {
			if binding.Database != "shared" || binding.connection == nil || binding.connection.Database != "shared" {
				t.Errorf("%s/%s bound to %q, want the provided server's database", member.Project.ID, binding.Datasource, binding.Database)
			}
		}
	}
	if recorded := lease.snapshot(); len(recorded.Databases) != 0 || recorded.ProvisionerDigest != "shareddigest" {
		t.Errorf("lease = %+v, want the digest and no database to drop", recorded)
	}
	if leftovers, _ := set.drop(context.Background()); len(leftovers) != 0 {
		t.Errorf("dropping a shared server's composition reported %v", leftovers)
	}
}

func TestProvisionDatabases_IsolatingProviderRecordsEachDatabaseBeforeCreatingIt(t *testing.T) {
	lease, err := createLease(t.TempDir(), "/apps/web")
	if err != nil {
		t.Fatalf("createLease: %v", err)
	}
	defer func() { _ = lease.release() }()
	plan := databasePlan()
	isolator := &recordingIsolator{lease: lease}

	set, err := provisionDatabases(context.Background(), lease.lease.ID, plan, lease, isolator)
	if err != nil {
		t.Fatalf("provisionDatabases: %v", err)
	}
	prefix := databasePrefix(lease.lease.ID)
	want := []string{prefix + "_apps_api_default", prefix + "_apps_worker_default", prefix + "_apps_worker_audit"}
	if set.isolation != IsolationDatabase || !slices.Equal(set.created, want) {
		t.Fatalf("created %v (isolation %s), want %v", set.created, set.isolation, want)
	}
	for i, recorded := range isolator.recordedBeforeCreate {
		if !recorded {
			t.Errorf("database %s was created before the lease named it", isolator.created[i])
		}
	}
	worker := plan.Members[1]
	if worker.Databases[0].connection.Database != want[1] || worker.Databases[0].Schema != "jobs" || worker.Databases[0].connection.Password != fakePassword {
		t.Errorf("worker default binding = %+v / %+v", worker.Databases[0], worker.Databases[0].connection)
	}

	if leftovers, _ := set.drop(context.Background()); len(leftovers) != 0 {
		t.Errorf("drop leftovers = %v", leftovers)
	}
	isolator.existing[prefix+"_stray_default"] = true
	if leftovers, _ := set.drop(context.Background()); len(leftovers) != 1 || !strings.Contains(leftovers[0], "_stray_default") {
		t.Errorf("a database of this composition that survived was not reported: %v", leftovers)
	}
}

func TestProvisionDatabases_FailureNamesTheMemberAndPhase(t *testing.T) {
	lease, err := createLease(t.TempDir(), "/apps/web")
	if err != nil {
		t.Fatalf("createLease: %v", err)
	}
	defer func() { _ = lease.release() }()
	_, err = provisionDatabases(context.Background(), lease.lease.ID, databasePlan(), lease, failingServer{})
	composeErr := composeError(t, err)
	if composeErr.Code != CodeDatabaseFailed || composeErr.Member != "/apps/api" || composeErr.Phase != PhaseDatabases {
		t.Fatalf("error = %v, want compose.database_failed for /apps/api", err)
	}
	if strings.Contains(err.Error(), fakePassword) {
		t.Errorf("the error carries a password: %v", err)
	}

	set, err := provisionDatabases(context.Background(), lease.lease.ID, &Plan{Members: []*Member{{Project: &workspace.Project{ID: "/web"}}}}, lease, nil)
	if err != nil || set.isolation != IsolationNone {
		t.Errorf("a composition without databases touched the provider: %v", err)
	}
}

// recordingIsolator checks, at every create, that the lease already names the
// database.
type recordingIsolator struct {
	fakeIsolator
	lease                *leaseHandle
	recordedBeforeCreate []bool
}

func (r *recordingIsolator) CreateDatabase(ctx context.Context, digest, name string) error {
	r.recordedBeforeCreate = append(r.recordedBeforeCreate, slices.Contains(r.lease.snapshot().Databases, name))
	return r.fakeIsolator.CreateDatabase(ctx, digest, name)
}

type failingServer struct{}

func (failingServer) Provision() (pdb.Connection, string, error) {
	return pdb.Connection{}, "", errors.New("docker: failed to start postgres test container")
}
