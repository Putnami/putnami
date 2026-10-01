package jobs

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

func TestPlan_SessionPrerequisitePolicy(t *testing.T) {
	t.Parallel()
	ws, cloud, publisher := sessionPrerequisiteFixture()

	t.Run("preview suppresses the entire prerequisite subtree", func(t *testing.T) {
		planned, err := Plan(ws, []string{"deploy"}, ws.Projects[:1],
			[]*extension.ExtensionDescription{cloud, publisher},
			extension.ParamMap{"preview": true, "apps": "/B"}, nil, nil)
		if err != nil {
			t.Fatalf("Plan: %v", err)
		}
		if got, want := jobKeys(planned), []string{"test-workspace:deploy~apply"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("preview plan = %v, want only %v", got, want)
		}
	})

	t.Run("selection policy and gates apply to every prerequisite contributor", func(t *testing.T) {
		// The caller selected A, while apps explicitly overrides that selection
		// with B/C/D. B remains eligible, C has deploy.enabled=false, and D has
		// deployPrerequisite=false.
		planned, err := Plan(ws, []string{"deploy"}, ws.Projects[:1],
			[]*extension.ExtensionDescription{cloud, publisher},
			extension.ParamMap{"apps": "/B,/C,/D"}, nil, nil)
		if err != nil {
			t.Fatalf("Plan: %v", err)
		}

		for _, job := range planned {
			if (job.Project.ID == "/A" || job.Project.ID == "/C" || job.Project.ID == "/D") &&
				job.CommandName() == "publish" {
				t.Fatalf("ineligible project %s received prerequisite publish job %s", job.Project.ID, job.Key())
			}
		}

		// B protects its config with publishConfig=false. That removes only the
		// Cloud config contributor; migration and the independent Docker
		// contributor remain in the same publish command.
		if findSessionPrerequisiteJob(planned, "/B", "@test/cloud", "publish", "config") != nil {
			t.Fatal("publishConfig=false planned the protected config publisher")
		}
		docker := findSessionPrerequisiteJob(planned, "/B", "@test/go", "publish", "docker")
		migration := findSessionPrerequisiteJob(planned, "/B", "@test/cloud", "publish", "migration")
		if docker == nil || migration == nil {
			t.Fatalf("missing eligible publish contributors; jobs=%v", jobKeys(planned))
		}
		if docker.JobDef.BoundParams["docker"] != true || migration.JobDef.BoundParams["publishConfig"] != false {
			t.Fatalf("prerequisite runtime params = docker:%v publishConfig:%v, want true/false",
				docker.JobDef.BoundParams["docker"], migration.JobDef.BoundParams["publishConfig"])
		}

		wantGateKeys := []string{
			"/B:build~compile",
			"/B:lint~check",
			"/B:test~run",
			"/B:validate~features",
			"test-workspace:validate-workspace~architecture",
		}
		for _, publish := range []*ScheduledJob{docker, migration} {
			if !reflect.DeepEqual(publish.DependsOn, wantGateKeys) {
				t.Fatalf("%s functional deps = %v, want every verification leaf %v", publish.Key(), publish.DependsOn, wantGateKeys)
			}
		}

		deploy := findSessionPrerequisiteJob(planned, "test-workspace", "@test/cloud", "deploy", "apply")
		if deploy == nil {
			t.Fatalf("deploy missing; jobs=%v", jobKeys(planned))
		}
		wantPublishLeaves := []string{migration.Key(), docker.Key()}
		if !reflect.DeepEqual(deploy.DependsOn, wantPublishLeaves) {
			t.Fatalf("deploy functional deps = %v, want publish leaves %v so a publish failure blocks deploy", deploy.DependsOn, wantPublishLeaves)
		}
	})

	t.Run("prerequisite params do not change an explicit publish", func(t *testing.T) {
		planned, err := Plan(ws, []string{"publish"}, ws.Projects[1:2],
			[]*extension.ExtensionDescription{cloud, publisher}, nil, nil, nil)
		if err != nil {
			t.Fatalf("Plan: %v", err)
		}
		if findSessionPrerequisiteJob(planned, "/B", "@test/cloud", "publish", "config") == nil {
			t.Fatalf("explicit publish inherited deploy-only publishConfig=false; jobs=%v", jobKeys(planned))
		}
		if findSessionPrerequisiteJob(planned, "/B", "@test/go", "publish", "docker") != nil {
			t.Fatalf("explicit publish inherited prerequisite-only docker=true; jobs=%v", jobKeys(planned))
		}
	})

	t.Run("explicit prerequisite cannot collide with local prerequisite params", func(t *testing.T) {
		_, err := Plan(ws, []string{"publish", "deploy"}, ws.Projects[1:2],
			[]*extension.ExtensionDescription{cloud, publisher},
			extension.ParamMap{"apps": "/B"}, nil, nil)
		if err == nil {
			t.Fatal("Plan accepted one publish identity with explicit and prerequisite-local params")
		}
		for _, want := range []string{"publish", "explicit", "local parameter"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("collision error %q does not mention %q", err, want)
			}
		}
	})

	t.Run("empty selection override falls back to the current selection", func(t *testing.T) {
		planned, err := Plan(ws, []string{"deploy"}, ws.Projects[:2],
			[]*extension.ExtensionDescription{cloud, publisher},
			extension.ParamMap{"apps": ""}, nil, nil)
		if err != nil {
			t.Fatalf("Plan: %v", err)
		}
		for _, projectID := range []string{"/A", "/B"} {
			if findSessionPrerequisiteJob(planned, projectID, "@test/go", "publish", "docker") == nil {
				t.Fatalf("fallback selection omitted %s; jobs=%v", projectID, jobKeys(planned))
			}
		}
		if findSessionPrerequisiteJob(planned, "/C", "@test/go", "publish", "docker") != nil {
			t.Fatalf("fallback selection widened beyond the current selection; jobs=%v", jobKeys(planned))
		}
	})
}

