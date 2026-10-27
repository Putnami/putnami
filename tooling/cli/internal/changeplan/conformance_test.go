package changeplan

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"go.putnami.dev/cli/model/jobs"
	ciproto "go.putnami.dev/protocol/ci"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// changePlanCorpus is the protocol's ChangePlan conformance corpus. The CLI
// test reads the committed files rather than a copy, so the producer and the
// published contract are held to one set of documents.
var changePlanCorpus = filepath.Join("..", "..", "..", "..", "protocols", "ci", "fixtures", "change-plan")

type corpusExpectations struct {
	Valid   map[string]string `json:"valid"`
	Invalid map[string]string `json:"invalid"`
}

func readCorpusExpectations(t *testing.T) corpusExpectations {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(changePlanCorpus, "expectations.json"))
	if err != nil {
		t.Fatalf("read the protocol change-plan corpus: %v", err)
	}
	var index corpusExpectations
	if err := json.Unmarshal(data, &index); err != nil {
		t.Fatalf("decode expectations.json: %v", err)
	}
	if len(index.Valid) == 0 || len(index.Invalid) == 0 {
		t.Fatalf("expectations.json lists %d valid and %d invalid plans; the corpus proves nothing", len(index.Valid), len(index.Invalid))
	}
	return index
}

func readCorpusPlan(t *testing.T, kind, name string) (ciproto.ChangePlan, []byte) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(changePlanCorpus, kind, name))
	if err != nil {
		t.Fatal(err)
	}
	var plan ciproto.ChangePlan
	if err := json.Unmarshal(data, &plan); err != nil {
		t.Fatalf("decode %s/%s: %v", kind, name, err)
	}
	return plan, data
}

func encodePlan(t *testing.T, plan ciproto.ChangePlan) []byte {
	t.Helper()
	encoded, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(encoded, '\n')
}

// TestBuildImpactProjectsOntoTheChangePlanCorpus runs the planner data of
// every valid ChangePlan fixture through BuildImpact, the path every emitted
// plan takes, reversed and with duplicates. The protocol's projection of the
// result for the fixture's repository must be the committed bytes and digest,
// so the CLI canonicalizes every member a ChangePlan shares with an ImpactPlan.
func TestBuildImpactProjectsOntoTheChangePlanCorpus(t *testing.T) {
	spectest.Proves(t, "ci/ci-contract", "producers-reproduce-the-corpus", "the-cli-projects-onto-the-change-plan-corpus")
	t.Parallel()
	for name, digest := range readCorpusExpectations(t).Valid {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			plan, data := readCorpusPlan(t, "valid", name)
			built, err := BuildImpact(scrambledInput(plan, corpusCommands))
			if err != nil {
				t.Fatalf("BuildImpact: %v", err)
			}
			projected, err := ciproto.ChangePlanFromImpactPlan(built, plan.Repository)
			if err != nil {
				t.Fatalf("ChangePlanFromImpactPlan: %v", err)
			}
			if projected.Digest != digest {
				t.Fatalf("projected digest = %s, want %s", projected.Digest, digest)
			}
			if encoded := encodePlan(t, projected); !bytes.Equal(encoded, data) {
				t.Fatalf("the projection differs from the corpus:\n--- corpus ---\n%s\n--- projected ---\n%s", data, encoded)
			}
		})
	}
}

// impactPlanCorpus is the protocol's ImpactPlan conformance corpus, read from
// the committed files like changePlanCorpus.
var impactPlanCorpus = filepath.Join("..", "..", "..", "..", "protocols", "ci", "fixtures", "impact-plan")

func readImpactCorpus(t *testing.T, kind, name string) (ciproto.ImpactPlan, []byte) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(impactPlanCorpus, kind, name))
	if err != nil {
		t.Fatal(err)
	}
	var plan ciproto.ImpactPlan
	if err := json.Unmarshal(data, &plan); err != nil {
		t.Fatalf("decode %s/%s: %v", kind, name, err)
	}
	return plan, data
}

func readImpactCorpusExpectations(t *testing.T) corpusExpectations {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(impactPlanCorpus, "expectations.json"))
	if err != nil {
		t.Fatalf("read the protocol impact-plan corpus: %v", err)
	}
	var index corpusExpectations
	if err := json.Unmarshal(data, &index); err != nil {
		t.Fatalf("decode expectations.json: %v", err)
	}
	if len(index.Valid) == 0 || len(index.Invalid) == 0 {
		t.Fatalf("expectations.json lists %d valid and %d invalid plans; the corpus proves nothing", len(index.Valid), len(index.Invalid))
	}
	return index
}

// scrambledImpactInput is scrambledInput for an impact plan: its members in
// reverse order and repeated, and its command list as it is.
func scrambledImpactInput(plan ciproto.ImpactPlan) Input {
	return scrambledInput(ciproto.ChangePlan{
		Generator: plan.Generator, BaseSHA: plan.BaseSHA, HeadSHA: plan.HeadSHA, ChangedFiles: plan.ChangedFiles,
		Impact: plan.Impact, Tasks: plan.Tasks, Cache: plan.Cache,
	}, plan.Commands)
}

func encodeImpactPlan(t *testing.T, plan ciproto.ImpactPlan) []byte {
	t.Helper()
	encoded, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(encoded, '\n')
}

