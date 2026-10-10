package ci

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
)

// impactPlanExpectations is fixtures/impact-plan/expectations.json, the corpus
// index: for every valid plan, the fixtures/change-plan/valid plan it projects
// to under that plan's repository, and the refusal of every invalid one.
type impactPlanExpectations struct {
	Valid   map[string]string `json:"valid"`
	Invalid map[string]string `json:"invalid"`
}

func readImpactPlanExpectations(t *testing.T) impactPlanExpectations {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(readFixture(t, "impact-plan", "expectations.json")))
	decoder.DisallowUnknownFields()
	var index impactPlanExpectations
	if err := decoder.Decode(&index); err != nil {
		t.Fatalf("decode expectations.json: %v", err)
	}
	return index
}

// readImpactPlanFixture decodes one fixture and requires the file to be the
// exact indented encoding of the decoded plan, which pins the wire member
// names, their order, and which members are omitted when empty.
func readImpactPlanFixture(t *testing.T, kind, name string) ImpactPlan {
	t.Helper()
	data := readFixture(t, filepath.Join("impact-plan", kind), name)
	var plan ImpactPlan
	if err := json.Unmarshal(data, &plan); err != nil {
		t.Fatalf("decode %s/%s: %v", kind, name, err)
	}
	encoded, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		t.Fatalf("encode %s/%s: %v", kind, name, err)
	}
	if !bytes.Equal(append(encoded, '\n'), data) {
		t.Fatalf("%s/%s is not the wire encoding of the plan it holds:\n--- file ---\n%s\n--- encoded ---\n%s",
			kind, name, data, encoded)
	}
	return plan
}

func TestImpactPlanCorpusMatchesExpectations(t *testing.T) {
	index := readImpactPlanExpectations(t)
	for kind, listed := range map[string]map[string]string{"valid": index.Valid, "invalid": index.Invalid} {
		entries, err := os.ReadDir(filepath.Join("fixtures", "impact-plan", kind))
		if err != nil {
			t.Fatal(err)
		}
		var onDisk, names []string
		for _, entry := range entries {
			onDisk = append(onDisk, entry.Name())
		}
		for name := range listed {
			names = append(names, name)
		}
		sort.Strings(names)
		if len(onDisk) == 0 || !slices.Equal(onDisk, names) {
			t.Fatalf("fixtures/impact-plan/%s holds %v but expectations.json lists %v", kind, onDisk, names)
		}
	}
}

