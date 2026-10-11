package changeplan

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	ciproto "go.putnami.dev/protocol/ci"
	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

func TestBuildCanonicalDigestIsStable(t *testing.T) {
	app := &workspace.Project{ID: "/app", Name: "app", Path: "apps/app"}
	lib := &workspace.Project{ID: "/lib", Name: "lib", Path: "libs/lib"}
	first, err := Build(changePlanInput(app, lib, []string{"apps/app/main.go", "libs/lib/lib.go"}, ciproto.ChangePlanCache{
		Status:  "available",
		Entries: []ciproto.ChangePlanCacheEntry{{TaskKey: "/lib:build", Key: "key-lib"}, {TaskKey: "/app:lint", Key: "key-app"}},
	}), testRepository)
	if err != nil {
		t.Fatalf("Build first: %v", err)
	}
	second, err := Build(changePlanInput(app, lib, []string{"libs/lib/lib.go", "apps/app/main.go", "apps/app/main.go"}, ciproto.ChangePlanCache{
		Status:  "available",
		Entries: []ciproto.ChangePlanCacheEntry{{TaskKey: "/app:lint", Key: "key-app"}, {TaskKey: "/lib:build", Key: "key-lib"}},
	}), testRepository)
	if err != nil {
		t.Fatalf("Build second: %v", err)
	}
	firstJSON, err := json.Marshal(first)
	if err != nil {
		t.Fatalf("marshal first: %v", err)
	}
	secondJSON, err := json.Marshal(second)
	if err != nil {
		t.Fatalf("marshal second: %v", err)
	}
	if string(firstJSON) != string(secondJSON) {
		t.Fatalf("equivalent inputs emitted different documents:\n%s\n%s", firstJSON, secondJSON)
	}
	if first.Digest != second.Digest {
		t.Fatalf("digest differs: %s != %s", first.Digest, second.Digest)
	}
	if err := ciproto.ValidateChangePlan(first); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	recomputed, err := ciproto.RecomputeChangePlanDigest(first)
	if err != nil {
		t.Fatalf("RecomputeChangePlanDigest: %v", err)
	}
	if recomputed != first.Digest {
		t.Fatalf("recomputed digest = %s, want %s", recomputed, first.Digest)
	}
}

func TestDigestExcludesAdvisoryCachePresence(t *testing.T) {
	app := &workspace.Project{ID: "/app", Name: "app", Path: "apps/app"}
	lib := &workspace.Project{ID: "/lib", Name: "lib", Path: "libs/lib"}
	cold, err := Build(changePlanInput(app, lib, []string{"apps/app/main.go"}, ciproto.ChangePlanCache{
		Status:  "available",
		Entries: []ciproto.ChangePlanCacheEntry{{TaskKey: "/app:lint", Key: "key-app", Present: false}},
	}), testRepository)
	if err != nil {
		t.Fatalf("Build cold: %v", err)
	}
	warm, err := Build(changePlanInput(app, lib, []string{"apps/app/main.go"}, ciproto.ChangePlanCache{
		Status:  "available",
		Entries: []ciproto.ChangePlanCacheEntry{{TaskKey: "/app:lint", Key: "key-app", Present: true}},
	}), testRepository)
	if err != nil {
		t.Fatalf("Build warm: %v", err)
	}
	if cold.Digest != warm.Digest {
		t.Fatalf("cache presence changed digest: %s != %s", cold.Digest, warm.Digest)
	}
	if err := ciproto.ValidateChangePlan(cold); err != nil {
		t.Fatalf("Validate cold: %v", err)
	}
	if err := ciproto.ValidateChangePlan(warm); err != nil {
		t.Fatalf("Validate warm: %v", err)
	}
}

func TestBuildDoesNotSerializeInvocationDetails(t *testing.T) {
	app := &workspace.Project{ID: "/app", Name: "app", Path: "apps/app"}
	job := testJob(app, "lint~secret", 0)
	job.JobDef.Command = "private-command"
	job.JobDef.Args = []string{"--token", "super-secret-token"}
	job.JobDef.Cwd = "private-working-directory"
	job.JobDef.Env = map[string]string{"API_TOKEN": "super-secret-token"}
	document, err := Build(Input{
		Generator:        ciproto.ChangePlanGenerator{Name: "putnami", Version: "test"},
		Commands:         []string{"lint"},
		BaseSHA:          strings.Repeat("a", 40),
		HeadSHA:          strings.Repeat("b", 40),
		ChangedFiles:     []string{"apps/app/main.go"},
		DirectProjects:   []*workspace.Project{app},
		ImpactedProjects: []*workspace.Project{app},
		Planned:          []*jobs.ScheduledJob{job},
		Cache:            ciproto.ChangePlanCache{Status: "disabled"},
	}, testRepository)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("marshal document: %v", err)
	}
	for _, forbidden := range []string{"private-command", "super-secret-token", "private-working-directory", "API_TOKEN"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("document leaked %q: %s", forbidden, encoded)
		}
	}
}

