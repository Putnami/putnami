package compose

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// firstPartyServe are the serve-workload commands of a workspace whose
// language extension declares serve with the serve workload.
var firstPartyServe = []string{"serve"}

func planWorkspace(t *testing.T, projects ...*workspace.Project) *workspace.Workspace {
	t.Helper()
	root := t.TempDir()
	for _, project := range projects {
		if project.Path == "" {
			project.Path = strings.TrimPrefix(project.ID, "/")
		}
	}
	return workspace.NewWorkspace(root, &wsproto.Config{}, projects)
}

func composeError(t *testing.T, err error) *Error {
	t.Helper()
	var composeErr *Error
	if !errors.As(err, &composeErr) {
		t.Fatalf("error %v is not a composition error", err)
	}
	return composeErr
}

func TestPlanFor_ClosureIsTransitiveRunsWithInTopologicalOrder(t *testing.T) {
	spectest.Proves(t, "cli/workload-qualification", "runs-with-closure-served-behind-stable-proxies",
		"the-closure-is-transitive-runs-with-in-topological-order")
	web := &workspace.Project{ID: "/apps/web", Name: "@acme/web", RunsWith: []string{"@acme/api", "/apps/auth"}}
	api := &workspace.Project{ID: "/apps/api", Name: "@acme/api", RunsWith: []string{"@acme/store"}}
	auth := &workspace.Project{ID: "/apps/auth", Name: "@acme/auth", RunsWith: []string{"@acme/store"}}
	store := &workspace.Project{ID: "/apps/store", Name: "@acme/store"}
	// Dependencies alone link nothing: a generated client package is not a
	// workload the web app runs with.
	unrelated := &workspace.Project{ID: "/apps/unrelated", Name: "@acme/unrelated"}
	web.Dependencies = []string{"@acme/unrelated"}
	ws := planWorkspace(t, web, api, auth, store, unrelated)

	plan, err := PlanFor(ws, web, firstPartyServe)
	if err != nil {
		t.Fatalf("PlanFor: %v", err)
	}
	if got, want := plan.ProjectIDs(), []string{"/apps/store", "/apps/api", "/apps/auth", "/apps/web"}; !slices.Equal(got, want) {
		t.Fatalf("members = %v, want %v (dependencies first, each once, target last)", got, want)
	}
	if plan.Target.Project != web {
		t.Errorf("target = %s, want /apps/web", plan.Target.Project.ID)
	}
	webMember, _ := plan.Member("/apps/web")
	var providers []string
	for _, provider := range webMember.runsWith {
		providers = append(providers, provider.Project.ID)
	}
	if !slices.Equal(providers, []string{"/apps/api", "/apps/auth"}) {
		t.Errorf("web runs with %v, want its own declared providers only", providers)
	}
}

func TestPlanFor_RejectsLibraryImageUnknownAndCycle(t *testing.T) {
	spectest.Proves(t, "cli/workload-qualification", "runs-with-closure-served-behind-stable-proxies",
		"an-unserveable-member-is-refused-before-anything-starts")
	cases := []struct {
		name     string
		projects func() []*workspace.Project
		code     string
		member   string
	}{
		{
			name: "library target",
			projects: func() []*workspace.Project {
				return []*workspace.Project{{ID: "/lib", Name: "lib", Type: "library"}}
			},
			code: CodeNotServeable, member: "/lib",
		},
		{
			name: "image dependency",
			projects: func() []*workspace.Project {
				return []*workspace.Project{
					{ID: "/app", Name: "app", RunsWith: []string{"base"}},
					{ID: "/base", Name: "base", Type: "image"},
				}
			},
			code: CodeNotServeable, member: "/base",
		},
		{
			name: "unknown member",
			projects: func() []*workspace.Project {
				return []*workspace.Project{{ID: "/app", Name: "app", RunsWith: []string{"@acme/missing"}}}
			},
			code: CodeUnknownMember, member: "/app",
		},
		{
			name: "cycle",
			projects: func() []*workspace.Project {
				return []*workspace.Project{
					{ID: "/a", Name: "a", RunsWith: []string{"b"}},
					{ID: "/b", Name: "b", RunsWith: []string{"/a"}},
				}
			},
			code: CodeCycle, member: "/a",
		},
		{
			name: "serve disabled on the project",
			projects: func() []*workspace.Project {
				return []*workspace.Project{{ID: "/app", Name: "app", Config: &wsproto.ProjectConfig{
					Disable: &wsproto.ProjectDisableConfig{Jobs: []string{"@putnami/typescript:serve"}},
				}}}
			},
			code: CodeServeDisabled, member: "/app",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			projects := tc.projects()
			ws := planWorkspace(t, projects...)
			_, err := PlanFor(ws, projects[0], firstPartyServe)
			composeErr := composeError(t, err)
			if composeErr.Code != tc.code || composeErr.Member != tc.member || composeErr.Phase != PhasePlan {
				t.Fatalf("error = %s/%s/%s (%v), want %s/%s/plan", composeErr.Code, composeErr.Member, composeErr.Phase, err, tc.code, tc.member)
			}
		})
	}

	app := &workspace.Project{ID: "/app", Name: "app"}
	ws := planWorkspace(t, app)
	ws.Config.Disable = &wsproto.DisableConfig{Jobs: []string{"serve"}}
	if _, err := PlanFor(ws, app, firstPartyServe); composeError(t, err).Code != CodeServeDisabled {
		t.Errorf("a workspace-level serve disable was not refused: %v", err)
	}
}