// TestImpactPlanValidCorpusProjectsOntoTheChangePlanCorpus holds the one
// projection to the frozen ChangePlan v1 corpus: every valid impact plan,
// projected with the repository of the ChangePlan fixture it names, must be
// that fixture byte for byte, digest included.
func TestImpactPlanValidCorpusProjectsOntoTheChangePlanCorpus(t *testing.T) {
	index := readImpactPlanExpectations(t)
	changePlans := readChangePlanExpectations(t)
	for name, changePlanName := range index.Valid {
		t.Run(name, func(t *testing.T) {
			plan := readImpactPlanFixture(t, "valid", name)
			if err := ValidateImpactPlan(plan); err != nil {
				t.Fatalf("ValidateImpactPlan: %v", err)
			}
			want := readFixture(t, filepath.Join("change-plan", "valid"), changePlanName)
			repository := readChangePlanFixture(t, "valid", changePlanName).Repository
			projected, err := ChangePlanFromImpactPlan(plan, repository)
			if err != nil {
				t.Fatalf("ChangePlanFromImpactPlan: %v", err)
			}
			encoded, err := json.MarshalIndent(projected, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(append(encoded, '\n'), want) {
				t.Fatalf("projection differs from change-plan/valid/%s:\n--- want ---\n%s\n--- got ---\n%s",
					changePlanName, want, encoded)
			}
			if digest := changePlans.Valid[changePlanName]; projected.Digest != digest {
				t.Fatalf("projected digest = %s, want %s", projected.Digest, digest)
			}
		})
	}
}

func TestImpactPlanInvalidCorpus(t *testing.T) {
	index := readImpactPlanExpectations(t)
	for name, refusal := range index.Invalid {
		t.Run(name, func(t *testing.T) {
			err := ValidateImpactPlan(readImpactPlanFixture(t, "invalid", name))
			if err == nil || err.Error() != refusal {
				t.Fatalf("ValidateImpactPlan = %v, want %q", err, refusal)
			}
		})
	}
}

// TestValidateImpactPlanRefusals holds each refusal to its own defect: every
// case changes one member of the golden plan.
func TestValidateImpactPlanRefusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*ImpactPlan)
		want   string
	}{
		{"generator name", func(p *ImpactPlan) { p.Generator.Name = "" }, "impact plan generator is incomplete"},
		{"nil commands", func(p *ImpactPlan) { p.Commands = nil }, "impact plan commands are not canonical"},
		{"empty command", func(p *ImpactPlan) { p.Commands = []string{"lint", ""} }, "impact plan commands are not canonical"},
		{"space in command", func(p *ImpactPlan) { p.Commands = []string{" lint"} }, "impact plan commands are not canonical"},
		{"tab in command", func(p *ImpactPlan) { p.Commands = []string{"lint\ttest"} }, "impact plan commands are not canonical"},
		{"uppercase head", func(p *ImpactPlan) { p.HeadSHA = strings.ToUpper(p.HeadSHA) },
			"impact plan revisions must be full lowercase commit IDs"},
		{"unsorted changed files", func(p *ImpactPlan) { slices.Reverse(p.ChangedFiles) }, "impact plan changedFiles is not canonical"},
		{"duplicate project", func(p *ImpactPlan) {
			p.Impact.Projects = append(p.Impact.Projects, p.Impact.Projects[len(p.Impact.Projects)-1])
		}, "impact plan project lists are not canonical"},
		{"deadline", func(p *ImpactPlan) { p.Tasks[0].DeadlineMS = 0 }, "impact plan tasks are not canonical"},
		{"unknown cache status", func(p *ImpactPlan) { p.Cache.Status = "warm" }, `unknown cache summary status "warm"`},
		{"disabled with entries", func(p *ImpactPlan) { p.Cache.Status = "disabled" }, "disabled cache summary has entries"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := readImpactPlanFixture(t, "valid", "golden.json")
			tc.mutate(&plan)
			if err := ValidateImpactPlan(plan); err == nil || err.Error() != tc.want {
				t.Fatalf("ValidateImpactPlan = %v, want %q", err, tc.want)
			}
		})
	}
}

// TestValidateImpactPlanKeepsTheRequestedCommandOrder holds that the command
// list is ordered by request, not sorted: a reversed list is still valid.
func TestValidateImpactPlanKeepsTheRequestedCommandOrder(t *testing.T) {
	plan := readImpactPlanFixture(t, "valid", "golden.json")
	slices.Reverse(plan.Commands)
	if err := ValidateImpactPlan(plan); err != nil {
		t.Fatalf("ValidateImpactPlan of a reversed command list = %v, want nil", err)
	}
}

// TestChangePlanFromImpactPlanRefusals holds the projection's own refusals and
// its delegation: the impact plan's version and command list are checked as
// the impact plan's, every shared member as the resulting ChangePlan's.
func TestChangePlanFromImpactPlanRefusals(t *testing.T) {
	repository := ChangePlanRepository{Remote: "origin", URL: "https://example.test/org/repo.git"}
	for _, tc := range []struct {
		name       string
		mutate     func(*ImpactPlan)
		repository ChangePlanRepository
		want       string
	}{
		{"version", func(p *ImpactPlan) { p.Version = 2 }, repository, "unsupported impact plan version 2"},
		{"duplicate commands", func(p *ImpactPlan) { p.Commands = []string{"lint", "lint"} }, repository,
			"impact plan commands are not canonical"},
		{"credentials", func(*ImpactPlan) {}, ChangePlanRepository{Remote: "origin", URL: "https://token@example.test/org/repo.git"},
			"change plan repository identity is incomplete or contains credentials"},
		{"no remote", func(*ImpactPlan) {}, ChangePlanRepository{URL: repository.URL},
			"change plan repository identity is incomplete or contains credentials"},
		{"short base", func(p *ImpactPlan) { p.BaseSHA = p.BaseSHA[:12] }, repository,
			"change plan revisions must be full lowercase commit IDs"},
		{"unsorted tasks", func(p *ImpactPlan) { slices.Reverse(p.Tasks) }, repository, "change plan tasks are not canonical"},
		{"generator", func(p *ImpactPlan) { p.Generator.Version = "" }, repository, "change plan generator is incomplete"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := readImpactPlanFixture(t, "valid", "golden.json")
			tc.mutate(&plan)
			if _, err := ChangePlanFromImpactPlan(plan, tc.repository); err == nil || err.Error() != tc.want {
				t.Fatalf("ChangePlanFromImpactPlan = %v, want %q", err, tc.want)
			}
		})
	}
}

