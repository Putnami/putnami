package planner

import (
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"

	jobmodel "go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs/cachekey"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// pairwiseOverlapLines is the scan overlappingOutputLines replaced: every pair
// of a root group, later against each earlier one. It is the oracle the
// indexed scan must reproduce line for line.
func pairwiseOverlapLines(planned []*jobmodel.ScheduledJob, ws *workspace.Workspace, limit int) []string {
	owners := cachekey.DeclaredOutputOwners(planned, ws)
	if len(owners) < 2 {
		return nil
	}
	byRoot := map[string][]cachekey.DeclaredOutputOwner{}
	var order []string
	for _, owner := range owners {
		if _, seen := byRoot[owner.Ref.Root]; !seen {
			order = append(order, owner.Ref.Root)
		}
		byRoot[owner.Ref.Root] = append(byRoot[owner.Ref.Root], owner)
	}
	var lines []string
	total := 0
	for _, root := range order {
		group := byRoot[root]
		for i := 1; i < len(group); i++ {
			for j := 0; j < i; j++ {
				later, earlier := group[i], group[j]
				if later.Job == earlier.Job && later.ID == earlier.ID {
					continue
				}
				if later.ID == earlier.ID && sameManifestTask(later.Job, earlier.Job) {
					continue
				}
				laterRef, earlierRef := later.Ref, earlier.Ref
				if sameManifestTask(later.Job, earlier.Job) {
					laterRef, earlierRef = laterRef.WithoutCarveOuts(), earlierRef.WithoutCarveOuts()
				}
				if !extension.OutputsOverlap(laterRef, earlierRef) {
					continue
				}
				total++
				if len(lines) < limit {
					lines = append(lines, fmt.Sprintf("  %s output %q (%s) overlaps %s output %q (%s)",
						later.Job.Key(), later.ID, describeOutputRef(later.Ref),
						earlier.Job.Key(), earlier.ID, describeOutputRef(earlier.Ref)))
				}
			}
		}
	}
	if total > len(lines) {
		lines = append(lines, fmt.Sprintf("  ... and %d more overlapping outputs", total-len(lines)))
	}
	return lines
}

// randomOverlapPlan builds a plan over two projects whose tasks declare outputs
// drawn from a small vocabulary of nested, differently cased and ported paths,
// some of them ceding a subpath, so equal paths, ancestors, ports and
// carve-outs all occur, inside one task and across tasks.
func randomOverlapPlan(random *rand.Rand) ([]*jobmodel.ScheduledJob, *workspace.Workspace) {
	projects := []*workspace.Project{
		{ID: "/app", Name: "app", Path: "app"},
		{ID: "/app/lib", Name: "lib", Path: "app/lib"},
	}
	ws := &workspace.Workspace{Root: "/ws", Projects: projects}
	paths := []string{".gen", ".GEN/client", ".gen/client/a.ts", ".gen/clients", "dist", "dist/x", "Dist/X/y", "lib", "lib/.gen", "out/a/b/c"}
	roots := []string{extension.OutputRootProject, extension.OutputRootWorkspace}
	declared := func() extension.DeclaredOutput {
		output := extension.DeclaredOutput{Kind: extension.OutputKindDirectory, Root: roots[random.Intn(len(roots))]}
		if random.Intn(6) == 0 {
			output.PathFrom = "port"
			return output
		}
		output.Path = paths[random.Intn(len(paths))]
		if random.Intn(4) == 0 {
			output.Excludes = []string{paths[random.Intn(len(paths))]}
		}
		return output
	}
	var planned []*jobmodel.ScheduledJob
	tasks := 2 + random.Intn(6)
	for task := 0; task < tasks; task++ {
		outputs := map[string]extension.DeclaredOutput{}
		for count := 1 + random.Intn(3); count > 0; count-- {
			outputs[fmt.Sprintf("out%d", random.Intn(3))] = declared()
		}
		taskName := fmt.Sprintf("task-%d", task)
		ext := &extension.ExtensionDescription{Name: "@test/random", Tasks: map[string]extension.TaskDefinition{
			taskName: {Kind: "command", Command: "true", Declares: &extension.TaskDeclaration{Outputs: outputs}},
		}}
		project := projects[random.Intn(len(projects))]
		// One manifest task is sometimes scheduled under two commands: one owner.
		for _, command := range []string{"build", "test"}[:1+random.Intn(2)] {
			step := fmt.Sprintf("s%d", task)
			planned = append(planned, &jobmodel.ScheduledJob{
				Project:   project,
				Extension: ext,
				JobDef:    &extension.JobDefinition{Name: command + "~" + step, CommandName: command, StepID: step},
				Step:      &extension.PipelineStep{ID: step, Task: taskName},
			})
		}
	}
	return planned, ws
}

// TestOverlappingOutputLines_MatchesThePairwiseScan pins that the indexed scan
// rejects every overlap the pairwise scan rejects, naming the same owners, in
// the same order, and with the same truncation.
func TestOverlappingOutputLines_MatchesThePairwiseScan(t *testing.T) {
	t.Parallel()
	random := rand.New(rand.NewSource(1587))
	overlapping := 0
	for trial := 0; trial < 2000; trial++ {
		planned, ws := randomOverlapPlan(random)
		limit := 1 + random.Intn(8)
		want := pairwiseOverlapLines(planned, ws, limit)
		got := overlappingOutputLines(planned, ws, limit)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("trial %d: indexed scan\n%s\npairwise scan\n%s", trial, strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
		if len(want) > 0 {
			overlapping++
		}
	}
	if overlapping < 200 {
		t.Fatalf("only %d of 2000 plans overlap; the fixture no longer exercises the check", overlapping)
	}
}