func TestPlan_BindServeJobsNamesTheMemberWithoutAServeStep(t *testing.T) {
	provider := &workspace.Project{ID: "/provider", Name: "provider"}
	consumer := &workspace.Project{ID: "/consumer", Name: "consumer", RunsWith: []string{"provider"}}
	ws := planWorkspace(t, provider, consumer)
	plan, err := PlanFor(ws, consumer, firstPartyServe)
	if err != nil {
		t.Fatalf("PlanFor: %v", err)
	}
	err = plan.BindServeJobs([]*jobs.ScheduledJob{{Project: consumer}})
	composeErr := composeError(t, err)
	if composeErr.Code != CodeNoServeCommand || composeErr.Member != "/provider" {
		t.Fatalf("error = %v, want compose.no_serve_command for /provider", err)
	}
	if err := plan.BindServeJobs([]*jobs.ScheduledJob{{Project: consumer}, {Project: provider}}); err != nil {
		t.Fatalf("BindServeJobs with both steps: %v", err)
	}
}

// A member that the serve steps of two commands serve, the first-party serve
// and a daemon another extension declares with the serve workload, is refused:
// compose starts one workload per member and does not pick one. Two steps of
// one command are not refused.
func TestPlan_BindServeJobsRefusesAMemberThatTwoCommandsServe(t *testing.T) {
	spectest.Proves(t, "cli/workload-qualification", "runs-with-closure-served-behind-stable-proxies",
		"an-unserveable-member-is-refused-before-anything-starts")
	app := &workspace.Project{ID: "/app", Name: "app"}
	plan, err := PlanFor(planWorkspace(t, app), app, []string{"daemon", "serve"})
	if err != nil {
		t.Fatalf("PlanFor: %v", err)
	}
	step := func(name string) *jobs.ScheduledJob {
		return &jobs.ScheduledJob{Project: app, JobDef: &extension.JobDefinition{Name: name}}
	}
	err = plan.BindServeJobs([]*jobs.ScheduledJob{step("serve~serve"), step("daemon")})
	composeErr := composeError(t, err)
	if composeErr.Code != CodeAmbiguousServe || composeErr.Member != "/app" || composeErr.Phase != PhasePlan ||
		!strings.Contains(composeErr.Message, "daemon, serve") {
		t.Fatalf("error = %v, want compose.ambiguous_serve for /app naming daemon and serve", err)
	}
	if err := plan.BindServeJobs([]*jobs.ScheduledJob{step("serve~a"), step("serve~b")}); err != nil {
		t.Fatalf("two steps of one command: %v", err)
	}
}

