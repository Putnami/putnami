package changeplan

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	ciproto "go.putnami.dev/protocol/ci"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs"
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

// TestBuildReproducesTheValidCorpus runs every valid protocol fixture back
// through Build, the path every emitted plan takes. Build receives the
// fixture's planner data reversed and with duplicates, so it must canonicalize
// it into the exact committed bytes and digest.
func TestBuildReproducesTheValidCorpus(t *testing.T) {
	t.Parallel()
	for name, digest := range readCorpusExpectations(t).Valid {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			plan, data := readCorpusPlan(t, "valid", name)
			built, err := Build(scrambledInput(plan))
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			if built.Digest != digest {
				t.Fatalf("Build digest = %s, want %s", built.Digest, digest)
			}
			if encoded := encodePlan(t, built); !bytes.Equal(encoded, data) {
				t.Fatalf("Build emitted a different document:\n--- corpus ---\n%s\n--- built ---\n%s", data, encoded)
			}
		})
	}
}

// TestBuildNeverEmitsAnInvalidCorpusPlan holds the producer to every refusal:
// the protocol refuses each invalid fixture with its expected reason, and
// Build given the same planner data either refuses it for that reason or
// emits a different, valid plan.
func TestBuildNeverEmitsAnInvalidCorpusPlan(t *testing.T) {
	t.Parallel()
	for name, refusal := range readCorpusExpectations(t).Invalid {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			plan, data := readCorpusPlan(t, "invalid", name)
			if err := ciproto.ValidateChangePlan(plan); err == nil || err.Error() != refusal {
				t.Fatalf("ValidateChangePlan = %v, want %q", err, refusal)
			}
			built, err := Build(scrambledInput(plan))
			if err != nil {
				if err.Error() != refusal {
					t.Fatalf("Build refused with %q, want %q", err, refusal)
				}
				return
			}
			if bytes.Equal(encodePlan(t, built), data) {
				t.Fatal("Build emitted the invalid fixture byte for byte")
			}
			if err := ciproto.ValidateChangePlan(built); err != nil {
				t.Fatalf("Build emitted a plan the protocol refuses: %v", err)
			}
		})
	}
}

// scrambledInput is the planner data a plan projects, in reverse order and
// with every list entry repeated.
func scrambledInput(plan ciproto.ChangePlan) Input {
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
		Repository:       plan.Repository,
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
