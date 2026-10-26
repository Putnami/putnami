package planner

import (
	"path/filepath"
	"slices"
	"sort"
	"testing"

	jobmodel "go.putnami.dev/cli/model/jobs"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// The dependents rule: a project that depends on a generated client compiles
// it, so none of its jobs runs while the client's generator rewrites it. The
// client lies in its own project, and a resource conflicts only within one
// project, so only the readByDependents output the generators declare orders
// the two (protocols/extension ADR 0014).

const (
	dependentsClient    = "/clients/auth"
	dependentsConsumer  = "/consumer"
	dependentsGenerator = dependentsClient + ":clientgen~generate-go"
)

// dependentsWorkspace is a Go client project that carries the clientgen
// extension and a Go application that depends on it.
func dependentsWorkspace(t *testing.T) (*workspace.Workspace, []*workspace.Project) {
	t.Helper()
	root := t.TempDir()
	writeProjectFile(t, root, "clients/auth", "go.mod", "module example.com/clients/auth\n\ngo 1.25\n")
	writeProjectFile(t, root, "clients/auth", "client.gen.go", "package auth\n")
	writeProjectFile(t, root, "consumer", "go.mod",
		"module example.com/consumer\n\ngo 1.25\n\nrequire example.com/clients/auth v0.0.0\n")
	writeProjectFile(t, root, "consumer", "main.go", "package main\n\nfunc main() {}\n")
	client := &workspace.Project{
		ID: dependentsClient, Name: "auth", Path: "clients/auth", Type: "library",
		Extensions: []string{"@putnami/go", clientgenExtensionName},
		Config:     wsproto.LoadProjectConfig(filepath.Join(root, "clients", "auth")),
	}
	consumer := &workspace.Project{
		ID: dependentsConsumer, Name: "consumer", Path: "consumer", Type: "application",
		Extensions:   []string{"@putnami/go"},
		Dependencies: []string{dependentsClient},
		Config:       wsproto.LoadProjectConfig(filepath.Join(root, "consumer")),
	}
	projects := []*workspace.Project{client, consumer}
	return testWorkspace(root, projects...), projects
}

// clientgenWithoutReadByDependents is the real clientgen manifest with the
// readByDependents flag cleared on every declared output: the plan as it was
// before the generators declared it.
func clientgenWithoutReadByDependents(t *testing.T) *extension.ExtensionDescription {
	t.Helper()
	description := loadClientgenExtension(t)
	for _, task := range description.Tasks {
		if task.Declares == nil {
			continue
		}
		for id, output := range task.Declares.Outputs {
			output.ReadByDependents = false
			task.Declares.Outputs[id] = output
		}
	}
	return description
}

// planEdges indexes every edge of a plan by the waiting job.
func planEdges(planned []*jobmodel.ScheduledJob) map[string][]string {
	edges := make(map[string][]string, len(planned))
	for _, job := range planned {
		edges[job.Key()] = append(append([]string(nil), job.DependsOn...), job.SerializeAfter...)
	}
	return edges
}

// planReaches reports whether from runs after to over the plan's edges.
func planReaches(edges map[string][]string, from, to string) bool {
	seen := map[string]bool{}
	stack := []string{from}
	for len(stack) > 0 {
		key := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if key == to {
			return true
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		stack = append(stack, edges[key]...)
	}
	return false
}

// TestPlan_DependentsWaitForTheGeneratedClientWriter is the acceptance of the
// dependents rule against the REAL Go and clientgen manifests. Every job of a
// project that depends on the client runs after the client's generators, the
// rule adds ordering edges to the generators and nothing else, and no
// functional edge moves, so no cache key moves. Without the flag the same plan
// leaves consumer jobs unordered with the generator, which is the race the
// rule closes.
func TestPlan_DependentsWaitForTheGeneratedClientWriter(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "dependents-wait-for-an-output-they-read",
		"every-dependent-job-waits-for-the-generated-client-writer")
	goExtension := loadGoPlannerExtension(t)
	for _, commands := range [][]string{{"clientgen", "build"}, {"lint", "test", "build", "validate"}} {
		t.Run(commands[0]+"-"+commands[len(commands)-1], func(t *testing.T) {
			ws, selected := dependentsWorkspace(t)
			planned, err := Plan(ws, commands, selected,
				[]*extension.ExtensionDescription{goExtension, loadClientgenExtension(t)}, nil, nil, nil)
			if err != nil {
				t.Fatalf("plan %v: %v", commands, err)
			}
			if err := ValidatePlanDAG(planned); err != nil {
				t.Fatalf("plan %v is not a DAG: %v", commands, err)
			}
			before, err := Plan(ws, commands, selected,
				[]*extension.ExtensionDescription{goExtension, clientgenWithoutReadByDependents(t)}, nil, nil, nil)
			if err != nil {
				t.Fatalf("plan %v without readByDependents: %v", commands, err)
			}

			var generators []string
			for _, job := range planned {
				if job.Project.ID == dependentsClient && job.CommandName() == "clientgen" {
					generators = append(generators, job.Key())
				}
			}
			sort.Strings(generators)
			if !slices.Contains(generators, dependentsGenerator) {
				t.Fatalf("clientgen jobs planned for the client = %v, want %s among them", generators, dependentsGenerator)
			}

			edges := planEdges(planned)
			beforeEdges := planEdges(before)
			unorderedBefore := 0
			consumerJobs := 0
			for _, job := range planned {
				if job.Project.ID != dependentsConsumer || jobmodel.IsFinalizerJob(job) {
					continue
				}
				consumerJobs++
				for _, generator := range generators {
					if !planReaches(edges, job.Key(), generator) {
						t.Errorf("%s does not wait for %s: it can compile the client while the generator rewrites it",
							job.Key(), generator)
					}
				}
				if !planReaches(beforeEdges, job.Key(), dependentsGenerator) &&
					!planReaches(beforeEdges, dependentsGenerator, job.Key()) {
					unorderedBefore++
				}
			}
			if consumerJobs == 0 {
				t.Fatalf("no consumer job planned: %v", planKeySet(planned))
			}
			if unorderedBefore == 0 {
				t.Fatalf("without readByDependents every consumer job is already ordered with %s; "+
					"the fixture no longer reproduces the race", dependentsGenerator)
			}

			// The rule adds SerializeAfter edges to a generator and nothing
			// else: DependsOn, the cache-key family, stays as it was.
			beforeByKey := make(map[string]*jobmodel.ScheduledJob, len(before))
			for _, job := range before {
				beforeByKey[job.Key()] = job
			}
			if len(before) != len(planned) {
				t.Fatalf("the flag changed the planned jobs: %d with it, %d without", len(planned), len(before))
			}
			for _, job := range planned {
				previous := beforeByKey[job.Key()]
				if previous == nil {
					t.Fatalf("%s is planned only with the flag", job.Key())
				}
				if !slices.Equal(job.DependsOn, previous.DependsOn) {
					t.Errorf("%s DependsOn moved from %v to %v; the rule adds ordering edges only",
						job.Key(), previous.DependsOn, job.DependsOn)
				}
				for _, key := range previous.SerializeAfter {
					if !slices.Contains(job.SerializeAfter, key) {
						t.Errorf("%s lost the serialize edge to %s", job.Key(), key)
					}
				}
				for _, key := range job.SerializeAfter {
					if !slices.Contains(previous.SerializeAfter, key) && !slices.Contains(generators, key) {
						t.Errorf("%s gained a serialize edge to %s, which is not a client generator", job.Key(), key)
					}
				}
			}
		})
	}
}

// dependentsJob builds one planned job of a project for the focused tests of
// the dependents rule.
func dependentsJob(project *workspace.Project, ext *extension.ExtensionDescription, name string, step *extension.PipelineStep, deps ...string) *jobmodel.ScheduledJob {
	return &jobmodel.ScheduledJob{
		Project:   project,
		Extension: ext,
		JobDef:    &extension.JobDefinition{Name: name, ExtensionName: ext.Name},
		Step:      step,
		DependsOn: deps,
	}
}

// TestPlan_DependentsRuleOrdersOnlyDependentsAndKeepsThePlanADAG pins the edges
// the rule adds and the ones it refuses. A direct and a transitive dependent
// wait for the writer. The writer's own project, an unrelated project and a
// finalizer get no edge, and neither does a job the writer already depends on,
// nor one that already depends on the writer. An output outside the project
// root orders nothing. No DependsOn edge moves, and the plan stays a DAG.
func TestPlan_DependentsRuleOrdersOnlyDependentsAndKeepsThePlanADAG(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "dependents-wait-for-an-output-they-read",
		"the-dependents-rule-orders-only-dependents-and-keeps-the-plan-a-dag")
	ext := &extension.ExtensionDescription{
		Name: "@test/gen",
		Tasks: map[string]extension.TaskDefinition{
			"gen": {Kind: "command", Command: "gen", Declares: &extension.TaskDeclaration{Outputs: map[string]extension.DeclaredOutput{
				"client": {Kind: extension.OutputKindDirectory, PathFrom: "clientOutput", ReadByDependents: true},
			}}},
			"shared": {Kind: "command", Command: "gen", Declares: &extension.TaskDeclaration{Outputs: map[string]extension.DeclaredOutput{
				"client": {Kind: extension.OutputKindDirectory, Root: extension.OutputRootWorkspace, Path: "dist/clients", ReadByDependents: true},
			}}},
			"run": {Kind: "command", Command: "run"},
		},
	}
	client := &workspace.Project{ID: "/client", Name: "client", Path: "client"}
	consumer := &workspace.Project{ID: "/consumer", Name: "consumer", Path: "consumer", Dependencies: []string{"/client"}}
	app := &workspace.Project{ID: "/app", Name: "app", Path: "app", Dependencies: []string{"/consumer"}}
	other := &workspace.Project{ID: "/other", Name: "other", Path: "other"}
	ws := testWorkspace(t.TempDir(), client, consumer, app, other)

	run := &extension.PipelineStep{ID: "run", Task: "run"}
	teardown := &extension.PipelineStep{
		ID: "teardown", Task: "run", RunOn: extensionproto.StepRunOnFinally,
		Finalizes: &extensionproto.FinalizesRelation{Producer: "setup", Consumers: []string{"run"}},
	}
	const writerKey = "/client:clientgen~generate"
	const sharedKey = "/client:clientgen~shared"
	prepare := dependentsJob(consumer, ext, "build~prepare", run)
	writer := dependentsJob(client, ext, "clientgen~generate", &extension.PipelineStep{ID: "generate", Task: "gen"}, prepare.Key())
	shared := dependentsJob(client, ext, "clientgen~shared", &extension.PipelineStep{ID: "shared", Task: "shared"})
	clientCompile := dependentsJob(client, ext, "build~compile", run)
	compile := dependentsJob(consumer, ext, "build~compile", run, prepare.Key())
	check := dependentsJob(consumer, ext, "lint~check", run, writerKey)
	finalizer := dependentsJob(consumer, ext, "test~teardown", teardown)
	appCompile := dependentsJob(app, ext, "build~compile", run)
	otherCompile := dependentsJob(other, ext, "build~compile", run)
	jobs := []*jobmodel.ScheduledJob{prepare, writer, shared, clientCompile, compile, check, finalizer, appCompile, otherCompile}
	if writer.Key() != writerKey || shared.Key() != sharedKey {
		t.Fatalf("writer keys = %s, %s", writer.Key(), shared.Key())
	}
	dependsOn := make(map[string][]string, len(jobs))
	for _, job := range jobs {
		dependsOn[job.Key()] = append([]string(nil), job.DependsOn...)
	}

	orderContractClients(jobs, ws)

	if err := ValidatePlanDAG(jobs); err != nil {
		t.Fatalf("the dependents rule closed a cycle: %v", err)
	}
	for _, job := range []*jobmodel.ScheduledJob{compile, appCompile} {
		if !slices.Contains(job.SerializeAfter, writerKey) {
			t.Errorf("%s SerializeAfter = %v, want it to wait for %s", job.Key(), job.SerializeAfter, writerKey)
		}
	}
	for _, job := range []*jobmodel.ScheduledJob{prepare, clientCompile, check, finalizer, otherCompile} {
		if slices.Contains(job.SerializeAfter, writerKey) {
			t.Errorf("%s waits for %s: %v", job.Key(), writerKey, job.SerializeAfter)
		}
	}
	if len(finalizer.SerializeAfter) != 0 {
		t.Errorf("finalizer %s carries ordering edges %v; the DAG never dispatches it", finalizer.Key(), finalizer.SerializeAfter)
	}
	for _, job := range jobs {
		if slices.Contains(job.SerializeAfter, sharedKey) {
			t.Errorf("%s waits for %s, whose output lies outside the project", job.Key(), sharedKey)
		}
		if !slices.Equal(job.DependsOn, dependsOn[job.Key()]) {
			t.Errorf("%s DependsOn moved from %v to %v", job.Key(), dependsOn[job.Key()], job.DependsOn)
		}
	}
}