func TestPlanFor_ReadsDatabasesAndProviderServiceIDs(t *testing.T) {
	provider := &workspace.Project{ID: "/provider", Name: "go.acme.dev/provider"}
	bare := &workspace.Project{ID: "/bare", Name: "bare"}
	consumer := &workspace.Project{ID: "/consumer", Name: "consumer", RunsWith: []string{"/provider", "bare"}}
	ws := planWorkspace(t, provider, bare, consumer)
	writeFile(t, ws.Root+"/provider/schema/openapi.json",
		`{"openapi":"3.0.3","x-putnami-client":{"protocolVersion":1,"service":{"id":"items","audience":"urn:acme:items"},"credentials":{}}}`)
	writeFile(t, ws.Root+"/consumer/infra/requirements.json",
		`{"protocolVersion":2,"databases":[{"name":"default","engine":"postgres","schemas":["app"]},{"name":"audit","engine":"postgres"}]}`)

	plan, err := PlanFor(ws, consumer, firstPartyServe)
	if err != nil {
		t.Fatalf("PlanFor: %v", err)
	}
	providerMember, _ := plan.Member("/provider")
	if !slices.Equal(providerMember.serviceIDs, []string{"items"}) {
		t.Errorf("provider service ids = %v, want [items]", providerMember.serviceIDs)
	}
	bareMember, _ := plan.Member("/bare")
	if len(bareMember.serviceIDs) != 0 || !slices.Contains(bareMember.notes, noContractNote) {
		t.Errorf("a provider without a contract: ids %v, notes %v", bareMember.serviceIDs, bareMember.notes)
	}
	consumerMember, _ := plan.Member("/consumer")
	want := []DatabaseBinding{{Datasource: "default", Schema: "app"}, {Datasource: "audit", Schema: "public"}}
	if len(consumerMember.Databases) != len(want) {
		t.Fatalf("databases = %+v, want %+v", consumerMember.Databases, want)
	}
	for i := range want {
		if consumerMember.Databases[i].Datasource != want[i].Datasource || consumerMember.Databases[i].Schema != want[i].Schema {
			t.Errorf("database %d = %+v, want %+v", i, consumerMember.Databases[i], want[i])
		}
	}

	writeFile(t, ws.Root+"/provider/schema/openapi.json", `{"x-putnami-client":{"protocolVersion":9}}`)
	if _, err := PlanFor(ws, consumer, firstPartyServe); composeError(t, err).Code != CodeInvalidRequirement {
		t.Errorf("an invalid client contract was accepted: %v", err)
	}
}

// A composition member runs the commands whose traits declare the serve
// workload, whatever their name: Up plans exactly those for its members, a
// disable entry turns off a member only when it names one of them, and a
// workspace whose loaded extensions declare none is compose.no_serve_command
// before anything is prepared, even when a command is named serve.
func TestUp_AMemberRunsTheServeWorkloadCommands(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "workload-lifecycle", "a-composition-member-is-a-serve-workload-step")
	app := &workspace.Project{ID: "/app", Name: "app"}
	library := &workspace.Project{ID: "/lib", Name: "lib", Type: "library"}
	ws := planWorkspace(t, app, library)
	stop := errors.New("stop after preparation")
	upWith := func(target *workspace.Project, workloads map[string]string) (prepared []string, err error) {
		_, err = up(context.Background(), Options{WorkspaceRoot: ws.Root, Config: &wsproto.Config{}, Target: target}, upDeps{
			loadWorkspace: func(string) (*workspace.Workspace, error) { return ws, nil },
			discover: func(*workspace.Workspace, *wsproto.Config) ([]*extension.ExtensionDescription, error) {
				return serveWorkloadExtensions(workloads), nil
			},
			prepare: func(_ context.Context, _ Options, _, serveCommands []string) (*preparation, error) {
				prepared = serveCommands
				return nil, stop
			},
			reap: func(string) []ReapedLease { return nil },
		})
		return prepared, err
	}

	prepared, err := upWith(app, map[string]string{"daemon": extension.WorkloadServe, "serve": "", "run": extension.WorkloadOnce})
	if !errors.Is(err, stop) || !slices.Equal(prepared, []string{"daemon"}) {
		t.Fatalf("prepared %v (%v), want the serve-workload daemon alone", prepared, err)
	}
	prepared, err = upWith(app, map[string]string{"serve": "", "run": extension.WorkloadOnce})
	if composeErr := composeError(t, err); composeErr.Code != CodeNoServeCommand || composeErr.Phase != PhasePlan || prepared != nil {
		t.Fatalf("error = %v, prepared %v; want compose.no_serve_command before preparation", err, prepared)
	}
	// A member that cannot serve is named before the missing workload.
	if _, err := upWith(library, nil); composeError(t, err).Code != CodeNotServeable {
		t.Errorf("a library member without a serve workload: %v, want compose.not_serveable", err)
	}

	disabled := &workspace.Project{ID: "/app", Name: "app", Config: &wsproto.ProjectConfig{
		Disable: &wsproto.ProjectDisableConfig{Jobs: []string{"@acme/lang:daemon"}},
	}}
	if _, err := PlanFor(planWorkspace(t, disabled), disabled, []string{"daemon"}); composeError(t, err).Code != CodeServeDisabled {
		t.Errorf("disabling the serve-workload daemon was not refused: %v", err)
	}
	other := &workspace.Project{ID: "/app", Name: "app", Config: &wsproto.ProjectConfig{
		Disable: &wsproto.ProjectDisableConfig{Jobs: []string{"serve"}},
	}}
	if _, err := PlanFor(planWorkspace(t, other), other, []string{"daemon"}); err != nil {
		t.Errorf("disabling a serve command that declares no workload refused the member: %v", err)
	}
}
