package compose

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

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

// Every member's serve run requests the one loaded command whose traits declare
// the serve workload; a workspace that loads none is compose.no_serve_command,
// naming the first member. Two extensions that declare the same command are one
// command.
func TestPlan_BindServeCommandsNamesTheCommandEveryMemberRequests(t *testing.T) {
	provider := &workspace.Project{ID: "/provider", Name: "provider"}
	consumer := &workspace.Project{ID: "/consumer", Name: "consumer", RunsWith: []string{"provider"}}
	ws := planWorkspace(t, provider, consumer)
	plan, err := PlanFor(ws, consumer, firstPartyServe)
	if err != nil {
		t.Fatalf("PlanFor: %v", err)
	}
	composeErr := composeError(t, plan.BindServeCommands(nil))
	if composeErr.Code != CodeNoServeCommand || composeErr.Member != "/provider" || composeErr.Phase != PhasePlan {
		t.Fatalf("error = %v, want compose.no_serve_command for /provider", composeErr)
	}
	if err := plan.BindServeCommands([]string{"serve", "serve"}); err != nil {
		t.Fatalf("BindServeCommands with one command twice: %v", err)
	}
	for _, member := range plan.Members {
		if member.ServeCommand != "serve" {
			t.Errorf("%s requests %q, want serve", member.Project.ID, member.ServeCommand)
		}
	}
}

// A workspace whose extensions declare two commands with the serve workload, the
// first-party serve and a daemon, is refused: compose starts one serve run per
// member and does not pick one.
func TestPlan_BindServeCommandsRefusesTwoServeWorkloadCommands(t *testing.T) {
	spectest.Proves(t, "cli/workload-qualification", "runs-with-closure-served-behind-stable-proxies",
		"an-unserveable-member-is-refused-before-anything-starts")
	app := &workspace.Project{ID: "/app", Name: "app"}
	plan, err := PlanFor(planWorkspace(t, app), app, []string{"daemon", "serve"})
	if err != nil {
		t.Fatalf("PlanFor: %v", err)
	}
	composeErr := composeError(t, plan.BindServeCommands([]string{"serve", "daemon"}))
	if composeErr.Code != CodeAmbiguousServe || composeErr.Member != "/app" || composeErr.Phase != PhasePlan ||
		!strings.Contains(composeErr.Message, "daemon, serve") {
		t.Fatalf("error = %v, want compose.ambiguous_serve for /app naming daemon and serve", composeErr)
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

// A composition member runs the command whose traits declare the serve
// workload, whatever its name: each member's serve run requests exactly that
// command, a disable entry turns off a member only when it names it, and a
// workspace whose loaded extensions declare none is compose.no_serve_command
// before anything starts, even when a command is named serve.
func TestUp_AMemberRunsTheServeWorkloadCommand(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "workload-lifecycle", "a-composition-member-is-a-serve-workload-step")
	upWith := func(fixture *compositionFixture, target *workspace.Project, workloads map[string]string) (*Composition, error) {
		deps := fixture.deps(nil)
		deps.discover = func(*workspace.Workspace, *wsproto.Config) ([]*extension.ExtensionDescription, error) {
			return serveWorkloadExtensions(workloads), nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), hangDetector)
		defer cancel()
		return up(ctx, Options{WorkspaceRoot: fixture.root, Config: &wsproto.Config{}, Target: target, ReadyTimeout: hangDetector}, deps)
	}

	fixture := newCompositionFixture(t, fixtureProject{id: "/app", name: "app"})
	comp, err := upWith(fixture, fixture.projects["/app"], map[string]string{"daemon": extension.WorkloadServe, "serve": "", "run": extension.WorkloadOnce})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	comp.Close(context.Background())
	if args := fixture.argv(t, "/app"); len(args) == 0 || args[0] != "daemon" {
		t.Fatalf("the serve run requested %v, want the serve-workload daemon", args)
	}

	idle := newCompositionFixture(t, fixtureProject{id: "/app", name: "app"})
	_, err = upWith(idle, idle.projects["/app"], map[string]string{"serve": "", "run": extension.WorkloadOnce})
	if composeErr := composeError(t, err); composeErr.Code != CodeNoServeCommand || composeErr.Phase != PhasePlan {
		t.Fatalf("error = %v, want compose.no_serve_command", err)
	}
	if _, err := os.Stat(filepath.Join(idle.root, "app", fakeCLIArgsFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a serve run started for a workspace without a serve-workload command")
	}
	// A member that cannot serve is named before the missing workload.
	library := &workspace.Project{ID: "/lib", Name: "lib", Type: "library"}
	libraryFixture := newCompositionFixture(t)
	libraryFixture.ws = planWorkspace(t, library)
	if _, err := upWith(libraryFixture, library, nil); composeError(t, err).Code != CodeNotServeable {
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