func TestPlan_SessionPrerequisiteInvocationCollisions(t *testing.T) {
	t.Parallel()
	t.Run("command roots with incompatible local params fail closed in both orders", func(t *testing.T) {
		for _, test := range []struct {
			name  string
			first bool
		}{
			{name: "prerequisite-active-first", first: false},
			{name: "prerequisite-inactive-first", first: true},
		} {
			t.Run(test.name, func(t *testing.T) {
				ws, cloud, publisher := sessionPrerequisiteFixture()
				cloud.Jobs["deploy"].PipelineSteps = []extension.PipelineStep{
					{ID: "first", Task: "deploy-apply", With: map[string]extension.InputBinding{
						"preview": {Value: test.first, HasValue: true},
					}},
					{ID: "second", Task: "deploy-check", With: map[string]extension.InputBinding{
						"preview": {Value: !test.first, HasValue: true},
					}},
				}
				cloud.Tasks["deploy-check"] = extension.TaskDefinition{Kind: "command", Command: "run"}

				_, err := Plan(ws, []string{"deploy"}, ws.Projects[:1],
					[]*extension.ExtensionDescription{cloud, publisher},
					extension.ParamMap{"apps": "/B"}, nil, nil)
				assertSessionPrerequisiteError(t, err, "deploy", "session prerequisite", "command roots", "preview")
			})
		}
	})

	t.Run("command roots may differ on params the relation does not observe", func(t *testing.T) {
		ws, cloud, publisher := sessionPrerequisiteFixture()
		cloud.Jobs["deploy"].Flags["worker"] = extension.FlagDefinition{Type: "string"}
		cloud.Jobs["deploy"].PipelineSteps = []extension.PipelineStep{
			{ID: "first", Task: "deploy-apply", With: map[string]extension.InputBinding{
				"worker": {Value: "alpha", HasValue: true},
			}},
			{ID: "second", Task: "deploy-check", With: map[string]extension.InputBinding{
				"worker": {Value: "beta", HasValue: true},
			}},
		}
		cloud.Tasks["deploy-check"] = extension.TaskDefinition{Kind: "command", Command: "run"}

		planned, err := Plan(ws, []string{"deploy"}, ws.Projects[:1],
			[]*extension.ExtensionDescription{cloud, publisher},
			extension.ParamMap{"apps": "/B"}, nil, nil)
		if err != nil {
			t.Fatalf("Plan rejected unrelated task-local parameters: %v", err)
		}
		if findSessionPrerequisiteJob(planned, "/B", "@test/go", "publish", "docker") == nil {
			t.Fatalf("compatible roots did not plan the shared prerequisite; jobs=%v", jobKeys(planned))
		}
	})

	t.Run("relations with incompatible local params fail closed", func(t *testing.T) {
		ws, cloud, publisher := sessionPrerequisiteFixture()
		promote := cloneJobDefinition(cloud.Jobs["deploy"])
		promote.Name = "promote"
		promote.PipelineSteps = []extension.PipelineStep{{ID: "apply", Task: "deploy-apply"}}
		promote.SessionPrerequisites = []extension.SessionPrerequisiteDefinition{{
			Command: "publish", ProjectsFromParam: "apps",
			ProjectIf: "params.deploy.enabled != false && params.deployPrerequisite != false",
			Params: map[string]extension.SessionPrerequisiteParamBinding{
				"docker": {Value: false},
			},
		}}
		cloud.Jobs["promote"] = promote

		_, err := Plan(ws, []string{"deploy", "promote"}, ws.Projects[1:2],
			[]*extension.ExtensionDescription{cloud, publisher},
			extension.ParamMap{"apps": "/B"}, nil, nil)
		assertSessionPrerequisiteError(t, err, "publish", "incompatible", "docker")
	})

	t.Run("prerequisite already emitted by dependsOn cannot acquire local params", func(t *testing.T) {
		ws, cloud, publisher := sessionPrerequisiteFixture()
		cloud.Jobs["deploy"].CommandDependsOn = []string{"!publish"}

		_, err := Plan(ws, []string{"deploy"}, ws.Projects[1:2],
			[]*extension.ExtensionDescription{cloud, publisher},
			extension.ParamMap{"apps": "/B"}, nil, nil)
		assertSessionPrerequisiteError(t, err, "publish", "already planned", "local parameter")
	})

	t.Run("prerequisite params cannot overwrite literal step params", func(t *testing.T) {
		ws, cloud, publisher := sessionPrerequisiteFixture()
		publisher.Jobs["publish"].PipelineSteps[0].With = map[string]extension.InputBinding{
			"docker": {Value: false, HasValue: true},
		}

		_, err := Plan(ws, []string{"deploy"}, ws.Projects[1:2],
			[]*extension.ExtensionDescription{cloud, publisher},
			extension.ParamMap{"apps": "/B"}, nil, nil)
		assertSessionPrerequisiteError(t, err, "docker", "literal", "local parameter")
	})

	t.Run("prerequisite params cannot overwrite a literal camel-case alias", func(t *testing.T) {
		ws, cloud, publisher := sessionPrerequisiteFixture()
		cloud.Jobs["publish"].PipelineSteps[1].With = map[string]extension.InputBinding{
			"publish-config": {Value: true, HasValue: true},
		}

		_, err := Plan(ws, []string{"deploy"}, ws.Projects[1:2],
			[]*extension.ExtensionDescription{cloud, publisher},
			extension.ParamMap{"apps": "/B"}, nil, nil)
		assertSessionPrerequisiteError(t, err, "publishConfig", "publish-config", "literal", "local parameter")
	})

	t.Run("missing fromProjectParam fails closed", func(t *testing.T) {
		ws, cloud, publisher := sessionPrerequisiteFixture()
		ws.Projects[1].Config.Options["@test/cloud:deploy"]["deploy"] = extension.ParamMap{}

		_, err := Plan(ws, []string{"deploy"}, ws.Projects[1:2],
			[]*extension.ExtensionDescription{cloud, publisher},
			extension.ParamMap{"apps": "/B"}, nil, nil)
		assertSessionPrerequisiteError(t, err, "publishConfig", "not found", "/B")
	})
}