func TestBuildRejectsTaskWithoutHardDeadline(t *testing.T) {
	app := &workspace.Project{ID: "/app", Name: "app", Path: "apps/app"}
	in := changePlanInput(app, app, []string{"apps/app/main.go"}, ciproto.ChangePlanCache{Status: "disabled"})
	in.Planned = []*jobs.ScheduledJob{testJob(app, "lint~unbounded", -1)}
	if _, err := Build(in, testRepository); err == nil || !strings.Contains(err.Error(), "no hard deadline") {
		t.Fatalf("Build error = %v, want hard-deadline rejection", err)
	}
}

// TestBuildIsTheChangePlanOfBuildImpact holds the one projection: the
// ChangePlan Build emits is exactly ChangePlanFromImpactPlan of the ImpactPlan
// BuildImpact emits for the same planner data, and the ImpactPlan keeps the
// planned command list in its order.
func TestBuildIsTheChangePlanOfBuildImpact(t *testing.T) {
	spectest.Proves(t, "cli/impact-plan", "one-projection", "build-is-the-change-plan-of-build-impact")
	app := &workspace.Project{ID: "/app", Name: "app", Path: "apps/app"}
	lib := &workspace.Project{ID: "/lib", Name: "lib", Path: "libs/lib"}
	in := changePlanInput(app, lib, []string{"libs/lib/lib.go", "apps/app/main.go"}, ciproto.ChangePlanCache{Status: "disabled"})
	impact, err := BuildImpact(in)
	if err != nil {
		t.Fatalf("BuildImpact: %v", err)
	}
	if err := ciproto.ValidateImpactPlan(impact); err != nil {
		t.Fatalf("ValidateImpactPlan: %v", err)
	}
	if strings.Join(impact.Commands, ",") != "lint,build" {
		t.Fatalf("commands = %v, want the planned order lint,build", impact.Commands)
	}
	if len(impact.Impact.TransitiveDependents) != 1 || impact.Impact.TransitiveDependents[0].ID != "/lib" {
		t.Fatalf("transitive dependents = %#v, want /lib", impact.Impact.TransitiveDependents)
	}
	projected, err := ciproto.ChangePlanFromImpactPlan(impact, testRepository)
	if err != nil {
		t.Fatalf("ChangePlanFromImpactPlan: %v", err)
	}
	built, err := Build(in, testRepository)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	projectedJSON, err := json.Marshal(projected)
	if err != nil {
		t.Fatal(err)
	}
	builtJSON, err := json.Marshal(built)
	if err != nil {
		t.Fatal(err)
	}
	if string(projectedJSON) != string(builtJSON) {
		t.Fatalf("Build and the projection of BuildImpact differ:\n%s\n%s", builtJSON, projectedJSON)
	}
}

// TestBuildImpactRefusesWhatTheProtocolRefuses holds BuildImpact to
// ValidateImpactPlan: a command list the planner did not name, or names twice,
// is refused rather than emitted.
func TestBuildImpactRefusesWhatTheProtocolRefuses(t *testing.T) {
	spectest.Proves(t, "cli/impact-plan", "one-projection", "an-invalid-plan-is-never-emitted")
	app := &workspace.Project{ID: "/app", Name: "app", Path: "apps/app"}
	for _, commands := range [][]string{nil, {"lint", "lint"}, {"lint,test"}} {
		in := changePlanInput(app, app, []string{"apps/app/main.go"}, ciproto.ChangePlanCache{Status: "disabled"})
		in.Commands = commands
		if _, err := BuildImpact(in); err == nil || err.Error() != "impact plan commands are not canonical" {
			t.Fatalf("commands %q: BuildImpact error = %v, want the protocol's command refusal", commands, err)
		}
	}
	in := changePlanInput(app, app, []string{"apps/app/main.go"}, ciproto.ChangePlanCache{Status: "disabled"})
	in.BaseSHA = "abc"
	if _, err := BuildImpact(in); err == nil || err.Error() != "impact plan revisions must be full lowercase commit IDs" {
		t.Fatalf("BuildImpact error = %v, want the protocol's revision refusal", err)
	}
	in.Planned = []*jobs.ScheduledJob{{Project: app}}
	if _, err := BuildImpact(in); err == nil || err.Error() != "impact plan contains incomplete planned task" {
		t.Fatalf("BuildImpact error = %v, want the projection's incomplete-task refusal", err)
	}
}