// TestChangePlanFromImpactPlanDerivesTransitiveDependentsAndEmptyLists holds
// the projection to one output per plan: the transitive dependents are always
// derived from the closure, and a nil list projects exactly like an empty one,
// so neither can change the digest.
func TestChangePlanFromImpactPlanDerivesTransitiveDependentsAndEmptyLists(t *testing.T) {
	repository := ChangePlanRepository{Remote: "origin", URL: "https://example.test/org/repo.git"}
	golden := readImpactPlanFixture(t, "valid", "golden.json")
	want, err := ChangePlanFromImpactPlan(golden, repository)
	if err != nil {
		t.Fatal(err)
	}
	for _, transitive := range [][]ChangePlanProject{nil, {}, golden.Impact.TransitiveDependents[:1]} {
		plan := readImpactPlanFixture(t, "valid", "golden.json")
		plan.Impact.TransitiveDependents = transitive
		got, err := ChangePlanFromImpactPlan(plan, repository)
		if err != nil {
			t.Fatalf("transitive dependents %v: %v", transitive, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("transitive dependents %v projected to %+v, want %+v", transitive, got.Impact, want.Impact)
		}
	}

	empty := readImpactPlanFixture(t, "valid", "sha256-no-tasks.json")
	spelledNil := empty
	spelledNil.Impact, spelledNil.Tasks = ChangePlanImpact{}, nil
	fromEmpty, err := ChangePlanFromImpactPlan(empty, repository)
	if err != nil {
		t.Fatal(err)
	}
	fromNil, err := ChangePlanFromImpactPlan(spelledNil, repository)
	if err != nil {
		t.Fatal(err)
	}
	emptyBytes, _ := json.Marshal(fromEmpty)
	nilBytes, _ := json.Marshal(fromNil)
	if !bytes.Equal(emptyBytes, nilBytes) || fromEmpty.Digest != fromNil.Digest {
		t.Fatalf("a nil list projected differently from an empty one:\n%s\n%s", emptyBytes, nilBytes)
	}
	if !bytes.Contains(nilBytes, []byte(`"tasks":[]`)) || !bytes.Contains(nilBytes, []byte(`"projects":[]`)) {
		t.Fatalf("a nil list must project as []: %s", nilBytes)
	}
}

// TestChangePlanFromImpactPlanSharesNoListWithItsInput holds the projection to
// a pure function: changing the ChangePlan it returns leaves the impact plan
// as it was, so a digest stamped on one can never be invalidated through the
// other.
func TestChangePlanFromImpactPlanSharesNoListWithItsInput(t *testing.T) {
	plan := readImpactPlanFixture(t, "valid", "golden.json")
	before, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	projected, err := ChangePlanFromImpactPlan(plan, ChangePlanRepository{Remote: "origin", URL: "https://example.test/org/repo.git"})
	if err != nil {
		t.Fatal(err)
	}
	projected.ChangedFiles[0] = "changed"
	projected.Impact.DirectProjects[0].ID = "/changed"
	projected.Impact.Projects[0].ID = "/changed"
	projected.Tasks[0].Identity.Key = "changed"
	projected.Tasks[0].ResourceClass.Reads[0].ID = "changed"
	projected.Tasks[1].DependsOn[0] = "changed"
	projected.Tasks[1].ResourceClass.Writes[0].ID = "changed"
	projected.Tasks[0].SerializeAfter[0] = "changed"
	projected.Cache.Entries[0].Key = "changed"
	after, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("changing the projection changed its input:\n%s\n%s", before, after)
	}
}