func TestPlan_SessionPrerequisiteWorkspaceOnceReuse(t *testing.T) {
	t.Parallel()
	ws, orchestrator, publisher := workspaceOnceSessionPrerequisiteFixture()

	t.Run("compatible constants merge two resolved selections", func(t *testing.T) {
		planned, err := Plan(ws, []string{"deploy"}, ws.Projects,
			[]*extension.ExtensionDescription{orchestrator, publisher},
			extension.ParamMap{"appsA": "/A", "appsB": "/B"}, nil, nil)
		if err != nil {
			t.Fatalf("Plan: %v", err)
		}
		publish := findSessionPrerequisiteJob(planned, "test-workspace", "@test/workspace-publisher", "publish", "emit")
		if publish == nil {
			t.Fatalf("workspace-once publish missing; jobs=%v", jobKeys(planned))
		}
		gotProjects := make([]string, 0, len(publish.SelectedProjects))
		for _, project := range publish.SelectedProjects {
			gotProjects = append(gotProjects, project.ID)
		}
		if want := []string{"/A", "/B"}; !reflect.DeepEqual(gotProjects, want) {
			t.Fatalf("merged SelectedProjects = %v, want %v", gotProjects, want)
		}
		if got := publish.JobDef.BoundParams["channel"]; got != "canary" {
			t.Fatalf("workspace-once constant param = %v, want canary", got)
		}
	})

	t.Run("multi-project fromProjectParam is ambiguous", func(t *testing.T) {
		orchestrator.Jobs["deploy"].SessionPrerequisites = []extension.SessionPrerequisiteDefinition{{
			Command: "publish", ProjectsFromParam: "appsA",
			Params: map[string]extension.SessionPrerequisiteParamBinding{
				"channel": {FromProjectParam: "release.channel"},
			},
		}}
		_, err := Plan(ws, []string{"deploy"}, ws.Projects,
			[]*extension.ExtensionDescription{orchestrator, publisher},
			extension.ParamMap{"appsA": "/A,/B"}, nil, nil)
		assertSessionPrerequisiteError(t, err, "workspace-once", "fromProjectParam", "multiple projects")
	})

	t.Run("different gate policies cannot widen a reused workspace prerequisite", func(t *testing.T) {
		ws, orchestrator, publisher := workspaceOnceSessionPrerequisiteFixture()
		orchestrator.Jobs["deploy"].SessionPrerequisites[0].DependsOn = []string{"lint"}
		orchestrator.Jobs["lint"] = sessionPrerequisiteJob("@test/orchestrator", "lint", "check", "lint", "")
		orchestrator.Jobs["lint"].Flags = map[string]extension.FlagDefinition{
			"enabled": {Type: "boolean", Default: false},
		}
		orchestrator.Jobs["lint"].PipelineSteps[0].If = "params.enabled"
		orchestrator.Tasks["lint"] = extension.TaskDefinition{Kind: "command", Command: "run"}
		ws.Projects[0].Config.Options["@test/orchestrator:lint"] = extension.ParamMap{"enabled": false}
		ws.Projects[1].Config.Options["@test/orchestrator:lint"] = extension.ParamMap{"enabled": true}

		_, err := Plan(ws, []string{"deploy"}, ws.Projects,
			[]*extension.ExtensionDescription{orchestrator, publisher},
			extension.ParamMap{"appsA": "/A", "appsB": "/B"}, nil, nil)
		assertSessionPrerequisiteError(t, err, "workspace-once", "publish", "gate policy")
	})

	t.Run("explicit selection cannot escape prerequisite gate provenance", func(t *testing.T) {
		for _, commands := range [][]string{{"publish", "deploy"}, {"deploy", "publish"}} {
			ws, orchestrator, publisher := workspaceOnceSessionPrerequisiteFixture()
			orchestrator.Jobs["deploy"].SessionPrerequisites = []extension.SessionPrerequisiteDefinition{{
				Command: "publish", ProjectsFromParam: "appsB", DependsOn: []string{"lint"},
			}}
			orchestrator.Jobs["lint"] = sessionPrerequisiteJob("@test/orchestrator", "lint", "check", "lint", "")
			orchestrator.Jobs["lint"].Flags = map[string]extension.FlagDefinition{
				"enabled": {Type: "boolean", Default: false},
			}
			orchestrator.Jobs["lint"].PipelineSteps[0].If = "params.enabled"
			orchestrator.Tasks["lint"] = extension.TaskDefinition{Kind: "command", Command: "run"}
			ws.Projects[0].Config.Options["@test/orchestrator:lint"] = extension.ParamMap{"enabled": true}
			ws.Projects[1].Config.Options["@test/orchestrator:lint"] = extension.ParamMap{"enabled": false}

			_, err := Plan(ws, commands, ws.Projects[:1],
				[]*extension.ExtensionDescription{orchestrator, publisher},
				extension.ParamMap{"appsB": "/B"}, nil, nil)
			assertSessionPrerequisiteError(t, err, "workspace-once", "publish", "/A", "gate policy provenance")
		}
	})
}

func TestPlan_SessionPrerequisiteIndirectCycleRejected(t *testing.T) {
	t.Parallel()
	ws, cloud, _ := sessionPrerequisiteFixture()
	cloud.Jobs["deploy"].SessionPrerequisites = []extension.SessionPrerequisiteDefinition{{Command: "publish"}}
	cloud.Jobs["publish"].SessionPrerequisites = []extension.SessionPrerequisiteDefinition{{Command: "release"}}
	cloud.Jobs["release"] = &extension.JobDefinition{
		ExtensionName: "@test/cloud", Name: "release", Activation: "workspace",
		SessionPrerequisites: []extension.SessionPrerequisiteDefinition{{Command: "deploy"}},
		PipelineSteps:        []extension.PipelineStep{{ID: "release", Task: "publish-migration"}},
	}

	_, err := Plan(ws, []string{"deploy"}, ws.Projects[:1],
		[]*extension.ExtensionDescription{cloud}, nil, nil, nil)
	assertSessionPrerequisiteError(t, err, "cycle")
}