// TestProjectionRefusalsNameTheDocument holds every refusal of the projection
// to the document it was building: BuildImpact refuses as the impact plan,
// while Build and ProjectTasks keep the change plan's messages byte for byte.
func TestProjectionRefusalsNameTheDocument(t *testing.T) {
	t.Parallel()
	app := &workspace.Project{ID: "/app", Name: "app", Path: "apps/app"}
	conflicting := &workspace.Project{ID: "/app", Name: "other", Path: "apps/app"}
	slower := testJob(app, "lint", 30_000)
	for _, tc := range []struct {
		name    string
		edit    func(*Input)
		message string
	}{
		{"incomplete project identity", func(in *Input) {
			in.DirectProjects = []*workspace.Project{{ID: "/app"}}
		}, "%s project identity is incomplete"},
		{"conflicting project identities", func(in *Input) {
			in.ImpactedProjects = []*workspace.Project{app, conflicting}
		}, `%s has conflicting project identities for "/app"`},
		{"incomplete planned task", func(in *Input) {
			in.Planned = []*jobs.ScheduledJob{{Project: app}}
		}, "%s contains incomplete planned task"},
		{"incomplete task identity", func(in *Input) {
			job := testJob(app, "lint", 0)
			job.Identity = &protocolcli.TaskIdentity{}
			in.Planned = []*jobs.ScheduledJob{job}
		}, "%s contains incomplete task identity"},
		{"conflicting tasks", func(in *Input) {
			in.Planned = []*jobs.ScheduledJob{testJob(app, "lint", 0), slower}
		}, `%s has conflicting tasks for "` + slower.TypedIdentity().Key + `"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			in := changePlanInput(app, app, []string{"apps/app/main.go"}, ciproto.ChangePlanCache{Status: "disabled"})
			tc.edit(&in)
			if _, err := BuildImpact(in); err == nil || err.Error() != fmt.Sprintf(tc.message, "impact plan") {
				t.Errorf("BuildImpact error = %v, want %q", err, fmt.Sprintf(tc.message, "impact plan"))
			}
			if _, err := Build(in, testRepository); err == nil || err.Error() != fmt.Sprintf(tc.message, "change plan") {
				t.Errorf("Build error = %v, want %q", err, fmt.Sprintf(tc.message, "change plan"))
			}
			if !strings.Contains(tc.message, "task") {
				return
			}
			if _, err := ProjectTasks(in.Planned); err == nil || err.Error() != fmt.Sprintf(tc.message, "change plan") {
				t.Errorf("ProjectTasks error = %v, want %q", err, fmt.Sprintf(tc.message, "change plan"))
			}
		})
	}
}

// testRepository is the repository identity every ChangePlan in these tests
// is built for.
var testRepository = ciproto.ChangePlanRepository{Remote: "origin", URL: "https://example.test/org/repo.git"}

func changePlanInput(app, lib *workspace.Project, changed []string, cache ciproto.ChangePlanCache) Input {
	return Input{
		Generator:        ciproto.ChangePlanGenerator{Name: "putnami", Version: "test"},
		Commands:         []string{"lint", "build"},
		BaseSHA:          strings.Repeat("a", 40),
		HeadSHA:          strings.Repeat("b", 40),
		ChangedFiles:     changed,
		DirectProjects:   []*workspace.Project{app},
		ImpactedProjects: []*workspace.Project{lib, app},
		Planned:          []*jobs.ScheduledJob{testJob(lib, "build", 12_000), testJob(app, "lint", 0)},
		Cache:            cache,
	}
}

func testJob(project *workspace.Project, name string, timeout int) *jobs.ScheduledJob {
	return &jobs.ScheduledJob{
		Project:   project,
		Extension: &extension.ExtensionDescription{Name: "@putnami/test", Version: "1.0.0"},
		JobDef: &extension.JobDefinition{
			Name:           name,
			CommandName:    strings.Split(name, "~")[0],
			TimeoutMs:      timeout,
			ContractDigest: "tc1:test-contract",
			Reads:          []extension.ResourceRef{{ID: extension.ResourceIDSources, Scope: extension.ResourceScopeProject}},
		},
	}
}