// TestBuildImpactReproducesTheValidCorpus runs every valid protocol impact
// plan back through BuildImpact, the path every emitted impact plan takes,
// from scrambled planner data, which must emit the committed bytes.
func TestBuildImpactReproducesTheValidCorpus(t *testing.T) {
	spectest.Proves(t, "ci/ci-contract", "producers-reproduce-the-corpus", "the-cli-reproduces-the-impact-plan-corpus")
	t.Parallel()
	for name := range readImpactCorpusExpectations(t).Valid {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			plan, data := readImpactCorpus(t, "valid", name)
			built, err := BuildImpact(scrambledImpactInput(plan))
			if err != nil {
				t.Fatalf("BuildImpact: %v", err)
			}
			if encoded := encodeImpactPlan(t, built); !bytes.Equal(encoded, data) {
				t.Fatalf("BuildImpact emitted a different document:\n--- corpus ---\n%s\n--- built ---\n%s", data, encoded)
			}
		})
	}
}

// TestBuildImpactNeverEmitsAnInvalidCorpusPlan holds the producer to every
// impact plan refusal: the protocol refuses each invalid fixture with its
// expected reason, and BuildImpact given the same planner data either refuses
// it for that reason or emits a different, valid plan.
func TestBuildImpactNeverEmitsAnInvalidCorpusPlan(t *testing.T) {
	spectest.Proves(t, "ci/ci-contract", "producers-reproduce-the-corpus", "the-cli-never-emits-a-plan-the-protocol-refuses")
	t.Parallel()
	for name, refusal := range readImpactCorpusExpectations(t).Invalid {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			plan, data := readImpactCorpus(t, "invalid", name)
			if err := ciproto.ValidateImpactPlan(plan); err == nil || err.Error() != refusal {
				t.Fatalf("ValidateImpactPlan = %v, want %q", err, refusal)
			}
			built, err := BuildImpact(scrambledImpactInput(plan))
			if err != nil {
				if err.Error() != refusal {
					t.Fatalf("BuildImpact refused with %q, want %q", err, refusal)
				}
				return
			}
			if bytes.Equal(encodeImpactPlan(t, built), data) {
				t.Fatal("BuildImpact emitted the invalid fixture byte for byte")
			}
			if err := ciproto.ValidateImpactPlan(built); err != nil {
				t.Fatalf("BuildImpact emitted a plan the protocol refuses: %v", err)
			}
		})
	}
}

// corpusCommands is the command list a ChangePlan corpus plan is rebuilt
// for. A ChangePlan does not name its commands, so any canonical list yields
// the same document.
var corpusCommands = []string{"lint", "test", "build", "validate"}

// scrambledInput is the planner data a plan projects, in reverse order and
// with every list entry repeated, for commands.
func scrambledInput(plan ciproto.ChangePlan, commands []string) Input {
	projects := func(in []ciproto.ChangePlanProject) []*workspace.Project {
		out := make([]*workspace.Project, 0, 2*len(in))
		for _, project := range slices.Backward(in) {
			out = append(out, &workspace.Project{ID: project.ID, Name: project.Name, Path: project.Path})
			out = append(out, &workspace.Project{ID: project.ID, Name: project.Name, Path: project.Path})
		}
		return out
	}
	planned := make([]*jobs.ScheduledJob, 0, 2*len(plan.Tasks))
	for _, task := range slices.Backward(plan.Tasks) {
		planned = append(planned, plannedTask(task), plannedTask(task))
	}
	cache := plan.Cache
	cache.Entries = slices.Clone(cache.Entries)
	slices.Reverse(cache.Entries)
	return Input{
		Generator:        plan.Generator,
		Commands:         commands,
		BaseSHA:          plan.BaseSHA,
		HeadSHA:          plan.HeadSHA,
		ChangedFiles:     doubledReverse(plan.ChangedFiles),
		DirectProjects:   projects(plan.Impact.DirectProjects),
		ImpactedProjects: projects(plan.Impact.Projects),
		Planned:          planned,
		Cache:            cache,
	}
}

func plannedTask(task ciproto.ChangePlanTask) *jobs.ScheduledJob {
	identity := task.Identity
	heavy := task.ResourceClass.Heavy
	weight := task.ResourceClass.CPUWeight
	resources := func(in []ciproto.ChangePlanResource) []extension.ResourceRef {
		out := make([]extension.ResourceRef, 0, len(in))
		for _, resource := range slices.Backward(in) {
			out = append(out, extension.ResourceRef{ID: resource.ID, Scope: resource.Scope})
		}
		return out
	}
	return &jobs.ScheduledJob{
		Project:   &workspace.Project{ID: identity.Project.ID, Name: identity.Project.Name},
		Extension: &extension.ExtensionDescription{Name: identity.Provider.Extension, Version: identity.Provider.Version},
		JobDef: &extension.JobDefinition{
			Name:           identity.Task.Name,
			CommandName:    identity.Task.Command,
			TimeoutMs:      task.DeadlineMS,
			Heavy:          &heavy,
			CPUWeight:      &weight,
			Reads:          resources(task.ResourceClass.Reads),
			Writes:         resources(task.ResourceClass.Writes),
			ContractDigest: task.ContractDigest,
		},
		DependsOn:      doubledReverse(task.DependsOn),
		SerializeAfter: doubledReverse(task.SerializeAfter),
		Identity:       &identity,
	}
}

func doubledReverse(values []string) []string {
	out := make([]string, 0, 2*len(values))
	for _, value := range slices.Backward(values) {
		out = append(out, value, value)
	}
	return out
}
