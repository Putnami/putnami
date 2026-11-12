package planner

import (
	"slices"
	"strings"
	"testing"

	jobmodel "go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs/cachekey"
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
func contractPlan(t *testing.T, providerDependsOnClient bool) []*jobmodel.ScheduledJob {
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
		},
	}
	projects := []*workspace.Project{provider, client}
	ws := workspace.NewWorkspace(root, nil, projects)
	ws.Name = "ws"

	planned, err := Plan(ws, []string{"build"}, projects,
		[]*extension.ExtensionDescription{contractBuildExtension()}, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	return planned
}

// TestPlan_TheContractEdgeOrdersNoJob is the acceptance of the contract edge
// in the plan: a client bound to its provider through generatedFrom generates
// from the provider's committed contract, so none of its jobs waits for a
// provider job, by a functional edge or an ordering one, and the provider
// contributes nothing to the client's cache keys (CLI ADR 0087).
func TestPlan_TheContractEdgeOrdersNoJob(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/workspace-discovery-selection", "a-client-binds-from-generated-from", "the-contract-edge-orders-no-job")
	planned := contractPlan(t, false)

	if findScheduled(planned, "/clients/catalog:build~generate") == nil {
		t.Fatalf("client generate missing from plan %v", planKeySet(planned))
	}
	for _, job := range planned {
		if job.Project.ID != "/clients/catalog" {
			continue
		}
		for _, edge := range append(append(slices.Clone(job.DependsOn), job.SerializeAfter...), cachekey.CacheKeyDependencies(job)...) {
			if strings.HasPrefix(edge, "/services/catalog:") {
				t.Errorf("client job %s waits for the provider job %s; the contract edge orders no job", job.Key(), edge)
			}
		}
	}
}

// TestPlan_AProviderThatImportsItsClientStillPlansADAG pins that a provider
// that imports its own generated client runs after it through the ordinary
// dependency edge, and the plan stays schedulable.
func TestPlan_AProviderThatImportsItsClientStillPlansADAG(t *testing.T) {
	t.Parallel()
	// Plan runs ValidatePlanDAG, so a nil error already proves the plan acyclic.
	planned := contractPlan(t, true)

	providerGenerate := findScheduled(planned, "/services/catalog:build~generate")
	if providerGenerate == nil {
		t.Fatalf("provider generate missing from plan %v", planKeySet(planned))
	}
	if !slices.Contains(providerGenerate.DependsOn, "/clients/catalog:build~generate") {
		t.Errorf("provider generate DependsOn = %v, want the client's generate through ^generate", providerGenerate.DependsOn)
	}
}
