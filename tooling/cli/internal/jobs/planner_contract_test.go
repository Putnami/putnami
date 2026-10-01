package jobs

import (
	"slices"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// contractBuildExtension is a two-step build pipeline whose first step carries
// the cross-project `^generate` reference every language extension declares.
func contractBuildExtension() *extension.ExtensionDescription {
	return &extension.ExtensionDescription{
		Name: "@putnami/ts",
		Jobs: map[string]*extension.JobDefinition{
			"build": {
				ExtensionName: "@putnami/ts",
				Name:          "build",
				Kind:          "command",
				Command:       "bun",
				Cache:         true,
				PipelineSteps: []extension.PipelineStep{
					{ID: "generate", Task: "build-generate", DependsOn: []string{"^generate"}},
					{ID: "transpile", Task: "build-transpile", DependsOn: []string{"generate"}},
				},
			},
		},
		Tasks: map[string]extension.TaskDefinition{
			"build-generate":  {Kind: "command", Command: "bun"},
			"build-transpile": {Kind: "command", Command: "bun"},
		},
	}
}

// contractPlan plans `build` for a provider, the generated client of its
// contract, and a consumer of that client.
func contractPlan(t *testing.T, providerDependsOnClient bool) []*ScheduledJob {
	t.Helper()
	root := t.TempDir()

	provider := &workspace.Project{
		ID: "/services/catalog", Name: "catalog", Path: "services/catalog",
		Extensions: []string{"@putnami/ts"}, ContractServiceID: "catalog.items",
	}
	if providerDependsOnClient {
		provider.Dependencies = []string{"catalog-client"}
	}
	client := &workspace.Project{
		ID: "/clients/catalog", Name: "catalog-client", Path: "clients/catalog",
		Extensions: []string{"@putnami/ts"},
		GeneratedClient: &workspace.GeneratedClientBinding{
			ServiceID:      "catalog.items",
			ContractSHA256: "1111111111111111111111111111111111111111111111111111111111111111",
			Language:       "ts",
			ManifestPath:   "clients/catalog/client.putnami.json",
		},
	}
	projects := []*workspace.Project{provider, client}
	ws := workspace.NewWorkspace(root, nil, projects)
	ws.Name = "ws"

	planned, err := Plan(ws, []string{"build"}, projects,
		[]*extension.ExtensionDescription{contractBuildExtension()}, nil, nil, nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	return planned
}

// TestPlan_ContractEdgeOrdersWithoutFoldingTheProvidersKey is the acceptance of
// the contract edge in the plan: the client's generate waits for the provider's
// generate, and the provider contributes nothing to the client's cache key.
//
// cacheKeyDependencies names exactly the plan keys computeJobCacheHashWith
// folds into a job's key, so the provider's absence from it IS the key
// independence: a provider implementation change with an unchanged contract
// leaves the client's key where it was and the cache answers.
func TestPlan_ContractEdgeOrdersWithoutFoldingTheProvidersKey(t *testing.T) {
	t.Parallel()
	planned := contractPlan(t, false)

	clientGenerate := findScheduled(planned, "/clients/catalog:build~generate")
	if clientGenerate == nil {
		t.Fatalf("client generate missing from plan %v", planKeySet(planned))
	}
	const providerGenerate = "/services/catalog:build~generate"

	if !slices.Contains(clientGenerate.SerializeAfter, providerGenerate) {
		t.Errorf("client generate SerializeAfter = %v, want it to wait for %s",
			clientGenerate.SerializeAfter, providerGenerate)
	}
	if slices.Contains(cacheKeyDependencies(clientGenerate), providerGenerate) {
		t.Errorf("client generate cache-key edges %v fold the provider's key; the contract is its only provider-side input",
			cacheKeyDependencies(clientGenerate))
	}

	// Nothing downstream of the client picks the provider up either: the fold
	// travels through DependsOn, and the ordering edge is not one.
	clientTranspile := findScheduled(planned, "/clients/catalog:build~transpile")
	if clientTranspile == nil {
		t.Fatal("client transpile missing from plan")
	}
	if slices.Contains(cacheKeyDependencies(clientTranspile), providerGenerate) {
		t.Errorf("client transpile cache-key edges %v fold the provider's key", cacheKeyDependencies(clientTranspile))
	}
}

// TestPlan_ContractEdgeKeepsThePlanADAG pins the refusal that keeps the plan
// schedulable: a provider that imports its own generated client already runs
// after it, so the contract edge is not added back in the other direction.
func TestPlan_ContractEdgeKeepsThePlanADAG(t *testing.T) {
	t.Parallel()
	planned := contractPlan(t, true)

	clientGenerate := findScheduled(planned, "/clients/catalog:build~generate")
	if clientGenerate == nil {
		t.Fatal("client generate missing from plan")
	}
	if slices.Contains(clientGenerate.SerializeAfter, "/services/catalog:build~generate") {
		t.Errorf("client generate waits for a provider that already waits for it: %v", clientGenerate.SerializeAfter)
	}
	// Plan runs validatePlanDAG, so reaching here at all means the plan has no
	// cycle; the assertion above states which edge was dropped to keep it so.
}

// clientTreeFixture places a provider, the generated client of its contract, and
// a project nested in the provider that is not a client.
type clientTreeFixture struct {
	// clientPath is the client project's workspace-relative directory.
	clientPath string
	// writerRoot is the root the generator's pathFrom output resolves under.
	writerRoot string
	// providerDependsOnClient makes the provider import its own client.
	providerDependsOnClient bool
	// clientOnly selects the client alone, so the provider is never planned.
	clientOnly bool
}

const (
	treeProvider = "/services/catalog"
	treeClient   = "/services/catalog/clients/ts"
	treeConsumer = "/services/catalog/consumer"
	// treeWriter is the provider job whose task declares the generated client.
	treeWriter = treeProvider + ":clientgen~generate-ts"
)

// clientTreeExtensions returns a language extension and a generator shaped like
// the real ones. The generator's `clientgen` command runs after `build` and
// declares the generated client as a durable directory at a path its run
// reports. The language extension's transpile declares a literal directory and
// a pathFrom FILE, and its `test` runs a finalizer.
func clientTreeExtensions(writerRoot string) []*extension.ExtensionDescription {
	lang := contractBuildExtension()
	lang.Jobs["lint"] = &extension.JobDefinition{
		ExtensionName: "@putnami/ts", Name: "lint", Kind: "command", Command: "bun",
		PipelineSteps: []extension.PipelineStep{{ID: "check", Task: "lint-check"}},
	}
	lang.Jobs["test"] = &extension.JobDefinition{
		ExtensionName: "@putnami/ts", Name: "test", Kind: "command", Command: "bun",
		PipelineSteps: []extension.PipelineStep{
			{ID: "setup", Task: "test-setup"},
			{ID: "run", Task: "test-run", DependsOn: []string{"setup"}},
			{
				ID: "teardown", Task: "test-teardown", RunOn: extensionproto.StepRunOnFinally,
				Finalizes: &extensionproto.FinalizesRelation{Producer: "setup", Consumers: []string{"run"}},
			},
		},
	}
	lang.Tasks["build-transpile"] = extension.TaskDefinition{
		Kind: "command", Command: "bun",
		Declares: &extension.TaskDeclaration{Outputs: map[string]extension.DeclaredOutput{
			"dist":     {Kind: extension.OutputKindDirectory, Path: "dist"},
			"manifest": {Kind: extension.OutputKindFile, PathFrom: "manifestPath"},
		}},
	}
	for _, task := range []string{"lint-check", "test-setup", "test-run", "test-teardown"} {
		lang.Tasks[task] = extension.TaskDefinition{Kind: "command", Command: "bun"}
	}
	generator := &extension.ExtensionDescription{
		Name: "@putnami/clientgen",
		Jobs: map[string]*extension.JobDefinition{
			"clientgen": {
				ExtensionName: "@putnami/clientgen", Name: "clientgen", Kind: "command", Command: "gen",
				CommandDependsOn: []string{"build"},
				PipelineSteps:    []extension.PipelineStep{{ID: "generate-ts", Task: "clientgen-ts"}},
			},
		},
		Tasks: map[string]extension.TaskDefinition{
			"clientgen-ts": {
				Kind: "command", Command: "gen",
				Declares: &extension.TaskDeclaration{Outputs: map[string]extension.DeclaredOutput{
					"client": {Kind: extension.OutputKindDirectory, Root: writerRoot, PathFrom: "typescriptClientOutput"},
				}},
			},
		},
	}
	return []*extension.ExtensionDescription{lang, generator}
}

// clientTreePlan plans build, lint, test and clientgen over the fixture.
func clientTreePlan(t *testing.T, fixture clientTreeFixture) []*ScheduledJob {
	t.Helper()
	provider := &workspace.Project{
		ID: treeProvider, Name: "catalog", Path: "services/catalog",
		Extensions: []string{"@putnami/ts", "@putnami/clientgen"}, ContractServiceID: "catalog.items",
	}
	if fixture.providerDependsOnClient {
		provider.Dependencies = []string{"catalog-client"}
	}
	client := &workspace.Project{
		ID: treeClient, Name: "catalog-client", Path: fixture.clientPath,
		Extensions: []string{"@putnami/ts"},
		GeneratedClient: &workspace.GeneratedClientBinding{
			ServiceID:      "catalog.items",
			ContractSHA256: "1111111111111111111111111111111111111111111111111111111111111111",
			Language:       "ts",
			ManifestPath:   fixture.clientPath + "/client.putnami.json",
		},
	}
	consumer := &workspace.Project{
		ID: treeConsumer, Name: "catalog-consumer", Path: "services/catalog/consumer",
		Extensions: []string{"@putnami/ts"},
	}
	projects := []*workspace.Project{provider, client, consumer}
	ws := workspace.NewWorkspace(t.TempDir(), nil, projects)
	ws.Name = "ws"
	selected := projects
	if fixture.clientOnly {
		selected = []*workspace.Project{client}
	}
	planned, err := Plan(ws, []string{"build", "lint", "test", "clientgen"}, selected,
		clientTreeExtensions(fixture.writerRoot), nil, nil, nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	return planned
}

// scheduledOrFatal returns the planned job with key, failing the test when the
// plan lacks it.
func scheduledOrFatal(t *testing.T, planned []*ScheduledJob, key string) *ScheduledJob {
	t.Helper()
	job := findScheduled(planned, key)
	if job == nil {
		t.Fatalf("%s missing from plan %v", key, planKeySet(planned))
	}
	return job
}

// TestPlan_ContractClientJobsWaitForTheProviderTreeWriter is the acceptance of
// the tree rule: every job of a client nested in its provider waits for the
// provider job whose task declares a durable directory at a reported path, and
// the wait is ordering alone.
func TestPlan_ContractClientJobsWaitForTheProviderTreeWriter(t *testing.T) {
	t.Parallel()
	planned := clientTreePlan(t, clientTreeFixture{
		clientPath: "services/catalog/clients/ts",
		writerRoot: extension.OutputRootProject,
	})

	// lint~check and test~setup declare no dependency at all, and
	// build~transpile references no provider step: only the tree rule orders
	// them.
	for _, step := range []string{"build~generate", "build~transpile", "lint~check", "test~setup", "test~run"} {
		job := scheduledOrFatal(t, planned, treeClient+":"+step)
		if !slices.Contains(job.SerializeAfter, treeWriter) {
			t.Errorf("%s SerializeAfter = %v, want it to wait for %s", job.Key(), job.SerializeAfter, treeWriter)
		}
		if slices.Contains(cacheKeyDependencies(job), treeWriter) {
			t.Errorf("%s cache-key edges %v fold the provider's writer; the edge is ordering alone",
				job.Key(), cacheKeyDependencies(job))
		}
		// A literal directory and a pathFrom FILE are not a client tree.
		if slices.Contains(job.SerializeAfter, treeProvider+":build~transpile") {
			t.Errorf("%s waits for the provider's transpile, which declares no pathFrom directory: %v",
				job.Key(), job.SerializeAfter)
		}
	}

	// The invocation runtime dispatches a finalizer, so the DAG orders none.
	if teardown := scheduledOrFatal(t, planned, treeClient+":test~teardown"); len(teardown.SerializeAfter) != 0 {
		t.Errorf("client finalizer carries ordering edges %v; the DAG never dispatches it", teardown.SerializeAfter)
	}
}

// TestPlan_ContractClientTreeRuleNeverWidensTheSelection pins the no-widening
// rule: a client selected alone waits for nothing, and nothing of its provider
// enters the plan.
func TestPlan_ContractClientTreeRuleNeverWidensTheSelection(t *testing.T) {
	t.Parallel()
	planned := clientTreePlan(t, clientTreeFixture{
		clientPath: "services/catalog/clients/ts",
		writerRoot: extension.OutputRootProject,
		clientOnly: true,
	})

	for _, job := range planned {
		if job.Project.ID == treeProvider {
			t.Errorf("provider job %s entered a plan that selected the client alone", job.Key())
		}
		if len(job.SerializeAfter) != 0 {
			t.Errorf("%s waits for %v with its provider unplanned", job.Key(), job.SerializeAfter)
		}
	}
	scheduledOrFatal(t, planned, treeClient+":lint~check")
}

// TestPlan_ContractClientTreeRuleKeepsThePlanADAG pins the cycle guard: when the
// provider imports its own client, the client job the writer depends on stays
// unordered with it, and every other client job still waits.
func TestPlan_ContractClientTreeRuleKeepsThePlanADAG(t *testing.T) {
	t.Parallel()
	// Plan runs validatePlanDAG, so a nil error already proves the plan acyclic.
	planned := clientTreePlan(t, clientTreeFixture{
		clientPath:              "services/catalog/clients/ts",
		writerRoot:              extension.OutputRootProject,
		providerDependsOnClient: true,
	})

	// The writer runs after the provider's build, whose generate imports the
	// client's generate.
	generate := scheduledOrFatal(t, planned, treeClient+":build~generate")
	if slices.Contains(generate.SerializeAfter, treeWriter) ||
		slices.Contains(generate.SerializeAfter, treeProvider+":build~generate") {
		t.Errorf("client generate waits for provider jobs that already wait for it: %v", generate.SerializeAfter)
	}
	for _, step := range []string{"build~transpile", "lint~check", "test~run"} {
		job := scheduledOrFatal(t, planned, treeClient+":"+step)
		if !slices.Contains(job.SerializeAfter, treeWriter) {
			t.Errorf("%s SerializeAfter = %v, want it to wait for %s", job.Key(), job.SerializeAfter, treeWriter)
		}
	}
}

// TestPlan_ContractClientTreeRuleOrdersClientsOnly pins that the rule follows
// the contract edge, not the directory: a project nested in the provider that
// is no generated client waits for nothing.
func TestPlan_ContractClientTreeRuleOrdersClientsOnly(t *testing.T) {
	t.Parallel()
	planned := clientTreePlan(t, clientTreeFixture{
		clientPath: "services/catalog/clients/ts",
		writerRoot: extension.OutputRootProject,
	})

	seen := 0
	for _, job := range planned {
		if job.Project.ID != treeConsumer {
			continue
		}
		seen++
		if len(job.SerializeAfter) != 0 {
			t.Errorf("non-client %s waits for %v", job.Key(), job.SerializeAfter)
		}
	}
	if seen == 0 {
		t.Fatalf("fixture planned no job for %s: %v", treeConsumer, planKeySet(planned))
	}
}

// TestPlan_ContractClientTreeRuleFollowsTheOutputRoot pins how the rule decides
// reach from the plan: a project-rooted output reaches a client only when the
// two directories nest, a workspace-rooted one reaches any client, and a
// command-output one reaches none.
func TestPlan_ContractClientTreeRuleFollowsTheOutputRoot(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		clientPath string
		writerRoot string
		wantWait   bool
	}{
		{"project root, client inside the provider", "services/catalog/clients/ts", extension.OutputRootProject, true},
		{"project root, client outside the provider", "clients/catalog", extension.OutputRootProject, false},
		{"project root, sibling sharing a name prefix", "services/catalog-clients/ts", extension.OutputRootProject, false},
		{"workspace root, client outside the provider", "clients/catalog", extension.OutputRootWorkspace, true},
		{"command-output root, client inside the provider", "services/catalog/clients/ts", extension.OutputRootCommandOutput, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			planned := clientTreePlan(t, clientTreeFixture{clientPath: tc.clientPath, writerRoot: tc.writerRoot})
			scheduledOrFatal(t, planned, treeWriter)
			check := scheduledOrFatal(t, planned, treeClient+":lint~check")
			if got := slices.Contains(check.SerializeAfter, treeWriter); got != tc.wantWait {
				t.Errorf("client lint~check waits for %s = %v, want %v (SerializeAfter %v)",
					treeWriter, got, tc.wantWait, check.SerializeAfter)
			}
		})
	}
}