func TestPlan_SessionPrerequisiteFinalizersStayOutsideFunctionalDAG(t *testing.T) {
	t.Parallel()
	ws, cloud, publisher := sessionPrerequisiteFixture()
	cloud.Jobs["test"].PipelineSteps = []extension.PipelineStep{
		{ID: "setup", Task: "test-setup"},
		{ID: "run", Task: "test-run", DependsOn: []string{"setup"}},
		{
			ID: "teardown", Task: "test-teardown", RunOn: extensionproto.StepRunOnFinally,
			Finalizes: &extensionproto.FinalizesRelation{Producer: "setup", Consumers: []string{"run"}},
		},
	}
	cloud.Tasks["test-setup"] = extension.TaskDefinition{Kind: "command", Command: "run"}
	cloud.Tasks["test-teardown"] = extension.TaskDefinition{Kind: "command", Command: "run"}

	planned, err := Plan(ws, []string{"deploy"}, ws.Projects[:1],
		[]*extension.ExtensionDescription{cloud, publisher},
		extension.ParamMap{"apps": "/B"}, nil, nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	publish := findSessionPrerequisiteJob(planned, "/B", "@test/cloud", "publish", "migration")
	if publish == nil {
		t.Fatalf("publish missing; jobs=%v", jobKeys(planned))
	}
	wantLeaf := "/B:test~run"
	for _, dependency := range publish.DependsOn {
		if dependency == "/B:test~teardown" {
			t.Fatalf("publish depends on held finalizer %q; dependencies=%v", dependency, publish.DependsOn)
		}
	}
	if !stringSliceContains(publish.DependsOn, wantLeaf) {
		t.Fatalf("publish dependencies = %v, want functional test leaf %q", publish.DependsOn, wantLeaf)
	}
	if err := validatePlanDAG(schedulableJobs(planned)); err != nil {
		t.Fatalf("DAG after holding finalizers is not schedulable: %v", err)
	}
}

func TestPlan_SessionPrerequisiteEmptyGateFacts(t *testing.T) {
	t.Parallel()
	t.Run("an unknown gate remains a planning error", func(t *testing.T) {
		ws, cloud, publisher := sessionPrerequisiteFixture()
		cloud.Jobs["deploy"].SessionPrerequisites[0].DependsOn = []string{"lnti"}

		_, err := Plan(ws, []string{"deploy"}, ws.Projects[:1],
			[]*extension.ExtensionDescription{cloud, publisher},
			extension.ParamMap{"apps": "/B"}, nil, nil)
		assertSessionPrerequisiteError(t, err, `unknown gate "lnti"`)
	})

	t.Run("declared gate with zero contributors is an explicit no-op", func(t *testing.T) {
		ws, cloud, publisher := sessionPrerequisiteFixture()
		cloud.Jobs["lint"].Flags = map[string]extension.FlagDefinition{
			"enabled": {Type: "boolean", Default: false},
		}
		cloud.Jobs["lint"].PipelineSteps[0].If = "params.enabled"

		planned, err := Plan(ws, []string{"deploy"}, ws.Projects[:1],
			[]*extension.ExtensionDescription{cloud, publisher},
			extension.ParamMap{"apps": "/B"}, nil, nil)
		if err != nil {
			t.Fatalf("Plan: %v", err)
		}
		publishes := sessionPrerequisiteCommandJobs(planned, "/B", "publish")
		if len(publishes) == 0 {
			t.Fatalf("publish prerequisite missing; jobs=%v", jobKeys(planned))
		}
		for _, publish := range publishes {
			if got, want := sessionPrerequisiteNoopGates(publish), []string{"lint"}; !reflect.DeepEqual(got, want) {
				t.Fatalf("%s no-op gates = %v, want %v", publish.Key(), got, want)
			}
			for _, realGate := range []string{"test", "build", "validate", "validate-workspace"} {
				if stringSliceContains(sessionPrerequisiteNoopGates(publish), realGate) {
					t.Fatalf("%s marked real gate %q as no-op: %v", publish.Key(), realGate, sessionPrerequisiteNoopGates(publish))
				}
			}
		}
	})

	t.Run("a partial contributor remains a real functional leaf", func(t *testing.T) {
		ws, cloud, publisher := sessionPrerequisiteFixture()
		ws.Projects[2].Config.Options["@test/cloud:deploy"] = extension.ParamMap{
			"deployPrerequisite": true,
			"deploy":             extension.ParamMap{"enabled": true, "publishConfig": true},
		}
		ws.Projects[1].Config.Options["@test/cloud:lint"] = extension.ParamMap{"enabled": true}
		ws.Projects[2].Config.Options["@test/cloud:lint"] = extension.ParamMap{"enabled": false}
		cloud.Jobs["lint"].Flags = map[string]extension.FlagDefinition{
			"enabled": {Type: "boolean", Default: false},
		}
		cloud.Jobs["lint"].PipelineSteps[0].If = "params.enabled"

		planned, err := Plan(ws, []string{"deploy"}, ws.Projects[:1],
			[]*extension.ExtensionDescription{cloud, publisher},
			extension.ParamMap{"apps": "/B,/C"}, nil, nil)
		if err != nil {
			t.Fatalf("Plan: %v", err)
		}
		const lintLeaf = "/B:lint~check"
		if findSessionPrerequisiteJob(planned, "/C", "@test/cloud", "lint", "check") != nil {
			t.Fatalf("disabled /C lint contributor was planned; jobs=%v", jobKeys(planned))
		}
		for _, projectID := range []string{"/B", "/C"} {
			for _, publish := range sessionPrerequisiteCommandJobs(planned, projectID, "publish") {
				if got := sessionPrerequisiteNoopGates(publish); stringSliceContains(got, "lint") {
					t.Fatalf("%s marked partially contributed lint as no-op: %v", publish.Key(), got)
				}
				if !stringSliceContains(publish.DependsOn, lintLeaf) {
					t.Fatalf("%s deps = %v, want real lint leaf %s", publish.Key(), publish.DependsOn, lintLeaf)
				}
			}
		}
	})

	t.Run("an omitted gate never manufactures a no-op fact", func(t *testing.T) {
		ws, cloud, publisher := sessionPrerequisiteFixture()
		delete(cloud.Jobs, "lint")
		definition := &cloud.Jobs["deploy"].SessionPrerequisites[0]
		definition.DependsOn = []string{"test", "build", "validate", "validate-workspace"}

		planned, err := Plan(ws, []string{"deploy"}, ws.Projects[:1],
			[]*extension.ExtensionDescription{cloud, publisher},
			extension.ParamMap{"apps": "/B"}, nil, nil)
		if err != nil {
			t.Fatalf("Plan: %v", err)
		}
		for _, publish := range sessionPrerequisiteCommandJobs(planned, "/B", "publish") {
			if got := sessionPrerequisiteNoopGates(publish); len(got) != 0 {
				t.Fatalf("%s inherited undeclared no-op gates %v", publish.Key(), got)
			}
		}
	})
}

func TestPlan_SessionPrerequisiteTransitiveSelectionUsesOwner(t *testing.T) {
	t.Parallel()
	ws, cloud, publisher := sessionPrerequisiteFixture()
	cloud.Jobs["publish"].SessionPrerequisites = []extension.SessionPrerequisiteDefinition{{Command: "release"}}
	cloud.Jobs["release"] = sessionPrerequisiteJob("@test/cloud", "release", "emit", "release", "")
	cloud.Tasks["release"] = extension.TaskDefinition{Kind: "command", Command: "run"}

	planned, err := Plan(ws, []string{"deploy"}, ws.Projects[:1],
		[]*extension.ExtensionDescription{cloud, publisher},
		extension.ParamMap{"apps": "/B"}, nil, nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if findSessionPrerequisiteJob(planned, "/B", "@test/cloud", "release", "emit") == nil {
		t.Fatalf("publish(B) prerequisite release did not retain B; jobs=%v", jobKeys(planned))
	}
	if findSessionPrerequisiteJob(planned, "/A", "@test/cloud", "release", "emit") != nil {
		t.Fatalf("publish(B) prerequisite release fell back to root selection A; jobs=%v", jobKeys(planned))
	}
}

func TestPlan_SessionPrerequisiteTransitiveParamsAndFixpoint(t *testing.T) {
	t.Parallel()
	t.Run("synthetic params activate the next prerequisite", func(t *testing.T) {
		ws, cloud, publisher := sessionPrerequisiteFixture()
		cloud.Jobs["deploy"].SessionPrerequisites[0].Params["mode"] = extension.SessionPrerequisiteParamBinding{Value: "canary"}
		cloud.Jobs["publish"].Flags["mode"] = extension.FlagDefinition{Type: "string", Default: "stable"}
		cloud.Jobs["publish"].SessionPrerequisites = []extension.SessionPrerequisiteDefinition{{
			Command: "release", If: "params.mode == 'canary'",
		}}
		cloud.Jobs["release"] = sessionPrerequisiteJob("@test/cloud", "release", "emit", "release", "")
		cloud.Tasks["release"] = extension.TaskDefinition{Kind: "command", Command: "run"}

		planned, err := Plan(ws, []string{"deploy"}, ws.Projects[:1],
			[]*extension.ExtensionDescription{cloud, publisher},
			extension.ParamMap{"apps": "/B"}, nil, nil)
		if err != nil {
			t.Fatalf("Plan: %v", err)
		}
		if findSessionPrerequisiteJob(planned, "/B", "@test/cloud", "release", "emit") == nil {
			t.Fatalf("publish(mode=canary) did not activate transitive release; jobs=%v", jobKeys(planned))
		}
	})

	t.Run("workspace-once selection mutation advances the fixpoint", func(t *testing.T) {
		ws, orchestrator, publisher := workspaceOnceSessionPrerequisiteFixture()
		orchestrator.Jobs["deploy"].SessionPrerequisites = []extension.SessionPrerequisiteDefinition{{
			Command: "publish", ProjectsFromParam: "appsB",
		}}
		publisher.Jobs["publish"].SessionPrerequisites = []extension.SessionPrerequisiteDefinition{{
			Command: "release", ProjectIf: "params.release.enabled",
		}}
		publisher.Jobs["release"] = sessionPrerequisiteJob("@test/workspace-publisher", "release", "emit", "release", "")
		publisher.Tasks["release"] = extension.TaskDefinition{Kind: "command", Command: "run"}
		ws.Projects[0].Config.Options["@test/workspace-publisher:publish"] = extension.ParamMap{
			"release": extension.ParamMap{"enabled": false},
		}
		ws.Projects[1].Config.Options["@test/workspace-publisher:publish"] = extension.ParamMap{
			"release": extension.ParamMap{"enabled": true},
		}

		planned, err := Plan(ws, []string{"publish", "deploy"}, ws.Projects[:1],
			[]*extension.ExtensionDescription{orchestrator, publisher},
			extension.ParamMap{"appsB": "/B"}, nil, nil)
		if err != nil {
			t.Fatalf("Plan: %v", err)
		}
		if findSessionPrerequisiteJob(planned, "/B", "@test/workspace-publisher", "release", "emit") == nil {
			t.Fatalf("workspace publish union A+B did not advance to transitive release(B); jobs=%v", jobKeys(planned))
		}
	})
}

func TestSessionPrerequisiteParamProjectionMatchesDispatch(t *testing.T) {
	t.Parallel()
	for _, order := range [][]string{
		{"foo-bar", "fooBar", "foo--bar"},
		{"foo--bar", "fooBar", "foo-bar"},
	} {
		exact := make(extension.ParamMap)
		for _, name := range order {
			switch name {
			case "foo-bar":
				exact[name] = "kebab"
			case "foo--bar":
				exact[name] = "double-kebab"
			case "fooBar":
				exact[name] = "camel"
			}
		}
		projected, sources := projectParamAliases(exact)
		want := extension.ParamMap{
			"foo-bar":  "kebab",
			"foo--bar": "double-kebab",
			"fooBar":   "camel",
		}
		if !reflect.DeepEqual(projected, want) {
			t.Fatalf("projection for insertion order %v = %#v, want every exact key plus camel alias %#v", order, projected, want)
		}
		for name := range want {
			if sources[name] != name {
				t.Fatalf("projection source for exact %q = %q, want exact spelling", name, sources[name])
			}
		}
	}

	for _, order := range [][]string{
		{"foo-bar", "foo--bar"},
		{"foo--bar", "foo-bar"},
	} {
		exact := make(extension.ParamMap)
		for _, name := range order {
			exact[name] = name
		}
		projected, sources := projectParamAliases(exact)
		if got := projected["fooBar"]; got != "foo--bar" {
			t.Fatalf("collapsed alias for insertion order %v = %v, want deterministic sorted source foo--bar", order, got)
		}
		if sources["fooBar"] != "foo--bar" || projected["foo-bar"] != "foo-bar" || projected["foo--bar"] != "foo--bar" {
			t.Fatalf("multi-spelling projection for insertion order %v = %#v sources=%#v", order, projected, sources)
		}
	}

	job := &ScheduledJob{
		Project:   &workspace.Project{ID: "/A", Name: "A", Path: "A", Config: &wsproto.ProjectConfig{}},
		Extension: &extension.ExtensionDescription{Name: "@test/publisher"},
		JobDef: &extension.JobDefinition{
			ExtensionName: "@test/publisher", Name: "publish",
			BoundParams: extension.ParamMap{"foo-bar": "bound-kebab"},
		},
	}
	params := resolvedJobParams(job, extension.ParamMap{"fooBar": "exact-camel"}, nil)
	if params["foo-bar"] != "bound-kebab" || params["fooBar"] != "bound-kebab" {
		t.Fatalf("dispatch projection = %#v, want higher-precedence bound layer to expose both spellings", params)
	}
}

func TestPlan_SessionPrerequisiteKebabOverrideActivatesCamelCondition(t *testing.T) {
	t.Parallel()
	ws, cloud, publisher := sessionPrerequisiteFixture()
	cloud.Jobs["deploy"].SessionPrerequisites[0].Params["foo-bar"] = extension.SessionPrerequisiteParamBinding{Value: true}
	publisher.Jobs["publish"].Flags["fooBar"] = extension.FlagDefinition{Type: "boolean", Default: false}
	publisher.Jobs["publish"].PipelineSteps = append(publisher.Jobs["publish"].PipelineSteps,
		extension.PipelineStep{ID: "alias", Task: "publish-alias", If: "params.fooBar"})
	publisher.Tasks["publish-alias"] = extension.TaskDefinition{Kind: "command", Command: "run"}

	planned, err := Plan(ws, []string{"deploy"}, ws.Projects[:1],
		[]*extension.ExtensionDescription{cloud, publisher},
		extension.ParamMap{"apps": "/B"}, nil, nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	alias := findSessionPrerequisiteJob(planned, "/B", "@test/go", "publish", "alias")
	if alias == nil {
		t.Fatalf("foo-bar prerequisite override did not activate if params.fooBar; jobs=%v", jobKeys(planned))
	}
	if alias.JobDef.BoundParams["foo-bar"] != true {
		t.Fatalf("alias task BoundParams = %#v, want exact foo-bar override retained", alias.JobDef.BoundParams)
	}
}

func TestPlan_SessionPrerequisiteInvalidExpressionsFailClosed(t *testing.T) {
	t.Parallel()
	t.Run("malformed command condition", func(t *testing.T) {
		ws, cloud, publisher := sessionPrerequisiteFixture()
		cloud.Jobs["deploy"].SessionPrerequisites[0].If = "!params.preview &&"
		_, err := Plan(ws, []string{"deploy"}, ws.Projects[:1],
			[]*extension.ExtensionDescription{cloud, publisher}, nil, nil, nil)
		assertSessionPrerequisiteError(t, err, "session prerequisite", "if", "invalid", "!params.preview &&")
	})

	t.Run("misspelled project condition path", func(t *testing.T) {
		ws, cloud, publisher := sessionPrerequisiteFixture()
		cloud.Jobs["deploy"].SessionPrerequisites[0].ProjectIf = "param.deploy.enabled != false"
		_, err := Plan(ws, []string{"deploy"}, ws.Projects[:1],
			[]*extension.ExtensionDescription{cloud, publisher}, nil, nil, nil)
		assertSessionPrerequisiteError(t, err, "session prerequisite", "projectIf", "invalid", "param.deploy.enabled != false")
	})
}

// One owner, two relations, disjoint selections: the deploy owner gates its
// apps' publish behind the five verification commands while a second relation
// packages a deploy-disabled dependency project behind the same commands. Every
// prerequisite root must wait for the UNION of the owner's gate leaves — the
// per-relation wiring this pins replaced left one relation's leaves invisible
// to the sibling's jobs, and the process-capability contract (every protected
// job after every leaf of each required command over the FINAL plan) failed
// closed on the first production deploy DAG that packaged a dependency,
// with the error "capability job … is not functionally after required
// leaf …:build~cloud-image-layers".
func TestPlan_SessionPrerequisiteSharedOwnerGateFrontier(t *testing.T) {
	ws, cloud, publisher := sessionPrerequisiteFixture()

	dependencyBase := &workspace.Project{
		ID: "/E", Name: "E", Path: "E", Extensions: []string{"@test/cloud", "@test/go"},
		Config: &wsproto.ProjectConfig{Options: map[string]extension.ParamMap{
			"@test/cloud:deploy": {"deployPrerequisite": true, "deploy": extension.ParamMap{
				"enabled": false, "packageAsDependency": true,
			}},
		}},
	}
	ws = workspace.NewWorkspace("/workspace", nil, append(append([]*workspace.Project(nil), ws.Projects...), dependencyBase))
	ws.Name = "test-workspace"

	gates := []string{"lint", "test", "build", "validate", "validate-workspace"}
	cloud.Jobs["deploy"].SessionPrerequisites = append(cloud.Jobs["deploy"].SessionPrerequisites,
		extension.SessionPrerequisiteDefinition{
			Command: "package", If: "!params.preview", ProjectsFromParam: "apps",
			ProjectIf: "params.deploy.enabled == false && params.deploy.packageAsDependency == true",
			DependsOn: gates,
		})
	cloud.Jobs["package"] = &extension.JobDefinition{
		ExtensionName: "@test/cloud", Name: "package", Activation: "workspace",
		PipelineSteps: []extension.PipelineStep{{ID: "image", Task: "package-image"}},
	}
	cloud.Tasks["package-image"] = extension.TaskDefinition{Kind: "command", Command: "run"}

	planned, err := Plan(ws, []string{"deploy"}, ws.Projects[:1],
		[]*extension.ExtensionDescription{cloud, publisher},
		extension.ParamMap{"apps": "/B,/E"}, nil, nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	frontier := []string{
		"/B:build~compile", "/B:lint~check", "/B:test~run", "/B:validate~features",
		"/E:build~compile", "/E:lint~check", "/E:test~run", "/E:validate~features",
		"test-workspace:validate-workspace~architecture",
	}

	pkg := findSessionPrerequisiteJob(planned, "/E", "@test/cloud", "package", "image")
	if pkg == nil {
		t.Fatalf("dependency package missing; jobs=%v", jobKeys(planned))
	}
	if !reflect.DeepEqual(pkg.DependsOn, frontier) {
		t.Fatalf("package frontier = %v, want the owner union %v", pkg.DependsOn, frontier)
	}

	for _, step := range []string{"docker"} {
		publish := findSessionPrerequisiteJob(planned, "/B", "@test/go", "publish", step)
		if publish == nil {
			t.Fatalf("publish %s missing; jobs=%v", step, jobKeys(planned))
		}
		if !reflect.DeepEqual(publish.DependsOn, frontier) {
			t.Fatalf("publish %s frontier = %v, want the owner union %v", step, publish.DependsOn, frontier)
		}
	}

	deploy := findSessionPrerequisiteJob(planned, "test-workspace", "@test/cloud", "deploy", "apply")
	if deploy == nil {
		t.Fatalf("deploy missing; jobs=%v", jobKeys(planned))
	}
	for _, leaf := range []string{pkg.Key()} {
		if !stringSliceContains(deploy.DependsOn, leaf) {
			t.Fatalf("deploy deps %v miss dependency package leaf %s", deploy.DependsOn, leaf)
		}
	}
}

func sessionPrerequisiteFixture() (*workspace.Workspace, *extension.ExtensionDescription, *extension.ExtensionDescription) {
	project := func(id string, cloudOptions extension.ParamMap) *workspace.Project {
		return &workspace.Project{
			ID: id, Name: id[1:], Path: id[1:], Extensions: []string{"@test/cloud", "@test/go"},
			Config: &wsproto.ProjectConfig{Options: map[string]extension.ParamMap{"@test/cloud:deploy": cloudOptions}},
		}
	}
	projects := []*workspace.Project{
		project("/A", extension.ParamMap{"deployPrerequisite": true, "deploy": extension.ParamMap{"publishConfig": true}}),
		project("/B", extension.ParamMap{"deployPrerequisite": true, "deploy": extension.ParamMap{"publishConfig": false}}),
		project("/C", extension.ParamMap{"deployPrerequisite": true, "deploy": extension.ParamMap{"enabled": false}}),
		project("/D", extension.ParamMap{"deployPrerequisite": false}),
	}
	ws := workspace.NewWorkspace("/workspace", nil, projects)
	ws.Name = "test-workspace"

	cloud := &extension.ExtensionDescription{
		Name: "@test/cloud",
		Jobs: map[string]*extension.JobDefinition{
			"deploy": {
				ExtensionName: "@test/cloud", Name: "deploy", Activation: "workspace-once",
				Flags: map[string]extension.FlagDefinition{
					"preview": {Type: "boolean", Default: false},
					"apps":    {Type: "string", Default: ""},
				},
				SessionPrerequisites: []extension.SessionPrerequisiteDefinition{{
					Command: "publish", If: "!params.preview", ProjectsFromParam: "apps",
					ProjectIf: "params.deploy.enabled != false && params.deployPrerequisite != false",
					DependsOn: []string{"lint", "test", "build", "validate", "validate-workspace"},
					Params: map[string]extension.SessionPrerequisiteParamBinding{
						"docker":        {Value: true},
						"publishConfig": {FromProjectParam: "deploy.publishConfig"},
					},
				}},
				PipelineSteps: []extension.PipelineStep{{ID: "apply", Task: "deploy-apply"}},
			},
			"publish": {
				ExtensionName: "@test/cloud", Name: "publish", Activation: "workspace",
				Flags: map[string]extension.FlagDefinition{
					"publishConfig": {Type: "boolean", Default: true},
				},
				PipelineSteps: []extension.PipelineStep{
					{ID: "config", Task: "publish-config", If: "params.publishConfig != false"},
					{ID: "migration", Task: "publish-migration"},
				},
			},
			"lint":               sessionPrerequisiteJob("@test/cloud", "lint", "check", "lint-run", ""),
			"test":               sessionPrerequisiteJob("@test/cloud", "test", "run", "test-run", ""),
			"build":              sessionPrerequisiteJob("@test/cloud", "build", "compile", "build-compile", ""),
			"validate":           sessionPrerequisiteJob("@test/cloud", "validate", "features", "validate-features", ""),
			"validate-workspace": sessionPrerequisiteJob("@test/cloud", "validate-workspace", "architecture", "validate-architecture", "workspace-once"),
		},
		Tasks: map[string]extension.TaskDefinition{
			"deploy-apply":          {Kind: "command", Command: "run"},
			"publish-config":        {Kind: "command", Command: "run"},
			"publish-migration":     {Kind: "command", Command: "run"},
			"lint-run":              {Kind: "command", Command: "run"},
			"test-run":              {Kind: "command", Command: "run"},
			"build-compile":         {Kind: "command", Command: "run"},
			"validate-features":     {Kind: "command", Command: "run"},
			"validate-architecture": {Kind: "command", Command: "run"},
		},
	}
	publisher := &extension.ExtensionDescription{
		Name: "@test/go",
		Jobs: map[string]*extension.JobDefinition{
			"publish": {
				ExtensionName: "@test/go", Name: "publish", Activation: "workspace",
				Flags: map[string]extension.FlagDefinition{
					"docker": {Type: "boolean", Default: false},
				},
				PipelineSteps: []extension.PipelineStep{{ID: "docker", Task: "publish-docker", If: "params.docker"}},
			},
		},
		Tasks: map[string]extension.TaskDefinition{
			"publish-docker": {Kind: "command", Command: "run"},
		},
	}
	return ws, cloud, publisher
}

func workspaceOnceSessionPrerequisiteFixture() (*workspace.Workspace, *extension.ExtensionDescription, *extension.ExtensionDescription) {
	project := func(id, channel string) *workspace.Project {
		return &workspace.Project{
			ID: id, Name: id[1:], Path: id[1:], Extensions: []string{"@test/orchestrator", "@test/workspace-publisher"},
			Config: &wsproto.ProjectConfig{Options: map[string]extension.ParamMap{
				"@test/orchestrator:deploy": {"release": extension.ParamMap{"channel": channel}},
			}},
		}
	}
	ws := workspace.NewWorkspace("/workspace", nil, []*workspace.Project{
		project("/A", "stable"), project("/B", "canary"),
	})
	ws.Name = "test-workspace"
	orchestrator := &extension.ExtensionDescription{
		Name: "@test/orchestrator",
		Jobs: map[string]*extension.JobDefinition{
			"deploy": {
				ExtensionName: "@test/orchestrator", Name: "deploy", Activation: "workspace-once",
				Flags: map[string]extension.FlagDefinition{
					"appsA": {Type: "string"}, "appsB": {Type: "string"},
				},
				SessionPrerequisites: []extension.SessionPrerequisiteDefinition{
					{Command: "publish", ProjectsFromParam: "appsA", Params: map[string]extension.SessionPrerequisiteParamBinding{"channel": {Value: "canary"}}},
					{Command: "publish", ProjectsFromParam: "appsB", Params: map[string]extension.SessionPrerequisiteParamBinding{"channel": {Value: "canary"}}},
				},
				PipelineSteps: []extension.PipelineStep{{ID: "apply", Task: "deploy"}},
			},
		},
		Tasks: map[string]extension.TaskDefinition{"deploy": {Kind: "command", Command: "run"}},
	}
	publisher := &extension.ExtensionDescription{
		Name: "@test/workspace-publisher",
		Jobs: map[string]*extension.JobDefinition{
			"publish": {
				ExtensionName: "@test/workspace-publisher", Name: "publish", Activation: "workspace-once",
				Flags:         map[string]extension.FlagDefinition{"channel": {Type: "string", Default: "stable"}},
				PipelineSteps: []extension.PipelineStep{{ID: "emit", Task: "publish"}},
			},
		},
		Tasks: map[string]extension.TaskDefinition{"publish": {Kind: "command", Command: "run"}},
	}
	return ws, orchestrator, publisher
}

func assertSessionPrerequisiteError(t *testing.T, err error, fragments ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("Plan unexpectedly succeeded")
	}
	for _, fragment := range fragments {
		if !strings.Contains(err.Error(), fragment) {
			t.Fatalf("Plan error %q does not contain %q", err, fragment)
		}
	}
}

func sessionPrerequisiteJob(extensionName, command, step, task, activation string) *extension.JobDefinition {
	return &extension.JobDefinition{
		ExtensionName: extensionName, Name: command, Activation: activation,
		PipelineSteps: []extension.PipelineStep{{ID: step, Task: task}},
	}
}

func findSessionPrerequisiteJob(jobs []*ScheduledJob, projectID, extensionName, command, step string) *ScheduledJob {
	for _, job := range jobs {
		if job.Project.ID == projectID && job.Extension != nil && job.Extension.Name == extensionName &&
			job.CommandName() == command && job.Step != nil && job.Step.ID == step {
			return job
		}
	}
	return nil
}

func sessionPrerequisiteCommandJobs(jobs []*ScheduledJob, projectID, command string) []*ScheduledJob {
	var matched []*ScheduledJob
	for _, job := range jobs {
		if job != nil && job.Project != nil && job.Project.ID == projectID && job.CommandName() == command {
			matched = append(matched, job)
		}
	}
	sort.Slice(matched, func(left, right int) bool { return matched[left].Key() < matched[right].Key() })
	return matched
}

// Reflection keeps the RED behavioral: before the model gains the execution-
// only fact, planning fails on the empty gate rather than this test failing to
// compile merely because the field does not exist yet.
func sessionPrerequisiteNoopGates(job *ScheduledJob) []string {
	if job == nil {
		return nil
	}
	field := reflect.ValueOf(job).Elem().FieldByName("SessionPrerequisiteNoopGates")
	if !field.IsValid() || field.Kind() != reflect.Slice {
		return nil
	}
	values := make([]string, field.Len())
	for index := 0; index < field.Len(); index++ {
		values[index] = field.Index(index).String()
	}
	return values
}

func stringSliceContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
